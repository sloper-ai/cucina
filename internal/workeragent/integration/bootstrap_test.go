// SPDX-License-Identifier: FSL-1.1-ALv2

// Package integration_test exercises `cucina-worker-agent bootstrap` end to
// end on localhost: a fake IMDS (IMDSv2), an in-process EnrollmentService over
// TLS built from the generated stubs, the real Buildbarn renderer and the real
// filesystem below a temporary directory.
package integration_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/workeragent"
	"github.com/sloper-ai/cucina/internal/workeragent/agenttest"
	"github.com/sloper-ai/cucina/internal/workeragent/bootdata"
	"github.com/sloper-ai/cucina/internal/workeragent/hostos"
	"github.com/sloper-ai/cucina/internal/workeragent/imds"
	"github.com/sloper-ai/cucina/internal/workeragent/imds/imdsfake"
)

const (
	instanceID = "i-0123456789abcdef0"
	enrollName = "enroll.cucina.test"
	identity   = `{"accountId":"000000000000","architecture":"x86_64","availabilityZone":"us-west-1b",` +
		`"imageId":"ami-0123456789abcdef0","instanceId":"` + instanceID + `","instanceType":"m6id.xlarge",` +
		`"pendingTime":"2026-10-02T09:59:30Z","privateIp":"192.0.2.10","region":"us-west-1","version":"2017-09-30"}`
)

// enrollBehaviour scripts the fake controller.
type enrollBehaviour struct {
	fail       []error
	failAlways error
	// foreignKey makes the controller certify a different key than the CSR's.
	foreignKey bool
}

// fakeEnrollment is an in-process EnrollmentService built from the generated
// stubs: it fails the first len(fail) calls (or every call with failAlways),
// then issues a worker certificate for the CSR like the controller.
type fakeEnrollment struct {
	cucinav1.UnimplementedEnrollmentServiceServer
	t  testing.TB
	ca *agenttest.CA
	enrollBehaviour

	mu    sync.Mutex
	calls int
	last  *cucinav1.EnrollWorkerRequest
	keys  []string // public key (SPKI DER, base64) of every CSR received
}

func (f *fakeEnrollment) EnrollWorker(_ context.Context, req *cucinav1.EnrollWorkerRequest) (*cucinav1.EnrollWorkerResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.last = req
	if block, _ := pem.Decode(req.GetCsrPem()); block != nil {
		if csr, err := x509.ParseCertificateRequest(block.Bytes); err == nil {
			spki, _ := x509.MarshalPKIXPublicKey(csr.PublicKey)
			f.keys = append(f.keys, base64.StdEncoding.EncodeToString(spki))
		}
	}
	f.mu.Unlock()
	switch {
	case f.failAlways != nil:
		return nil, f.failAlways
	case n <= len(f.fail):
		return nil, f.fail[n-1]
	}
	doc, err := imds.ParseIdentityDocument(req.GetInstanceIdentityDocument())
	if err != nil || req.GetProtocol().GetMajor() != 1 {
		return nil, status.Error(codes.InvalidArgument, "bad request")
	}
	// Like internal/enroll: the signature must be the base64 PKCS#7 (DER) body of
	// /rsa2048, not the RSA-1024 /signature form.
	if der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(req.GetSignature()), "")); err != nil || len(der) == 0 || der[0] != 0x30 {
		return nil, status.Error(codes.Unauthenticated, "signature is not the PKCS#7 body of /rsa2048")
	}
	csr := req.GetCsrPem()
	if f.foreignKey {
		csr = agenttest.NewCSR(f.t)
	}
	settings := agenttest.Settings()
	return &cucinav1.EnrollWorkerResponse{
		CertificatePem: f.ca.WorkerCert(f.t, csr, settings.GetPool(), doc.InstanceID, agenttest.Epoch.Add(24*time.Hour)),
		CaPem:          f.ca.CertPEM,
		ExpiresAt:      timestamppb.New(agenttest.Epoch.Add(24 * time.Hour)),
		Pool:           settings.GetPool(),
		Generation:     "g7",
		Settings:       settings,
	}, nil
}

func (f *fakeEnrollment) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// fakeStorage is a machine with one 237 GB instance-store NVMe device and an
// EBS root volume; Prepare "formats and mounts" by creating the directory.
type fakeStorage struct {
	mu       sync.Mutex
	prepared []string
}

func (s *fakeStorage) Disks(context.Context) ([]workeragent.Disk, error) {
	return []workeragent.Disk{
		{ID: "/dev/nvme0n1", Kind: workeragent.DiskEBS, SizeBytes: 12 << 30, Root: true, Partitioned: true, MountPoints: []string{"/"}},
		{ID: "/dev/nvme1n1", Kind: workeragent.DiskInstanceStore, SizeBytes: 237e9},
	}, nil
}

func (s *fakeStorage) Prepare(_ context.Context, plan workeragent.PlacementPlan, mountPoint string) (string, uint64, error) {
	s.mu.Lock()
	s.prepared = append(s.prepared, plan.Disk.ID)
	s.mu.Unlock()
	return mountPoint, 220 << 30, os.MkdirAll(mountPoint, 0o755)
}

type bootEnv struct {
	paths  workeragent.Paths
	clock  agenttest.AutoClock
	power  *agenttest.Power
	enroll *fakeEnrollment
	imds   *imdsfake.Server
	logs   *bytes.Buffer
	b      *workeragent.Bootstrap
}

func newBootEnv(t *testing.T, enroll *fakeEnrollment, serverCA *agenttest.CA) *bootEnv {
	t.Helper()
	ca := enroll.ca
	if serverCA == nil {
		serverCA = ca
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCA.ServerCert(t, enrollName)}, MinVersion: tls.VersionTLS12,
	})))
	cucinav1.RegisterEnrollmentServiceServer(srv, enroll)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	bd, err := bootdata.Encode(bootdata.BootData{
		EnrollEndpoint: lis.Addr().String(), ServerName: enrollName, CAPEM: string(ca.CertPEM),
		Cluster: "e2e", Pool: "linux-x86-64", Generation: "g7",
	})
	require.NoError(t, err)
	md := imdsfake.New(t)
	md.SetUserData(bd)
	// The body of /rsa2048: base64 PKCS#7 (DER SEQUENCE), wrapped like IMDS does.
	md.SetIdentity([]byte(identity), "MIAGCSqGSIb3DQEHAqCAMIACAQEx\nDzANBglghkgBZQMEAgEFADCABgkq\n", instanceID)
	md.SetTags(map[string]string{"cucina:pool": "linux-x86-64", "cucina:generation": "g7", "cucina:cluster": "e2e"})

	e := &bootEnv{
		paths: workeragent.DefaultPaths("linux").Under(t.TempDir()),
		clock: agenttest.NewAutoClock(), power: &agenttest.Power{}, enroll: enroll, imds: md, logs: &bytes.Buffer{},
	}
	e.b = &workeragent.Bootstrap{
		Paths: e.paths, GOOS: "linux", GOARCH: "amd64", Version: "test", VCPUs: 4,
		IMDS: imds.New(md.URL),
		DialEnroller: func(bd bootdata.BootData) (workeragent.Enroller, func() error, error) {
			en, err := workeragent.DialEnroller(bd)
			if err != nil {
				return nil, nil, err
			}
			return en, en.Close, nil
		},
		Storage: &fakeStorage{}, Renderer: workeragent.BBConfigRenderer{},
		Host: &agenttest.Host{Clock: e.clock, Boot: agenttest.Epoch.Add(-40 * time.Second), ID: "boot-1", Memory: 16 << 30},
		FS:   hostos.FS{}, Clock: e.clock, Power: e.power,
		Log:       slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		BuildUser: &workeragent.UnixUser{UID: 1001, GID: 1001},
	}
	return e
}

// Guards: R-POOL-3 + R-SEC-3 (enroll at every boot with the signed identity
// document, verify the controller against the boot data CA — no TOFU) and
// "fail closed and fast" (refusals power off at once, transient errors are
// retried with backoff for at most the bootstrap deadline, then power off).
func TestBootstrapEnrollmentOutcomes(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "controller starting")
	cases := []struct {
		name          string
		enroll        enrollBehaviour
		rogueTLS      bool // the endpoint presents a certificate from another CA
		noUserData    bool
		emptyUserData bool
		badUserData   bool
		wantReason    string // "" = success
		wantCalls     func(int) bool
	}{
		{name: "transient errors then success", enroll: enrollBehaviour{fail: []error{unavailable, status.Error(codes.ResourceExhausted, "rate limited"), unavailable}},
			wantCalls: func(n int) bool { return n == 4 }},
		{name: "enrollment denied", enroll: enrollBehaviour{failAlways: status.Error(codes.PermissionDenied, "instance not in pool")},
			wantReason: "enrollment-refused", wantCalls: func(n int) bool { return n == 1 }},
		{name: "identity document rejected", enroll: enrollBehaviour{failAlways: status.Error(codes.Unauthenticated, "bad signature")},
			wantReason: "enrollment-refused", wantCalls: func(n int) bool { return n == 1 }},
		{name: "controller unreachable for the whole deadline", enroll: enrollBehaviour{failAlways: unavailable},
			wantReason: "enrollment-unreachable", wantCalls: func(n int) bool { return n >= 3 }},
		{name: "server certificate from another CA", rogueTLS: true,
			wantReason: "enrollment-refused", wantCalls: func(n int) bool { return n == 0 }},
		{name: "certificate for another key", enroll: enrollBehaviour{foreignKey: true},
			wantReason: "enrollment-invalid", wantCalls: func(n int) bool { return n == 1 }},
		{name: "user data is not boot data", badUserData: true,
			wantReason: "boot-data", wantCalls: func(n int) bool { return n == 0 }},
		{name: "no user data: not a controller launch (Fast Launch pre-provisioning, image builds)", noUserData: true,
			wantReason: workeragent.ReasonNotAWorker, wantCalls: func(n int) bool { return n == 0 }},
		// Bug (images agent, AMI bake): Packer builders' IMDS answers 200 with an
		// empty body; bootstrap must not power the builder off.
		{name: "empty user data (Packer builders): not a controller launch", emptyUserData: true,
			wantReason: workeragent.ReasonNotAWorker, wantCalls: func(n int) bool { return n == 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enroll := &fakeEnrollment{t: t, ca: agenttest.NewCA(t, "cucina test ca"), enrollBehaviour: tc.enroll}
			var serverCA *agenttest.CA
			if tc.rogueTLS {
				serverCA = agenttest.NewCA(t, "rogue ca")
			}
			e := newBootEnv(t, enroll, serverCA)
			if tc.noUserData {
				e.imds.SetUserData(nil)
			}
			if tc.emptyUserData {
				e.imds.SetUserData([]byte("\n"))
			}
			if tc.badUserData {
				e.imds.SetUserData([]byte("#!/bin/sh\necho not boot data\n"))
			}
			err := e.b.Run(context.Background())
			require.True(t, tc.wantCalls(enroll.callCount()), "EnrollWorker calls: %d", enroll.callCount())
			elapsed := e.clock.Now().Sub(agenttest.Epoch)
			if tc.wantReason == "" {
				require.NoError(t, err)
				require.Empty(t, e.power.Calls())
				return
			}
			var be *workeragent.BootError
			require.ErrorAs(t, err, &be)
			require.Equal(t, tc.wantReason, be.Reason)
			if tc.wantReason == workeragent.ReasonNotAWorker {
				require.Empty(t, e.power.Calls(), "a launch without user data is left alone")
				return
			}
			require.Equal(t, []string{"bootstrap: " + tc.wantReason}, e.power.Calls(), "powers off exactly once")
			if tc.wantReason == "enrollment-unreachable" {
				require.LessOrEqual(t, elapsed, workeragent.DefaultBootstrapDeadline, "bounded by the deadline")
				require.Greater(t, elapsed, workeragent.DefaultBootstrapDeadline/2, "kept retrying with backoff")
			} else {
				require.Less(t, elapsed, time.Second, "refusals power off immediately, without backoff")
			}
			_, statErr := os.Stat(e.paths.WorkerConfig())
			require.ErrorIs(t, statErr, os.ErrNotExist, "no configuration without an identity")
		})
	}
}

// Guards: contract §5.2 outputs — bootstrap exits 0 only when everything
// bb_runner/bb_worker need exists, with the right modes; the key never leaves
// the machine nor reaches the logs; a second call in the same boot is
// idempotent (no second enrollment).
func TestBootstrapWritesWorkerFiles(t *testing.T) {
	ca := agenttest.NewCA(t, "cucina test ca")
	enroll := &fakeEnrollment{t: t, ca: ca}
	e := newBootEnv(t, enroll, nil)
	require.NoError(t, e.b.Run(context.Background()))

	modes := map[string]os.FileMode{
		e.paths.KeyFile(): 0o600, e.paths.CertFile(): 0o644, e.paths.CAFile(): 0o644,
		e.paths.WorkerConfig(): 0o644, e.paths.RunnerConfig(): 0o644, e.paths.EnvFile: 0o644,
		e.paths.StateFile: 0o644, e.paths.LastActivityFile(): 0o644, e.paths.LastContactFile(): 0o644,
		e.paths.DeadmanConfig: 0o644,
	}
	for p, want := range modes {
		fi, err := os.Stat(p)
		require.NoError(t, err, p)
		require.Equal(t, want, fi.Mode().Perm(), p)
	}

	worker, err := os.ReadFile(e.paths.WorkerConfig())
	require.NoError(t, err)
	require.True(t, json.Valid(worker))
	require.Contains(t, string(worker), instanceID, "worker id label node = instance ID")
	require.Contains(t, string(worker), e.paths.InstanceStoreMount, "L1 on the instance-store mount")
	caFile, err := os.ReadFile(e.paths.CAFile())
	require.NoError(t, err)
	require.Equal(t, ca.CertPEM, caFile)
	env, err := os.ReadFile(e.paths.EnvFile)
	require.NoError(t, err)
	require.Contains(t, string(env), "CUCINA_NODE='"+instanceID+"'")

	// The image's shell dead-man timer (the backstop) gets the pool's limits.
	deadman, err := os.ReadFile(e.paths.DeadmanConfig)
	require.NoError(t, err)
	require.Contains(t, string(deadman), "IDLE_LIMIT_SECONDS=1800\nCONTACT_LIMIT_SECONDS=600\nMAX_UPTIME_SECONDS=43200\n")

	st, err := workeragent.LoadState(hostos.FS{}, e.paths.StateFile)
	require.NoError(t, err)
	require.Equal(t, []string{"linux-x86-64", instanceID, "g7", workeragent.PlacementInstanceStore},
		[]string{st.Pool, st.Node, st.Generation, st.L1Placement})
	require.Equal(t, agenttest.Epoch.Add(24*time.Hour), st.CertNotAfter.UTC())

	// The private key is what bb_worker reads, and nothing else carries it.
	keyPEM, err := os.ReadFile(e.paths.KeyFile())
	require.NoError(t, err)
	block, _ := pem.Decode(keyPEM)
	require.NotNil(t, block)
	for _, secret := range []string{string(keyPEM), base64.StdEncoding.EncodeToString(block.Bytes)[:40]} {
		require.NotContains(t, e.logs.String(), secret, "key material in logs")
	}
	require.NotContains(t, string(enroll.last.GetCsrPem()), "PRIVATE KEY")

	// Second call during the same boot (e.g. bb-worker.service restarting).
	require.NoError(t, e.b.Run(context.Background()))
	require.Equal(t, 1, enroll.callCount(), "no second enrollment in the same boot")
	require.Empty(t, e.power.Calls())
	keyAgain, err := os.ReadFile(e.paths.KeyFile())
	require.NoError(t, err)
	require.Equal(t, keyPEM, keyAgain, "the key of this boot is kept")
}

// failingRenderer fails like a bootstrap that crashed after enrolling.
type failingRenderer struct{ workeragent.BBConfigRenderer }

func (failingRenderer) Worker(*cucinav1.WorkerSettings, workeragent.Machine) ([]byte, error) {
	return nil, errors.New("simulated crash after enrollment")
}

// Guards: R-SEC-3 one enrollment per boot — a bootstrap that dies after
// enrolling retries with the same key in the same boot (the controller only
// re-issues for the same key within its crash-recovery window); a new boot
// always gets a new key.
func TestBootstrapKeyPerBoot(t *testing.T) {
	enroll := &fakeEnrollment{t: t, ca: agenttest.NewCA(t, "cucina test ca")}
	e := newBootEnv(t, enroll, nil)
	e.b.Renderer = failingRenderer{}
	require.Error(t, e.b.Run(context.Background()))
	e.b.Renderer = workeragent.BBConfigRenderer{}
	require.NoError(t, e.b.Run(context.Background()))
	require.NoError(t, os.Remove(e.paths.StateFile)) // forget the completed bootstrap
	e.b.Host = &agenttest.Host{Clock: e.clock, Boot: agenttest.Epoch, ID: "boot-2", Memory: 16 << 30}
	require.NoError(t, e.b.Run(context.Background()))

	enroll.mu.Lock()
	defer enroll.mu.Unlock()
	require.Len(t, enroll.keys, 3)
	require.Equal(t, enroll.keys[0], enroll.keys[1], "same boot: same key")
	require.NotEqual(t, enroll.keys[1], enroll.keys[2], "new boot: new key")
}
