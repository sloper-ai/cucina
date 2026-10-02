// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlink_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/sloper-ai/cucina/internal/hostd"
	"github.com/sloper-ai/cucina/internal/hostd/config"
	"github.com/sloper-ai/cucina/internal/hostd/facts"
	"github.com/sloper-ai/cucina/internal/hostd/render"
	"github.com/sloper-ai/cucina/internal/hostd/secretstore"
	"github.com/sloper-ai/cucina/internal/hostd/sys"
	"github.com/sloper-ai/cucina/internal/hostlink/hostlinktest"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
	"github.com/sloper-ai/cucina/internal/providers/tart"
	"github.com/sloper-ai/cucina/internal/providers/tart/faketart"
)

// TestHostFleetConformance runs the HostFleet conformance suite (R-TEST-8b)
// against hostlink with a real hostd (user mode, fake tart, in-memory mTLS).
// Waits are event-driven: hostlink signals every host state change.
func TestHostFleetConformance(t *testing.T) {
	porttest.RunHostFleet(t, func(t *testing.T) porttest.HostFleetHarness {
		n := hostlinktest.NewNet()
		ctrl := hostlinktest.NewController(t, n, nil)
		ctrl.Approve(t, serial)
		ft := faketart.New()
		ft.Registry[image] = faketart.Image{Ref: image, SizeGB: 70, Private: true}
		cfg := config.Default()
		cfg.ControllerURL = "https://" + hostlinktest.EnrollAddr
		cfg.CACertificates = ctrl.CAPool()
		cfg.SiteEnrollmentToken = ctrl.Token
		cfg.MetricsListen = ""
		dial := grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return n.Dial(ctx, addr) })
		a, err := hostd.New(hostd.Options{
			Config: cfg, UserMode: true, StateDir: t.TempDir(), Version: "test",
			Exec: ft, FS: sys.FS{}, Clock: hostlinktest.Clock{}, Secrets: secretstore.File{Dir: t.TempDir()},
			Runtime: tart.New(tart.Options{Exec: ft}), Render: render.BBConfig{},
			Facts: func(context.Context) (facts.Facts, error) {
				return facts.Facts{Serial: serial, Cores: 18, MemoryGiB: 64, MacOSVersion: "27.0"}, nil
			},
			StorageRelayListener: n.Listen("hostd.test:8981"), SchedulerRelayListener: n.Listen("hostd.test:8983"),
			RelayDial:         n.Dial,
			Gateway:           func(netip.Addr) (netip.Addr, bool) { return netip.MustParseAddr("192.168.64.1"), true },
			EnrollDialOptions: []grpc.DialOption{dial}, HostDialOptions: []grpc.DialOption{dial},
		})
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		t.Cleanup(func() {
			cancel()
			<-done
			ft.PowerOff()
		})
		await := func(t *testing.T, what string, cond func() bool) {
			t.Helper()
			guard := time.After(30 * time.Second) // hang guard; progress is event-driven
			for !cond() {
				select {
				case <-ctrl.Changed:
				case <-guard:
					t.Fatalf("timed out waiting for %s", what)
				}
			}
		}
		h := porttest.HostFleetHarness{Fleet: ctrl.Host(), Host: serial, Pool: "macos-xcode27", Image: image, Generation: "g1", Await: await}
		await(t, "host online", func() bool {
			hs, _ := ctrl.Host().Hosts(context.Background())
			return len(hs) == 1 && hs[0].Online
		})
		return h
	})
}
