// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

const (
	testAccount = "000000000000"
	testRegion  = "us-west-1"
	testCluster = "cucina-test"
	testPool    = "linux-x86-64"
	testImage   = "ami-0123456789abcdef0"
)

// ------------------------------------------------------------ fake AWS signer

// awsSigner plays AWS: an RSA-2048 key + certificate per Region, signing
// identity documents as PKCS#7 SignedData like IMDS …/instance-identity/rsa2048.
type awsSigner struct {
	keys  map[string]*rsa.PrivateKey
	certs map[string]*x509.Certificate
}

var (
	signerOnce sync.Once
	signer     *awsSigner
)

func testAWS(t testing.TB) *awsSigner {
	t.Helper()
	signerOnce.Do(func() {
		signer = &awsSigner{keys: map[string]*rsa.PrivateKey{}, certs: map[string]*x509.Certificate{}}
		for i, region := range []string{testRegion, "us-east-1", "attacker"} {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), Subject: pkix.Name{Organization: []string{"Amazon Web Services LLC"}},
				NotBefore: time.Unix(0, 0), NotAfter: time.Date(2195, 1, 1, 0, 0, 0, 0, time.UTC)}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
			if err != nil {
				panic(err)
			}
			cert, _ := x509.ParseCertificate(der)
			signer.keys[region], signer.certs[region] = key, cert
		}
	})
	return signer
}

// verifier trusts the fake AWS certificates of us-west-1 and us-east-1 (not "attacker").
func (a *awsSigner) verifier(t testing.TB) *enroll.IdentityVerifier {
	t.Helper()
	certs := map[string][]byte{}
	for _, r := range []string{testRegion, "us-east-1"} {
		certs[r] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.certs[r].Raw})
	}
	v, err := enroll.NewIdentityVerifierWithCerts(certs)
	require.NoError(t, err)
	return v
}

type sigOpts struct {
	ber      bool // indefinite lengths + chunked OCTET STRINGs
	noAttrs  bool // sign the content directly
	detached bool // omit the content from the SignedData
}

// sign returns the base64 PKCS#7 body for doc signed with region's key.
func (a *awsSigner) sign(t testing.TB, region string, doc []byte, o sigOpts) string {
	t.Helper()
	key, cert := a.keys[region], a.certs[region]
	mustDER := func(v any) []byte {
		b, err := asn1.Marshal(v)
		require.NoError(t, err)
		return b
	}
	oid := func(o asn1.ObjectIdentifier) []byte { return mustDER(o) }
	seq := func(parts ...[]byte) []byte {
		return mustDER(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: bytes.Join(parts, nil)})
	}
	set := func(parts ...[]byte) []byte {
		return mustDER(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: bytes.Join(parts, nil)})
	}
	ctx := func(tag int, inner []byte) []byte {
		return mustDER(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, IsCompound: true, Bytes: inner})
	}
	algo := func(o asn1.ObjectIdentifier) []byte { return seq(oid(o), []byte{0x05, 0x00}) }
	var (
		oidData       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
		oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
		oidSHA256     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
		oidRSA        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
		oidCT         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
		oidMD         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
		oidTime       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 5}
	)
	digest := sha256.Sum256(doc)
	var signedAttrs []byte // [0] IMPLICIT
	toSign := digest[:]
	if !o.noAttrs {
		attrs := bytes.Join([][]byte{
			seq(oid(oidCT), set(oid(oidData))),
			seq(oid(oidTime), set(mustDER(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)))),
			seq(oid(oidMD), set(mustDER(digest[:]))),
		}, nil)
		asSet := set(attrs)
		sum := sha256.Sum256(asSet)
		toSign = sum[:]
		signedAttrs = append([]byte{0xA0}, asSet[1:]...)
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, toSign)
	require.NoError(t, err)
	signerInfo := seq(mustDER(1), seq(cert.RawIssuer, mustDER(cert.SerialNumber)), algo(oidSHA256), signedAttrs, algo(oidRSA), mustDER(sig))
	encap := seq(oid(oidData), ctx(0, mustDER(doc)))
	if o.detached {
		encap = seq(oid(oidData))
	}
	sd := seq(mustDER(1), set(algo(oidSHA256)), encap, ctx(0, cert.Raw), set(signerInfo))
	der := seq(oid(oidSignedData), ctx(0, sd))
	if o.ber {
		der = toBER(t, der)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// toBER rewrites DER with indefinite lengths on every constructed element and
// splits long OCTET STRINGs into constructed chunks (what streaming signers emit).
func toBER(t testing.TB, der []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var walk func(b []byte) []byte
	walk = func(b []byte) []byte {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		require.NoError(t, err)
		tag := rv.FullBytes[:len(rv.FullBytes)-len(rv.Bytes)]
		hdrTag := tag[0]
		switch {
		case rv.IsCompound:
			out.WriteByte(hdrTag)
			out.WriteByte(0x80)
			inner := rv.Bytes
			for len(inner) > 0 {
				inner = walk(inner)
			}
			out.Write([]byte{0, 0})
		case rv.Class == asn1.ClassUniversal && rv.Tag == asn1.TagOctetString && len(rv.Bytes) > 16:
			out.Write([]byte{0x24, 0x80})
			for p := rv.Bytes; len(p) > 0; {
				n := min(len(p), 16)
				out.Write([]byte{0x04, byte(n)})
				out.Write(p[:n])
				p = p[n:]
			}
			out.Write([]byte{0, 0})
		default:
			out.Write(rv.FullBytes)
		}
		return rest
	}
	walk(der)
	return out.Bytes()
}

// identityDocument renders an IMDS-style identity document.
func identityDocument(t testing.TB, inst ports.Instance, mutate func(map[string]any)) []byte {
	t.Helper()
	doc := map[string]any{
		"accountId": testAccount, "architecture": "x86_64", "availabilityZone": inst.AZ, "billingProducts": nil,
		"devpayProductCodes": nil, "marketplaceProductCodes": nil, "imageId": inst.ImageID, "instanceId": inst.ID,
		"instanceType": inst.Type, "kernelId": nil, "pendingTime": inst.LaunchTime.UTC().Format(time.RFC3339),
		"privateIp": inst.PrivateIP.String(), "ramdiskId": nil, "region": testRegion, "version": "2017-09-30",
	}
	if mutate != nil {
		mutate(doc)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	return b
}

// ------------------------------------------------------------ environment

type fakePools struct {
	gen     string
	unknown map[string]bool
}

func (p *fakePools) SettingsFor(pool, node string) (*cucinav1.WorkerSettings, string, error) {
	if p.unknown[pool] {
		return nil, "", fmt.Errorf("pool %q is unknown", pool)
	}
	return &cucinav1.WorkerSettings{Pool: pool, Node: node, SchedulerEndpoint: "scheduler.cucina.internal:8983", ServerName: "workers.cucina.internal"}, p.gen, nil
}

type revocation struct{ sub, reason, actor string }

type fakeRevoker struct {
	mu   sync.Mutex
	subs []revocation
}

func (r *fakeRevoker) RevokeSubject(_ context.Context, sub, reason, actor string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, revocation{sub, reason, actor})
	return nil
}

type env struct {
	t        *testing.T
	clock    *fakes.Clock
	compute  *fakes.Compute
	ca       *pki.CA
	issuer   *pki.Issuer
	aws      *awsSigner
	launches enroll.LedgerLaunches
	ledgerMu sync.Mutex
	ledgers  map[domain.PoolName]*scaling.Ledger
	pools    *fakePools
	revoker  *fakeRevoker
	srv      *enroll.Server
	hosts    enroll.HostStore
	tokens   enroll.TokenStore
	replay   enroll.ReplayStore
}

type stores struct {
	hosts  enroll.HostStore
	tokens enroll.TokenStore
	replay enroll.ReplayStore
}

func memoryStores() stores {
	return stores{enroll.NewMemoryHosts(), enroll.NewMemoryTokens(), enroll.NewMemoryReplay()}
}

func newEnv(t *testing.T, st stores, mutate func(*enroll.Deps)) *env {
	t.Helper()
	return newEnvAt(t, st, pkitest.Epoch, mutate)
}

// newEnvAt starts the fake clock at start (tests that hand certificates to real
// Buildbarn processes, which verify with the wall clock, start at time.Now()).
func newEnvAt(t *testing.T, st stores, start time.Time, mutate func(*enroll.Deps)) *env {
	t.Helper()
	clock := fakes.NewClock(start)
	cfg := fakes.DefaultComputeConfig()
	cfg.VisibleMin, cfg.VisibleMax = 0, 0
	cfg.Buckets = map[string]fakes.BucketSpec{} // no API throttling in these tests
	e := &env{t: t, clock: clock, compute: fakes.NewCompute(clock, fakes.NewRand(1), cfg), aws: testAWS(t),
		pools: &fakePools{gen: "g7", unknown: map[string]bool{}}, revoker: &fakeRevoker{},
		hosts: st.hosts, tokens: st.tokens, replay: st.replay}
	e.ca = pkitest.NewCA(t, clock.Now())
	var err error
	e.issuer, err = pki.NewIssuer(e.ca, clock, pki.Policy{})
	require.NoError(t, err)
	e.ledgers = map[domain.PoolName]*scaling.Ledger{}
	e.launches = enroll.LedgerLaunches{Cluster: testCluster, Ledger: e.ledger}
	d := enroll.Deps{
		Issuer: e.issuer, Clock: clock, Pools: e.pools,
		Identity: e.aws.verifier(t), Compute: e.compute, Launches: e.launches, Replay: st.replay,
		Hosts: st.hosts, Tokens: st.tokens, Revoker: e.revoker,
		Options: enroll.Options{ClusterID: testCluster, AWSAccountID: testAccount, AWSRegion: testRegion, HostEndpoint: "hosts.cucina.example:8446"},
	}
	if mutate != nil {
		mutate(&d)
	}
	e.srv, err = enroll.New(d)
	require.NoError(t, err)
	return e
}

// launch starts an instance of testPool with the controller's tags (tag edits via mutate).
func (e *env) launch(mutate func(map[string]string)) ports.Instance {
	e.t.Helper()
	return e.launchPool(testPool, mutate)
}

// ledger plays the WorkerPool's persisted launch ledger.
func (e *env) ledger(_ context.Context, pool domain.PoolName) (scaling.Ledger, error) {
	e.ledgerMu.Lock()
	defer e.ledgerMu.Unlock()
	if l := e.ledgers[pool]; l != nil {
		return *l, nil
	}
	return scaling.Ledger{}, nil
}

// nextToken allocates the next launch token of pool like the autoscaler does.
func (e *env) nextToken(pool domain.PoolName) string {
	e.ledgerMu.Lock()
	defer e.ledgerMu.Unlock()
	l := e.ledgers[pool]
	if l == nil {
		l = &scaling.Ledger{Epoch: "e1a2b3c4"}
		e.ledgers[pool] = l
	}
	l.Next++
	return scaling.MakeToken(scaling.TokenPrefix(testCluster, pool), l.Epoch, l.Next-1)
}

func (e *env) launchPool(pool domain.PoolName, mutate func(map[string]string)) ports.Instance {
	e.t.Helper()
	tok := e.nextToken(pool)
	tags := map[string]string{
		domain.TagManagedBy: domain.ManagedByValue, domain.TagCluster: testCluster, domain.TagPool: string(pool),
		domain.TagGeneration: "g7", domain.TagLaunchToken: tok, domain.TagRole: "worker",
	}
	if mutate != nil {
		mutate(tags)
	}
	inst, err := e.compute.Launch(context.Background(), ports.LaunchRequest{
		Pool: pool, Generation: "g7", Token: tok, ImageID: testImage, InstanceTypes: []string{"c8i.2xlarge"},
		SubnetIDs: []string{"subnet-a"}, Tags: tags,
	})
	require.NoError(e.t, err)
	return inst
}

func v1() *cucinav1.ProtocolVersion { return &cucinav1.ProtocolVersion{Major: 1} }

// workerRequest is a well-formed EnrollWorker request for inst.
func (e *env) workerRequest(inst ports.Instance, key crypto.Signer) *cucinav1.EnrollWorkerRequest {
	e.t.Helper()
	doc := identityDocument(e.t, inst, nil)
	return &cucinav1.EnrollWorkerRequest{
		Protocol: v1(), InstanceIdentityDocument: doc, Signature: e.aws.sign(e.t, testRegion, doc, sigOpts{}),
		CsrPem: pkitest.CSR(e.t, key, nil), AgentVersion: "test", Os: "linux", Arch: "x86_64",
	}
}

// createToken returns a fresh token string.
func (e *env) createToken(site string, maxHosts uint32, ttl time.Duration) (id, token string) {
	e.t.Helper()
	req := &cucinav1.CreateEnrollTokenRequest{Site: site, MaxHosts: maxHosts}
	if ttl > 0 {
		req.Ttl = durationpb.New(ttl)
	}
	resp, err := e.srv.Admin().CreateEnrollToken(context.Background(), req, "admin@test")
	require.NoError(e.t, err)
	return resp.GetId(), resp.GetToken()
}

func (e *env) hostRequest(token, serial string, key crypto.Signer) *cucinav1.EnrollHostRequest {
	return &cucinav1.EnrollHostRequest{Protocol: v1(), SiteToken: token, SerialNumber: serial, Hostname: "mini-" + serial,
		CsrPem: pkitest.CSR(e.t, key, nil), Facts: &cucinav1.HostFacts{Site: "office-1", AgentVersion: "test"}}
}

func (e *env) enrollHost(req *cucinav1.EnrollHostRequest) *cucinav1.EnrollHostResponse {
	e.t.Helper()
	resp, err := e.srv.EnrollHost(context.Background(), req)
	require.NoError(e.t, err)
	return resp
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
