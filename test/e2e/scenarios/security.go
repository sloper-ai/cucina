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
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
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
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mock issuer %s: HTTP %d", issuer, resp.StatusCode)
	}
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
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil {
		return resp.StatusCode, "", fmt.Errorf("STS returned invalid JSON (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK && (e.AccessToken == "" || e.TokenType != "Bearer" || e.ExpiresIn <= 0 || e.Error != "") {
		return resp.StatusCode, e.Error, fmt.Errorf("STS returned no usable access token")
	}
	if resp.StatusCode != http.StatusOK && e.AccessToken != "" {
		return resp.StatusCode, e.Error, fmt.Errorf("STS included an access token in an error response")
	}
	return resp.StatusCode, e.Error, nil
}

const (
	idTokenType = "urn:ietf:params:oauth:token-type:id_token"
	jwtType     = "urn:ietf:params:oauth:token-type:jwt"
)

func secScenario(id, title string, timeout time.Duration, req []harness.Requirement, run func(*harness.Context) error) *harness.Scenario {
	s := &harness.Scenario{
		ID: id, Title: "Security: " + title, Requires: append([]harness.Requirement{harness.RequiresKubernetes}, req...),
		Envs: []harness.EnvKind{harness.EnvAWS}, Cost: harness.CostNone, Essential: true, Timeout: timeout,
		Post: Guards, Run: run,
	}
	if id == "T10c" || id == "T10e" {
		// These force real remote work, not free authentication-only probes.
		s.Cost, s.EstimateUSD, s.MaxInstances = harness.CostHigh, 20, 4
		s.Run = func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			if svc.EC2 == nil {
				return harness.Skip("security workload requires worker inventory/spend accounting")
			}
			_, err = sampled(c, 15*time.Second, func() error {
				if err := run(c); err != nil {
					return err
				}
				_, residue, err := waitZero(c, 30*time.Minute)
				if err != nil {
					return err
				}
				if !residue.Zero() {
					return harness.Fail("security workload did not scale back to zero: %s", residue.String())
				}
				return nil
			})
			return err
		}
	}
	return s
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
	descriptor, err := harness.DescriptorPath(c.Env.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(descriptor), 0o700); err != nil {
		return err
	}
	cfgDir, err := os.MkdirTemp(filepath.Dir(descriptor), "t10a-login-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(cfgDir) }()
	args := []string{"login", c.Env.Endpoints.STS, "--no-browser", "--provider", "google", "--credential-store", "file"}
	if c.Env.Endpoints.CAFile != "" {
		args = append(args, "--ca-file", c.Env.Endpoints.CAFile)
	}
	cmd := exec.CommandContext(c, bin, args...)
	// The cluster CA is explicit; SSL_CERT_FILE adds the independent mock-IdP
	// CA. Credentials use a private, disposable file store, not the user's keychain.
	cmd.Env = append(os.Environ(), "CUCINA_CONFIG_DIR="+cfgDir, "NO_COLOR=1", "HTTPS_PROXY="+proxy, "https_proxy="+proxy,
		"NO_PROXY=127.0.0.1,localhost,"+hostOf(c.Env.Endpoints.STS), "SSL_CERT_FILE="+c.Env.IdP.CAFile)
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
		status                            int
		oauthError                        string
		waitExpiry                        time.Duration
	}{
		{"wrong hd", "google", "wrong-hd", idTokenType, http.StatusForbidden, "access_denied", 0},
		{"unverified email", "google", "unverified-email", idTokenType, http.StatusForbidden, "access_denied", 0},
		{"wrong aud", "google", "wrong-aud", idTokenType, http.StatusBadRequest, "invalid_grant", 0},
		{"foreign repository id", "github", "foreign-repo", jwtType, http.StatusForbidden, "access_denied", 0},
		{"pull_request_target", "github", "pull-request-target", jwtType, http.StatusForbidden, "access_denied", 0},
		{"expired token", "google-short", "valid", idTokenType, http.StatusBadRequest, "invalid_grant", 90 * time.Second},
	}
	control := func(issuer, typ string) error {
		scenario := "valid"
		if issuer == "github" {
			scenario = "push-main"
		} else {
			issuer = "google"
		}
		tok, err := mintMock(c, c.Env, issuer, scenario)
		if err != nil {
			return err
		}
		code, oauthErr, err := exchange(c, c.Env, tok, typ)
		if err != nil {
			return err
		}
		if code != http.StatusOK || oauthErr != "" {
			return harness.Fail("%s valid control: HTTP %d %s", issuer, code, oauthErr)
		}
		return nil
	}
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
		pass := code == tc.status && oauthErr == tc.oauthError
		c.Check(harness.CheckResult{Name: tc.name, Kind: "sts", Pass: pass, Value: fmt.Sprintf("HTTP %d %s", code, oauthErr)})
		if !pass {
			return harness.Fail("%s: HTTP %d %s, want HTTP %d %s", tc.name, code, oauthErr, tc.status, tc.oauthError)
		}
		if err := control(tc.issuer, tc.tokenType); err != nil {
			return err
		}
	}
	return nil
}

// CredentialUse is non-secret evidence emitted when Bazel calls its helper.
type CredentialUse struct {
	At          float64 `json:"at"`
	Issued      int64   `json:"issued"`
	Expires     int64   `json:"expires"`
	Kid         string  `json:"kid"`
	Fingerprint string  `json:"fingerprint"`
}

// TokenRenewalCheck is the T10c oracle over one invocation's BEP, compact
// execution log and non-secret helper trace; it is usable by the harness/report.
type TokenRenewalCheck struct {
	Started, Finished time.Time
	Uses              []CredentialUse
	RemoteStarts      []time.Time
}

func (TokenRenewalCheck) CheckName() string { return "one active build renews beyond one token TTL" }

func (e TokenRenewalCheck) Evaluate(_ context.Context, _ *harness.Context) harness.CheckResult {
	r := harness.CheckResult{Name: e.CheckName(), Kind: "auth"}
	if len(e.Uses) == 0 || e.Started.IsZero() || !e.Finished.After(e.Started) {
		r.Detail = "missing invocation or helper timestamps"
		return r
	}
	first := e.Uses[0]
	for _, u := range e.Uses {
		if u.At < first.At {
			first = u
		}
	}
	expiry := time.Unix(first.Expires, 0)
	ttl := time.Duration(first.Expires-first.Issued) * time.Second
	if ttl <= 0 || !expiry.After(e.Started) || !e.Finished.After(expiry) || e.Finished.Sub(e.Started) <= ttl {
		r.Detail = "this invocation did not span a whole token TTL"
		return r
	}
	renewed, beforeExpiry, afterExpiry := false, false, false
	for _, u := range e.Uses {
		at := time.Unix(0, int64(u.At*1e9))
		if at.Before(e.Started) || at.After(e.Finished) || u.Fingerprint == "" || u.Expires <= u.Issued {
			r.Detail = "helper evidence does not belong to this invocation"
			return r
		}
		renewed = renewed || (u.Fingerprint != first.Fingerprint && u.Expires > first.Expires && u.Issued > first.Issued && !time.Unix(u.Issued, 0).Before(e.Started))
	}
	for _, at := range e.RemoteStarts {
		if at.Before(e.Started) || at.After(e.Finished) {
			continue
		}
		beforeExpiry = beforeExpiry || at.Before(expiry)
		afterExpiry = afterExpiry || at.After(expiry)
	}
	r.Pass = renewed && beforeExpiry && afterExpiry
	if !r.Pass {
		r.Detail = "missing in-build renewal or remote execution on both sides of JWT expiry"
	}
	return r
}

// recordCredentials wraps the public helper protocol, recording only token
// times/kid and a SHA-256 fingerprint. Bearer tokens stay in the helper pipe,
// never in execution artifacts. Only Bazel invokes it; observation cannot renew.
func recordCredentials(lr *laneRun) (string, func() ([]CredentialUse, error), error) {
	c, h := lr.c, lr.host
	cli := hjoin(h, h.WorkDir(), "bin", "cucinactl")
	wrapper := hjoin(h, h.WorkDir(), c.Scenario.ID+"-credential-helper")
	trace := wrapper + ".jsonl"
	program := `#!/usr/bin/env python3
# SPDX-License-Identifier: FSL-1.1-ALv2
import base64, hashlib, json, os, subprocess, sys, time
r = subprocess.run(COMMAND, input=sys.stdin.buffer.read(), stdout=subprocess.PIPE)
if r.returncode:
    sys.exit(r.returncode)
try:
    doc = json.loads(r.stdout)
    auth = doc["headers"]["Authorization"][0]
    if not auth.startswith("Bearer "):
        raise ValueError()
    token = auth[7:]
    parts = token.split(".")
    header, claims = [json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4))) for p in parts[:2]]
    use = {"at": time.time(), "issued": int(claims["iat"]), "expires": int(claims["exp"]),
           "kid": header["kid"], "fingerprint": hashlib.sha256(token.encode()).hexdigest()}
    fd = os.open(TRACE, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    with os.fdopen(fd, "w") as out:
        out.write(json.dumps(use) + "\n")
except Exception:
    print("credential evidence is missing or invalid", file=sys.stderr)
    sys.exit(1)
sys.stdout.buffer.write(r.stdout)
`
	program = strings.ReplaceAll(program, "COMMAND", jsonString([]string{cli, "credential-helper", "get"}))
	program = strings.ReplaceAll(program, "TRACE", jsonString(trace))
	local := filepath.Join(c.Dir(), "credential-recorder.py")
	if err := os.WriteFile(local, []byte(program), 0o700); err != nil {
		return "", nil, err
	}
	if err := h.Put(c, local, wrapper); err != nil {
		return "", nil, err
	}
	// Put is root-owned on SSM clients. Set the executable mode as root and
	// create only the metadata trace as the non-root Bazel user's writable file.
	res, err := h.Run(c, "chmod 0755 "+shellQuote(wrapper)+" && install -o "+shellQuote(lr.user)+" -m 0600 /dev/null "+shellQuote(trace), remote.Opts{})
	if err != nil {
		return "", nil, err
	}
	if err := res.Err(); err != nil {
		return "", nil, err
	}
	res, err = h.Run(c, "command -v python3 >/dev/null && test -w "+shellQuote(trace), remote.Opts{User: lr.user})
	if err != nil {
		return "", nil, err
	}
	if err := res.Err(); err != nil {
		return "", nil, harness.Skip("T10 credential evidence requires python3 and a writable client trace")
	}
	read := func() ([]CredentialUse, error) {
		res, err := h.Run(c, readFileScript(h, trace), remote.Opts{User: lr.user})
		if err != nil {
			return nil, err
		}
		if err := res.Err(); err != nil {
			return nil, err
		}
		var uses []CredentialUse
		for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
			var u CredentialUse
			if json.Unmarshal([]byte(line), &u) != nil || u.At <= 0 || u.Issued <= 0 || u.Expires <= u.Issued || u.Kid == "" || u.Fingerprint == "" {
				return nil, harness.Fail("no valid Bazel credential-helper evidence")
			}
			uses = append(uses, u)
		}
		return uses, nil
	}
	target := c.Env.Endpoints.RemoteExecution
	if client := c.Env.Clients[lr.lane.Host]; client.RemoteExecution != "" {
		target = client.RemoteExecution
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return "", nil, harness.Fail("credential helper needs a REAPI URL with a host")
	}
	return "--credential_helper=" + u.Hostname() + "=" + wrapper, read, nil
}

// T10c adds a serial, one-minute-per-action remote fixture to the unchanged
// Abseil test invocation. It guarantees work across a whole JWT TTL without
// depending on Abseil's machine-dependent speed or between-invocation renewal.
func runT10c(c *harness.Context) error {
	control, err := campaignToken(c)
	if err != nil {
		return err
	}
	parts := strings.Split(control.Raw, ".")
	var claims struct {
		Issued  int64 `json:"iat"`
		Expires int64 `json:"exp"`
	}
	if len(parts) != 3 {
		return harness.Fail("missing initial JWT lifetime")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Expires <= claims.Issued {
		return harness.Fail("missing initial JWT lifetime")
	}
	ttl := time.Duration(claims.Expires-claims.Issued) * time.Second
	if ttl > 30*time.Minute {
		return harness.Skip("T10c's bounded remote fixture supports token TTLs up to 30 minutes")
	}
	lr, err := openLane(c, LinuxLane)
	if err != nil {
		return err
	}
	flag, read, err := recordCredentials(lr)
	if err != nil {
		return err
	}
	dir := hjoin(lr.host, lr.host.WorkDir(), "t10c-renewal")
	var build strings.Builder
	build.WriteString("# SPDX-License-Identifier: FSL-1.1-ALv2\n")
	steps := int((ttl+time.Minute-1)/time.Minute) + 2
	for i := 0; i < steps; i++ {
		srcs, cmd := "[]", "/bin/sleep 60 && printf '"+uuid.NewString()+"\\n' >$@"
		if i > 0 {
			srcs = fmt.Sprintf("[\":step_%d\"]", i-1)
			cmd = fmt.Sprintf("/bin/sleep 60 && /bin/cat $(location :step_%d) >$@", i-1)
		}
		fmt.Fprintf(&build, "genrule(name = %q, srcs = %s, outs = [%q], cmd = %q, visibility = [\"//visibility:public\"])\n", fmt.Sprintf("step_%d", i), srcs, fmt.Sprintf("step_%d.txt", i), cmd)
	}
	for name, contents := range map[string]string{"MODULE.bazel": "# SPDX-License-Identifier: FSL-1.1-ALv2\nmodule(name = \"cucina_renewal\")\n", "BUILD.bazel": build.String()} {
		local := filepath.Join(c.Dir(), "renewal-"+name)
		if err := os.WriteFile(local, []byte(contents), 0o600); err != nil {
			return err
		}
		if err := lr.host.Put(c, local, hjoin(lr.host, dir, name)); err != nil {
			return err
		}
	}
	res, err := lr.host.Run(c, "chmod 0755 "+shellQuote(dir)+" && chmod 0644 "+shellQuote(hjoin(lr.host, dir, "MODULE.bazel"))+" "+shellQuote(hjoin(lr.host, dir, "BUILD.bazel")), remote.Opts{})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}
	o, err := lr.bazel("ttl-spanning-test", BuildOpts{Command: "test", ForceExecute: true, Extra: []string{flag,
		"--inject_repository=cucina_renewal=" + dir, "--nobuild_tests_only", fmt.Sprintf("@cucina_renewal//:step_%d", steps-1)}})
	if err != nil {
		return err
	}
	if err := mustSucceed(o, "single build spanning the token TTL"); err != nil {
		return err
	}
	uses, err := read()
	if err != nil {
		return err
	}
	log, err := execlog.ReadFile(o.ExecLogPath)
	if err != nil {
		return err
	}
	if o.BEP == nil {
		return harness.Fail("missing invocation timestamps")
	}
	check := TokenRenewalCheck{Started: o.BEP.Started, Finished: o.BEP.Finished, Uses: uses}
	for _, spawn := range log.Spawns {
		if spawn.Remote() {
			check.RemoteStarts = append(check.RemoteStarts, spawn.Start)
		}
	}
	verdict := check.Evaluate(c, c)
	c.Check(verdict)
	c.Record("credentialUses", uses)
	c.Metric("span_seconds", o.BEP.Finished.Sub(o.BEP.Started).Seconds(), "s")
	c.Metric("token_ttl_seconds", ttl.Seconds(), "s")
	if !verdict.Pass {
		return harness.Fail("%s", verdict.Detail)
	}
	return nil
}

func campaignToken(c *harness.Context) (canary.Token, error) {
	if c.Env.Secrets.ServiceKeyFile == "" || c.Env.Endpoints.STS == "" || c.Env.Endpoints.RemoteExecution == "" {
		return canary.Token{}, harness.Skip("STS, REAPI and secrets.serviceKeyFile are required for a valid control")
	}
	hc, err := stsHTTP(c.Env)
	if err != nil {
		return canary.Token{}, err
	}
	return (&canary.STS{URL: c.Env.Endpoints.STS, KeyFile: c.Env.Secrets.ServiceKeyFile, HTTP: hc, Now: c.Now}).Token(c)
}

func createServiceKey(c *harness.Context, svc *infra.Services, subject, purpose string) (string, string, error) {
	account, ok := strings.CutPrefix(subject, "sa:")
	if !ok || account == "" {
		return "", "", harness.Fail("campaign writer is not a service account")
	}
	var created struct {
		KeyID   string `json:"key_id"`
		Account string `json:"account"`
		Key     string `json:"key"`
	}
	if err := svc.CucinactlJSON(c, &created, "keys", "create", "--account", account, "--description", purpose+"-"+c.Env.RunID, "--ttl", "1h"); err != nil {
		return "", "", err
	}
	if created.KeyID == "" || created.Key == "" || created.Account != account {
		return "", "", harness.Fail("keys create returned no key/key_id or a different account")
	}
	return created.Key, created.KeyID, nil
}

// revokeCreatedKey cleans up the exchange credential; it is NOT the deny-list proof.
func revokeCreatedKey(c *harness.Context, svc *infra.Services, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c), 30*time.Second)
	defer cancel()
	_, err := svc.Cucinactl(ctx, "keys", "revoke", id, "--yes")
	c.Check(harness.CheckResult{Name: "temporary service key revoked", Kind: "cleanup", Pass: err == nil, Detail: errString(err)})
}

func casAccess(ctx context.Context, env *harness.Env, token string) error {
	conn, err := endpoint(env).Dial(token)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = repb.NewContentAddressableStorageClient(conn).FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{
		InstanceName: instanceName(env), BlobDigests: []*repb.Digest{{Hash: strings.Repeat("cd", 32), SizeBytes: 1}}})
	return err
}

// T10d revokes the JWT's session, not the exchange-only service key. Both
// probes reuse their original tokens, which stay unexpired throughout the bound.
func runT10d(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	control, err := campaignToken(c)
	if err != nil {
		return err
	}
	key, id, err := createServiceKey(c, svc, control.Subject, "e2e-t10d")
	if err != nil {
		return err
	}
	defer revokeCreatedKey(c, svc, id)
	hc, err := stsHTTP(c.Env)
	if err != nil {
		return err
	}
	tok, err := (&canary.STS{URL: c.Env.Endpoints.STS, Key: key, HTTP: hc, Now: c.Now}).Token(c)
	if err != nil {
		return err
	}
	// Token was signature-verified by STS.Token; only extract the session here.
	parts := strings.Split(tok.Raw, ".")
	var claims struct {
		SID string `json:"sid"`
	}
	if len(parts) != 3 {
		return harness.Fail("STS returned a malformed JWT")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(body, &claims) != nil || claims.SID == "" {
		return harness.Fail("STS returned no JWT session")
	}
	if !tok.Expiry.After(c.Now().Add(4*time.Minute)) || !control.Expiry.After(c.Now().Add(4*time.Minute)) {
		return harness.Skip("token TTL must exceed the 3-minute revocation bound plus expiry skew")
	}
	for _, raw := range []string{control.Raw, tok.Raw} {
		if err := casAccess(c, c.Env, raw); err != nil {
			return harness.Fail("valid token failed before revocation: %s", status.Code(err))
		}
	}
	revoked := c.Now()
	if _, err := svc.Cucinactl(c, "keys", "revoke", "--sid", claims.SID, "--reason", "T10d existing-token proof", "--yes"); err != nil {
		return err
	}
	deadline, cancel := context.WithTimeout(c, 3*time.Minute-c.Now().Sub(revoked))
	defer cancel()
	for {
		if err := casAccess(deadline, c.Env, control.Raw); err != nil {
			return harness.Fail("valid control unavailable during revocation: %s", status.Code(err))
		}
		code := status.Code(casAccess(deadline, c.Env, tok.Raw))
		if code == codes.PermissionDenied {
			if err := casAccess(deadline, c.Env, control.Raw); err != nil {
				return harness.Fail("valid control failed after denial: %s", status.Code(err))
			}
			took := c.Now().Sub(revoked)
			c.Metric("revocation_seconds", took.Seconds(), "s")
			if took > 3*time.Minute {
				return harness.Fail("revocation took %s", took)
			}
			return nil
		}
		if code != codes.OK {
			return harness.Fail("revocation probe returned %s, not PermissionDenied", code)
		}
		if err := remote.RealSleep(deadline, 5*time.Second); err != nil {
			return harness.Fail("existing token not denied within 3 minutes")
		}
	}
}

// T10e rotates only after this invocation is executing, observes a changed
// signing kid while it is still active, and joins/cancels the workload on all paths.
func runT10e(c *harness.Context) error {
	if c.Env.Kubernetes == nil || len(c.Env.Kubernetes.RotateSigningKey) == 0 {
		return harness.Skip("no kubernetes.rotateSigningKey command in the environment descriptor")
	}
	argv := c.Env.Kubernetes.RotateSigningKey
	lr, err := openLane(c, LinuxLane)
	if err != nil {
		return err
	}
	old, err := campaignToken(c)
	if err != nil {
		return err
	}
	flag, read, err := recordCredentials(lr)
	if err != nil {
		return err
	}
	invocation := uuid.NewString()
	ctx, cancel := context.WithCancel(c)
	done := make(chan error, 1)
	finished := false
	defer func() {
		cancel()
		if !finished {
			<-done
		}
	}()
	go func() {
		o, err := lr.bazel("build-during-rotation", BuildOpts{Command: "test", Context: ctx, ForceExecute: true,
			Extra: []string{flag, "--invocation_id=" + invocation}})
		if err == nil {
			err = mustSucceed(o, "build during signing-key rotation")
		}
		done <- err
	}()
	stillRunning := func() error {
		select {
		case err := <-done:
			finished = true
			if err != nil {
				return err
			}
			return harness.Fail("workload ended before signing-key rotation was observed during execution")
		default:
			return nil
		}
	}
	active := func() (bool, error) {
		if err := stillRunning(); err != nil {
			return false, err
		}
		var ops struct {
			Operations []struct {
				Stage, Name      string
				ToolInvocationID string `json:"tool_invocation_id"`
				InvocationID     string `json:"invocation_id"`
			} `json:"operations"`
		}
		if err := lr.svc.CucinactlJSON(c, &ops, "ops", "list", "--stage", "executing", "--invocation", invocation); err != nil {
			return false, err
		}
		for _, op := range ops.Operations {
			if op.Name != "" && strings.EqualFold(op.Stage, "executing") && (op.ToolInvocationID == invocation || op.InvocationID == invocation) {
				return true, stillRunning()
			}
		}
		return false, nil
	}
	for {
		ok, err := active()
		if err != nil {
			return err
		}
		if ok {
			break
		}
		if err := remote.RealSleep(ctx, time.Second); err != nil {
			return err
		}
	}
	uses, err := read()
	if err != nil {
		return err
	}
	if uses[0].Kid != old.KeyID {
		return harness.Fail("workload and pre-rotation control use different signing keys")
	}
	if err := stillRunning(); err != nil {
		return err
	}
	if err := c.Step("rotate the signing key during active execution", func() error {
		r, err := scenarioExec(c).Run(ctx, ports.Command{Path: argv[0], Args: argv[1:], Env: append(os.Environ(), "KUBECONFIG="+c.Env.Kubernetes.Kubeconfig)})
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return harness.Fail("signing-key rotation command exited %d", r.ExitCode)
		}
		return nil
	}); err != nil {
		return err
	}
	for {
		if err := stillRunning(); err != nil {
			return err
		}
		fresh, err := campaignToken(c)
		if err != nil {
			return err
		}
		if fresh.KeyID != old.KeyID {
			if ok, err := active(); err != nil {
				return err
			} else if ok {
				for _, raw := range []string{old.Raw, fresh.Raw} {
					if err := casAccess(ctx, c.Env, raw); err != nil {
						return harness.Fail("old/new signing-key control failed: %s", status.Code(err))
					}
				}
				if err := stillRunning(); err != nil {
					return err
				}
				c.Record("signingKeys", map[string]string{"before": old.KeyID, "after": fresh.KeyID})
				break
			}
		}
		if err := remote.RealSleep(ctx, time.Second); err != nil {
			return err
		}
	}
	err = <-done
	finished = true
	return err
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
	t, err := (&canary.STS{URL: c.Env.Endpoints.STS, KeyFile: kf, HTTP: hc, Now: c.Now}).Token(c)
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
	control, err := campaignToken(c)
	if err != nil {
		return err
	}
	writer, err := endpoint(c.Env).Dial(control.Raw)
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	d := &repb.Digest{Hash: strings.Repeat("ab", 32), SizeBytes: 1}
	if _, err := repb.NewActionCacheClient(writer).UpdateActionResult(c, &repb.UpdateActionResultRequest{InstanceName: instanceName(c.Env), ActionDigest: d, ActionResult: &repb.ActionResult{}}); err != nil {
		return harness.Fail("valid writer cannot write the AC: %s", status.Code(err))
	}
	_, getErr := ac.GetActionResult(c, &repb.GetActionResultRequest{InstanceName: instanceName(c.Env), ActionDigest: d})
	_, putErr := ac.UpdateActionResult(c, &repb.UpdateActionResultRequest{InstanceName: instanceName(c.Env), ActionDigest: d, ActionResult: &repb.ActionResult{}})
	readOK := getErr == nil
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
	control, err := campaignToken(c)
	if err != nil {
		return err
	}
	if err := casAccess(c, c.Env, control.Raw); err != nil {
		return harness.Fail("authenticated control failed: %s", status.Code(err))
	}
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
		ok := code == codes.Unauthenticated
		c.Check(harness.CheckResult{Name: n + " without a token", Kind: "auth", Pass: ok, Value: code.String()})
		if !ok {
			bad = append(bad, n+"="+code.String())
		}
	}
	if len(bad) > 0 {
		return harness.Fail("unauthenticated calls not rejected: %s", strings.Join(bad, ", "))
	}
	if err := casAccess(c, c.Env, control.Raw); err != nil {
		return harness.Fail("authenticated control failed after denial: %s", status.Code(err))
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

// tlsProbe sends the HTTP/2 preface so TLS 1.3's deferred client-auth alert is
// read too. EOF, timeout, DNS errors and local server-trust failures are not denials.
func tlsProbe(ctx context.Context, addr string, cfg *tls.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := tls.Dialer{Config: cfg, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"); err != nil {
		return err
	}
	_, err = io.ReadFull(conn, make([]byte, 9))
	return err
}

func certificateAlert(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "remote error" {
		return false
	}
	switch op.Err.Error() {
	case "tls: certificate required", "tls: bad certificate", "tls: unknown certificate authority", "tls: access denied":
		return true
	}
	return false
}

// T10h pairs each invalid-certificate probe with a reachable, trusted control.
func runT10h(c *harness.Context) error {
	s := c.Env.Secrets
	if c.Env.Endpoints.Host == "" || c.Env.Endpoints.WorkerListener == "" || !c.Env.Has(harness.RequiresLinuxClient) ||
		s.HostCertFile == "" || s.HostKeyFile == "" || s.WorkerCertFile == "" || s.WorkerKeyFile == "" || c.Env.Endpoints.CAFile == "" {
		return harness.Skip("T10h requires both mTLS endpoints, linux-client, CA and valid host/worker certificate-key controls")
	}
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	cfg, err := endpoint(c.Env).TLSConfig()
	if err != nil {
		return err
	}
	cfg.MinVersion, cfg.NextProtos = tls.VersionTLS13, []string{"h2"}
	valid, err := tls.LoadX509KeyPair(s.HostCertFile, s.HostKeyFile)
	if err != nil {
		return err
	}
	cfg.Certificates = []tls.Certificate{valid}
	impostor, err := selfSignedClient()
	if err != nil {
		return err
	}
	for _, certs := range [][]tls.Certificate{nil, {impostor}} {
		if err := tlsProbe(c, c.Env.Endpoints.Host, cfg); err != nil {
			return harness.Fail("valid host certificate control failed: %v", err)
		}
		invalid := cfg.Clone()
		// Force the self-signed certificate to be offered even when its issuer
		// is not in the server's acceptable-CA list.
		invalid.Certificates = certs
		if len(certs) > 0 {
			invalid.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &impostor, nil }
		}
		err := tlsProbe(c, c.Env.Endpoints.Host, invalid)
		if !certificateAlert(err) {
			return harness.Fail("host probe needs an explicit client-certificate alert, got %v", err)
		}
		if err := tlsProbe(c, c.Env.Endpoints.Host, cfg); err != nil {
			return harness.Fail("host control failed after denial: %v", err)
		}
	}
	// The private worker listener is reachable only from the Linux client.
	h, err := svc.Host(c, "linux-client")
	if err != nil {
		return err
	}
	files := []string{s.WorkerCertFile, s.WorkerKeyFile, c.Env.Endpoints.CAFile}
	var paths []string
	for i, file := range files {
		dst := hjoin(h, h.WorkDir(), "secrets", fmt.Sprintf("t10h-%d", i))
		if err := remote.PutPrivate(c, h, file, dst, svc.ClientUser(h.Name())); err != nil {
			return err
		}
		paths = append(paths, dst)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c), 30*time.Second)
		defer cancel()
		_, _ = h.Run(ctx, "rm -f "+shellQuote(paths[0])+" "+shellQuote(paths[1])+" "+shellQuote(paths[2]), remote.Opts{User: svc.ClientUser(h.Name())})
	}()
	addr := c.Env.Endpoints.WorkerListener
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	verify := " -verify_hostname " + shellQuote(host)
	if net.ParseIP(host) != nil {
		verify = " -verify_ip " + shellQuote(host)
	}
	base := "timeout 15 openssl s_client -brief -tls1_3 -verify_return_error" + verify + " -CAfile " + shellQuote(paths[2]) + " -connect " + shellQuote(addr) + " -alpn h2"
	for _, control := range []bool{true, false, true} {
		cmd := base
		if control {
			cmd += " -cert " + shellQuote(paths[0]) + " -key " + shellQuote(paths[1])
		}
		r, err := h.Run(c, cmd+" </dev/null 2>&1", remote.Opts{User: svc.ClientUser(h.Name()), Timeout: 20 * time.Second})
		if err != nil {
			return err
		}
		out := string(r.Stdout)
		if control {
			if r.ExitCode != 0 || !strings.Contains(out, "CONNECTION ESTABLISHED") || !strings.Contains(out, "Verification: OK") {
				return harness.Fail("valid worker certificate control did not establish verified TLS")
			}
		} else if !strings.Contains(out, "tlsv13 alert certificate required") || r.ExitCode == 124 {
			return harness.Fail("worker probe did not receive the certificate_required TLS alert")
		}
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
	if err := c.Err(); err != nil {
		return err
	}
	if len(open) == 0 {
		return harness.Fail("port scan found no reachable TLS endpoint; empty measurement cannot prove exposure")
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
