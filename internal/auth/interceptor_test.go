// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"crypto/ecdsa"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

type denyList map[string]bool

func (d denyList) IsDenied(sid, sub string) bool { return d["sid:"+sid] || d["sub:"+sub] }

// TestInterceptor guards R-AUTH-11: the management API accepts only valid Cucina JWTs,
// read-only methods need execute or admin somewhere, mutating methods need admin on every
// instance name, unknown methods are denied.
func TestInterceptor(t *testing.T) {
	clock := keystest.NewClock(t0)
	kid, priv, err := keys.GenerateKey(nil)
	require.NoError(t, err)
	ks, err := keys.NewKeySet(keys.Bootstrap(kid, t0), map[string]*ecdsa.PrivateKey{kid: priv})
	require.NoError(t, err)
	const iss = "https://cucina.example.com"
	minter := &keys.Minter{Issuer: iss, Keys: keys.StaticKeys{Set: ks}, Clock: clock}
	deny := denyList{}
	verifier := &keys.Verifier{Issuer: iss, Keys: keys.StaticKeys{Set: ks}, Clock: clock, Deny: deny}
	ic := auth.NewInterceptor(verifier, auth.ManagementRequirements(), []string{"main", "team-a"})

	mint := func(sub string, s keys.Scopes) string {
		raw, _, err := minter.Mint(keys.MintRequest{Subject: sub, Scopes: s, TTL: 15 * time.Minute})
		require.NoError(t, err)
		return raw
	}
	both := []string{"main", "team-a"}
	dev := mint("google:dev", keys.Scopes{CASRead: both, Execute: both})
	reader := mint("google:reader", keys.Scopes{CASRead: both, ACRead: both})
	partialAdmin := mint("google:partial", keys.Scopes{Admin: []string{"main"}})
	admin := mint("google:admin", keys.Scopes{Admin: both})
	revoked := mint("google:revoked", keys.Scopes{Admin: both})
	deny["sub:google:revoked"] = true
	otherKID, otherPriv, _ := keys.GenerateKey(nil)
	foreignSet, _ := keys.NewKeySet(keys.Bootstrap(otherKID, t0), map[string]*ecdsa.PrivateKey{otherKID: otherPriv})
	foreign, _, _ := (&keys.Minter{Issuer: iss, Keys: keys.StaticKeys{Set: foreignSet}, Clock: clock}).Mint(
		keys.MintRequest{Subject: "google:admin", Scopes: keys.Scopes{Admin: both}, TTL: time.Minute})
	wrongIss, _, _ := (&keys.Minter{Issuer: "https://other.example.com", Keys: keys.StaticKeys{Set: ks}, Clock: clock}).Mint(
		keys.MintRequest{Subject: "google:admin", Scopes: keys.Scopes{Admin: both}, TTL: time.Minute})

	const (
		read    = "/cucina.v1.ManagementService/ListPools"
		mutate  = "/cucina.v1.ManagementService/DrainWorker"
		unknown = "/cucina.v1.ManagementService/DoesNotExist"
	)
	rows := []struct {
		name   string
		method string
		header string // authorization metadata ("" = none)
		want   codes.Code
	}{
		{"unauthenticated request", read, "", codes.Unauthenticated},
		{"not a bearer token", read, "Basic " + dev, codes.Unauthenticated},
		{"token signed by a foreign key", mutate, "Bearer " + foreign, codes.Unauthenticated},
		{"token of another issuer", mutate, "Bearer " + wrongIss, codes.Unauthenticated},
		{"deny-listed subject", mutate, "Bearer " + revoked, codes.Unauthenticated},
		{"read with execute", read, "Bearer " + dev, codes.OK},
		{"read with only cache verbs", read, "Bearer " + reader, codes.PermissionDenied},
		{"mutation without admin", mutate, "Bearer " + dev, codes.PermissionDenied},
		{"mutation with admin on one instance only", mutate, "Bearer " + partialAdmin, codes.PermissionDenied},
		{"mutation with admin everywhere", mutate, "bearer " + admin, codes.OK},
		{"unknown method", unknown, "Bearer " + admin, codes.PermissionDenied},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			if r.header != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", r.header))
			}
			var caller *auth.Caller
			_, err := ic.Unary()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: r.method}, func(ctx context.Context, _ any) (any, error) {
				caller, _ = auth.CallerFromContext(ctx)
				return nil, nil
			})
			assert.Equal(t, r.want, status.Code(err), "%v", err)
			if r.want == codes.OK {
				require.NotNil(t, caller)
				assert.True(t, keys.ValidSubject(caller.Subject))
			} else {
				assert.Nil(t, caller, "handler must not run")
			}
		})
	}

	t.Run("expired token", func(t *testing.T) {
		c := keystest.NewClock(t0)
		short, _, err := (&keys.Minter{Issuer: iss, Keys: keys.StaticKeys{Set: ks}, Clock: c}).Mint(keys.MintRequest{Subject: "google:a", Scopes: keys.Scopes{Admin: both}, TTL: time.Minute})
		require.NoError(t, err)
		v := &keys.Verifier{Issuer: iss, Keys: keys.StaticKeys{Set: ks}, Clock: c}
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+short))
		_, err = auth.NewInterceptor(v, auth.ManagementRequirements(), both).Authorize(ctx, mutate)
		require.NoError(t, err)
		c.Advance(time.Minute)
		_, err = auth.NewInterceptor(v, auth.ManagementRequirements(), both).Authorize(ctx, mutate)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("stream handlers see the caller", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+dev))
		var caller *auth.Caller
		err := ic.Stream()(nil, &fakeStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: "/cucina.v1.ManagementService/WatchOverview"},
			func(_ any, ss grpc.ServerStream) error {
				caller, _ = auth.CallerFromContext(ss.Context())
				return nil
			})
		require.NoError(t, err)
		require.NotNil(t, caller)
		assert.Equal(t, "google:dev", caller.Subject)
	})
}

// TestManagementRequirements guards the method table derived from the proto: read-only
// prefixes are readable except the sensitive reads of ADR 0580; everything else mutates.
func TestManagementRequirements(t *testing.T) {
	m := auth.ManagementRequirements()
	for method, want := range map[string]auth.Requirement{
		"GetStatus": auth.RequireRead, "WatchOverview": auth.RequireRead, "ListPools": auth.RequireRead, "WatchOperations": auth.RequireRead,
		"StreamWorkerLogs": auth.RequireAdmin, "CollectSupportBundle": auth.RequireAdmin, "ListServiceKeys": auth.RequireAdmin,
		"ListRevocations": auth.RequireAdmin, "ListEnrollTokens": auth.RequireAdmin,
		"DrainWorker": auth.RequireAdmin, "GarbageCollectPool": auth.RequireAdmin, "HostDiagnostics": auth.RequireAdmin,
		"CreateServiceKey": auth.RequireAdmin, "RevokePrincipal": auth.RequireAdmin, "KillOperations": auth.RequireAdmin,
	} {
		assert.Equal(t, want, m["/cucina.v1.ManagementService/"+method], method)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeStream) Context() context.Context { return f.ctx }
