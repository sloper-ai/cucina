// SPDX-License-Identifier: FSL-1.1-ALv2

// Package relay is hostd's L4 pass-through from the VMs to their upstreams
// (ADR 0700): VM → host L2 (loopback bb_storage) and VM → the controller's
// scheduler worker endpoint, where the VM's own short-lived certificate makes
// mTLS end-to-end. VMs therefore talk only to their host (R-MAC-4), hostd (root)
// makes every connection itself (Local Network privacy, R-MAC-10), and a VM can
// only connect from the IP hostd assigned to it, on the vmnet bridge, within a
// per-VM connection limit.
package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Admission maps a peer address to the VM allowed to use it.
type Admission interface {
	// VMForIP returns the managed VM that owns ip.
	VMForIP(ip netip.Addr) (vm string, ok bool)
}

// Relay accepts VM connections on Listen and pipes them to Upstream().
type Relay struct {
	Name     string // "storage" | "scheduler" (logs, metrics)
	Listen   string
	Upstream func() string // host:port; "" = unavailable
	Admit    Admission
	// SameNetwork reports whether local and remote addresses share a directly
	// attached network (connections must arrive on the VM bridge, not the LAN).
	SameNetwork   func(local, remote netip.Addr) bool
	MaxConnsPerVM int
	DialTimeout   time.Duration
	// OnUpstream is called after every upstream dial attempt (dead-man input).
	OnUpstream func(vm string, ok bool)
	Log        *slog.Logger
	// Listener, if set, is used instead of binding Listen (tests: in-memory listeners).
	Listener net.Listener
	// DialUpstream overrides the upstream dialer (tests).
	DialUpstream func(ctx context.Context, addr string) (net.Conn, error)

	BytesFromVMs atomic.Uint64
	BytesToVMs   atomic.Uint64
	Rejected     atomic.Uint64

	mu    sync.Mutex
	ln    net.Listener
	conns map[string]int // live connections per VM
	open  map[net.Conn]struct{}
}

// Addr is the bound listen address (after Start).
func (r *Relay) Addr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return nil
	}
	return r.ln.Addr()
}

// Start binds the listener.
func (r *Relay) Start() error {
	ln := r.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", r.Listen); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.ln = ln
	r.conns = map[string]int{}
	r.open = map[net.Conn]struct{}{}
	r.mu.Unlock()
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	if r.MaxConnsPerVM <= 0 {
		r.MaxConnsPerVM = 64
	}
	if r.DialTimeout <= 0 {
		r.DialTimeout = 10 * time.Second
	}
	return nil
}

// Serve accepts until ctx is done.
func (r *Relay) Serve(ctx context.Context) error {
	r.mu.Lock()
	ln := r.ln
	r.mu.Unlock()
	if ln == nil {
		return errors.New("relay: Start not called")
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		r.mu.Lock()
		for c := range r.open {
			_ = c.Close()
		}
		r.mu.Unlock()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go r.handle(ctx, c)
	}
}

func addrOf(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(ta.IP); ok {
			return ip.Unmap()
		}
	}
	return netip.Addr{}
}

// LiveVMs returns the VMs with at least one open relayed connection.
func (r *Relay) LiveVMs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.conns))
	for vm, n := range r.conns {
		if n > 0 {
			out = append(out, vm)
		}
	}
	return out
}

func (r *Relay) handle(ctx context.Context, c net.Conn) {
	remote, local := addrOf(c.RemoteAddr()), addrOf(c.LocalAddr())
	vm, ok := r.Admit.VMForIP(remote)
	if !ok || (r.SameNetwork != nil && !r.SameNetwork(local, remote)) {
		r.Rejected.Add(1)
		r.Log.Warn("relay: rejected connection", "relay", r.Name, "remote", remote.String(), "local", local.String())
		_ = c.Close()
		return
	}
	r.mu.Lock()
	if r.conns[vm] >= r.MaxConnsPerVM {
		r.mu.Unlock()
		r.Rejected.Add(1)
		r.Log.Warn("relay: per-VM connection limit reached", "relay", r.Name, "vm", vm)
		_ = c.Close()
		return
	}
	r.conns[vm]++
	r.open[c] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.conns[vm]--
		delete(r.open, c)
		r.mu.Unlock()
		_ = c.Close()
	}()

	target := ""
	if r.Upstream != nil {
		target = r.Upstream()
	}
	if target == "" {
		if r.OnUpstream != nil {
			r.OnUpstream(vm, false)
		}
		return
	}
	var up net.Conn
	var err error
	if r.DialUpstream != nil {
		up, err = r.DialUpstream(ctx, target)
	} else {
		d := net.Dialer{Timeout: r.DialTimeout, KeepAlive: 30 * time.Second}
		up, err = d.DialContext(ctx, "tcp", target)
	}
	if r.OnUpstream != nil {
		r.OnUpstream(vm, err == nil)
	}
	if err != nil {
		r.Log.Warn("relay: upstream dial failed", "relay", r.Name, "vm", vm, "err", err)
		return
	}
	r.mu.Lock()
	r.open[up] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.open, up)
		r.mu.Unlock()
		_ = up.Close()
	}()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(counting{up, &r.BytesFromVMs}, c)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(counting{c, &r.BytesToVMs}, up)
		closeWrite(c)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// counting counts bytes as they are relayed (long-lived gRPC connections).
type counting struct {
	w io.Writer
	n *atomic.Uint64
}

func (c counting) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(uint64(n))
	return n, err
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = c.Close()
}

// InterfaceNetworks reports whether local and remote are on the same directly
// attached network according to the host's interfaces (the vmnet bridge for VMs).
func InterfaceNetworks(local, remote netip.Addr) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			p, err := netip.ParsePrefix(ipn.String())
			if err != nil {
				continue
			}
			if p.Addr().Unmap() == local && p.Masked().Contains(remote) {
				return true
			}
		}
	}
	return false
}

// GatewayFor returns the host's address on the interface whose network
// contains vmIP (the vmnet gateway the VM uses to reach hostd).
func GatewayFor(vmIP netip.Addr) (netip.Addr, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			p, err := netip.ParsePrefix(ipn.String())
			if err != nil {
				continue
			}
			if p.Masked().Contains(vmIP) && p.Addr().Unmap() != vmIP {
				return p.Addr().Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}
