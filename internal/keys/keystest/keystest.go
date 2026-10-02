// SPDX-License-Identifier: FSL-1.1-ALv2

// Package keystest provides fakes for the key and token tests of internal/keys,
// internal/auth and internal/sts: an in-memory keys.Objects with Kubernetes'
// optimistic-concurrency semantics and fault injection, and a manually advanced
// ports.Clock.
package keystest

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sloper-ai/cucina/internal/keys"
)

// Objects is an in-memory keys.Objects. It assigns ResourceVersions, rejects updates
// carrying a stale ResourceVersion with keys.ErrConflict and can fail the next call of
// an operation ("GetSecret", "UpdateConfigMap", …) with an injected error.
type Objects struct {
	mu      sync.Mutex
	rv      int
	secrets map[string]*corev1.Secret
	cms     map[string]*corev1.ConfigMap
	fail    map[string]error
	writes  int
}

// NewObjects returns an empty store.
func NewObjects() *Objects {
	return &Objects{secrets: map[string]*corev1.Secret{}, cms: map[string]*corev1.ConfigMap{}, fail: map[string]error{}}
}

// FailNext makes the next call of op return err.
func (o *Objects) FailNext(op string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail[op] = err
}

// Writes counts successful create/update calls.
func (o *Objects) Writes() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writes
}

func (o *Objects) injected(op string) error {
	if err, ok := o.fail[op]; ok {
		delete(o.fail, op)
		return err
	}
	return nil
}

func (o *Objects) nextRV() string {
	o.rv++
	return strconv.Itoa(o.rv)
}

// GetSecret implements keys.Objects.
func (o *Objects) GetSecret(_ context.Context, name string) (*corev1.Secret, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.injected("GetSecret"); err != nil {
		return nil, err
	}
	s, ok := o.secrets[name]
	if !ok {
		return nil, fmt.Errorf("secret %q: %w", name, keys.ErrNotFound)
	}
	return s.DeepCopy(), nil
}

// CreateSecret implements keys.Objects.
func (o *Objects) CreateSecret(_ context.Context, s *corev1.Secret) (*corev1.Secret, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.injected("CreateSecret"); err != nil {
		return nil, err
	}
	if _, ok := o.secrets[s.Name]; ok {
		return nil, fmt.Errorf("secret %q: %w", s.Name, keys.ErrAlreadyExists)
	}
	c := s.DeepCopy()
	c.ResourceVersion = o.nextRV()
	o.secrets[s.Name] = c
	o.writes++
	return c.DeepCopy(), nil
}

// UpdateSecret implements keys.Objects.
func (o *Objects) UpdateSecret(_ context.Context, s *corev1.Secret) (*corev1.Secret, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.injected("UpdateSecret"); err != nil {
		return nil, err
	}
	cur, ok := o.secrets[s.Name]
	if !ok {
		return nil, fmt.Errorf("secret %q: %w", s.Name, keys.ErrNotFound)
	}
	if s.ResourceVersion != "" && s.ResourceVersion != cur.ResourceVersion {
		return nil, fmt.Errorf("secret %q: %w", s.Name, keys.ErrConflict)
	}
	c := s.DeepCopy()
	c.ResourceVersion = o.nextRV()
	o.secrets[s.Name] = c
	o.writes++
	return c.DeepCopy(), nil
}

// GetConfigMap implements keys.Objects.
func (o *Objects) GetConfigMap(_ context.Context, name string) (*corev1.ConfigMap, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.injected("GetConfigMap"); err != nil {
		return nil, err
	}
	c, ok := o.cms[name]
	if !ok {
		return nil, fmt.Errorf("configmap %q: %w", name, keys.ErrNotFound)
	}
	return c.DeepCopy(), nil
}

// CreateConfigMap implements keys.Objects.
func (o *Objects) CreateConfigMap(_ context.Context, c *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.injected("CreateConfigMap"); err != nil {
		return nil, err
	}
	if _, ok := o.cms[c.Name]; ok {
		return nil, fmt.Errorf("configmap %q: %w", c.Name, keys.ErrAlreadyExists)
	}
	n := c.DeepCopy()
	n.ResourceVersion = o.nextRV()
	o.cms[c.Name] = n
	o.writes++
	return n.DeepCopy(), nil
}

// UpdateConfigMap implements keys.Objects.
func (o *Objects) UpdateConfigMap(_ context.Context, c *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.injected("UpdateConfigMap"); err != nil {
		return nil, err
	}
	cur, ok := o.cms[c.Name]
	if !ok {
		return nil, fmt.Errorf("configmap %q: %w", c.Name, keys.ErrNotFound)
	}
	if c.ResourceVersion != "" && c.ResourceVersion != cur.ResourceVersion {
		return nil, fmt.Errorf("configmap %q: %w", c.Name, keys.ErrConflict)
	}
	n := c.DeepCopy()
	n.ResourceVersion = o.nextRV()
	o.cms[c.Name] = n
	o.writes++
	return n.DeepCopy(), nil
}

// Clock is a manually advanced ports.Clock.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewClock starts at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now implements ports.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After implements ports.Clock; the channel fires when Advance passes now+d.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Sleep implements ports.Clock.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-c.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Advance moves time forward and fires due timers.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// Set moves the clock to t (forward only).
func (c *Clock) Set(t time.Time) {
	if d := t.Sub(c.Now()); d > 0 {
		c.Advance(d)
	}
}
