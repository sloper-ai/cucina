// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

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
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// The T10 identity scenarios run against navikt/mock-oauth2-server 6.0.4
// (deploy/aws-e2e/scripts/campaign/mock-oauth2-server.sh) with Google-shaped
// (/google, /google-short) and GitHub-shaped (/github) issuers. The issuer URL
// is the mock's in-cluster HTTPS address (the STS requires HTTPS and an
// issuer that equals its discovery document's); the dev Mac reaches it
// through a kubectl port-forward (idp.localAddr) by dialing the forward for
// that host name (ADR 1003).

func endpoint(env *harness.Env) canary.Endpoint {
	return canary.Endpoint{Target: env.Endpoints.RemoteExecution, InstanceName: instanceName(env), CAFile: env.Endpoints.CAFile}
}

func instanceName(env *harness.Env) string {
	if env.Endpoints.InstanceName != "" {
		return env.Endpoints.InstanceName
	}
	return "main"
}

func stsHTTP(env *harness.Env) (*http.Client, error) {
	cfg, err := endpoint(env).TLSConfig()
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

// mockClient returns an HTTP client that reaches the mock issuers' in-cluster
// host through the local port-forward, verifying its certificate against the
// throwaway mock CA.
func mockClient(env *harness.Env) (*http.Client, error) {
	u, err := url.Parse(env.IdP.MockOAuth2URL)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
	if env.IdP.CAFile != "" {
		pem, err := os.ReadFile(env.IdP.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		cfg.RootCAs = pool
	}
	target := u.Host
	if env.IdP.LocalAddr != "" {
		target = env.IdP.LocalAddr
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{TLSClientConfig: cfg, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == u.Host {
			addr = target
		}
		return d.DialContext(ctx, network, addr)
	}}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}

// mintMock obtains a token from the mock issuer with the claims mapped to
// `scenario` (client_credentials; the mock's requestMappings match on scope).
func mintMock(ctx context.Context, env *harness.Env, issuer, scenario string) (string, error) {
	hc, err := mockClient(env)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID(env)}, "client_secret": {"e2e"}, "scope": {"openid " + scenario}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(env.IdP.MockOAuth2URL, "/")+"/"+issuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var tr struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	if tr.IDToken != "" {
		return tr.IDToken, nil
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("mock issuer %s returned no token (%s)", issuer, resp.Status)
	}
	return tr.AccessToken, nil
}

// connectProxy is a minimal HTTP CONNECT proxy for cucinactl's OIDC client
// (HTTPS_PROXY): CONNECTs to the mock's in-cluster host go to the local
// port-forward; anything else is refused. It returns the proxy URL and a
// stop function.
func connectProxy(env *harness.Env) (string, func(), error) {
	u, err := url.Parse(env.IdP.MockOAuth2URL)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != u.Host {
			http.Error(w, "e2e proxy: only CONNECT to the mock issuer", http.StatusForbidden)
			return
		}
		up, err := net.DialTimeout("tcp", env.IdP.LocalAddr, 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = up.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		conn, _, err := hj.Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		go func() { _, _ = io.Copy(up, conn); _ = up.Close() }()
		go func() { _, _ = io.Copy(conn, up); _ = conn.Close() }()
	})}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

func clientID(env *harness.Env) string {
	if env.IdP != nil && env.IdP.ClientID != "" {
		return env.IdP.ClientID
	}
	return "cucina-e2e"
}

// exchange performs the RFC 8693 exchange at the STS and returns the HTTP
// status and RFC 6749 error code.
func exchange(ctx context.Context, env *harness.Env, subject, tokenType string) (int, string, error) {
	hc, err := stsHTTP(env)
	if err != nil {
		return 0, "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "subject_token": {subject}, "subject_token_type": {tokenType}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(env.Endpoints.STS, "/")+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
	return resp.StatusCode, e.Error, nil
}

const (
	idTokenType = "urn:ietf:params:oauth:token-type:id_token"
	jwtType     = "urn:ietf:params:oauth:token-type:jwt"
)

func secScenario(id, title string, timeout time.Duration, req []harness.Requirement, run func(*harness.Context) error) *harness.Scenario {
	return &harness.Scenario{
		ID: id, Title: "Security: " + title, Requires: append([]harness.Requirement{harness.RequiresKubernetes}, req...),
		Envs: []harness.EnvKind{harness.EnvAWS}, Cost: harness.CostNone, Essential: true, Timeout: timeout,
		Post: Guards, Run: run,
	}
}

func t10() []*harness.Scenario {
	idp := []harness.Requirement{harness.RequiresIdP}
	return []*harness.Scenario{
		secScenario("T10a", "cucinactl login, auth code + PKCE over loopback (non-interactive)", 10*time.Minute, append(idp, harness.RequiresCucinactl), runT10a),
		secScenario("T10b", "wrong hd, unverified email, wrong aud, foreign repository, pull_request_target, expired token rejected", 15*time.Minute, idp, runT10b),
		secScenario("T10c", "a build longer than one token TTL renews transparently", 2*time.Hour, []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCucinactl}, runT10c),
		secScenario("T10d", "deny-list revocation takes effect within 3 min", 15*time.Minute, []harness.Requirement{harness.RequiresCucinactl}, runT10d),
		secScenario("T10e", "signing-key rotation during a build", 2*time.Hour, []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCucinactl}, runT10e),
		secScenario("T10f", "read-only principal cannot write the action cache", 10*time.Minute, nil, runT10f),
		secScenario("T10g", "unauthenticated requests are rejected", 5*time.Minute, nil, runT10g),
		secScenario("T10h", "worker or host without a valid certificate is rejected", 10*time.Minute, nil, runT10h),
		secScenario("T10i", "external port scan shows only TLS endpoints", 30*time.Minute, nil, runT10i),
	}
}

// T10a: `cucinactl login <sts> --no-browser` prints the authorization URL; a
// non-interactive mock issuer redirects straight to the CLI's loopback
// listener, which completes PKCE and the STS exchange.
func runT10a(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	bin, err := svc.CucinactlPath()
	if err != nil {
		return err
	}
	if c.Env.IdP.LocalAddr == "" {
		return harness.Skip("no idp.localAddr (kubectl port-forward to the mock issuer)")
	}
	proxy, stopProxy, err := connectProxy(c.Env)
	if err != nil {
		return err
	}
	defer stopProxy()
	hc, err := mockClient(c.Env)
	if err != nil {
		return err
	}
	cfgDir := c.Dir() + "/cucinactl-config"
	cmd := exec.CommandContext(c, bin, "login", c.Env.Endpoints.STS, "--no-browser")
	// The CLI reaches the in-cluster issuer through the CONNECT proxy and must
	// trust the throwaway mock CA (SSL_CERT_FILE / CUCINA_EXTRA_CA_FILE).
	cmd.Env = append(os.Environ(), "CUCINA_CONFIG_DIR="+cfgDir, "NO_COLOR=1", "HTTPS_PROXY="+proxy, "https_proxy="+proxy,
		"NO_PROXY=127.0.0.1,localhost,"+hostOf(c.Env.Endpoints.STS), "SSL_CERT_FILE="+c.Env.IdP.CAFile, "CUCINA_EXTRA_CA_FILE="+c.Env.IdP.CAFile)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	urlRe := regexp.MustCompile(`https?://\S+`)
	found := make(chan string, 1)
	var transcript strings.Builder
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			transcript.WriteString(line + "\n")
			if u := urlRe.FindString(line); u != "" && strings.Contains(u, "authorize") {
				select {
				case found <- u:
				default:
				}
			}
		}
	}()
	select {
	case u := <-found:
		if err := c.Step("follow the authorization URL (mock issuer redirects to the loopback)", func() error {
			req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
			if err != nil {
				return err
			}
			// The mock redirects to the CLI's 127.0.0.1 listener; following
			// it delivers the code (that hop is plain loopback HTTP).
			resp, err := hc.Do(req)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			return nil
		}); err != nil {
			return harness.Fail("%v", err)
		}
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		return harness.Fail("cucinactl login printed no authorization URL")
	}
	if err := cmd.Wait(); err != nil {
		return harness.Fail("cucinactl login: %v", err)
	}
	whoCmd := exec.CommandContext(c, bin, "whoami", "--output", "json")
	whoCmd.Env = cmd.Env
	who, err := whoCmd.Output()
	if err != nil {
		return harness.Fail("cucinactl whoami: %v", err)
	}
	c.Record("whoami", json.RawMessage(who))
	c.Check(harness.CheckResult{Name: "logged in through the mock Google issuer", Kind: "cli", Pass: strings.Contains(string(who), "google:"), Value: string(who)})
	return nil
}

// T10b: each subject token must be rejected (RFC 6749 error, no Cucina
// token); the valid controls must be accepted.
func runT10b(c *harness.Context) error {
	cases := []struct {
		name, issuer, scenario, tokenType string
		accept                            bool
		waitExpiry                        time.Duration
	}{
		{"google valid (control)", "google", "valid", idTokenType, true, 0},
		{"wrong hd", "google", "wrong-hd", idTokenType, false, 0},
		{"unverified email", "google", "unverified-email", idTokenType, false, 0},
		{"wrong aud", "google", "wrong-aud", idTokenType, false, 0},
		{"github valid push (control)", "github", "push-main", jwtType, true, 0},
		{"foreign repository id", "github", "foreign-repo", jwtType, false, 0},
		{"pull_request_target", "github", "pull-request-target", jwtType, false, 0},
		{"expired token", "google-short", "valid", idTokenType, false, 90 * time.Second}, // 30 s tokens, wait past expiry + skew
	}
	var bad []string
	for _, tc := range cases {
		tok, err := mintMock(c, c.Env, tc.issuer, tc.scenario)
		if err != nil {
			return err
		}
		if tc.waitExpiry > 0 {
			if err := remote.RealSleep(c, tc.waitExpiry); err != nil {
				return err
			}
		}
		code, oauthErr, err := exchange(c, c.Env, tok, tc.tokenType)
		if err != nil {
			return err
		}
		accepted := code == http.StatusOK
		pass := accepted == tc.accept
		c.Check(harness.CheckResult{Name: tc.name, Kind: "sts", Pass: pass, Value: fmt.Sprintf("HTTP %d %s", code, oauthErr)})
		if !pass {
			bad = append(bad, fmt.Sprintf("%s: HTTP %d %s", tc.name, code, oauthErr))
		}
	}
	if len(bad) > 0 {
		return harness.Fail("%s", strings.Join(bad, "; "))
	}
	return nil
}

// T10c: one Bazel server keeps building past the 15-minute token TTL; the
// credential helper renews without failures.
func runT10c(c *harness.Context) error {
	lr, err := openLane(c, LinuxLane)
	if err != nil {
		return err
	}
	start := c.Now()
	for i := 0; c.Now().Sub(start) < 17*time.Minute; i++ {
		o, err := lr.bazel(fmt.Sprintf("long-%d", i), BuildOpts{Command: "test", Extra: []string{"--noremote_accept_cached"}})
		if err != nil {
			return err
		}
		if err := mustSucceed(o, "build spanning the token TTL"); err != nil {
			return err
		}
	}
	c.Metric("span_seconds", c.Now().Sub(start).Seconds(), "s")
	return nil
}

// T10d: a minted token keeps working until its principal is revoked, then
// fails within 3 minutes (deny-list propagation to the frontends).
func runT10d(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	var created map[string]any
	if err := svc.CucinactlJSON(c, &created, "keys", "create", "e2e-t10d-"+c.Env.RunID); err != nil {
		return err
	}
	key, id := str(field(created, "key")), str(field(created, "id"))
	if key == "" || id == "" {
		return harness.Fail("keys create returned no key/id")
	}
	hc, err := stsHTTP(c.Env)
	if err != nil {
		return err
	}
	tok, err := (&canary.STS{URL: c.Env.Endpoints.STS, Key: key, HTTP: hc}).Token(c)
	if err != nil {
		return harness.Fail("mint with the new key: %v", err)
	}
	probe := func() canary.Result {
		p := &canary.Probe{Kind: canary.KindCache, Endpoint: endpoint(c.Env), Tokens: canary.StaticToken(tok.Raw), Timeout: time.Minute}
		return p.Run(c)
	}
	if r := probe(); !r.Success {
		return harness.Fail("the token did not work before revocation: %s", r.Error)
	}
	if _, err := svc.Cucinactl(c, "keys", "revoke", id, "--yes"); err != nil {
		return err
	}
	revoked := c.Now()
	for {
		r := probe()
		if !r.Success {
			took := c.Now().Sub(revoked)
			c.Metric("revocation_seconds", took.Seconds(), "s")
			c.Check(harness.CheckResult{Name: "revocation ≤ 3 min", Kind: "auth", Pass: took <= 3*time.Minute, Value: took.String(), Detail: r.Error})
			if took > 3*time.Minute {
				return harness.Fail("revocation took %s", took)
			}
			return nil
		}
		if c.Now().Sub(revoked) > 5*time.Minute {
			return harness.Fail("the revoked principal's token still works after 5 min")
		}
		if err := remote.RealSleep(c, 10*time.Second); err != nil {
			return err
		}
	}
}

// T10e: a signing-key rotation while a build runs causes no failure.
func runT10e(c *harness.Context) error {
	argv := c.Env.Kubernetes.RotateSigningKey
	if len(argv) == 0 {
		return harness.Skip("no kubernetes.rotateSigningKey command in the environment descriptor")
	}
	lr, err := openLane(c, LinuxLane)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		o, err := lr.bazel("build-during-rotation", BuildOpts{Command: "test", Extra: []string{"--noremote_accept_cached"}})
		if err == nil {
			err = mustSucceed(o, "build during signing-key rotation")
		}
		done <- err
	}()
	if err := remote.RealSleep(c, 2*time.Minute); err != nil {
		return err
	}
	if err := c.Step("rotate the signing key", func() error {
		cmd := exec.CommandContext(c, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Env.Kubernetes.Kubeconfig)
		out, err := cmd.CombinedOutput()
		c.Record("rotation", tail(string(out), 4000))
		return err
	}); err != nil {
		return err
	}
	return <-done
}

func readOnlyToken(c *harness.Context) (string, error) {
	kf := c.Env.Secrets.ReadOnlyKeyFile
	if kf == "" {
		return "", harness.Skip("no secrets.readOnlyKeyFile in the environment descriptor")
	}
	hc, err := stsHTTP(c.Env)
	if err != nil {
		return "", err
	}
	t, err := (&canary.STS{URL: c.Env.Endpoints.STS, KeyFile: kf, HTTP: hc}).Token(c)
	return t.Raw, err
}

// T10f: AC reads work, UpdateActionResult is denied.
func runT10f(c *harness.Context) error {
	tok, err := readOnlyToken(c)
	if err != nil {
		return err
	}
	conn, err := endpoint(c.Env).Dial(tok)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ac := repb.NewActionCacheClient(conn)
	d := &repb.Digest{Hash: strings.Repeat("ab", 32), SizeBytes: 1}
	_, getErr := ac.GetActionResult(c, &repb.GetActionResultRequest{InstanceName: instanceName(c.Env), ActionDigest: d})
	_, putErr := ac.UpdateActionResult(c, &repb.UpdateActionResultRequest{InstanceName: instanceName(c.Env), ActionDigest: d, ActionResult: &repb.ActionResult{}})
	readOK := getErr == nil || status.Code(getErr) == codes.NotFound
	denied := status.Code(putErr) == codes.PermissionDenied
	c.Check(harness.CheckResult{Name: "AC read allowed", Kind: "auth", Pass: readOK, Value: status.Code(getErr).String()})
	c.Check(harness.CheckResult{Name: "AC write denied", Kind: "auth", Pass: denied, Value: status.Code(putErr).String()})
	if !readOK || !denied {
		return harness.Fail("read-only principal: read %v, write %v", status.Code(getErr), status.Code(putErr))
	}
	return nil
}

// T10g: no token, no access (REAPI through the client endpoint).
func runT10g(c *harness.Context) error {
	conn, err := endpoint(c.Env).Dial("")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	inst := instanceName(c.Env)
	calls := map[string]error{}
	_, calls["GetCapabilities"] = repb.NewCapabilitiesClient(conn).GetCapabilities(c, &repb.GetCapabilitiesRequest{InstanceName: inst})
	_, calls["FindMissingBlobs"] = repb.NewContentAddressableStorageClient(conn).FindMissingBlobs(c, &repb.FindMissingBlobsRequest{InstanceName: inst,
		BlobDigests: []*repb.Digest{{Hash: strings.Repeat("cd", 32), SizeBytes: 1}}})
	_, calls["GetActionResult"] = repb.NewActionCacheClient(conn).GetActionResult(c, &repb.GetActionResultRequest{InstanceName: inst,
		ActionDigest: &repb.Digest{Hash: strings.Repeat("ef", 32), SizeBytes: 1}})
	var bad []string
	names := make([]string, 0, len(calls))
	for n := range calls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		code := status.Code(calls[n])
		ok := code == codes.Unauthenticated || code == codes.PermissionDenied
		c.Check(harness.CheckResult{Name: n + " without a token", Kind: "auth", Pass: ok, Value: code.String()})
		if !ok {
			bad = append(bad, n+"="+code.String())
		}
	}
	if len(bad) > 0 {
		return harness.Fail("unauthenticated calls not rejected: %s", strings.Join(bad, ", "))
	}
	return nil
}

func selfSignedClient() (tls.Certificate, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e-impostor"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{{Scheme: "spiffe", Host: "cucina", Path: "/host/e2e-impostor"}}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, nil
}

// mtlsRejected dials an mTLS endpoint with the given client certificates and
// reports whether the server refused (handshake failure or an alert on the
// first read).
func mtlsRejected(addr string, certs []tls.Certificate) (bool, string) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr,
		&tls.Config{InsecureSkipVerify: true, Certificates: certs, NextProtos: []string{"h2"}}) //nolint:gosec // probing the server's client-auth only
	if err != nil {
		return true, err.Error()
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, rerr := conn.Read(make([]byte, 1))
	if rerr != nil && !strings.Contains(rerr.Error(), "timeout") {
		return true, rerr.Error()
	}
	return false, "handshake accepted"
}

// T10h: the host endpoint (public) and the worker listener (private, probed
// from the Linux client) refuse clients without a Cucina-issued certificate.
func runT10h(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	var bad []string
	if addr := c.Env.Endpoints.Host; addr != "" {
		impostor, err := selfSignedClient()
		if err != nil {
			return err
		}
		for name, certs := range map[string][]tls.Certificate{"no client certificate": nil, "self-signed client certificate": {impostor}} {
			ok, why := mtlsRejected(addr, certs)
			c.Check(harness.CheckResult{Name: "host endpoint, " + name, Kind: "tls", Pass: ok, Value: why})
			if !ok {
				bad = append(bad, "host endpoint accepted "+name)
			}
		}
	} else {
		c.Note("no endpoints.host: host endpoint not probed")
	}
	if addr := c.Env.Endpoints.WorkerListener; addr != "" && c.Env.Has(harness.RequiresLinuxClient) {
		h, err := svc.Host(c, "linux-client")
		if err != nil {
			return err
		}
		res, err := h.Run(c, fmt.Sprintf("timeout 20 openssl s_client -connect %s -alpn h2 </dev/null 2>&1 | tail -n 20", quoteFor(h, addr)), remote.Opts{})
		if err != nil {
			return err
		}
		out := string(res.Stdout)
		ok := strings.Contains(out, "alert") || strings.Contains(out, "certificate required") || strings.Contains(out, "handshake failure")
		c.Check(harness.CheckResult{Name: "worker listener without a client certificate", Kind: "tls", Pass: ok, Value: tail(out, 400)})
		if !ok {
			bad = append(bad, "worker listener accepted a client without a certificate")
		}
	} else {
		c.Note("no endpoints.workerListener or linux-client: worker listener not probed")
	}
	if len(bad) > 0 {
		return harness.Fail("%s", strings.Join(bad, "; "))
	}
	return nil
}

// T10i: scan every TCP port of the public endpoint from the allowed source
// (the dev Mac) and require a TLS handshake on each open one.
func runT10i(c *harness.Context) error {
	host := c.Env.Endpoints.PublicHost
	if host == "" {
		return harness.Skip("no endpoints.publicHost")
	}
	var open []int
	var how string
	if nm, err := exec.LookPath("nmap"); err == nil {
		how = "nmap -Pn -sT -p-"
		out, err := exec.CommandContext(c, nm, "-Pn", "-sT", "-p-", "--min-rate", "2000", "-oX", "-", host).Output()
		if err != nil {
			return fmt.Errorf("nmap: %w", err)
		}
		open = parseNmapXML(out)
	} else {
		how = "Go TCP connect scan of 1-65535"
		open = connectScan(c, host, 1, 65535, 400)
	}
	c.Record("openPorts", open)
	var bad []string
	for _, p := range open {
		addr := net.JoinHostPort(host, fmt.Sprint(p))
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // only checking that the port speaks TLS
		ok := err == nil
		if conn != nil {
			_ = conn.Close()
		}
		if err != nil && strings.Contains(err.Error(), "certificate required") {
			ok = true // mTLS endpoint: TLS, client certificate required
		}
		c.Check(harness.CheckResult{Name: fmt.Sprintf("port %d speaks TLS", p), Kind: "scan", Pass: ok, Value: fmt.Sprint(err)})
		if !ok {
			bad = append(bad, fmt.Sprint(p))
		}
	}
	c.Note("scan: %s found %d open ports: %v", how, len(open), open)
	if len(bad) > 0 {
		return harness.Fail("non-TLS open ports: %s", strings.Join(bad, ", "))
	}
	return nil
}

func parseNmapXML(b []byte) []int {
	var run struct {
		Hosts []struct {
			Ports []struct {
				PortID int `xml:"portid,attr"`
				State  struct {
					State string `xml:"state,attr"`
				} `xml:"state"`
			} `xml:"ports>port"`
		} `xml:"host"`
	}
	_ = xml.Unmarshal(b, &run)
	var open []int
	for _, h := range run.Hosts {
		for _, p := range h.Ports {
			if p.State.State == "open" {
				open = append(open, p.PortID)
			}
		}
	}
	sort.Ints(open)
	return open
}

func connectScan(ctx context.Context, host string, from, to, workers int) []int {
	ports := make(chan int)
	var mu sync.Mutex
	var open []int
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := net.Dialer{Timeout: 2 * time.Second}
			for p := range ports {
				conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(p)))
				if err == nil {
					_ = conn.Close()
					mu.Lock()
					open = append(open, p)
					mu.Unlock()
				}
			}
		}()
	}
	for p := from; p <= to && ctx.Err() == nil; p++ {
		ports <- p
	}
	close(ports)
	wg.Wait()
	sort.Ints(open)
	return open
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
