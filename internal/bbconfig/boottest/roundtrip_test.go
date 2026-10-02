// SPDX-License-Identifier: FSL-1.1-ALv2

package boottest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/bbtest"
)

// TestActionRoundTrip runs one real action through pinned bb_storage (as
// frontend and storage), bb_scheduler, and the bb_runner and bb_worker that
// internal/bbconfig renders for a macOS VM: Execute through the frontend, the
// worker fetches the input through its L1, the runner executes, the worker
// uploads the output and writes the action result (R-CP-2, R-TEST-6). The WAN
// variant also proves the zstd hop (R-DATA-3) and the NFSv4 build directory.
func TestActionRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("rendered macOS worker profiles run on darwin only")
	}
	for _, tc := range []struct {
		name           string
		buildDirectory string
		wanCompression bool
	}{
		{"native in-AZ", bbconfig.BuildDirectoryNative, false},
		{"nfsv4 WAN zstd", bbconfig.BuildDirectoryNFSv4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pki := bbtest.NewPKI(t)
			server := pki.LoopbackServer(t, "server", "spiffe://cucina/server/frontend")
			worker := pki.Workload(t, "worker", workerURI)
			storageClient, storageWorker := bbtest.FreeAddr(t), bbtest.FreeAddr(t)
			schedClient, schedWorker, schedBQS := bbtest.FreeAddr(t), bbtest.FreeAddr(t), bbtest.FreeAddr(t)

			s := macSettings()
			s.BuildDirectory, s.WanCompression = tc.buildDirectory, tc.wanCompression
			s.SchedulerEndpoint, s.StorageEndpoint = schedWorker, storageWorker
			s.MetricsPort = uint32(freePort(t))
			m := testMachine(t, bbconfig.OSDarwin, pki, worker)
			plan, err := bbconfig.PlanWorker(s, m)
			require.NoError(t, err)
			require.Equal(t, tc.wanCompression, plan.Compression)

			bbtest.BootStorage(t, bbtest.StorageConfig(bbtest.StorageOptions{
				ClientListen: storageClient, WorkerListen: storageWorker, ServerKeyPair: &server, CAPEM: pki.CAPEM,
				SchedulerAddress: schedClient, Compression: true,
			}), bbtest.AllReady(bbtest.GRPCReady(storageClient, nil), bbtest.GRPCReady(storageWorker, pki.ClientTLS(nil, "localhost"))))
			var queues []bbtest.Queue
			for _, r := range plan.Runners {
				props := map[string]string{}
				for _, p := range r.Properties {
					props[p.Name] = p.Value
				}
				queues = append(queues, bbtest.Queue{InstanceNamePrefix: r.InstanceNamePrefix, Properties: props, SizeClasses: []uint32{s.SizeClass}})
			}
			bbtest.BootScheduler(t, bbtest.SchedulerConfig(bbtest.SchedulerOptions{
				ClientListen: schedClient, WorkerListen: schedWorker, BuildQueueStateListen: schedBQS,
				ServerKeyPair: &server, CAPEM: pki.CAPEM, StorageAddress: storageClient, Queues: queues,
			}), bbtest.AllReady(bbtest.GRPCReady(schedClient, nil), bbtest.GRPCReady(schedWorker, pki.ClientTLS(nil, "localhost"))))

			dir := t.TempDir()
			createDirectories(t, dir, plan.Directories)
			runnerConfig, err := bbconfig.RenderRunner(s, m)
			require.NoError(t, err)
			bbtest.BootRunner(t, runnerConfig, bbtest.GRPCReady("unix://"+plan.RunnerSocket, nil), bbtest.WithDir(dir))
			workerConfig, err := bbconfig.RenderWorker(s, m)
			require.NoError(t, err)
			w := startWorker(t, workerConfig, dir, plan)

			conn, err := bbtest.Dial(storageClient, nil)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			re := bbtest.NewREClient(conn, "main")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			digest, err := re.Upload(ctx, bbtest.Action{
				Args:        []string{"/bin/sh", "-c", `printf 'hello %s' "$GREETING" > out/greeting.txt && cat input.txt >> out/greeting.txt`},
				Env:         map[string]string{"GREETING": "cucina", "PATH": "/bin:/usr/bin"},
				Inputs:      map[string][]byte{"input.txt": []byte("!")},
				OutputPaths: []string{"out/greeting.txt"},
				Platform:    map[string]string{"OSFamily": "macos", "ISA": "arm-a64"}, // the generic runner
			})
			require.NoError(t, err)
			exec, err := re.Start(ctx, digest, &remoteexecution.RequestMetadata{ToolInvocationId: "inv-roundtrip", TargetId: "//boottest:roundtrip"})
			require.NoError(t, err)
			type result struct {
				resp *remoteexecution.ExecuteResponse
				err  error
			}
			done := make(chan result, 1)
			go func() {
				r, err := exec.Wait()
				done <- result{r, err}
			}()
			var resp *remoteexecution.ExecuteResponse
			select {
			case r := <-done:
				require.NoError(t, r.err)
				resp = r.resp
			case <-w.Done():
				skipIfMountDenied(t, w)
				t.Fatalf("bb_worker exited: %v\n%s", w.Err(), bbtest.Tail(w.Logs(), 20))
			}
			require.Equal(t, int32(0), resp.GetResult().GetExitCode(), "stderr digest %v", resp.GetResult().GetStderrDigest())
			out, err := re.OutputFile(ctx, resp, "out/greeting.txt")
			require.NoError(t, err)
			assert.Equal(t, "hello cucina!", string(out))

			// R-RE-4: the action ran on a thread of this pool's node.
			var id map[string]string
			require.NoError(t, json.Unmarshal([]byte(resp.GetResult().GetExecutionMetadata().GetWorker()), &id))
			assert.Equal(t, s.Pool, id["pool"])
			assert.Equal(t, s.Node, id["node"])
			assert.Contains(t, id, "thread")

			// R-OBS-1: the rendered diagnostics listener serves Prometheus metrics;
			// with WAN compression the bounded zstd pool did the transfers.
			metrics := scrape(t, "http://127.0.0.1:"+strconv.Itoa(int(s.MetricsPort))+"/metrics")
			assert.Contains(t, metrics, "buildbarn_")
			used := regexp.MustCompile(`(?m)^buildbarn_zstd_pool_acquisitions_total\{[^}]*\} [1-9]`).MatchString(metrics)
			assert.Equal(t, tc.wanCompression, used, "zstd pool use must match the WAN compression setting")
		})
	}
}

// skipIfMountDenied skips when the worker stopped because this user may not
// mount NFS (a host capability).
func skipIfMountDenied(t *testing.T, w *bbtest.Process) {
	t.Helper()
	logs := w.Logs()
	if regexp.MustCompile(`Failed to expose build directory mount.*not permitted`).MatchString(logs) {
		t.Skipf("this user may not create NFSv4 mounts here")
	}
}

func scrape(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
