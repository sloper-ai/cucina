// SPDX-License-Identifier: FSL-1.1-ALv2

package link_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/link"
	"github.com/sloper-ai/cucina/internal/hostlink/hostlinktest"
)

// Guards: R-OBS-1 protocol compatibility/freshness — old controllers receive no
// snapshots, and failed sessions never replay old samples as current evidence.
func TestMetricsAreNegotiatedAndNeverReplayed(t *testing.T) {
	for _, minor := range []uint32{0, 1} {
		t.Run(string(rune('0'+minor)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				n := hostlinktest.NewNet()
				peer := &metricsPeer{minor: minor}
				server := grpc.NewServer()
				cucinav1.RegisterHostServiceServer(server, peer)
				listener := n.Listen("metrics.test:8446")
				go func() { _ = server.Serve(listener) }()
				defer server.Stop()
				l := link.New(link.Options{Endpoint: func() string { return "metrics.test:8446" },
					TLS: func() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS12} },
					DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()),
						grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return n.Dial(ctx, addr) })},
					Hello:     func() *cucinav1.Hello { return &cucinav1.Hello{SerialNumber: "TESTSERIAL01"} },
					Heartbeat: func() *cucinav1.Heartbeat { return &cucinav1.Heartbeat{} },
					Clock:     hostlinktest.Clock{}, MinBackoff: time.Second, MaxBackoff: time.Second,
				})
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- l.Run(ctx) }()
				defer func() { cancel(); require.NoError(t, <-done) }()
				synctest.Wait()
				session, supported := l.MetricsSession()
				require.Equal(t, minor >= 1, supported)
				snapshot := &cucinav1.MetricsSnapshot{Source: cucinav1.MetricsSnapshot_SOURCE_HOSTD,
					PrometheusText: []byte("# TYPE go_goroutines gauge\ngo_goroutines 3\n")}
				l.SendMetrics(session, snapshot)
				synctest.Wait()
				require.Equal(t, int64(minor), peer.snapshots.Load())
				n.Down("metrics.test:8446", true)
				synctest.Wait()
				l.SendMetrics(session, snapshot) // unavailable: must be dropped
				n.Down("metrics.test:8446", false)
				<-time.After(5 * time.Second)
				synctest.Wait()
				current, _ := l.MetricsSession()
				require.Greater(t, current, session)
				l.SendMetrics(session, snapshot) // old scrape finishing after reconnect
				synctest.Wait()
				require.Equal(t, int64(minor), peer.snapshots.Load())
				l.SendMetrics(current, snapshot)
				synctest.Wait()
				require.Equal(t, int64(2*minor), peer.snapshots.Load())
			})
		})
	}
}

// A protocol peer, not an interaction mock: it accepts Hello and continuously
// receives heartbeats/snapshots while the fault network replaces connections.
type metricsPeer struct {
	cucinav1.UnimplementedHostServiceServer
	minor     uint32
	snapshots atomic.Int64
}

func (p *metricsPeer) Connect(stream cucinav1.HostService_ConnectServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_Welcome{Welcome: &cucinav1.Welcome{
		Protocol: &cucinav1.ProtocolVersion{Major: 1, Minor: p.minor},
	}}}); err != nil {
		return err
	}
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if m.GetMetricsSnapshot() != nil {
			p.snapshots.Add(1)
		}
	}
}
