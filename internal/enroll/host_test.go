// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

const (
	pending  = cucinav1.EnrollHostResponse_STATUS_PENDING
	approved = cucinav1.EnrollHostResponse_STATUS_APPROVED
	denied   = cucinav1.EnrollHostResponse_STATUS_DENIED
	invalid  = cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID
)

func hostIdentity(t *testing.T, serial string) pki.Identity {
	t.Helper()
	id, err := pki.HostIdentity(serial)
	require.NoError(t, err)
	return id
}

// hostLifecycle is the Mac host admission story of R-SEC-3 / UC19 / T14 against
// one set of stores: unknown serial -> Pending MacHost -> idempotent polling ->
// approve -> certificate; re-enrollment denied; token revocation does not
// affect the enrolled host; removal ends renewal and frees the serial.
func hostLifecycle(t *testing.T, st stores) {
	e := newEnv(t, st, nil)
	ctx := context.Background()
	tokenID, token := e.createToken("office-1", 2, 0)
	key, otherKey := pkitest.Key(t), pkitest.Key(t)
	const serial = "C02XK0AAJGH6"

	resp := e.enrollHost(e.hostRequest(token, serial, key))
	require.Equal(t, pending, resp.GetStatus(), resp.GetMessage())
	require.Equal(t, 30*time.Second, resp.GetRetryAfter().AsDuration())
	require.Contains(t, resp.GetMessage(), "cucinactl hosts approve "+serial)
	host, err := st.hosts.Get(ctx, serial)
	require.NoError(t, err, "unknown serials create a Pending MacHost an admin can approve")
	require.False(t, host.Approved)
	require.Equal(t, "office-1", host.Site)
	require.Equal(t, tokenID, host.TokenID)

	resp = e.enrollHost(e.hostRequest(token, serial, key)) // re-signed CSR, same key
	require.Equal(t, pending, resp.GetStatus(), "idempotent polling")
	hosts, err := st.hosts.List(ctx)
	require.NoError(t, err)
	require.Len(t, hosts, 1, "same serial + key -> same pending record")
	require.Equal(t, denied, e.enrollHost(e.hostRequest(token, serial, otherKey)).GetStatus(), "another key cannot take over a pending serial")

	_, err = e.srv.Admin().ApproveHost(ctx, serial, "admin@test")
	require.NoError(t, err)
	resp = e.enrollHost(e.hostRequest(token, serial, key))
	require.Equal(t, approved, resp.GetStatus(), resp.GetMessage())
	leaf := pkitest.ParseChain(t, resp.GetCertificatePem())[0]
	require.Equal(t, "spiffe://cucina/host/"+serial, leaf.URIs[0].String())
	require.Equal(t, e.clock.Now().Add(7*24*time.Hour), leaf.NotAfter)
	require.Equal(t, "hosts.cucina.example:8446", resp.GetHostEndpoint())
	require.NotEmpty(t, resp.GetCaPem())

	require.Equal(t, denied, e.enrollHost(e.hostRequest(token, serial, otherKey)).GetStatus(), "re-enrolling an enrolled serial needs an admin")
	require.Equal(t, approved, e.enrollHost(e.hostRequest(token, serial, key)).GetStatus(), "the bound key may fetch a fresh certificate (recovery)")

	require.NoError(t, e.srv.Admin().RevokeEnrollToken(ctx, tokenID, "admin@test"))
	id := hostIdentity(t, serial)
	e.clock.Advance(5 * 24 * time.Hour)
	renewed, err := e.srv.RenewCertificate(ctx, id, &cucinav1.RenewCertificateRequest{CsrPem: pkitest.CSR(t, otherKey, nil)})
	require.NoError(t, err, "revoking the token does not affect enrolled hosts")
	require.Equal(t, e.clock.Now().Add(7*24*time.Hour), renewed.GetExpiresAt().AsTime(), "renewal before expiry, key rotated")
	require.Equal(t, invalid, e.enrollHost(e.hostRequest(token, "H4X9K2LM7Q", key)).GetStatus(), "a revoked token blocks new enrollments")

	_, err = e.srv.Admin().RemoveHost(ctx, serial, "admin@test")
	require.NoError(t, err)
	_, err = e.srv.RenewCertificate(ctx, id, &cucinav1.RenewCertificateRequest{CsrPem: pkitest.CSR(t, otherKey, nil)})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "a removed host is not renewed")
	require.Equal(t, []revocation{{"spiffe://cucina/host/" + serial, "host removed", "admin@test"}}, e.revoker.subs,
		"its identity is deny-listed until the certificate expires")
	_, token2 := e.createToken("office-1", 1, 0)
	require.Equal(t, pending, e.enrollHost(e.hostRequest(token2, serial, otherKey)).GetStatus(), "a removed serial may enroll again")
}

// TestHostLifecycle runs the admission story on the in-memory stores.
func TestHostLifecycle(t *testing.T) { hostLifecycle(t, memoryStores()) }

// TestEnrollHostTokenChecks is the token row set of R-SEC-3: expired, revoked,
// wrong secret, unknown, malformed, wrong site and over-limit tokens are
// STATUS_TOKEN_INVALID, and the token is never echoed or logged.
func TestEnrollHostTokenChecks(t *testing.T) {
	e := newEnv(t, memoryStores(), nil)
	ctx := context.Background()
	key := pkitest.Key(t)
	_, valid := e.createToken("office-1", 1, 0)
	_, short := e.createToken("office-1", 5, time.Hour)
	revokedID, revoked := e.createToken("office-1", 5, 0)
	require.NoError(t, e.srv.Admin().RevokeEnrollToken(ctx, revokedID, "admin@test"))
	_, otherSite := e.createToken("lab-2", 5, 0)
	e.clock.Advance(2 * time.Hour)
	wrongSecret := valid[:len(valid)-4] + "AAAA"
	if wrongSecret == valid {
		wrongSecret = valid[:len(valid)-4] + "BBBB"
	}
	unknownID := "cuc_et_0123456789abcdef_" + valid[len(valid)-43:]

	require.Equal(t, pending, e.enrollHost(e.hostRequest(valid, "AAAAAAAAAA01", key)).GetStatus())
	for _, tc := range []struct {
		name, token, serial, site, msg string
	}{
		{"expired", short, "AAAAAAAAAA02", "", "expired"},
		{"revoked", revoked, "AAAAAAAAAA03", "", "revoked"},
		{"wrong secret", wrongSecret, "AAAAAAAAAA04", "", "invalid"},
		{"unknown id", unknownID, "AAAAAAAAAA05", "", "invalid"},
		{"malformed", "not-a-token", "AAAAAAAAAA06", "", "invalid"},
		{"empty", "", "AAAAAAAAAA07", "", "invalid"},
		{"host reports another site", otherSite, "AAAAAAAAAA08", "office-1", "another site"},
		{"maximum host count reached", valid, "AAAAAAAAAA09", "", "maximum host count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := e.hostRequest(tc.token, tc.serial, key)
			req.Facts.Site = tc.site
			resp := e.enrollHost(req)
			require.Equal(t, invalid, resp.GetStatus())
			require.Contains(t, resp.GetMessage(), tc.msg)
			if tc.token != "" {
				require.NotContains(t, resp.GetMessage(), tc.token)
			}
			_, err := e.hosts.Get(ctx, tc.serial)
			require.ErrorIs(t, err, enroll.ErrNotFound, "no MacHost for rejected tokens")
		})
	}
	// A pre-registered host bound to another site cannot be enrolled with this site's token.
	_, err := e.srv.Admin().RegisterSerials(ctx, &cucinav1.RegisterHostSerialsRequest{Serials: []string{"BBBBBBBBBB01"}, Site: "lab-2"}, "admin@test")
	require.NoError(t, err)
	_, token := e.createToken("office-1", 5, 0)
	require.Equal(t, invalid, e.enrollHost(e.hostRequest(token, "BBBBBBBBBB01", key)).GetStatus())
}

// TestPreRegisteredSerialsEnrollAtOnce covers `cucinactl hosts register` (serials
// pasted from Apple Business): the first exchange is approved immediately, and
// one site token serves several hosts (T14: "the same token works for a second VM").
func TestPreRegisteredSerialsEnrollAtOnce(t *testing.T) {
	e := newEnv(t, memoryStores(), nil)
	ctx := context.Background()
	reg, err := e.srv.Admin().RegisterSerials(ctx, &cucinav1.RegisterHostSerialsRequest{
		Serials: []string{"h4x9k2lm7q ", "H4X9K2LM7Q", "ZT2RYV7NNP"}, Site: "office-1", Labels: map[string]string{"rack": "a"},
	}, "admin@test")
	require.NoError(t, err)
	require.Equal(t, []string{"H4X9K2LM7Q", "ZT2RYV7NNP"}, reg.GetRegistered(), "serials are canonicalised and de-duplicated")
	_, token := e.createToken("office-1", 2, 0)
	for _, serial := range []string{"H4X9K2LM7Q", "ZT2RYV7NNP"} {
		resp := e.enrollHost(e.hostRequest(token, serial, pkitest.Key(t)))
		require.Equal(t, approved, resp.GetStatus(), resp.GetMessage())
	}
	host, err := e.hosts.Get(ctx, "ZT2RYV7NNP")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"rack": "a"}, host.Labels)
	require.True(t, host.Enrolled())
}

// TestHostServiceCertificates covers the certificate side of HostService:
// VM identities (R-MAC-4, docs/security.md: <= 12 h, serial from the caller's
// certificate) and renewal refusal for identities that are not live hosts.
func TestHostServiceCertificates(t *testing.T) {
	e := newEnv(t, memoryStores(), nil)
	ctx := context.Background()
	e.pools.unknown["ghost"] = true
	_, err := e.srv.Admin().RegisterSerials(ctx, &cucinav1.RegisterHostSerialsRequest{Serials: []string{"H4X9K2LM7Q"}}, "admin@test")
	require.NoError(t, err)
	_, token := e.createToken("office-1", 1, 0)
	require.Equal(t, approved, e.enrollHost(e.hostRequest(token, "H4X9K2LM7Q", pkitest.Key(t))).GetStatus())
	host := hostIdentity(t, "H4X9K2LM7Q")

	vm, err := e.srv.IssueVMIdentity(ctx, host, &cucinav1.IssueVMIdentityRequest{VmName: "vm-1", Pool: "macos-arm64-xcode27.0", CsrPem: pkitest.CSR(t, pkitest.Key(t), nil)})
	require.NoError(t, err)
	leaf := pkitest.ParseChain(t, vm.GetCertificatePem())[0]
	require.Equal(t, "spiffe://cucina/worker/macos-arm64-xcode27.0/H4X9K2LM7Q/vm-1", leaf.URIs[0].String())
	require.Equal(t, e.clock.Now().Add(12*time.Hour), leaf.NotAfter)
	require.Equal(t, "H4X9K2LM7Q/vm-1", vm.GetSettings().GetNode())

	for _, tc := range []struct {
		name string
		peer pki.Identity
		req  *cucinav1.IssueVMIdentityRequest
		code codes.Code
	}{
		{"unknown host", hostIdentity(t, "ZZZZZZZZZZ99"), &cucinav1.IssueVMIdentityRequest{VmName: "vm-1", Pool: "p"}, codes.PermissionDenied},
		{"worker identity", pki.Identity{Role: pki.RoleWorker, Pool: "p", InstanceID: "i-0123456789abcdef0"}, &cucinav1.IssueVMIdentityRequest{VmName: "vm-1", Pool: "p"}, codes.PermissionDenied},
		{"bad VM name", host, &cucinav1.IssueVMIdentityRequest{VmName: "../vm", Pool: "p", CsrPem: pkitest.CSR(t, pkitest.Key(t), nil)}, codes.InvalidArgument},
		{"unknown pool", host, &cucinav1.IssueVMIdentityRequest{VmName: "vm-1", Pool: "ghost", CsrPem: pkitest.CSR(t, pkitest.Key(t), nil)}, codes.FailedPrecondition},
	} {
		_, err := e.srv.IssueVMIdentity(ctx, tc.peer, tc.req)
		require.Equal(t, tc.code, status.Code(err), tc.name)
	}
	_, err = e.srv.RenewCertificate(ctx, pki.Identity{Role: pki.RoleWorker, Pool: "p", InstanceID: "i-0123456789abcdef0"}, &cucinav1.RenewCertificateRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}
