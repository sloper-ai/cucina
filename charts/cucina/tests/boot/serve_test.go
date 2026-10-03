// SPDX-License-Identifier: FSL-1.1-ALv2

package boot_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/buildqueuestate"
	"github.com/buildbarn/bb-storage/pkg/proto/fsac"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
	"github.com/sloper-ai/cucina/internal/bbtest"
)

// TestRenderedControlPlaneServes boots the small profile as rendered (two storage
// shards, scheduler, frontend, wired on loopback) and checks the behaviour the chart's
// configuration is responsible for, through the public endpoints:
//   - GetCapabilities advertises ZSTD and execution (R-CP-1, R-DATA-1);
//   - a client JWT with grants writes and reads CAS and AC (completeness checking passes);
//   - UC4 / R-CACHE-5: a token without ac_write cannot write the AC, and a deny-listed
//     sid is refused (R-AUTH-9);
//   - R-SEC-2: a worker certificate may write the AC on the worker listener, the
//     controller's certificate is not a worker;
//   - the File System Access Cache serves workers and hosts on the worker listener and
//     answers NotFound on a miss (virtual build directories rely on it, docs/dev/buildbarn.md); clients cannot use it;
//   - R-RE-2: Execute on a predeclared platform queues with zero workers instead of
//     failing; an undeclared platform is refused;
//   - R-SEC-4: BuildQueueState answers the controller's certificate only.
func TestRenderedControlPlaneServes(t *testing.T) {
	for _, renderer := range []struct{ name, env, command string }{{"helm3", "HELM3", "helm3"}, {"helm4", "HELM", "helm"}} {
		t.Run(renderer.name, func(t *testing.T) {
			// Bazel supplies both exact pins. Native Go retains normal prerequisite
			// discovery, with a distinct helm3 executable rather than another major.
			if os.Getenv("TEST_SRCDIR") != "" && os.Getenv(renderer.env) == "" {
				t.Fatalf("%s must name the pinned Helm renderer; no major-version fallback", renderer.env)
			}
			helm := charttest.Tool(t, renderer.env, renderer.command)
			t.Parallel()
			serveRenderedControlPlane(t, helm)
		})
	}
}

func serveRenderedControlPlane(t *testing.T, helm string) {
	storageBin := bbtest.Binary(t, bbtest.EnvStorage)
	schedulerBin := bbtest.Binary(t, bbtest.EnvScheduler)
	const issuer = "https://sts.cucina.test"
	kind := filepath.Join(charttest.RepoRoot(t), "release", "kind-values.yaml")
	// R-CP-1/-2: inspect the actual system-lane input before any localization or
	// fixture overrides. Helm 3 previously rendered null replicas and zero shards.
	assertSmallTopology(t, renderManifest(t, helm, []string{kind}))
	assertProfileOverrides(t, helm, kind)
	// Keep the existing queue/auth/CAS acceptance coverage: add sample pools and
	// small file-backed stores for localhost, but NEVER override storage.shards.
	manifests := renderManifest(t, helm, []string{kind, filepath.Join(charttest.ChartDir(t), "samples", "pools.yaml")},
		"endpoints.sts.url="+issuer,
		"storage.stores.cas.size=19Gi", "storage.stores.ac.size=32Mi", "storage.stores.fsac.size=32Mi", "storage.stores.iscc.size=16Mi")
	assertSmallTopology(t, manifests)
	cfgs := configsOf(t, manifests)

	pki := bbtest.NewPKI(t)
	server := pki.LoopbackServer(t, "server", "spiffe://cucina/server/frontend")
	denylist := []byte(`{"version":1,"sids":["sid:revoked-session-0000000"],"subs":[]}`)
	ctx := t.Context()

	boot := func(c bbConfig, bin string, peers wiring) wired {
		t.Helper()
		root := bbtest.ShortTempDir(t)
		w := wire(t, c, root, pki, server, peers)
		p := bbtest.Start(t, c.component, bin, w.config, bbtest.WithDir(root))
		if err := p.WaitReady(ctx, bbtest.GRPCReady(w.probe, w.probeTLS)); err != nil {
			t.Fatalf("%s did not start: %v\n%s", c.component, err, bbtest.Tail(p.Logs(), 40))
		}
		return w
	}
	var shards []string
	for range 2 {
		shards = append(shards, boot(cfgs["storage"], storageBin, wiring{}).listeners["grpcServers[0]"])
	}
	sched := boot(cfgs["scheduler"], schedulerBin, wiring{shards: shards, denylist: denylist})
	front := boot(cfgs["frontend"], storageBin, wiring{shards: shards, scheduler: sched.listeners["clientGrpcServers[0]"], denylist: denylist})

	dial := func(addr string, kp *bbtest.KeyPair, token string) *grpc.ClientConn {
		t.Helper()
		opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(pki.ClientTLS(kp, "localhost")))}
		if token != "" {
			opts = append(opts, grpc.WithPerRPCCredentials(charttest.Bearer(token)))
		}
		conn, err := grpc.NewClient(addr, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := conn.Close(); err != nil {
				t.Errorf("close gRPC connection: %v", err)
			}
		})
		return conn
	}
	all := []string{"main"}
	full := charttest.CucinaJWT(t, issuer, "google:full", "full-session-00000000000", map[string][]string{
		"cas_read": all, "cas_write": all, "ac_read": all, "ac_write": all, "execute": all})
	reader := charttest.CucinaJWT(t, issuer, "github:reader", "reader-session-000000000", map[string][]string{
		"cas_read": all, "ac_read": all})
	revoked := charttest.CucinaJWT(t, issuer, "google:revoked", "revoked-session-0000000", map[string][]string{
		"cas_read": all, "cas_write": all, "ac_read": all, "ac_write": all, "execute": all})
	clients := front.listeners["grpcServers[0]"]
	workers := front.listeners["grpcServers[1]"]

	// Capabilities: ZSTD and execution are advertised.
	caps, err := remoteexecution.NewCapabilitiesClient(dial(clients, nil, full)).GetCapabilities(ctx, &remoteexecution.GetCapabilitiesRequest{InstanceName: "main"})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if !slices.Contains(caps.GetCacheCapabilities().GetSupportedCompressors(), remoteexecution.Compressor_ZSTD) {
		t.Errorf("ZSTD is not advertised: %v", caps.GetCacheCapabilities().GetSupportedCompressors())
	}
	if !caps.GetExecutionCapabilities().GetExecEnabled() {
		t.Error("execution is not advertised")
	}

	// CAS + AC round trip with a full client token.
	blob := []byte("cucina chart test output")
	blobDigest := bbtest.DigestOf(blob)
	actionDigest := bbtest.DigestOf([]byte("cucina chart test action"))
	result := &remoteexecution.ActionResult{OutputFiles: []*remoteexecution.OutputFile{{Path: "out", Digest: blobDigest}}}
	fullConn := dial(clients, nil, full)
	if err := upload(ctx, fullConn, blob); err != nil {
		t.Fatalf("CAS write with cas_write: %v", err)
	}
	if got, err := bbtest.NewREClient(fullConn, "main").Read(ctx, blobDigest); err != nil || string(got) != string(blob) {
		t.Fatalf("CAS read with cas_read: %q, %v", got, err)
	}
	ac := func(conn *grpc.ClientConn) remoteexecution.ActionCacheClient {
		return remoteexecution.NewActionCacheClient(conn)
	}
	if _, err := ac(fullConn).UpdateActionResult(ctx, &remoteexecution.UpdateActionResultRequest{InstanceName: "main", ActionDigest: actionDigest, ActionResult: result}); err != nil {
		t.Fatalf("AC write with ac_write: %v", err)
	}
	if _, err := ac(fullConn).GetActionResult(ctx, &remoteexecution.GetActionResultRequest{InstanceName: "main", ActionDigest: actionDigest}); err != nil {
		t.Fatalf("AC read with ac_read (completeness checking): %v", err)
	}

	// UC4 / R-CACHE-5: read-only principals cannot write the AC; deny-listed sessions are refused.
	readerConn := dial(clients, nil, reader)
	if _, err := ac(readerConn).GetActionResult(ctx, &remoteexecution.GetActionResultRequest{InstanceName: "main", ActionDigest: actionDigest}); err != nil {
		t.Errorf("AC read with ac_read only: %v", err)
	}
	if _, err := ac(readerConn).UpdateActionResult(ctx, &remoteexecution.UpdateActionResultRequest{InstanceName: "main", ActionDigest: actionDigest, ActionResult: result}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("AC write without ac_write: got %v, want PermissionDenied", err)
	}
	if _, err := ac(dial(clients, nil, revoked)).GetActionResult(ctx, &remoteexecution.GetActionResultRequest{InstanceName: "main", ActionDigest: actionDigest}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("deny-listed sid: got %v, want PermissionDenied", err)
	}
	if _, err := ac(dial(clients, nil, "")).GetActionResult(ctx, &remoteexecution.GetActionResultRequest{InstanceName: "main", ActionDigest: actionDigest}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no token: got %v, want Unauthenticated", err)
	}

	// R-SEC-2: workers write the AC over mTLS; the controller certificate is not a worker.
	worker := pki.Workload(t, "worker", "spiffe://cucina/worker/linux-x86-64/i-0123456789abcdef0")
	if _, err := ac(dial(workers, &worker, "")).UpdateActionResult(ctx, &remoteexecution.UpdateActionResultRequest{InstanceName: "main", ActionDigest: actionDigest, ActionResult: result}); err != nil {
		t.Errorf("AC write by a worker: %v", err)
	}
	missing := &fsac.GetFileSystemAccessProfileRequest{InstanceName: "main", DigestFunction: remoteexecution.DigestFunction_SHA256, ReducedActionDigest: bbtest.DigestOf([]byte("no profile"))}
	if _, err := fsac.NewFileSystemAccessCacheClient(dial(workers, &worker, "")).GetFileSystemAccessProfile(ctx, missing); status.Code(err) != codes.NotFound {
		t.Errorf("FSAC miss for a worker: got %v, want NotFound", err)
	}
	host := pki.Workload(t, "host", "spiffe://cucina/host/C02TESTHOST1")
	update := &fsac.UpdateFileSystemAccessProfileRequest{InstanceName: "main", DigestFunction: remoteexecution.DigestFunction_SHA256,
		ReducedActionDigest: missing.ReducedActionDigest, FileSystemAccessProfile: &fsac.FileSystemAccessProfile{}}
	if _, err := fsac.NewFileSystemAccessCacheClient(dial(workers, &host, "")).UpdateFileSystemAccessProfile(ctx, update); err != nil {
		t.Errorf("FSAC write by a host (L2 on behalf of its VMs): %v", err)
	}
	if _, err := fsac.NewFileSystemAccessCacheClient(fullConn).GetFileSystemAccessProfile(ctx, missing); status.Code(err) != codes.PermissionDenied {
		t.Errorf("FSAC read by a client: got %v, want PermissionDenied", err)
	}
	controller := pki.Workload(t, "controller", "spiffe://cucina/controller")
	if _, err := ac(dial(workers, &controller, "")).GetActionResult(ctx, &remoteexecution.GetActionResultRequest{InstanceName: "main", ActionDigest: actionDigest}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("controller certificate on the worker listener: got %v, want Unauthenticated", err)
	}

	// R-RE-2: Execute queues at zero workers on a predeclared platform; undeclared ones fail fast.
	re := bbtest.NewREClient(fullConn, "main")
	declared, err := re.Upload(ctx, bbtest.Action{Args: []string{"/bin/true"}, Platform: map[string]string{"OSFamily": "linux", "ISA": "x86-64"}})
	if err != nil {
		t.Fatal(err)
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := re.Start(execCtx, declared, nil); err != nil {
		t.Errorf("Execute on a predeclared platform with zero workers: %v (want queued)", err)
	}
	undeclared, err := re.Upload(ctx, bbtest.Action{Args: []string{"/bin/true"}, Platform: map[string]string{"OSFamily": "linux", "ISA": "x86-64", "not-a-pool": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	// R-RE-2 quotes FAILED_PRECONDITION; the pinned bb_scheduler answers UNAVAILABLE ("No workers
	// exist for instance name prefix …"), which Bazel retries and then fails. Either way it is
	// not queued, which is what predeclaring prevents.
	if _, err := re.Start(execCtx, undeclared, nil); status.Code(err) != codes.Unavailable && status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Execute on an undeclared platform: got %v, want Unavailable/FailedPrecondition (not queued)", err)
	}

	// R-SEC-4: BuildQueueState is the controller's only.
	bqs := sched.listeners["buildQueueStateGrpcServers[0]"]
	queues, err := buildqueuestate.NewBuildQueueStateClient(dial(bqs, &controller, "")).ListPlatformQueues(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListPlatformQueues with the controller certificate: %v", err)
	}
	if len(queues.GetPlatformQueues()) == 0 {
		t.Error("the scheduler has no predeclared platform queues")
	}
	if _, err := buildqueuestate.NewBuildQueueStateClient(dial(bqs, &worker, "")).ListPlatformQueues(ctx, &emptypb.Empty{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("worker certificate on BuildQueueState: got %v, want Unauthenticated", err)
	}
}

// upload writes one blob with BatchUpdateBlobs.
func upload(ctx context.Context, conn *grpc.ClientConn, blob []byte) error {
	resp, err := remoteexecution.NewContentAddressableStorageClient(conn).BatchUpdateBlobs(ctx, &remoteexecution.BatchUpdateBlobsRequest{
		InstanceName:   "main",
		DigestFunction: remoteexecution.DigestFunction_SHA256,
		Requests:       []*remoteexecution.BatchUpdateBlobsRequest_Request{{Digest: bbtest.DigestOf(blob), Data: blob}},
	})
	if err != nil {
		return err
	}
	return status.ErrorProto(resp.GetResponses()[0].GetStatus())
}
