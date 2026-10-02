// SPDX-License-Identifier: FSL-1.1-ALv2

package keys_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmespath/go-jmespath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

// fataler is the part of testing.TB that *rapid.T also implements.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

func denied(t fataler, file []byte, sid, sub any) bool {
	t.Helper()
	md := map[string]any{"private": map[string]any{"sid": sid, "sub": sub}}
	res, err := jmespath.Search(keys.BuildbarnDenyFragment, map[string]any{
		"authenticationMetadata": md, "instanceName": "main", "files": map[string]any{"denylist": string(file)},
	})
	if err != nil {
		t.Fatalf("evaluating the deny fragment: %v", err)
	}
	return res != true
}

// TestDenyListExactMatch is a property over the deny-list encoding (R-AUTH-9): for any set
// of revoked sids and subjects (including subjects that are prefixes, suffixes or
// percent-encodings of each other) the Buildbarn deny fragment denies a principal exactly
// when its sid or its sub is revoked.
func TestDenyListExactMatch(t *testing.T) {
	alphabet := []string{"a", "b", "1", ":", "/", "@", "+", "=", ".", "~", "-", "_", "%", "2", "0"}
	subGen := rapid.Custom(func(rt *rapid.T) string {
		scheme := rapid.SampledFrom([]string{"google", "github", "sa", "spiffe", "s"}).Draw(rt, "scheme")
		rest := rapid.SliceOfN(rapid.SampledFrom(alphabet), 1, 6).Draw(rt, "rest")
		return scheme + ":" + strings.Join(rest, "")
	})
	sidGen := rapid.Custom(func(rt *rapid.T) string {
		return strings.Repeat(rapid.SampledFrom([]string{"A", "B", "-", "_"}).Draw(rt, "c"), 21) + rapid.SampledFrom([]string{"x", "y"}).Draw(rt, "d")
	})
	rapid.Check(t, func(rt *rapid.T) {
		subs := rapid.SliceOfNDistinct(subGen, 0, 6, rapid.ID[string]).Draw(rt, "subs")
		sids := rapid.SliceOfNDistinct(sidGen, 0, 3, rapid.ID[string]).Draw(rt, "sids")
		var revs []keys.Revocation
		for _, s := range subs {
			revs = append(revs, keys.Revocation{Kind: keys.RevokeSubject, Value: s})
		}
		for _, s := range sids {
			revs = append(revs, keys.Revocation{Kind: keys.RevokeSession, Value: s})
		}
		file, err := keys.EncodeDenyList(revs, t0)
		if err != nil {
			rt.Fatal(err)
		}
		sub := subGen.Draw(rt, "principal-sub")
		sid := sidGen.Draw(rt, "principal-sid")
		want := false
		for _, s := range subs {
			want = want || s == sub
		}
		for _, s := range sids {
			want = want || s == sid
		}
		if got := denied(rt, file, sid, sub); got != want {
			rt.Fatalf("sid %q sub %q: denied=%v want %v; file %s", sid, sub, got, want, file)
		}
		// A certificate principal has no sid.
		if got := denied(rt, file, nil, sub); got != func() bool {
			for _, s := range subs {
				if s == sub {
					return true
				}
			}
			return false
		}() {
			rt.Fatalf("certificate principal %q: denied=%v; file %s", sub, got, file)
		}
	})
}

// TestDenyListRejectsUnsafeValues guards the encoding's exactness assumption: a value JSON
// would escape (or that could never be minted) is refused instead of written fail-open.
func TestDenyListRejectsUnsafeValues(t *testing.T) {
	for _, r := range []keys.Revocation{
		{Kind: keys.RevokeSubject, Value: `google:"quoted"`},
		{Kind: keys.RevokeSubject, Value: `google:back\slash`},
		{Kind: keys.RevokeSubject, Value: "google:with space"},
		{Kind: keys.RevokeSubject, Value: "google:<script>"},
		{Kind: keys.RevokeSubject, Value: "google:ünïcode"},
		{Kind: keys.RevokeSubject, Value: "null"},
		{Kind: keys.RevokeSubject, Value: "google:" + strings.Repeat("x", 600)},
		{Kind: keys.RevokeSession, Value: "null"},
		{Kind: keys.RevokeSession, Value: "too-short"},
		{Kind: "jti", Value: "AAAAAAAAAAAAAAAAAAAAAA"},
	} {
		_, err := keys.EncodeDenyList([]keys.Revocation{r}, t0)
		assert.Error(t, err, "%s %q", r.Kind, r.Value)
	}
	safe := "sa:a.b_c~d:e@f/g+h=i-j9Z"
	got, err := keys.NormalizeSubject(safe)
	require.NoError(t, err)
	assert.Equal(t, safe, got, "safe characters are kept")
	got, err = keys.NormalizeSubject(`github:1:Build "and" test/ü`)
	require.NoError(t, err)
	assert.Equal(t, "github:1:Build%20%22and%22%20test/%C3%BC", got, "minted subjects are always encodable")
	_, err = keys.EncodeDenyList([]keys.Revocation{{Kind: keys.RevokeSubject, Value: got}}, t0)
	assert.NoError(t, err)
}

func newStore(objs keys.Objects, clock *keystest.Clock) *keys.RevocationStore {
	return &keys.RevocationStore{Objects: objs, ConfigMap: "cucina-denylist", Clock: clock}
}

// TestRevocationStore guards revocation propagation (R-AUTH-9, UC15): the ConfigMap that
// Buildbarn reads is rewritten before Revoke returns; the revoking replica refuses at
// once, other replicas after their next refresh; sid entries live exactly as long as a
// token can (max TTL + skew); subject entries until removed or expired; the 64 KiB cap.
func TestRevocationStore(t *testing.T) {
	ctx := context.Background()
	clock := keystest.NewClock(t0)
	objs := keystest.NewObjects()
	require.NoError(t, keys.EnsureSigningKeysIn(ctx, objs, authConfig(), clock, nil))
	a, b := newStore(objs, clock), newStore(objs, clock)
	require.NoError(t, b.Refresh(ctx))
	file := func() []byte {
		cm, err := objs.GetConfigMap(ctx, "cucina-denylist")
		require.NoError(t, err)
		return []byte(cm.Data["denylist.json"])
	}
	assert.Equal(t, keys.EmptyDenyList, string(file()))

	const sub, sid = "google:110248495921238986420", "Zm9vYmFyYmF6cXV4MTIzND"
	objs.FailNext("UpdateConfigMap", fmt.Errorf("injected: %w", keys.ErrConflict))
	rec, effectiveBy, err := a.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSubject, Value: sub, Reason: "left the company", Actor: "google:admin"})
	require.NoError(t, err, "a write conflict is retried")
	assert.Equal(t, t0.Add(3*time.Minute), effectiveBy)
	assert.True(t, rec.ExpiresAt.IsZero())
	assert.True(t, denied(t, file(), nil, sub), "Buildbarn's file is updated before Revoke returns")
	assert.True(t, a.IsDenied("", sub), "the revoking replica refuses renewal at once")
	assert.False(t, b.IsDenied("", sub), "another replica sees it after its refresh")
	require.NoError(t, b.Refresh(ctx))
	assert.True(t, b.IsDenied("", sub))

	_, _, err = a.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSession, Value: sid, Actor: "google:admin"})
	require.NoError(t, err)
	assert.True(t, denied(t, file(), sid, "google:someone-else"))
	assert.Equal(t, `{"version":1,"sids":["sid:`+sid+`"],"subs":["sub:`+sub+`"]}`, string(file()),
		"the exact file shape published in docs/security.md (chart test vectors depend on it)")
	clock.Advance(keys.MaxTokenTTL + keys.ClockSkew - time.Second)
	assert.True(t, a.IsDenied(sid, ""), "kept while a token carrying it can be valid")
	clock.Advance(time.Second)
	assert.False(t, a.IsDenied(sid, ""))
	require.NoError(t, a.Prune(ctx))
	assert.False(t, denied(t, file(), sid, "google:someone-else"), "pruned from Buildbarn's file")
	assert.True(t, denied(t, file(), nil, sub), "subjects stay")

	// Re-revoking is idempotent; a subject with an expiry disappears afterwards.
	_, _, err = a.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSubject, Value: sub, Actor: "google:other"})
	require.NoError(t, err)
	list, err := a.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, t0, list[0].CreatedAt, "the original revocation time is kept")
	_, _, err = a.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSubject, Value: "sa:temp", ExpiresAt: clock.Now().Add(time.Hour)})
	require.NoError(t, err)
	clock.Advance(time.Hour)
	require.NoError(t, a.Prune(ctx))
	assert.False(t, denied(t, file(), nil, "sa:temp"))

	require.NoError(t, a.Remove(ctx, keys.RevokeSubject, sub))
	assert.False(t, a.IsDenied("", sub))
	assert.Equal(t, keys.EmptyDenyList, string(file()))

	// The file is capped: every authorizer scans it on every request.
	var lastErr error
	for i := 0; i < 200 && lastErr == nil; i++ {
		_, _, lastErr = a.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSubject, Value: fmt.Sprintf("github:%04d:%s", i, strings.Repeat("w", 480))})
	}
	assert.ErrorIs(t, lastErr, keys.ErrDenyListFull)
	assert.LessOrEqual(t, len(file()), keys.MaxDenyListBytes)
}
