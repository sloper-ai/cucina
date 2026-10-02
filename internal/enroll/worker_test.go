// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// TestEnrollWorker is the accept/reject table of the EC2 trust boundary
// (R-SEC-3, R-POOL-3; T10(h) "worker without a valid certificate" starts here:
// no valid identity, no certificate).
func TestEnrollWorker(t *testing.T) {
	e := newEnv(t, memoryStores(), nil)
	key := pkitest.Key(t)
	ctx := context.Background()
	e.pools.unknown["ghost"] = true

	signed := func(inst ports.Instance, mutate func(map[string]any), region string, o sigOpts) *cucinav1.EnrollWorkerRequest {
		req := e.workerRequest(inst, key)
		req.InstanceIdentityDocument = identityDocument(t, inst, mutate)
		req.Signature = e.aws.sign(t, region, req.InstanceIdentityDocument, o)
		return req
	}
	for _, tc := range []struct {
		name string
		req  func() *cucinav1.EnrollWorkerRequest
		code codes.Code
		msg  string
	}{
		{"valid", func() *cucinav1.EnrollWorkerRequest { return e.workerRequest(e.launch(nil), key) }, codes.OK, ""},
		{"valid BER signature (indefinite lengths)", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), nil, testRegion, sigOpts{ber: true})
		}, codes.OK, ""},
		{"valid signature without signed attributes", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), nil, testRegion, sigOpts{noAttrs: true})
		}, codes.OK, ""},
		{"forged document", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			req.InstanceIdentityDocument = identityDocument(t, e.launch(nil), nil) // genuine signature, other document
			return req
		}, codes.Unauthenticated, "differs from the signed content"},
		{"modified document", func() *cucinav1.EnrollWorkerRequest {
			inst := e.launch(nil)
			req := e.workerRequest(inst, key)
			req.InstanceIdentityDocument = identityDocument(t, inst, func(d map[string]any) { d["instanceType"] = "c8i.48xlarge" })
			return req
		}, codes.Unauthenticated, "differs from the signed content"},
		{"wrong signature (not AWS)", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), nil, "attacker", sigOpts{})
		}, codes.Unauthenticated, "verification failed"},
		{"tampered signature", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			der, _ := base64.StdEncoding.DecodeString(req.Signature)
			der[len(der)-1] ^= 1
			req.Signature = base64.StdEncoding.EncodeToString(der)
			return req
		}, codes.Unauthenticated, ""},
		{"RSA-1024 /signature form", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			req.Signature = base64.StdEncoding.EncodeToString(make([]byte, 128))
			return req
		}, codes.Unauthenticated, "rsa2048"},
		{"Region without AWS certificate", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), func(d map[string]any) { d["region"] = "eu-west-9" }, "attacker", sigOpts{})
		}, codes.Unauthenticated, "no AWS certificate"},
		{"wrong account", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), func(d map[string]any) { d["accountId"] = "999999999999" }, testRegion, sigOpts{})
		}, codes.PermissionDenied, "another AWS account"},
		{"wrong Region", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), func(d map[string]any) { d["region"] = "us-east-1"; d["availabilityZone"] = "us-east-1a" }, "us-east-1", sigOpts{})
		}, codes.PermissionDenied, "Region"},
		{"stale pendingTime", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			e.clock.Advance(21 * time.Minute)
			return req
		}, codes.PermissionDenied, "stale"},
		{"pendingTime in the future", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), func(d map[string]any) {
				d["pendingTime"] = e.clock.Now().Add(10 * time.Minute).Format(time.RFC3339)
			}, testRegion, sigOpts{})
		}, codes.PermissionDenied, "future"},
		{"architecture mismatch", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			req.Arch = "arm64"
			return req
		}, codes.PermissionDenied, "architecture"},
		{"instance of another cluster (never visible here)", func() *cucinav1.EnrollWorkerRequest {
			return e.workerRequest(e.launch(func(t map[string]string) { t[domain.TagCluster] = "other" }), key)
		}, codes.Unavailable, "not visible"},
		{"instance without pool tag", func() *cucinav1.EnrollWorkerRequest {
			return e.workerRequest(e.launch(func(t map[string]string) { delete(t, domain.TagPool) }), key)
		}, codes.PermissionDenied, "no pool tag"},
		{"instance not managed by Cucina", func() *cucinav1.EnrollWorkerRequest {
			return e.workerRequest(e.launch(func(t map[string]string) { t[domain.TagManagedBy] = "someone" }), key)
		}, codes.PermissionDenied, "not managed by cucina-controller"},
		{"terminated instance", func() *cucinav1.EnrollWorkerRequest {
			inst := e.launch(nil)
			req := e.workerRequest(inst, key)
			_, err := e.compute.Terminate(ctx, testCluster, []string{inst.ID})
			require.NoError(t, err)
			e.clock.Advance(time.Minute)
			return req
		}, codes.PermissionDenied, "terminated"},
		{"instance not visible in Describe yet (retryable)", func() *cucinav1.EnrollWorkerRequest {
			inst := e.launch(nil)
			inst.ID = "i-0123456789abcdef0" // signed and fresh, but EC2 does not show it (yet)
			return signed(inst, nil, testRegion, sigOpts{})
		}, codes.Unavailable, "not visible"},
		{"launch token of another ledger epoch", func() *cucinav1.EnrollWorkerRequest {
			tok := scaling.MakeToken(scaling.TokenPrefix(testCluster, testPool), "ffffffff", 0)
			return e.workerRequest(e.launch(func(t map[string]string) { t[domain.TagLaunchToken] = tok }), key)
		}, codes.PermissionDenied, "not launched by this controller"},
		{"launch token of another cluster", func() *cucinav1.EnrollWorkerRequest {
			tok := scaling.MakeToken(scaling.TokenPrefix("other-cluster", testPool), "e1a2b3c4", 0)
			return e.workerRequest(e.launch(func(t map[string]string) { t[domain.TagLaunchToken] = tok }), key)
		}, codes.PermissionDenied, "not launched by this controller"},
		{"launch token never allocated", func() *cucinav1.EnrollWorkerRequest {
			tok := scaling.MakeToken(scaling.TokenPrefix(testCluster, testPool), "e1a2b3c4", 1_000_000)
			return e.workerRequest(e.launch(func(t map[string]string) { t[domain.TagLaunchToken] = tok }), key)
		}, codes.PermissionDenied, "not launched by this controller"},
		{"image differs from Describe", func() *cucinav1.EnrollWorkerRequest {
			return signed(e.launch(nil), func(d map[string]any) { d["imageId"] = "ami-0fffffffffffffff0" }, testRegion, sigOpts{})
		}, codes.PermissionDenied, "image"},
		{"pool not configured", func() *cucinav1.EnrollWorkerRequest {
			return e.workerRequest(e.launchPool("ghost", nil), key)
		}, codes.FailedPrecondition, "ghost"},
		{"CSR asking for a CA certificate", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			bc, _ := asn1.Marshal(struct{ IsCA bool }{true})
			req.CsrPem = pkitest.CSR(t, key, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: bc}}})
			return req
		}, codes.InvalidArgument, "bad certificate signing request"},
		{"newer protocol major", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			req.Protocol = &cucinav1.ProtocolVersion{Major: 2}
			return req
		}, codes.FailedPrecondition, "upgrade the controller"},
		{"no protocol version", func() *cucinav1.EnrollWorkerRequest {
			req := e.workerRequest(e.launch(nil), key)
			req.Protocol = nil
			return req
		}, codes.FailedPrecondition, "no protocol version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req()
			resp, err := e.srv.EnrollWorker(ctx, req)
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			if tc.code != codes.OK {
				require.Contains(t, status.Convert(err).Message(), tc.msg)
				require.Nil(t, resp)
				if tc.code == codes.Unavailable {
					require.NotEmpty(t, status.Convert(err).Details(), "retry hint for the agent")
				}
				return
			}
			chain := pkitest.ParseChain(t, resp.GetCertificatePem())
			var doc struct{ InstanceID string }
			require.NoError(t, jsonUnmarshal(req.GetInstanceIdentityDocument(), &doc))
			require.Equal(t, "spiffe://cucina/worker/"+testPool+"/"+doc.InstanceID, chain[0].URIs[0].String())
			require.Equal(t, e.clock.Now().Add(24*time.Hour), chain[0].NotAfter)
			require.Equal(t, testPool, resp.GetPool())
			require.Equal(t, "g7", resp.GetGeneration())
			require.Equal(t, doc.InstanceID, resp.GetSettings().GetNode())
			roots := x509.NewCertPool()
			require.True(t, roots.AppendCertsFromPEM(resp.GetCaPem()))
			_, err = chain[0].Verify(x509.VerifyOptions{Roots: roots, CurrentTime: e.clock.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			require.NoError(t, err)
		})
	}
}

// TestEnrollWorkerOnePerLaunch guards the replay rule (R-SEC-3): one
// certificate per instance launch, the same key may retry within the replay
// window (agent crash after the response), a different key never — so code
// running on the worker (which can read IMDS) cannot mint itself an identity.
func TestEnrollWorkerOnePerLaunch(t *testing.T) {
	e := newEnv(t, memoryStores(), nil)
	ctx := context.Background()
	agentKey, intruderKey := pkitest.Key(t), pkitest.Key(t)
	inst := e.launch(nil)

	_, err := e.srv.EnrollWorker(ctx, e.workerRequest(inst, agentKey))
	require.NoError(t, err)
	_, err = e.srv.EnrollWorker(ctx, e.workerRequest(inst, intruderKey))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "another key")
	e.clock.Advance(5 * time.Minute)
	_, err = e.srv.EnrollWorker(ctx, e.workerRequest(inst, agentKey))
	require.NoError(t, err, "same key within the replay window (crash recovery)")
	e.clock.Advance(6 * time.Minute)
	_, err = e.srv.EnrollWorker(ctx, e.workerRequest(inst, agentKey))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "after the window a reboot is replaced, not re-admitted")

	burst := e.launch(nil)
	for i := range 3 {
		_, err := e.srv.EnrollWorker(ctx, e.workerRequest(burst, agentKey))
		require.NoError(t, err, "issuance %d", i+1)
	}
	_, err = e.srv.EnrollWorker(ctx, e.workerRequest(burst, agentKey))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "too many")
}
