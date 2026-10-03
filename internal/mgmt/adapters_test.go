// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
	"github.com/sloper-ai/cucina/internal/mgmt"
	"github.com/sloper-ai/cucina/internal/pki"
)

// caAuditLedger is an in-memory sink for the public structured audit contract.
type caAuditLedger struct {
	mu     sync.Mutex
	events []mgmt.AuditEvent
}

func (a *caAuditLedger) Record(_ context.Context, e mgmt.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

func (a *caAuditLedger) snapshot() []mgmt.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]mgmt.AuditEvent(nil), a.events...)
}

// caConflictClient injects one optimistic-concurrency conflict. A phase that is
// incorrectly retried would then succeed, exposing the retry through stored state.
type caConflictClient struct {
	client.Client
	mu       sync.Mutex
	conflict bool
}

func (c *caConflictClient) failNextConflict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conflict = true
}

func (c *caConflictClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.mu.Lock()
	conflict := c.conflict
	c.conflict = false
	c.mu.Unlock()
	if conflict {
		return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("key-material-canary"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

// TestOperatorCARotation guards R-OPS-5/-6 and R-AUTH-11: the public TLS RPC
// requires deployment-wide admin, audits every attempted phase, enforces explicit
// operator attestations, and applies exactly the existing optimistic Secret phases.
// Invalid phases and conflicts cannot mutate the CA or disclose key material.
func TestOperatorCARotation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	material, err := pki.NewCAMaterial(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), pki.DefaultCAValidity, nil)
	require.NoError(t, err)
	object := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cucina", Name: "cucina-ca"}, Data: material}
	store := &caConflictClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(object).Build()}
	ledger := &caAuditLedger{}
	f := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) {
		d.CA = &mgmt.CAAdapter{Client: store, Namespace: "cucina", SecretName: "cucina-ca"}
		d.Auditor = ledger
	})
	rpc := serveTLS(t, f).client
	read := func() (*pki.CA, map[string][]byte, [32]byte) {
		t.Helper()
		var secret corev1.Secret
		require.NoError(t, store.Get(ctx, types.NamespacedName{Namespace: "cucina", Name: "cucina-ca"}, &secret))
		ca, err := pki.ParseCA(secret.Data)
		require.NoError(t, err)
		encoded, err := json.Marshal(secret.Data)
		require.NoError(t, err)
		return ca, secret.Data, sha256.Sum256(encoded)
	}
	initial, _, _ := read()
	originalRoot := initial.Certificate().Raw
	const (
		introduce = cucinav1.CARotationPhase_CA_ROTATION_PHASE_INTRODUCE
		activate  = cucinav1.CARotationPhase_CA_ROTATION_PHASE_ACTIVATE
		retire    = cucinav1.CARotationPhase_CA_ROTATION_PHASE_RETIRE
	)
	for _, tc := range []struct {
		name, token                     string
		phase                           cucinav1.CARotationPhase
		trust, leaves, conflict         bool
		code                            codes.Code
		roots                           int
		pending, originalSignerExpected bool
	}{
		{name: "anonymous", phase: introduce, code: codes.Unauthenticated},
		{name: "execute-only reader", token: "tok-reader", phase: introduce, code: codes.PermissionDenied},
		{name: "partial tenant admin", token: "tok-tenant", phase: introduce, code: codes.PermissionDenied},
		{name: "unspecified phase", token: "tok-admin", code: codes.InvalidArgument},
		{name: "unknown phase", token: "tok-admin", phase: cucinav1.CARotationPhase(99), code: codes.InvalidArgument},
		{name: "activate without introduce", token: "tok-admin", phase: activate, trust: true, code: codes.FailedPrecondition},
		{name: "conflict is aborted without retry", token: "tok-admin", phase: introduce, conflict: true, code: codes.Aborted},
		{name: "introduce", token: "tok-admin", phase: introduce, roots: 2, pending: true, originalSignerExpected: true},
		{name: "duplicate introduce", token: "tok-admin", phase: introduce, code: codes.FailedPrecondition},
		{name: "retire before activation", token: "tok-admin", phase: retire, leaves: true, code: codes.FailedPrecondition},
		{name: "activation needs trust attestation", token: "tok-admin", phase: activate, code: codes.FailedPrecondition},
		{name: "activate", token: "tok-admin", phase: activate, trust: true, roots: 2},
		{name: "retirement needs leaf attestation", token: "tok-admin", phase: retire, code: codes.FailedPrecondition},
		{name: "retire", token: "tok-admin", phase: retire, leaves: true, roots: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, beforeData, before := read()
			priorEvents := len(ledger.snapshot())
			if tc.conflict {
				store.failNextConflict()
			}
			resp, err := rpc.RotateCA(bearer(ctx, tc.token), &cucinav1.RotateCARequest{
				Phase: tc.phase, TrustDistributed: tc.trust, OldLeavesRetired: tc.leaves,
			})
			require.Equal(t, tc.code, status.Code(err))
			ca, data, after := read()
			if tc.code != codes.OK {
				require.Nil(t, resp)
				require.Equal(t, before, after, "rejected phases must leave the stored CA unchanged")
			} else {
				require.Equal(t, tc.phase, resp.GetPhase())
				require.Len(t, ca.Roots(), tc.roots)
				require.Equal(t, tc.pending, len(data[pki.SecretNextCAKey]) > 0)
				require.Equal(t, tc.originalSignerExpected, bytes.Equal(originalRoot, ca.Certificate().Raw))
				body, err := protojson.Marshal(resp)
				require.NoError(t, err)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(body, &fields))
				require.Len(t, fields, 1, "only the applied phase may leave the API")
				require.Contains(t, fields, "phase")
			}
			events := ledger.snapshot()
			require.Len(t, events, priorEvents+1, "each attempted phase is audited")
			event := events[len(events)-1]
			require.Equal(t, cucinav1.ManagementService_RotateCA_FullMethodName, event.Method)
			require.True(t, event.Mutating)
			require.Equal(t, tc.code.String(), event.Code)
			var audited cucinav1.RotateCARequest
			require.NoError(t, protojson.Unmarshal(event.Request, &audited))
			require.Equal(t, tc.phase, audited.GetPhase())
			require.Equal(t, tc.trust, audited.GetTrustDistributed())
			require.Equal(t, tc.leaves, audited.GetOldLeavesRetired())
			if tc.token != "" {
				require.Equal(t, tokens[tc.token].Subject, event.Principal)
			}
			record, err := json.Marshal(event)
			require.NoError(t, err)
			for _, secret := range [][]byte{beforeData[pki.SecretCAKey], beforeData[pki.SecretNextCAKey], data[pki.SecretCAKey], data[pki.SecretNextCAKey]} {
				if len(secret) > 0 && bytes.Contains(record, secret) {
					t.Fatal("CA key material reached the audit sink")
				}
			}
			if bytes.Contains(record, []byte("key-material-canary")) {
				t.Fatal("upstream Secret error content reached the audit sink")
			}
		})
	}

	t.Run("unconfigured CA is unavailable", func(t *testing.T) {
		plain := newFixture(t)
		_, err := serveTLS(t, plain).client.RotateCA(bearer(ctx, "tok-admin"), &cucinav1.RotateCARequest{Phase: introduce})
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
	t.Run("externally managed CA cannot rotate", func(t *testing.T) {
		var secret corev1.Secret
		require.NoError(t, store.Get(ctx, types.NamespacedName{Namespace: "cucina", Name: "cucina-ca"}, &secret))
		delete(secret.Data, pki.SecretCAKey)
		require.NoError(t, store.Update(ctx, &secret))
		_, err := rpc.RotateCA(bearer(ctx, "tok-admin"), &cucinav1.RotateCARequest{Phase: introduce})
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}

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
