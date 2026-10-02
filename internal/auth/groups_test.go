// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/auth/oidctest"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
	"github.com/sloper-ai/cucina/internal/ports"
)

// fakeGroups is a Cloud Identity stand-in whose memberships can change.
type fakeGroups struct {
	mu     sync.Mutex
	groups map[string][]string
	fail   error
}

func (f *fakeGroups) set(member string, groups ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[member] = groups
}

func (f *fakeGroups) DirectGroups(_ context.Context, member string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail; err != nil {
		f.fail = nil
		return nil, err
	}
	return append([]string(nil), f.groups[member]...), nil
}

// TestGroupLookup guards optional group resolution (R-AUTH-5, R-AUTH-6): memberships come
// from the backend, are cached for the policy's TTL (at most 10 min, so renewals re-check
// membership), errors are never cached and fail the exchange closed.
func TestGroupLookup(t *testing.T) {
	ctx := context.Background()
	clock := keystest.NewClock(t0)
	backend := &fakeGroups{groups: map[string][]string{"alice@example.com": {"eng@example.com"}}}
	g := auth.NewCachedGroups(backend, clock, 0)
	defer g.Close()
	alice := ports.Claims{"email": "alice@example.com", "email_verified": true}

	got, err := g.Groups(ctx, "cloud-identity", alice, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"eng@example.com"}, got)

	backend.set("alice@example.com", "eng@example.com", "admins@example.com")
	clock.Advance(4 * time.Minute)
	got, _ = g.Groups(ctx, "cloud-identity", alice, 5*time.Minute)
	assert.Equal(t, []string{"eng@example.com"}, got, "cached within the TTL")
	clock.Advance(2 * time.Minute)
	got, _ = g.Groups(ctx, "cloud-identity", alice, 5*time.Minute)
	assert.Equal(t, []string{"admins@example.com", "eng@example.com"}, got, "re-checked after the TTL")

	backend.set("alice@example.com")
	clock.Advance(10 * time.Minute)
	got, _ = g.Groups(ctx, "cloud-identity", alice, time.Hour)
	assert.Empty(t, got, "a TTL above 10 min is clamped")

	backend.fail = errors.New("cloud identity unavailable")
	clock.Advance(10 * time.Minute)
	_, err = g.Groups(ctx, "cloud-identity", alice, 5*time.Minute)
	require.Error(t, err)

	_, err = g.Groups(ctx, "cloud-identity", ports.Claims{"email": "alice@example.com", "email_verified": "false"}, time.Minute)
	require.Error(t, err, "unverified e-mail is never looked up")

	// Engine integration: group-based grants, and a failed lookup denies the exchange.
	clock2 := keystest.NewClock(t0)
	idp := auth.NewOIDCVerifier(clock2)
	gp := loadPolicy(t, "google-workspace.yaml")
	gp.Spec.GroupLookup = &v1alpha1.GroupLookupSpec{Provider: "cloud-identity", CacheTTL: &metav1.Duration{Duration: 5 * time.Minute}}
	gp.Spec.Grants = append(gp.Spec.Grants, v1alpha1.Grant{
		Name: "group-admins", Condition: "'admins@example.com' in groups", InstanceNames: []string{"*"}, Verbs: []string{"admin"},
	})
	fx := &fixture{clock: clock2, idp: idp, issuers: map[string]*oidctest.Issuer{}}
	fx.issuer(auth.GoogleIssuer)
	backend2 := &fakeGroups{groups: map[string][]string{"alice@example.com": {"admins@example.com"}}}
	e, err := auth.NewEngine(auth.EngineOptions{
		IdentityProvider: idp, Clock: clock2, InstanceNames: []string{"main"},
		Groups: auth.NewCachedGroups(backend2, clock2, 0), Log: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	for _, s := range e.Update([]v1alpha1.TrustPolicy{gp}) {
		require.True(t, s.Valid, s.Message)
	}
	fx.engine = e
	p, err := token(auth.GoogleIssuer, auth.TokenTypeIDToken, google())(fx)
	require.NoError(t, err)
	assert.Equal(t, []string{"admins@example.com"}, p.Groups)
	assert.Equal(t, []string{"main"}, p.Grants[auth.VerbAdmin])

	backend2.fail = errors.New("boom")
	_, err = token(auth.GoogleIssuer, auth.TokenTypeIDToken, google("email", "bob@example.com"))(fx)
	require.Error(t, err)
	assert.Equal(t, auth.CodeServerError, auth.AsError(err).Code, "a failed lookup fails closed")
}
