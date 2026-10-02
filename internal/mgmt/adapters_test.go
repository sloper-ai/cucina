// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
	"github.com/sloper-ai/cucina/internal/mgmt"
)

// TestRealCucinaJWTsAndKeyStore guards R-AUTH-9/-10/-11 across the agent boundary:
// over TLS, the management API verifies real Cucina JWTs minted by internal/keys
// (through NewTokenAuthenticator) and manages service keys and the deny-list of a
// real keys.Manager (through KeysAdapter). Admins on every instance name may
// mutate, execute-only callers may only read, foreign-issuer and expired tokens are
// refused, and a session revoked through the API is refused at once by the API.
func TestRealCucinaJWTsAndKeyStore(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := keystest.NewClock(now)
	objs := keystest.NewObjects()
	cfg := config.Auth{
		SigningKeySecret: "cucina-signing", JWKSConfigMap: "cucina-jwks", DenyListConfigMap: "cucina-denylist",
		ServiceKeysSecret: "cucina-service-keys", TokenTTL: config.Duration{Duration: 15 * time.Minute},
	}
	if err := keys.EnsureSigningKeysIn(ctx, objs, cfg, clock, nil); err != nil {
		t.Fatal(err)
	}
	km, err := keys.NewManager(objs, cfg, "https://cucina.example.com", keys.Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := km.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ka := &mgmt.KeysAdapter{Store: km}
	f := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) {
		d.Authenticator = mgmt.NewTokenAuthenticator(km.Verifier)
		d.Keys, d.Revocations = ka, ka
	})
	c := serveTLS(t, f).client

	mint := func(m *keys.Minter, subject string, scopes keys.Scopes, ttl time.Duration) (string, keys.Claims) {
		t.Helper()
		tok, claims, err := m.Mint(keys.MintRequest{Subject: subject, Scopes: scopes, TTL: ttl})
		if err != nil {
			t.Fatal(err)
		}
		return tok, claims
	}
	all := []string{"main", "tenant-b"}
	adminTok, _ := mint(km.Minter, "sa:break-glass", keys.Scopes{Admin: all, Execute: all}, 15*time.Minute)
	readerTok, readerClaims := mint(km.Minter, "google:42", keys.Scopes{Execute: []string{"main"}}, 15*time.Minute)
	foreign := &keys.Minter{Issuer: "https://attacker.example.com", Keys: km.Ring, Clock: clock}
	foreignTok, _ := mint(foreign, "sa:break-glass", keys.Scopes{Admin: all}, 15*time.Minute)
	shortTok, _ := mint(km.Minter, "google:7", keys.Scopes{Admin: all}, time.Minute)

	admin, reader := bearer(ctx, adminTok), bearer(ctx, readerTok)
	code := func(err error) codes.Code { return status.Code(err) }

	key, err := c.CreateServiceKey(admin, &cucinav1.CreateServiceKeyRequest{Account: "ci", Description: "CI cache writer"})
	if err != nil || !strings.HasPrefix(key.GetKey(), keys.ServiceKeyPrefix) || key.GetKeyId() == "" {
		t.Fatalf("CreateServiceKey = %v, %v", key, err)
	}
	if _, err := c.CreateServiceKey(admin, &cucinav1.CreateServiceKeyRequest{Account: keys.BreakGlassAccount}); code(err) != codes.InvalidArgument {
		t.Errorf("break-glass account: %v, want InvalidArgument", err)
	}
	if ks, err := c.ListServiceKeys(admin, &cucinav1.ListServiceKeysRequest{Account: "ci"}); err != nil || len(ks.GetKeys()) != 1 || ks.GetKeys()[0].GetKeyId() != key.GetKeyId() {
		t.Errorf("ListServiceKeys = %v, %v", ks, err)
	}
	if _, err := c.RevokeServiceKey(admin, &cucinav1.RevokeServiceKeyRequest{KeyId: "nope"}); code(err) != codes.NotFound {
		t.Errorf("revoking an unknown key: %v, want NotFound", err)
	}

	if _, err := c.ListPools(reader, &cucinav1.ListPoolsRequest{}); err != nil {
		t.Errorf("reader ListPools: %v", err)
	}
	if _, err := c.DrainWorker(reader, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa1"}); code(err) != codes.PermissionDenied {
		t.Errorf("reader DrainWorker: %v, want PermissionDenied", err)
	}
	if _, err := c.RevokePrincipal(admin, &cucinav1.RevokePrincipalRequest{Sid: "too-short"}); code(err) != codes.InvalidArgument {
		t.Errorf("malformed sid: %v, want InvalidArgument", err)
	}
	rev, err := c.RevokePrincipal(admin, &cucinav1.RevokePrincipalRequest{Sid: readerClaims.Session, Reason: "laptop lost"})
	if err != nil || rev.GetEffectiveBy().AsTime().After(now.Add(3*time.Minute)) {
		t.Fatalf("RevokePrincipal = %v, %v; want effective within 3 min", rev, err)
	}
	if _, err := c.ListPools(reader, &cucinav1.ListPoolsRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("revoked session: %v, want Unauthenticated", err)
	}
	if rs, err := c.ListRevocations(admin, &cucinav1.ListRevocationsRequest{}); err != nil || len(rs.GetRevocations()) != 1 ||
		rs.GetRevocations()[0].GetSid() != readerClaims.Session || rs.GetRevocations()[0].GetCreatedBy() != "sa:break-glass" {
		t.Errorf("ListRevocations = %v, %v", rs, err)
	}

	if _, err := c.ListPools(bearer(ctx, foreignTok), &cucinav1.ListPoolsRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("foreign issuer: %v, want Unauthenticated", err)
	}
	clock.Advance(2 * time.Minute)
	if _, err := c.ListPools(bearer(ctx, shortTok), &cucinav1.ListPoolsRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("expired token: %v, want Unauthenticated", err)
	}
	if _, err := c.DrainWorker(admin, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa1"}); err != nil {
		t.Errorf("admin DrainWorker: %v", err)
	}
}
