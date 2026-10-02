// SPDX-License-Identifier: FSL-1.1-ALv2

// Package hostlinktest is the in-process test bed for hostd ↔ controller
// integration tests (R-TEST-8g, "toxiproxy-style" faults without an external
// toxiproxy): an in-memory network with per-address fault injection (down,
// reset_peer, timeout/blackhole), a fake EnrollmentService and a test
// controller (HostService over mTLS). Everything is in memory so tests run
// inside testing/synctest bubbles with a fake clock.
package hostlinktest

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/test/bufconn"
)

// Net is an in-memory network keyed by "host:port".
type Net struct {
	mu        sync.Mutex
	listeners map[string]*bufconn.Listener
	down      map[string]bool
	blackhole map[string]bool
	conns     map[string]map[*faultConn]struct{}
	dials     map[string]int
}

// NewNet returns an empty network.
func NewNet() *Net {
	return &Net{listeners: map[string]*bufconn.Listener{}, down: map[string]bool{}, blackhole: map[string]bool{},
		conns: map[string]map[*faultConn]struct{}{}, dials: map[string]int{}}
}

// Listen binds addr.
func (n *Net) Listen(addr string) net.Listener {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := bufconn.Listen(1 << 20)
	n.listeners[addr] = l
	return l
}

// Unlisten removes addr (connections fail as "connection refused").
func (n *Net) Unlisten(addr string) {
	n.mu.Lock()
	l := n.listeners[addr]
	delete(n.listeners, addr)
	n.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
}

// ErrRefused is returned when nothing listens or the address is down.
var ErrRefused = errors.New("connection refused")

// Dial connects to addr.
func (n *Net) Dial(ctx context.Context, addr string) (net.Conn, error) {
	n.mu.Lock()
	n.dials[addr]++
	l := n.listeners[addr]
	down := n.down[addr]
	n.mu.Unlock()
	if l == nil || down {
		return nil, ErrRefused
	}
	c, err := l.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	fc := &faultConn{Conn: c, n: n, addr: addr, closed: make(chan struct{})}
	n.mu.Lock()
	if n.conns[addr] == nil {
		n.conns[addr] = map[*faultConn]struct{}{}
	}
	n.conns[addr][fc] = struct{}{}
	n.mu.Unlock()
	return fc, nil
}

// Dials counts dial attempts to addr.
func (n *Net) Dials(addr string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[addr]
}

// Down makes new connections to addr fail ("down" toxic); with reset it also
// resets existing ones.
func (n *Net) Down(addr string, down bool) {
	n.mu.Lock()
	n.down[addr] = down
	n.mu.Unlock()
	if down {
		n.ResetPeer(addr)
	}
}

// ResetPeer closes every live connection to addr ("reset_peer" toxic).
func (n *Net) ResetPeer(addr string) {
	n.mu.Lock()
	var cs []*faultConn
	for c := range n.conns[addr] {
		cs = append(cs, c)
	}
	n.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

// Blackhole stops delivering data on live and new connections to addr while
// on ("timeout" toxic): reads block until the connection is closed by a
// keepalive or deadline.
func (n *Net) Blackhole(addr string, on bool) {
	n.mu.Lock()
	n.blackhole[addr] = on
	n.mu.Unlock()
}

func (n *Net) blackholed(addr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.blackhole[addr]
}

type faultConn struct {
	net.Conn
	n      *Net
	addr   string
	once   sync.Once
	closed chan struct{}
}

func (c *faultConn) Read(p []byte) (int, error) {
	if c.n.blackholed(c.addr) {
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Read(p)
}

func (c *faultConn) Write(p []byte) (int, error) {
	if c.n.blackholed(c.addr) {
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		default:
			return len(p), nil // swallowed
		}
	}
	return c.Conn.Write(p)
}

func (c *faultConn) SetDeadline(t time.Time) error { return c.Conn.SetDeadline(t) }

func (c *faultConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.n.mu.Lock()
		delete(c.n.conns[c.addr], c)
		c.n.mu.Unlock()
	})
	return c.Conn.Close()
}
