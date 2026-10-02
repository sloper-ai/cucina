// SPDX-License-Identifier: FSL-1.1-ALv2

// Package boottest boots the pinned Buildbarn release binaries against the
// configurations internal/bbconfig renders (R-CP-2, R-TEST-6 "boot every
// pinned binary with each profile"). Integration tier: localhost only.
package boottest_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteworker"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/bbtest"
)

const (
	workerURI = "spiffe://cucina/worker/macos/TESTSERIAL01/vm-1"
	hostURI   = "spiffe://cucina/host/TESTSERIAL01"
	mib       = uint64(1) << 20
)

func props(kv ...string) []*cucinav1.PlatformProperty {
	var out []*cucinav1.PlatformProperty
	for i := 0; i < len(kv); i += 2 {
		out = append(out, &cucinav1.PlatformProperty{Name: kv[i], Value: kv[i+1]})
	}
	return out
}

// testMachine is a machine whose state lives in temporary directories, with
// the test PKI's CA bundle and the worker key pair installed in PKIDir.
func testMachine(t *testing.T, os string, pki *bbtest.PKI, worker bbtest.KeyPair) bbconfig.Machine {
	t.Helper()
	short := bbtest.ShortTempDir(t) // UNIX sockets: runner, NFSv4
	root := t.TempDir()
	m := bbconfig.Machine{
		OS:          os,
		Arch:        "arm64",
		VCPUs:       2,
		MemoryBytes: 8 << 30,
		StateRoot:   filepath.Join(short, "state"),
		BuildRoot:   filepath.Join(root, "buildroot"),
		RunDir:      filepath.Join(short, "run"),
		PKIDir:      filepath.Join(root, "pki"),
		CABundlePEM: pki.CAPEM,
		MetricsHost: "127.0.0.1",
	}
	bbtest.CopyKeyPair(t, worker, m.PKIDir, bbconfig.ClientCertificateFile, bbconfig.ClientPrivateKeyFile)
	return m
}

// createDirectories does what the worker agent and hostd do before starting
// bb_runner and bb_worker. Relative paths (Windows syntax on a POSIX host) are
// created below dir, the processes' working directory.
func createDirectories(t *testing.T, dir string, dirs []bbconfig.Directory) {
	t.Helper()
	for _, d := range dirs {
		p := d.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		require.NoError(t, os.MkdirAll(p, d.Mode))
	}
}

// fakeScheduler is an in-process OperationQueue server (mTLS, worker
// certificates required) that records Synchronize calls and holds idle workers
// like the real scheduler's long poll.
type fakeScheduler struct {
	remoteworker.UnimplementedOperationQueueServer
	Addr string

	mu      sync.Mutex
	seen    map[string]*remoteworker.SynchronizeRequest
	want    int
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newFakeScheduler(t *testing.T, pki *bbtest.PKI, server bbtest.KeyPair, wantThreads int) *fakeScheduler {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeScheduler{Addr: l.Addr().String(), seen: map[string]*remoteworker.SynchronizeRequest{}, want: wantThreads, reached: make(chan struct{}), release: make(chan struct{})}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{server.TLS},
		ClientCAs:    pki.Pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})))
	remoteworker.RegisterOperationQueueServer(s, f)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return f
}

func (f *fakeScheduler) Synchronize(ctx context.Context, req *remoteworker.SynchronizeRequest) (*remoteworker.SynchronizeResponse, error) {
	key, _ := json.Marshal([]any{req.InstanceNamePrefix, req.Platform.GetProperties(), req.WorkerId})
	f.mu.Lock()
	_, again := f.seen[string(key)]
	f.seen[string(key)] = req
	if !again && len(f.seen) == f.want {
		close(f.reached)
	}
	f.mu.Unlock()
	// Like the real scheduler's long poll, an idle worker waits for work; at
	// test end Release answers "stay idle" so bb_worker can shut down at once.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
		return &remoteworker.SynchronizeResponse{
			NextSynchronizationAt: timestamppb.Now(),
			DesiredState:          &remoteworker.DesiredState{WorkerState: &remoteworker.DesiredState_Idle{Idle: &emptypb.Empty{}}},
		}, nil
	}
}

// Release ends every pending and future long poll.
func (f *fakeScheduler) Release() { f.once.Do(func() { close(f.release) }) }

// Registered is ready once every expected runner thread has synchronized.
func (f *fakeScheduler) Registered(ctx context.Context) error {
	select {
	case <-f.reached:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeScheduler) Requests() []*remoteworker.SynchronizeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*remoteworker.SynchronizeRequest
	for _, r := range f.seen {
		out = append(out, r)
	}
	return out
}

// startWorker starts bb_worker. With an NFSv4 build directory it makes sure no
// mount outlives the test (a killed bb_worker cannot unmount) and skips when
// this user may not mount NFS (a host capability, not a configuration error).
func startWorker(t *testing.T, config []byte, dir string, plan *bbconfig.WorkerPlan) *bbtest.Process {
	t.Helper()
	if plan.BuildDirectory != bbconfig.BuildDirectoryNFSv4 {
		return bbtest.Start(t, "bb_worker", bbtest.Binary(t, bbtest.EnvWorker), config, bbtest.WithDir(dir))
	}
	// A graceful stop unmounts; a killed bb_worker leaves a mount whose server
	// is gone, and anything that stats it hangs. So: plenty of time to stop,
	// then a forced unmount found through the mount table (no stat).
	w := bbtest.Start(t, "bb_worker", bbtest.Binary(t, bbtest.EnvWorker), config, bbtest.WithDir(dir), bbtest.WithStopGrace(30*time.Second))
	t.Cleanup(func() {
		w.Stop()
		if mounted(plan.MountPath) {
			_ = exec.Command("/sbin/umount", "-f", plan.MountPath).Run()
		}
	})
	return w
}

// waitRegistered waits until the scheduler saw every runner thread. A host
// that does not let this user mount NFS skips instead of failing.
func waitRegistered(t *testing.T, w *bbtest.Process, ready bbtest.Readiness) {
	t.Helper()
	err := w.WaitReady(context.Background(), ready)
	if err == nil {
		return
	}
	select {
	case <-w.Done():
		if logs := w.Logs(); strings.Contains(logs, "Failed to expose build directory mount") && strings.Contains(logs, "not permitted") {
			t.Skipf("this user may not create NFSv4 mounts here: %s", bbtest.Tail(logs, 1))
		}
	default:
	}
	t.Fatal(err)
}

// mounted reports whether path is a mount point, from the mount table: it
// must not touch the path itself, which may be a stale NFS mount.
func mounted(path string) bool {
	out, err := exec.Command("/sbin/mount").Output()
	return err == nil && strings.Contains(string(out), " on "+path+" (")
}

func platformKey(req *remoteworker.SynchronizeRequest) string {
	var parts []string
	for _, p := range req.Platform.GetProperties() {
		parts = append(parts, p.Name+"="+p.Value)
	}
	return req.InstanceNamePrefix + "|" + strings.Join(parts, ";")
}
