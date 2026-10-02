// SPDX-License-Identifier: FSL-1.1-ALv2

package relay_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sloper-ai/cucina/internal/hostd/relay"
)

type admit struct {
	mu sync.Mutex
	m  map[netip.Addr]string
}

func (a *admit) VMForIP(ip netip.Addr) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	vm, ok := a.m[ip]
	return vm, ok
}

// echo is an upstream that echoes and counts connections.
func echo(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln
}

// TestRelay guards ADR 0700 / R-MAC-4: VMs reach their upstream only through
// hostd, only from an admitted VM address on the bridge network, within a
// per-VM connection limit; upstream dial outcomes feed the dead-man switch.
func TestRelay(t *testing.T) {
	up := echo(t)
	var mu sync.Mutex
	outcomes := map[string][]bool{}
	loop := netip.MustParseAddr("127.0.0.1")
	allowed := &admit{m: map[netip.Addr]string{loop: "vm-1"}}
	r := &relay.Relay{Name: "scheduler", Listen: "127.0.0.1:0", Admit: allowed, MaxConnsPerVM: 2,
		SameNetwork: func(local, remote netip.Addr) bool { return local == loop && remote == loop },
		Upstream:    func() string { return up.Addr().String() },
		OnUpstream: func(vm string, ok bool) {
			mu.Lock()
			defer mu.Unlock()
			outcomes[vm] = append(outcomes[vm], ok)
		}}
	require.NoError(t, r.Start())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Serve(ctx) }()

	roundTrip := func(c net.Conn) error {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second)) // hang guard only
		if _, err := c.Write([]byte("ping")); err != nil {
			return err
		}
		buf := make([]byte, 4)
		_, err := io.ReadFull(c, buf)
		return err
	}
	c1, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer func() { _ = c1.Close() }()
	require.NoError(t, roundTrip(c1), "an admitted VM reaches the upstream")
	c2, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer func() { _ = c2.Close() }()
	require.NoError(t, roundTrip(c2))
	require.ElementsMatch(t, []string{"vm-1"}, r.LiveVMs())

	c3, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer func() { _ = c3.Close() }()
	require.Error(t, roundTrip(c3), "the per-VM connection limit closes a third connection")

	allowed.mu.Lock()
	delete(allowed.m, loop)
	allowed.mu.Unlock()
	c4, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer func() { _ = c4.Close() }()
	require.Error(t, roundTrip(c4), "an address that is not a managed VM is refused")
	require.EqualValues(t, 2, r.Rejected.Load())
	mu.Lock()
	require.Equal(t, []bool{true, true}, outcomes["vm-1"])
	mu.Unlock()
	// An echoed packet may reach the peer before the relay goroutine resumes
	// its post-Write counter update. Half-close and drain to EOF: the relay
	// counts each write before propagating FIN, so this orders final totals
	// without polling, sleeping or assuming goroutine scheduling.
	for _, c := range []net.Conn{c1, c2} {
		require.NoError(t, c.(*net.TCPConn).CloseWrite())
		_, err := io.Copy(io.Discard, c)
		require.NoError(t, err)
	}
	require.EqualValues(t, 8, r.BytesFromVMs.Load())
	require.EqualValues(t, 8, r.BytesToVMs.Load())
}

// Guards: R-DATA-7 / T13 — persistent L2/TLS connections report both directions
// incrementally, before EOF; closing a connection neither loses nor doubles bytes.
func TestRelayCountsLiveTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ln := bufconn.Listen(1024)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r := &relay.Relay{
			Listener: ln,
			// bufconn uses opaque, non-IP addresses. The real TCP test above
			// separately proves bridge admission and per-VM connection limits.
			Admit:    &admit{m: map[netip.Addr]string{{}: "vm-1"}},
			Upstream: func() string { return "upstream.test:1" },
			DialUpstream: func(context.Context, string) (net.Conn, error) {
				upstream, peer := net.Pipe()
				go func() { _, _ = io.Copy(peer, peer); _ = peer.Close() }()
				return upstream, nil
			},
		}
		require.NoError(t, r.Start())
		done := make(chan error, 1)
		go func() { done <- r.Serve(ctx) }()
		defer func() { cancel(); require.NoError(t, <-done) }()
		client, err := ln.DialContext(ctx)
		require.NoError(t, err)
		defer func() { _ = client.Close() }()
		for i, payload := range []string{"ping", "pong"} {
			_, err := client.Write([]byte(payload))
			require.NoError(t, err)
			b := make([]byte, len(payload))
			_, err = io.ReadFull(client, b)
			require.NoError(t, err)
			require.Equal(t, payload, string(b))
			// Receipt of the echo alone does not order the relay goroutine's
			// post-Write accounting. Quiescence waits for that goroutine to
			// block on its next read, without closing this persistent stream.
			synctest.Wait()
			require.Equal(t, []string{"vm-1"}, r.LiveVMs())
			require.EqualValues(t, (i+1)*4, r.BytesFromVMs.Load())
			require.EqualValues(t, (i+1)*4, r.BytesToVMs.Load())
		}
		require.NoError(t, client.Close())
		synctest.Wait()
		require.Empty(t, r.LiveVMs())
		require.EqualValues(t, 8, r.BytesFromVMs.Load())
		require.EqualValues(t, 8, r.BytesToVMs.Load())
	})
}
