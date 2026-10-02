// SPDX-License-Identifier: FSL-1.1-ALv2

package canary

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sloper-ai/cucina/internal/bbtest"
)

// Integration tier: the cache and execution canaries run against the pinned
// bb_storage and bb_scheduler release binaries (internal/bbtest) on
// loopback, the STS exchange against an in-process HTTP server.

const issuer = "https://sts.cucina.test"

type signer struct {
	key *ecdsa.PrivateKey
	kid string
}

func newSigner(t *testing.T, kid string) signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return signer{key: k, kid: kid}
}

func (s signer) jwks() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &s.key.PublicKey, KeyID: s.kid, Algorithm: "ES256", Use: "sig"}}}
}

func (s signer) sign(t *testing.T, claims map[string]any) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: s.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.kid))
	require.NoError(t, err)
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	require.NoError(t, err)
	return raw
}

func claims(now time.Time, mut func(map[string]any)) map[string]any {
	c := map[string]any{
		"iss": issuer, "aud": "buildbarn", "sub": "sa:canary", "iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"jti": "j1", "sid": "s1", "cucina": map[string][]string{"cas_read": {"main"}, "ac_write": {"main"}},
	}
	if mut != nil {
		mut(c)
	}
	return c
}

// Guards R-TEST-7 "token mint/verify": a minted token is accepted only with a
// published kid, a valid ES256 signature, the STS issuer, audience
// "buildbarn", an unexpired exp and a cucina grant claim.
func TestVerify(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	good := newSigner(t, "k1")
	rogue := newSigner(t, "k1") // same kid, different key
	other := newSigner(t, "k2")
	for _, tc := range []struct {
		name string
		raw  string
		err  string
	}{
		{name: "valid", raw: good.sign(t, claims(now, nil))},
		{name: "foreign key with a published kid", raw: rogue.sign(t, claims(now, nil)), err: "signature"},
		{name: "unpublished kid", raw: other.sign(t, claims(now, nil)), err: "not in the published JWKS"},
		{name: "wrong issuer", raw: good.sign(t, claims(now, func(c map[string]any) { c["iss"] = "https://evil" })), err: "claims"},
		{name: "wrong audience", raw: good.sign(t, claims(now, func(c map[string]any) { c["aud"] = "other" })), err: "claims"},
		{name: "expired", raw: good.sign(t, claims(now, func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() })), err: "claims"},
		{name: "no grants", raw: good.sign(t, claims(now, func(c map[string]any) { delete(c, "cucina") })), err: "no cucina grants"},
		{name: "tampered", raw: tamper(good.sign(t, claims(now, nil))), err: "verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := Verify(tc.raw, good.jwks(), issuer, now)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "sa:canary", tok.Subject)
			require.Equal(t, "k1", tok.KeyID)
		})
	}
}

func tamper(raw string) string {
	parts := strings.Split(raw, ".")
	payload := []byte(parts[1])
	payload[len(payload)/2] ^= 1
	parts[1] = string(payload)
	return strings.Join(parts, ".")
}

// Guards the STS leg of the cache canary: discovery → RFC 8693 exchange with
// the service-key subject token type → verification against jwks_uri.
func TestSTSExchange(t *testing.T) {
	s := newSigner(t, "k1")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/cucina-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(s.jwks()) })
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("subject_token") != "cuc_sk_1_secret" || r.Form.Get("subject_token_type") != ServiceKeyTokenType {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": s.sign(t, claims(time.Now(), nil)), "token_type": "Bearer", "expires_in": 900})
	})
	tok, err := (&STS{URL: srv.URL, Key: "cuc_sk_1_secret"}).Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "sa:canary", tok.Subject)
	_, err = (&STS{URL: srv.URL, Key: "cuc_sk_1_revoked"}).Token(context.Background())
	require.ErrorContains(t, err, "invalid_grant")
}

// Guards in-cluster canaries (`helm test`, the controller's loop; found by the
// kind smoke test): discovery advertises the public token_endpoint/jwks_uri,
// which Pods cannot always resolve or reach (no DNS on kind, NLB hairpin), so
// URLs on the issuer's origin are rebased onto the STS URL the canary was
// given; the token is still verified against the advertised issuer, and URLs
// on other origins are followed as advertised.
func TestSTSExchangeThroughInClusterURL(t *testing.T) {
	s := newSigner(t, "k1")
	var jwksHits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jwksHits++
		_ = json.NewEncoder(w).Encode(s.jwks())
	}))
	defer other.Close()
	jwksURI := issuer + "/jwks.json"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/cucina-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "token_endpoint": issuer + "/token", "jwks_uri": jwksURI})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(s.jwks()) })
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": s.sign(t, claims(time.Now(), nil)), "token_type": "Bearer", "expires_in": 900})
	})
	tok, err := (&STS{URL: srv.URL + "/", Key: "cuc_sk_1_secret"}).Token(context.Background())
	require.NoError(t, err, "the advertised public URLs must be rebased onto the in-cluster STS URL")
	require.Equal(t, "sa:canary", tok.Subject)
	require.Zero(t, jwksHits)

	jwksURI = other.URL + "/keys" // another origin: followed as advertised
	_, err = (&STS{URL: srv.URL, Key: "cuc_sk_1_secret"}).Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, jwksHits)
}

func bootStorage(t *testing.T, scheduler string) string {
	addr := bbtest.FreeAddr(t)
	bbtest.BootStorage(t, bbtest.StorageConfig(bbtest.StorageOptions{ClientListen: addr, SchedulerAddress: scheduler, Compression: true}),
		bbtest.GRPCReady(addr, nil))
	return addr
}

// Guards R-TEST-7's cache canary: the AC/CAS round trip succeeds against the
// pinned bb_storage, reports the advertised compressors, and its result
// feeds the cucina_canary_* metrics.
func TestCacheCanaryAgainstBuildbarn(t *testing.T) {
	addr := bootStorage(t, "")
	p := &Probe{Kind: KindCache, Endpoint: Endpoint{Target: "grpc://" + addr, InstanceName: "main"}, Timeout: 30 * time.Second}
	r := p.Run(context.Background())
	require.True(t, r.Success, r.Error)
	var names []string
	for _, s := range r.Steps {
		names = append(names, s.Name)
	}
	require.Equal(t, []string{"capabilities", "cas-write", "cas-find-missing", "cas-read", "ac-write", "ac-read"}, names)
	require.Contains(t, r.Compressors, "ZSTD")

	reg := prometheus.NewPedanticRegistry()
	m := NewMetrics(reg)
	m.Observe(r)
	require.InDelta(t, 1.0, testutil.ToFloat64(m.runs.WithLabelValues(KindCache, "", "success")), 1e-9)
	require.InDelta(t, 1.0, testutil.ToFloat64(m.up.WithLabelValues(KindCache, "")), 1e-9)
	problems, err := testutil.CollectAndLint(m.runs)
	require.NoError(t, err)
	require.Empty(t, problems)
}

// Guards the failure path: an unreachable endpoint yields a failed result
// naming the step, quickly, and the up gauge drops to 0.
func TestCanaryFailureIsReported(t *testing.T) {
	p := &Probe{Kind: KindCache, Endpoint: Endpoint{Target: "grpc://" + bbtest.FreeAddr(t), InstanceName: "main"}, Timeout: 5 * time.Second}
	r := p.Run(context.Background())
	require.False(t, r.Success)
	require.Contains(t, r.Error, "capabilities")
	m := NewMetrics(prometheus.NewRegistry())
	m.Observe(r)
	require.InDelta(t, 0.0, testutil.ToFloat64(m.up.WithLabelValues(KindCache, "")), 1e-9)
	require.InDelta(t, 1.0, testutil.ToFloat64(m.runs.WithLabelValues(KindCache, "", "failure")), 1e-9)
}

// Guards R-TEST-7's execution canary: a tiny uncached action is routed to the
// pool's exact runner properties through the real bb_scheduler, executed by a
// (fake) worker thread of that queue, and verified by its stdout; the queue
// time and worker id are recorded.
func TestExecCanaryAgainstBuildbarn(t *testing.T) {
	props := map[string]string{"OSFamily": "linux", "ISA": "x86-64"}
	schedClient, schedWorker, schedBQS := bbtest.FreeAddr(t), bbtest.FreeAddr(t), bbtest.FreeAddr(t)
	storage := bootStorage(t, schedClient)
	bbtest.BootScheduler(t, bbtest.SchedulerConfig(bbtest.SchedulerOptions{
		ClientListen: schedClient, WorkerListen: schedWorker, BuildQueueStateListen: schedBQS, StorageAddress: storage,
		Queues: []bbtest.Queue{{InstanceNamePrefix: "main", Properties: props, SizeClasses: []uint32{0}}},
	}), bbtest.GRPCReady(schedClient, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wconn, err := bbtest.Dial(schedWorker, nil)
	require.NoError(t, err)
	defer func() { _ = wconn.Close() }()
	sconn, err := bbtest.Dial(storage, nil)
	require.NoError(t, err)
	defer func() { _ = sconn.Close() }()
	worker := bbtest.NewFakeWorker(wconn, map[string]string{"pool": "linux-x86-64", "node": "i-canary", "thread": "0"}, "main", props, 0)
	require.NoError(t, worker.Register(ctx))
	go func() {
		task, err := worker.Take(ctx)
		if err != nil {
			return
		}
		re := bbtest.NewREClient(sconn, "main")
		var cmd repb.Command
		if b, err := re.Read(ctx, task.GetAction().GetCommandDigest()); err == nil {
			_ = proto.Unmarshal(b, &cmd)
		}
		echo := strings.TrimPrefix(cmd.GetArguments()[len(cmd.GetArguments())-1], "echo ")
		now := time.Now()
		_ = worker.Complete(ctx, task, &repb.ExecuteResponse{Result: &repb.ActionResult{
			StdoutRaw: []byte(echo + "\n"),
			ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: `{"node":"i-canary","pool":"linux-x86-64","thread":"0"}`,
				QueuedTimestamp: timestamppb.New(now.Add(-1500 * time.Millisecond)), WorkerStartTimestamp: timestamppb.New(now)},
		}})
	}()
	p := &Probe{Kind: KindExec, Pool: "linux-x86-64", Platform: props, Endpoint: Endpoint{Target: "grpc://" + storage, InstanceName: "main"}, Timeout: 25 * time.Second}
	r := p.Run(ctx)
	require.True(t, r.Success, r.Error)
	require.Equal(t, 1500*time.Millisecond, r.QueueTime)
	require.Contains(t, r.Worker, "i-canary")
}

// Guards the one-shot path (CronJob, helm test): a pushed result is observed
// by the controller's handler; garbage is rejected.
func TestPushToHandler(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	srv := httptest.NewServer(Handler(m))
	defer srv.Close()
	r := Result{Kind: KindExec, Pool: "windows-x86-64", Success: true, Started: time.Unix(100, 0), Duration: time.Minute, QueueTime: 95 * time.Second}
	require.NoError(t, Push(context.Background(), srv.URL, r, nil))
	require.InDelta(t, 95.0, testutil.ToFloat64(m.queue.WithLabelValues("windows-x86-64")), 1e-9)
	resp, err := http.Post(srv.URL, "application/json", bytes.NewBufferString(`{"kind":"bogus"}`))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// Guards the in-process cadence: the loop probes immediately, then once per
// tick, observing each result (a fake clock drives the ticks).
func TestLoop(t *testing.T) {
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	l := &Loop{
		Every: 5 * time.Minute, Metrics: NewMetrics(prometheus.NewRegistry()),
		After: func(time.Duration) <-chan time.Time { return ticks },
		Probe: func(context.Context) Result {
			runs++
			if runs == 3 {
				cancel()
			}
			return Result{Kind: KindCache, Success: true}
		},
	}
	done := make(chan error)
	go func() { done <- l.Run(ctx) }()
	ticks <- time.Time{}
	ticks <- time.Time{}
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, 3, runs)
	require.InDelta(t, 2.0, testutil.ToFloat64(l.Metrics.runs.WithLabelValues(KindCache, "", "success")), 1e-9)
}
