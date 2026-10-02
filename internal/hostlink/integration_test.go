// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlink_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/hostd"
	"github.com/sloper-ai/cucina/internal/hostd/config"
	"github.com/sloper-ai/cucina/internal/hostd/facts"
	"github.com/sloper-ai/cucina/internal/hostd/identity"
	"github.com/sloper-ai/cucina/internal/hostd/render"
	"github.com/sloper-ai/cucina/internal/hostd/secretstore"
	"github.com/sloper-ai/cucina/internal/hostd/sys"
	"github.com/sloper-ai/cucina/internal/hostlink"
	"github.com/sloper-ai/cucina/internal/hostlink/hostlinktest"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/providers/tart"
	"github.com/sloper-ai/cucina/internal/providers/tart/faketart"
)

const (
	serial = "TESTSERIAL01"
	image  = "ghcr.io/sloper-ai/cucina-worker-macos:27.0-test"
)

// advanceUntil advances the synctest fake clock in 1 s steps until cond holds.
func advanceUntil(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s (fake clock): %s", limit, what)
		}
		<-time.After(time.Second)
	}
}

type hostEnv struct {
	token           string
	net             *hostlinktest.Net
	ctrl            *hostlinktest.Controller
	tart            *faketart.Tart
	stateDir        string
	secretDir       string
	agent           *hostd.Agent
	cancel          context.CancelFunc
	done            chan error
	metricsClient   *http.Client
	metricsGatherer prometheus.Gatherer
	enableL2        bool
	wanListener     net.Listener
}

// newEnv builds the test bed inside a synctest bubble; ef comes from
// hostlinktest.NewEnroll, created outside the bubble.
func newEnv(t *testing.T, ef *hostlinktest.Enroll) *hostEnv {
	n := hostlinktest.NewNet()
	ctrl := hostlinktest.NewController(t, n, ef)
	e := &hostEnv{token: ctrl.Token, net: n, ctrl: ctrl, tart: faketart.New(),
		stateDir: t.TempDir(), secretDir: t.TempDir()}
	e.tart.Registry[image] = faketart.Image{Ref: image, SizeGB: 70, Private: true}
	return e
}

// start runs hostd (user mode, in-memory network, fake tart).
func (e *hostEnv) start(t *testing.T) {
	cfg := config.Default()
	cfg.ControllerURL = "https://" + hostlinktest.EnrollAddr
	cfg.CACertificates = e.ctrl.CAPool()
	cfg.SiteEnrollmentToken = e.token
	cfg.MetricsListen = ""
	dial := grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return e.net.Dial(ctx, addr) })
	rt := tart.New(tart.Options{Exec: e.tart, Binary: "tart"})
	if e.metricsClient == nil {
		e.metricsClient = &http.Client{Transport: metricsTransport(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no metrics endpoint in this fixture")
		})}
	}
	if e.metricsGatherer == nil {
		// Virtual multi-day renewal tests must not repeatedly read real process
		// telemetry. The dedicated metrics scenario supplies a populated source.
		e.metricsGatherer = prometheus.NewRegistry()
	}
	l2Binary := ""
	if e.enableL2 {
		l2Binary = "bb_storage"
	}
	a, err := hostd.New(hostd.Options{
		L2Binary: l2Binary, WANRelayListener: e.wanListener,
		Config: cfg, UserMode: true, StateDir: e.stateDir, Version: "test",
		Exec: e.tart, FS: sys.FS{}, Clock: hostlinktest.Clock{}, Secrets: secretstore.File{Dir: e.secretDir},
		Runtime: rt, Render: render.BBConfig{},
		Facts: func(context.Context) (facts.Facts, error) {
			return facts.Facts{Serial: serial, Model: "Mac mini", Chip: "Apple M5 Pro", Cores: 18, MemoryGiB: 64,
				MacOSVersion: "27.0", TartVersion: "2.40.1", AgentVersion: "test", FileVault: "off", Hostname: "mini-1"}, nil
		},
		StorageRelayListener:   e.net.Listen("hostd.test:8981"),
		SchedulerRelayListener: e.net.Listen("hostd.test:8983"),
		RelayDial:              e.net.Dial,
		ScrapeActivity:         func(context.Context, netip.Addr, uint32) (uint64, error) { return 0, errors.New("no metrics") },
		MetricsClient:          e.metricsClient,
		MetricsGatherer:        e.metricsGatherer,
		Gateway:                func(netip.Addr) (netip.Addr, bool) { return netip.MustParseAddr("192.168.64.1"), true },
		SameNetwork:            func(netip.Addr, netip.Addr) bool { return true },
		EnrollDialOptions:      []grpc.DialOption{dial},
		HostDialOptions:        []grpc.DialOption{dial},
		MinBackoff:             time.Second, MaxBackoff: 10 * time.Second,
		TickInterval: 2 * time.Second, ProbeInterval: 30 * time.Second, HousekeepInterval: time.Hour,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	e.agent, e.cancel, e.done = a, cancel, make(chan error, 1)
	go func() { e.done <- a.Run(ctx) }()
}

func (e *hostEnv) stop(t *testing.T) {
	e.cancel()
	require.NoError(t, <-e.done)
}

// powerOff stops hostd and every VM (end of a bubble: no goroutine may outlive it).
func (e *hostEnv) powerOff(t *testing.T) {
	e.stop(t)
	e.tart.PowerOff()
	synctest.Wait()
}

func (e *hostEnv) hostState(t *testing.T) (ports.HostState, bool) {
	hs, err := e.ctrl.Host().Hosts(context.Background())
	require.NoError(t, err)
	for _, h := range hs {
		if h.Serial == serial {
			return h, true
		}
	}
	return ports.HostState{}, false
}

func (e *hostEnv) online(t *testing.T) func() bool {
	return func() bool { h, ok := e.hostState(t); return ok && h.Online }
}

func (e *hostEnv) vmState(t *testing.T, vm string) domain.VMState {
	h, ok := e.hostState(t)
	if !ok {
		return ""
	}
	for _, v := range h.VMs {
		if v.ID == serial+"/"+vm {
			return v.State
		}
	}
	return ""
}

func startReq(name string) ports.StartVMRequest {
	return ports.StartVMRequest{Pool: "macos-xcode27", Generation: "g1", Image: image, VMName: name, DiskGiB: 120, MaxAge: 7 * 24 * time.Hour}
}

// TestHostdEndToEnd guards R-SEC-3 (one-time token enrollment, pending until
// approved), R-MAC-6 (controller commands over the outbound mTLS stream),
// R-MAC-3 (clone, start with the measured flags, persistent stop, 2-VM cap,
// crash recovery, disk full) and R-MAC-4 (identity + config injected at boot).
func TestHostdEndToEnd(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.start(t)
		// Pending until an admin approves the serial; hostd honours retry_after.
		advanceUntil(t, "enrollment polled", time.Minute, func() bool { _, calls := e.ctrl.Counter.Exchanges(); return calls >= 2 })
		_, ok := e.hostState(t)
		require.False(t, ok, "a pending host must not connect")
		e.ctrl.Approve(t, serial)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		fleet := e.ctrl.Host()
		ctx := context.Background()

		// StartVM: pull with short-lived credentials, clone, run, inject identity and config.
		require.NoError(t, fleet.StartVM(ctx, serial, startReq("vm-1")))
		advanceUntil(t, "vm-1 registered", 5*time.Minute, func() bool { return e.vmState(t, "vm-1") == domain.VMRegistered })
		vm := e.tart.VM("cucina-vm-vm-1")
		require.NotNil(t, vm)
		require.Equal(t, 1, e.tart.Pulls[image], "image pulled once with registry credentials")
		require.Contains(t, vm.RunArgs, "--no-graphics")
		require.Contains(t, vm.RunArgs, "--root-disk-opts=caching=cached,sync=none")
		require.Equal(t, int64(120), vm.DiskGB)
		require.Equal(t, 8, vm.CPU, "(18-2)/2 vCPUs")
		require.Equal(t, 28*1024, vm.MemoryMiB, "(64-8)/2 GiB")
		g := vm.Guest
		for _, f := range []string{"/private/etc/cucina/bb/worker.json", "/private/etc/cucina/bb/runner.json",
			"/private/etc/cucina/pki/worker.crt", "/private/etc/cucina/pki/ca.crt"} {
			_, found := g.File(f)
			require.True(t, found, f)
		}
		key, found := g.File("/private/etc/cucina/pki/worker.key")
		require.True(t, found)
		require.Equal(t, int64(0o600), key.Mode)
		require.True(t, g.Running("ai.sloper.cucina.bb-worker") && g.Running("ai.sloper.cucina.bb-runner"))
		require.True(t, g.SpotlightOff)
		crt, _ := g.File("/private/etc/cucina/pki/worker.crt")
		leaf, err := identity.ParseLeaf(crt.Data)
		require.NoError(t, err)
		require.Equal(t, "spiffe://cucina/worker/macos-xcode27/"+serial+"/vm-1", leaf.URIs[0].String())
		wj, _ := g.File("/private/etc/cucina/bb/worker.json")
		require.Contains(t, string(wj.Data), "192.168.64.1:8983", "scheduler through hostd's relay")
		require.Contains(t, string(wj.Data), "192.168.64.1:8981", "storage through hostd's L2 relay")

		// A second VM fits; a third is refused (2-VM Apple limit, R-MAC-3).
		require.NoError(t, fleet.StartVM(ctx, serial, startReq("vm-2")))
		err = fleet.StartVM(ctx, serial, startReq("vm-3"))
		require.ErrorIs(t, err, hostlink.ErrRejected)
		advanceUntil(t, "vm-2 registered", 5*time.Minute, func() bool { return e.vmState(t, "vm-2") == domain.VMRegistered })
		require.Equal(t, 2, e.tart.RunningCount())

		// StopVM keeps the disk (persistent VM, L1 survives); StartVM again does not re-clone.
		require.NoError(t, fleet.StopVM(ctx, serial, "vm-2", time.Minute, "idle"))
		advanceUntil(t, "vm-2 stopped", time.Minute, func() bool { return e.vmState(t, "vm-2") == domain.VMStopped })
		require.NotNil(t, e.tart.VM("cucina-vm-vm-2"), "stopping keeps the VM")
		e.tart.VM("cucina-vm-vm-2").Guest.SetFile("/var/db/cucina/l1-marker", []byte("kept"))
		require.NoError(t, fleet.StartVM(ctx, serial, startReq("vm-2")))
		advanceUntil(t, "vm-2 registered again", 5*time.Minute, func() bool { return e.vmState(t, "vm-2") == domain.VMRegistered })
		_, kept := e.tart.VM("cucina-vm-vm-2").Guest.File("/var/db/cucina/l1-marker")
		require.True(t, kept, "restart reused the same disk")

		// A crashed VM is detected and restarted (crash-only).
		e.tart.Crash("cucina-vm-vm-1")
		advanceUntil(t, "vm-1 restarted after crash", 5*time.Minute, func() bool {
			v := e.tart.VM("cucina-vm-vm-1")
			return v != nil && v.State == "running" && v.Guest.Running("ai.sloper.cucina.bb-worker") && e.vmState(t, "vm-1") == domain.VMRegistered
		})

		// Disk full on clone: the VM fails, the host stays healthy.
		require.NoError(t, fleet.DeleteVM(ctx, serial, "vm-2"))
		advanceUntil(t, "vm-2 deleted", 2*time.Minute, func() bool { return e.tart.VM("cucina-vm-vm-2") == nil })
		e.tart.FailNext("clone", 1, "Error: No space left on device")
		require.NoError(t, fleet.StartVM(ctx, serial, startReq("vm-4")))
		advanceUntil(t, "vm-4 failed clone reported", time.Minute, func() bool {
			h, _ := e.hostState(t)
			for _, v := range h.VMs {
				if v.ID == serial+"/vm-4" {
					return true
				}
			}
			return false
		})
		advanceUntil(t, "vm-4 cloned after backoff", 5*time.Minute, func() bool { return e.vmState(t, "vm-4") == domain.VMRegistered })

		// The site token was exchanged exactly once.
		ex, _ := e.ctrl.Counter.Exchanges()
		require.Equal(t, 1, ex)
		e.powerOff(t)
	})
}

// TestReconnectAndResync guards R-MAC-1 (outbound connection with backoff
// reconnect), R-MAC-6 (offline host → VMs unavailable) and crash-only resync
// (R-TEST-7): toxiproxy-style reset_peer, timeout and down faults on the
// hostd→controller link and a controller restart that loses all state.
func TestReconnectAndResync(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.ctrl.Approve(t, serial)
		e.start(t)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		ctx := context.Background()
		require.NoError(t, e.ctrl.Host().StartVM(ctx, serial, startReq("vm-1")))
		advanceUntil(t, "vm-1 registered", 5*time.Minute, func() bool { return e.vmState(t, "vm-1") == domain.VMRegistered })
		sessions := e.agent.Link().Sessions()

		// reset_peer: the stream breaks; hostd reconnects and the host stays usable.
		e.net.ResetPeer(hostlinktest.HostAddr)
		advanceUntil(t, "reconnected after reset", 2*time.Minute, func() bool { return e.agent.Link().Sessions() > sessions })
		advanceUntil(t, "online after reset", time.Minute, e.online(t))
		require.NoError(t, e.ctrl.Host().Ping(ctx, serial))

		// timeout: the link silently drops data; keepalive/staleness detect it, the
		// controller marks the host offline and its VMs unavailable, hostd reconnects
		// once the link heals.
		sessions = e.agent.Link().Sessions()
		e.net.Blackhole(hostlinktest.HostAddr, true)
		advanceUntil(t, "vm unavailable while the host is unreachable", 3*time.Minute, func() bool {
			return e.vmState(t, "vm-1") == domain.VMUnavailable
		})
		e.net.Blackhole(hostlinktest.HostAddr, false)
		e.net.ResetPeer(hostlinktest.HostAddr)
		advanceUntil(t, "reconnected after timeout", 3*time.Minute, func() bool { return e.agent.Link().Sessions() > sessions })
		advanceUntil(t, "vm registered again", time.Minute, func() bool { return e.vmState(t, "vm-1") == domain.VMRegistered })

		// down: no connection possible for a while; commands fail fast as offline.
		e.net.Down(hostlinktest.HostAddr, true)
		advanceUntil(t, "host offline", 2*time.Minute, func() bool { h, _ := e.hostState(t); return !h.Online })
		require.ErrorIs(t, e.ctrl.Host().StopVM(ctx, serial, "vm-1", time.Minute, "idle"), hostlink.ErrHostOffline)
		dials := e.net.Dials(hostlinktest.HostAddr)
		<-time.After(time.Minute)
		require.Less(t, e.net.Dials(hostlinktest.HostAddr)-dials, 20, "reconnects back off")
		e.net.Down(hostlinktest.HostAddr, false)
		advanceUntil(t, "online after down", 2*time.Minute, e.online(t))

		// Controller restart: a fresh HostService knows nothing; the Hello inventory resyncs it.
		e.ctrl.StopHostService()
		e.ctrl.StartHostService(t)
		advanceUntil(t, "resynced after controller restart", 2*time.Minute, func() bool {
			return e.vmState(t, "vm-1") == domain.VMRegistered
		})
		require.Equal(t, 1, e.tart.RunningCount(), "the VM kept running through every fault")
		e.powerOff(t)
	})
}

// TestHostdRestartAdoptsVMs guards crash-only restarts (R-TEST-7, launchd
// KeepAlive): a restarted hostd rebuilds its state from tart and the journal,
// adopts the running VM without restarting it and never re-sends the token.
func TestHostdRestartAdoptsVMs(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.ctrl.Approve(t, serial)
		e.start(t)
		defer e.powerOff(t)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		require.NoError(t, e.ctrl.Host().StartVM(context.Background(), serial, startReq("vm-1")))
		advanceUntil(t, "vm-1 registered", 5*time.Minute, func() bool { return e.vmState(t, "vm-1") == domain.VMRegistered })
		pid := e.tart.VM("cucina-vm-vm-1").RunArgs
		e.stop(t)
		synctest.Wait() // observe the old stream's EOF before awaiting the new session

		e.start(t)
		advanceUntil(t, "online after restart", 2*time.Minute, e.online(t))
		advanceUntil(t, "vm adopted", time.Minute, func() bool { return e.vmState(t, "vm-1") == domain.VMRegistered })
		require.Equal(t, pid, e.tart.VM("cucina-vm-vm-1").RunArgs)
		require.Equal(t, 1, e.tart.RunningCount())
		ex, calls := e.ctrl.Counter.Exchanges()
		require.Equal(t, 1, ex)
		require.Equal(t, 1, calls, "the site token is never re-sent after enrollment")

		// A failed launch must also survive hostd's crash-only restart: a
		// daemon restart is not a successful VM launch and must not hot-loop.
		for range 3 {
			e.tart.FailNext("run", 1, "hypervisor start failure")
		}
		require.NoError(t, e.ctrl.Host().StartVM(t.Context(), serial, startReq("vm-2")))
		var retryAt time.Time
		advanceUntil(t, "third failed launch", time.Minute, func() bool {
			for _, vm := range e.agent.VMs().Snapshot().VMs {
				if vm.Name == "vm-2" && vm.Failures == 3 {
					retryAt = vm.RetryAt
					return true
				}
			}
			return false
		})
		e.stop(t)
		synctest.Wait()
		e.start(t)
		advanceUntil(t, "online with persisted retry", 10*time.Second, e.online(t))
		require.True(t, time.Now().Before(retryAt))
		<-time.After(time.Until(retryAt) - time.Nanosecond)
		synctest.Wait()
		require.Equal(t, "stopped", e.tart.VM("cucina-vm-vm-2").State, "restart must not bypass the pending launch backoff")
		advanceUntil(t, "retry succeeds after its original deadline", time.Minute, func() bool { return e.vmState(t, "vm-2") == domain.VMRegistered })
	})
}

// TestCommandIdempotency guards idempotent command handling keyed by
// command_id: a duplicate (a controller retry) is executed once and answered
// with the stored result.
func TestCommandIdempotency(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.ctrl.Approve(t, serial)
		e.start(t)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		pull := func() error {
			_, _, err := e.ctrl.Host().Command(context.Background(), serial, &cucinav1.ControllerMessage{CommandId: "pull-1",
				Message: &cucinav1.ControllerMessage_PullImage{PullImage: &cucinav1.PullImage{Image: image}}}, time.Minute)
			return err
		}
		require.NoError(t, pull())
		res, err := e.tart.Run(context.Background(), ports.Command{Args: []string{"prune", "--entries", "caches", "--space-budget", "0"}})
		require.NoError(t, err)
		require.Zero(t, res.ExitCode, "image evicted: re-executing the command would pull again")
		require.NoError(t, pull(), "duplicate answered from the stored result")
		require.Equal(t, 1, e.tart.Pulls[image], "duplicate command id executed once")
		e.powerOff(t)
	})
}

// TestEnrollmentRefused guards R-SEC-3 fail-fast: an invalid site token ends
// hostd with ErrTokenInvalid (exit code 3) instead of retrying forever.
func TestEnrollmentRefused(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.token = "cuc_et_wrong0000_notthesecret"
		e.start(t)
		err := <-e.done
		require.ErrorIs(t, err, identity.ErrTokenInvalid)
		ex, _ := e.ctrl.Counter.Exchanges()
		require.Zero(t, ex)
	})
}

// TestWorkerLogStreaming guards StreamWorkerLogs for Tart VMs (mgmt): one VM's
// unit log, the last N lines, followed while it grows, with certificates, keys
// and tokens redacted on the host before they leave it.
func TestWorkerLogStreaming(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.ctrl.Approve(t, serial)
		e.start(t)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		require.NoError(t, e.ctrl.Host().StartVM(context.Background(), serial, startReq("vm-1")))
		advanceUntil(t, "vm-1 registered", 5*time.Minute, func() bool { return e.vmState(t, "vm-1") == domain.VMRegistered })
		g := e.tart.VM("cucina-vm-vm-1").Guest
		g.SetFile("/var/log/cucina/bb_worker.log", []byte("line 1\nline 2\nkey -----BEGIN PRIVATE KEY-----\nMIGHAgEAMBMGsecret\n-----END PRIVATE KEY-----\nline 4\n"))

		r, err := e.ctrl.Host().Diagnostics(context.Background(), serial, hostlink.DiagnosticsRequest{VM: "vm-1", Unit: "bb-worker", TailLines: 4})
		require.NoError(t, err)
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NotContains(t, string(out), "line 1")
		require.Contains(t, string(out), "line 4")
		require.NotContains(t, string(out), "MIGHAgEAMBMGsecret")

		ctx, cancel := context.WithCancel(context.Background())
		r, err = e.ctrl.Host().Diagnostics(ctx, serial, hostlink.DiagnosticsRequest{VM: "vm-1", Unit: "bb-worker", TailLines: 1, Follow: true})
		require.NoError(t, err)
		got := make(chan string, 16)
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					got <- string(buf[:n])
				}
				if err != nil {
					close(got)
					return
				}
			}
		}()
		require.Equal(t, "line 4\n", <-got)
		g.AppendFile("/var/log/cucina/bb_worker.log", []byte("token cuc_et_abcdef_0123456789 used\nline 6\n"))
		require.Equal(t, "token [REDACTED] used\nline 6\n", <-got)
		cancel()
		for range got {
		}
		// Closing the stream sends CancelCommand: the host stops following long before FollowLimit.
		advanceUntil(t, "followed stream cancelled on the host", 30*time.Second, func() bool { return e.agent.Link().Running() == 0 })
		r, err = e.ctrl.Host().Diagnostics(context.Background(), serial, hostlink.DiagnosticsRequest{VM: "nope", Unit: "bb-worker"})
		require.NoError(t, err)
		_, err = io.ReadAll(r)
		require.ErrorIs(t, err, ports.ErrVMNotFound, "the host rejects an unknown VM")
		e.powerOff(t)
	})
}

// TestHostCertificateRenewal guards R-SEC-2 for hosts: hostd renews its
// certificate before expiry over mTLS (RenewCertificate, delegated to enroll,
// which records the renewal on the host's record) and keeps working.
func TestHostCertificateRenewal(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.ctrl.Approve(t, serial)
		e.start(t)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		first := e.agent.CertificateExpiry()
		require.False(t, first.IsZero())
		lifetime := time.Until(first)
		// Two thirds of the lifetime later the housekeeping loop renews.
		advanceUntil(t, "certificate renewed", lifetime, func() bool { return e.agent.CertificateExpiry().After(first) })
		hosts, err := ef.Server.Admin().ListHosts(context.Background())
		require.NoError(t, err)
		require.Len(t, hosts, 1)
		require.Equal(t, e.agent.CertificateExpiry(), hosts[0].CertExpiry, "enroll recorded the renewal")
		require.Less(t, time.Since(first.Add(-lifetime)), lifetime, "renewed before expiry")
		require.NoError(t, e.ctrl.Host().Ping(context.Background(), serial))
		e.powerOff(t)
	})
}
