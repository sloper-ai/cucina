// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/mgmt"
)

// tlsServer serves the fixture's management API over TLS on localhost and returns a
// client built from the generated stubs.
type tlsServer struct {
	gs     *grpc.Server
	conn   *grpc.ClientConn
	client cucinav1.ManagementServiceClient
}

func serveTLS(t *testing.T, f *fixture) *tlsServer {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}},
		MinVersion:   tls.VersionTLS13,
	})))
	f.srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &tlsServer{gs: gs, conn: conn, client: cucinav1.NewManagementServiceClient(conn)}
}

func bearer(ctx context.Context, tok string) context.Context {
	if tok == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

// invokeAny calls any ManagementService method with an empty request message and
// returns the status of the call. A stream counts as admitted once the server sent
// response headers (watch streams send them first); otherwise its status is read.
func invokeAny(ctx context.Context, conn *grpc.ClientConn, method string, stream bool) error {
	if !stream {
		return conn.Invoke(ctx, method, &emptypb.Empty{}, &emptypb.Empty{})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method)
	if err != nil {
		return err
	}
	if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
		return err
	}
	if err := cs.CloseSend(); err != nil {
		return err
	}
	if md, _ := cs.Header(); md != nil {
		return nil
	}
	return cs.RecvMsg(&emptypb.Empty{})
}

// TestEveryRPCEnforcesAccess guards R-AUTH-11 / R-SEC-4 / T10(g) on the wire: for
// every RPC of the generated descriptor (so new RPCs are covered automatically),
// callers without a valid Cucina JWT are rejected, principals without execute/admin
// get nothing, read principals cannot mutate or read sensitive data, and denied
// calls leave the fleet untouched.
func TestEveryRPCEnforcesAccess(t *testing.T) {
	f := newFixture(t)
	ts := serveTLS(t, f)
	desc := cucinav1.ManagementService_ServiceDesc
	type rpc struct {
		name   string
		stream bool
	}
	var rpcs []rpc
	for _, m := range desc.Methods {
		rpcs = append(rpcs, rpc{"/" + desc.ServiceName + "/" + m.MethodName, false})
	}
	for _, s := range desc.Streams {
		rpcs = append(rpcs, rpc{"/" + desc.ServiceName + "/" + s.StreamName, true})
	}
	access := mgmt.MethodAccess()
	for _, r := range rpcs {
		needsAdmin := access[r.name].RequiresAdmin()
		for _, c := range []struct {
			caller string
			token  string
			want   codes.Code // codes.OK: "not denied" (any non-auth outcome)
		}{
			{"anonymous", "", codes.Unauthenticated},
			{"forged token", "eyJhbGciOiJub25lIn0.e30.", codes.Unauthenticated},
			{"no execute or admin", "tok-casonly", codes.PermissionDenied},
			{"reader", "tok-reader", map[bool]codes.Code{true: codes.PermissionDenied, false: codes.OK}[needsAdmin]},
			{"tenant admin", "tok-tenant", map[bool]codes.Code{true: codes.PermissionDenied, false: codes.OK}[needsAdmin]},
		} {
			ctx, cancel := context.WithTimeout(bearer(context.Background(), c.token), 10*time.Second)
			err := invokeAny(ctx, ts.conn, r.name, r.stream)
			cancel()
			got := status.Code(err)
			if c.want == codes.OK {
				if got == codes.Unauthenticated || got == codes.PermissionDenied {
					t.Errorf("%s as %s: denied (%v), want allowed", r.name, c.caller, err)
				}
				continue
			}
			if got != c.want {
				t.Errorf("%s as %s: got %v (%v), want %v", r.name, c.caller, got, err, c.want)
			}
		}
	}
	if n := f.sched.drainCount() + len(f.sched.kills) + f.hosts.mutations() + f.ids.mutations(); n != 0 {
		t.Errorf("denied calls changed the fleet: %d mutations recorded", n)
	}
	if len(f.fleet.floors)+len(f.fleet.paused)+len(f.orphans.deleted) != 0 {
		t.Errorf("denied calls changed pools: floors %v paused %v deleted %v", f.fleet.floors, f.fleet.paused, f.orphans.deleted)
	}
}

type auditLine struct {
	Log         string          `json:"log"`
	Time        time.Time       `json:"time"`
	Principal   string          `json:"principal"`
	DisplayName string          `json:"display_name"`
	Method      string          `json:"method"`
	Mutating    bool            `json:"mutating"`
	Request     json.RawMessage `json:"request"`
	Code        string          `json:"code"`
	Duration    *float64        `json:"duration_seconds"`
	Peer        string          `json:"peer"`
}

func parseAudit(t *testing.T, raw string) []auditLine {
	t.Helper()
	var out []auditLine
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		var l auditLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("audit line is not JSON: %v", err)
		}
		out = append(out, l)
	}
	return out
}

// TestAuditLogOverTLS guards R-CLI-2 / R-AUTH-11: through a real TLS client, each
// mutating call writes exactly one structured record (principal, method, redacted
// request, result code, duration), denied attempts are recorded too, sensitive reads
// are recorded as non-mutations, plain reads are not recorded, and neither the key
// shown once nor secrets inside requests reach the audit log. It also checks that a
// cancelled WatchOverview stream releases its server-side slot.
func TestAuditLogOverTLS(t *testing.T) {
	f := newFixture(t)
	ts := serveTLS(t, f)
	c := ts.client
	adminCtx := bearer(context.Background(), "tok-admin")
	readerCtx := bearer(context.Background(), "tok-reader")
	const leakedJWT = "eyJhbGciOiJFUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJnb29nbGU6NDIifQ.c2lnbmF0dXJl"

	if _, err := c.ListPools(adminCtx, &cucinav1.ListPoolsRequest{}); err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if _, err := c.DrainWorker(adminCtx, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa1"}); err != nil {
		t.Fatalf("DrainWorker: %v", err)
	}
	key, err := c.CreateServiceKey(adminCtx, &cucinav1.CreateServiceKeyRequest{Account: "ci", Description: "replaces " + leakedJWT})
	if err != nil || key.GetKey() != plantedServiceKey {
		t.Fatalf("CreateServiceKey = %v, %v; want the key once", key, err)
	}
	if _, err := c.RevokePrincipal(adminCtx, &cucinav1.RevokePrincipalRequest{Sub: "google:42", Reason: "pasted cuc_sk_k9_bGVha2VkLWtleS12YWx1ZQ in chat"}); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	if _, err := c.ListServiceKeys(adminCtx, &cucinav1.ListServiceKeysRequest{}); err != nil {
		t.Fatalf("ListServiceKeys: %v", err)
	}
	if _, err := c.DrainWorker(readerCtx, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa2"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reader DrainWorker: %v, want PermissionDenied", err)
	}

	// A watcher over TLS gets an overview immediately; cancelling it frees the slot.
	wctx, cancel := context.WithCancel(readerCtx)
	ws, err := c.WatchOverview(wctx, &cucinav1.WatchOverviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ov, err := ws.Recv()
	if err != nil || len(ov.GetPools()) != 3 {
		t.Fatalf("first overview = %v, %v", ov, err)
	}
	cancel()
	ts.gs.GracefulStop() // returns only after every handler has returned
	if o, _, _, _ := f.srv.StreamCounts(); o != 0 {
		t.Errorf("overview watchers after cancel = %d, want 0", o)
	}

	raw := f.audit.String()
	for _, secret := range []string{plantedServiceKey, leakedJWT, "cuc_sk_k9_bGVha2VkLWtleS12YWx1ZQ"} {
		if strings.Contains(raw, secret) {
			t.Errorf("audit log contains secret %q", secret)
		}
	}
	got := parseAudit(t, raw)
	want := []struct {
		principal, method, code string
		mutating                bool
		requestHas              string
	}{
		{"sa:break-glass", cucinav1.ManagementService_DrainWorker_FullMethodName, "OK", true, `"node":"i-0aaaaaaaaaaaaaaa1"`},
		{"sa:break-glass", cucinav1.ManagementService_CreateServiceKey_FullMethodName, "OK", true, `"account":"ci"`},
		{"sa:break-glass", cucinav1.ManagementService_RevokePrincipal_FullMethodName, "OK", true, `"sub":"google:42"`},
		{"sa:break-glass", cucinav1.ManagementService_ListServiceKeys_FullMethodName, "OK", false, `{}`},
		{"google:42", cucinav1.ManagementService_DrainWorker_FullMethodName, "PermissionDenied", true, `"node":"i-0aaaaaaaaaaaaaaa2"`},
	}
	if len(got) != len(want) {
		t.Fatalf("audit records = %d, want %d (reads must not be audited):\n%s", len(got), len(want), raw)
	}
	for i, w := range want {
		g := got[i]
		if g.Log != "audit" || g.Principal != w.principal || g.Method != w.method || g.Code != w.code || g.Mutating != w.mutating {
			t.Errorf("record %d = %+v, want %+v", i, g, w)
		}
		if g.Time.IsZero() || g.Duration == nil || *g.Duration < 0 || !strings.HasPrefix(g.Peer, "127.0.0.1:") {
			t.Errorf("record %d lacks time/duration/peer: %+v", i, g)
		}
		if !strings.Contains(string(g.Request), w.requestHas) {
			t.Errorf("record %d request = %s, want it to contain %s", i, g.Request, w.requestHas)
		}
	}
	if !strings.Contains(string(got[1].Request), mgmt.Redacted) {
		t.Errorf("CreateServiceKey request summary %s does not show the redaction", got[1].Request)
	}
}
