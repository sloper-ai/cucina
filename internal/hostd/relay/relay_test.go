// SPDX-License-Identifier: FSL-1.1-ALv2

package relay_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	defer c1.Close()
	require.NoError(t, roundTrip(c1), "an admitted VM reaches the upstream")
	c2, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer c2.Close()
	require.NoError(t, roundTrip(c2))
	require.ElementsMatch(t, []string{"vm-1"}, r.LiveVMs())

	c3, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer c3.Close()
	require.Error(t, roundTrip(c3), "the per-VM connection limit closes a third connection")

	allowed.mu.Lock()
	delete(allowed.m, loop)
	allowed.mu.Unlock()
	c4, err := net.Dial("tcp", r.Addr().String())
	require.NoError(t, err)
	defer c4.Close()
	require.Error(t, roundTrip(c4), "an address that is not a managed VM is refused")
	require.EqualValues(t, 2, r.Rejected.Load())
	mu.Lock()
	require.Equal(t, []bool{true, true}, outcomes["vm-1"])
	mu.Unlock()
	require.EqualValues(t, 8, r.BytesFromVMs.Load())
}
