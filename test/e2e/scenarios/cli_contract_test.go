// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/providers/tart/faketart"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/scenarios"
)

// Guards: T10c — between-build renewal or a later fresh build is not mid-build renewal.
func TestRenewalBelongsToOneInvocation(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	first := scenarios.CredentialUse{At: float64(start.Add(time.Second).Unix()), Issued: start.Unix(), Expires: start.Add(15 * time.Minute).Unix(), Kid: "a", Fingerprint: "first"}
	renewed := scenarios.CredentialUse{At: float64(start.Add(13 * time.Minute).Unix()), Issued: start.Add(13 * time.Minute).Unix(), Expires: start.Add(28 * time.Minute).Unix(), Kid: "a", Fingerprint: "renewed"}
	for _, tc := range []struct {
		name       string
		begin, end time.Time
		uses       []scenarios.CredentialUse
		remote     []time.Time
		want       bool
	}{
		{"mid-build", start, start.Add(17 * time.Minute), []scenarios.CredentialUse{first, renewed}, []time.Time{start.Add(time.Minute), start.Add(16 * time.Minute)}, true},
		{"renewed between builds", start.Add(16 * time.Minute), start.Add(33 * time.Minute), []scenarios.CredentialUse{first, renewed}, []time.Time{start.Add(18 * time.Minute)}, false},
		{"no remote work", start, start.Add(17 * time.Minute), []scenarios.CredentialUse{first, renewed}, nil, false},
		{"no renewal", start, start.Add(17 * time.Minute), []scenarios.CredentialUse{first}, []time.Time{start.Add(16 * time.Minute)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var check harness.Check = scenarios.TokenRenewalCheck{Started: tc.begin, Finished: tc.end, Uses: tc.uses, RemoteStarts: tc.remote}
			r := check.Evaluate(t.Context(), nil)
			require.Equal(t, tc.want, r.Pass, r.Detail)
		})
	}
}

func scenarioContext(t *testing.T, id string) (*harness.Context, *fakes.Exec) {
	t.Helper()
	reg := harness.NewRegistry()
	scenarios.Register(reg)
	s, ok := reg.Get(id)
	require.True(t, ok)
	clock := fakes.NewClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	ex := fakes.NewExec(clock, fakes.NewRand(10))
	env := &harness.Env{Name: "contract", RunID: "contract", ArtifactsDir: t.TempDir(),
		Cucinactl: map[string]string{runtime.GOOS: "cucinactl"}}
	c := &harness.Context{Context: t.Context(), Env: env, Scenario: s,
		Result: &harness.Result{RunID: env.RunID}, Now: clock.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.Services = &infra.Services{Env: env, Exec: ex}
	return c, ex
}

// Guards: T10b — a server failure, missing route or rate limit is not an authentication denial.
func TestIdentityDenialsRequireExactStatus(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c, _ := scenarioContext(t, "T10b")
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					if r.URL.Path == "/google-short/token" {
						http.Error(w, "no short-token fixture", http.StatusServiceUnavailable)
						return
					}
					if err := r.ParseForm(); err != nil {
						http.Error(w, "invalid form", http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"id_token": r.Form.Get("scope")})
					return
				}
				if err := r.ParseForm(); err != nil {
					http.Error(w, "invalid form", http.StatusBadRequest)
					return
				}
				if strings.Contains(r.Form.Get("subject_token"), "valid") || strings.Contains(r.Form.Get("subject_token"), "push-main") {
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "control", "token_type": "Bearer", "expires_in": 900})
					return
				}
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "server_error"})
			}))
			t.Cleanup(srv.Close)
			ca := filepath.Join(t.TempDir(), "ca.pem")
			require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
			c.Env.IdP = &harness.IdPEnv{MockOAuth2URL: srv.URL, CAFile: ca}
			c.Env.Endpoints = harness.Endpoints{STS: srv.URL, CAFile: ca}
			require.Error(t, c.Scenario.Run(c))
			found := false
			for _, check := range c.Result.Checks {
				if check.Name == "wrong hd" {
					found = true
					require.False(t, check.Pass, "HTTP %d must not be reported as an auth denial", code)
				}
			}
			require.True(t, found, "the rejection must record a failed check")
		})
	}
}

// Guards: T10h/T13/T14 — missing prerequisites never produce acceptance
// evidence, and T13 uses the MacHost wire phase (Online), not a pod's Ready condition.
func TestScenarioMissingPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name, id, phase string
		want            harness.Status
	}{
		{name: "T10h", id: "T10h", want: harness.StatusSkip},
		{name: "T13/no hosts", id: "T13", want: harness.StatusFail},
		{name: "T13/online host", id: "T13", phase: v1alpha1.MacHostOnline, want: harness.StatusError},
		{name: "T13/pending host", id: "T13", phase: v1alpha1.MacHostPending, want: harness.StatusFail},
		{name: "T13/offline host", id: "T13", phase: v1alpha1.MacHostOffline, want: harness.StatusFail},
		{name: "T13/draining host", id: "T13", phase: v1alpha1.MacHostDraining, want: harness.StatusFail},
		{name: "T13/cordoned host", id: "T13", phase: v1alpha1.MacHostCordoned, want: harness.StatusFail},
		{name: "T13/denied host", id: "T13", phase: v1alpha1.MacHostDenied, want: harness.StatusFail},
		{name: "T13/unsupported Ready", id: "T13", phase: "Ready", want: harness.StatusFail},
		{name: "T14", id: "T14", want: harness.StatusSkip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ex := scenarioContext(t, tc.id)
			c.Env.DevMac = &harness.DevMacEnv{PkgPath: "initial.pkg", MDMKit: "kit"}
			poolUnavailable := errors.New("pool API unavailable in this readiness fixture")
			ex.Handle("cucinactl", func(_ context.Context, cmd ports.Command) (ports.ExecResult, error) {
				if len(cmd.Args) >= 2 && cmd.Args[0] == "hosts" && cmd.Args[1] == "list" {
					if tc.phase == "" {
						return ports.ExecResult{Stdout: []byte(`{"hosts":[]}`)}, nil
					}
					body, err := json.Marshal(map[string]any{"hosts": []map[string]any{{"serial": "contract-host", "phase": tc.phase, "slots": 2, "cordoned": false}}})
					return ports.ExecResult{Stdout: body}, err
				}
				if len(cmd.Args) >= 2 && cmd.Args[0] == "pools" && cmd.Args[1] == "describe" {
					return ports.ExecResult{}, poolUnavailable
				}
				return ports.ExecResult{ExitCode: 2}, nil
			})
			err := c.Scenario.Run(c)
			require.Equal(t, tc.want, harness.Classify(err), "%v", err)
			if tc.phase == v1alpha1.MacHostOnline {
				// Accept the healthy host, but preserve the next public boundary's
				// failure rather than contacting a real Mac or starting a workload.
				require.ErrorIs(t, err, poolUnavailable)
			}
		})
	}
}

// Guards: T20/R-CLI-3 — registered scenario commands use the CLI's config view subcommand.
func TestCLIReadCommandsRespectContract(t *testing.T) {
	c, ex := scenarioContext(t, "T20")
	ex.Handle("cucinactl", func(_ context.Context, cmd ports.Command) (ports.ExecResult, error) {
		if len(cmd.Args) > 0 && cmd.Args[0] == "config" && (len(cmd.Args) < 2 || cmd.Args[1] != "view") {
			return ports.ExecResult{ExitCode: 2}, nil
		}
		return ports.ExecResult{Stdout: []byte(`{}`)}, nil
	})
	// The absent Abseil pin stops before any real client or VM is opened.
	require.Equal(t, harness.StatusSkip, harness.Classify(c.Scenario.Run(c)))
	found := false
	for _, check := range c.Result.Checks {
		if strings.HasPrefix(check.Name, "cucinactl config ") {
			found = true
			require.True(t, check.Pass, check.Name)
		}
	}
	require.True(t, found)
}

// Guards: T10d — deny-list the existing JWT session, not its exchange key, with a reachable control.
func TestExistingTokenRevocationContract(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			c, ex := scenarioContext(t, "T10d")
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			require.NoError(t, err)
			jwk := jose.JSONWebKey{Key: &key.PublicKey, KeyID: "test", Algorithm: "ES256", Use: "sig"}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "test"))
			require.NoError(t, err)
			var denied atomic.Bool
			tokenFor := func(issuer, sid string) string {
				raw, err := jwt.Signed(signer).Claims(map[string]any{
					"iss": issuer, "aud": "buildbarn", "sub": "sa:writer", "sid": sid,
					"iat": c.Now().Unix(), "exp": c.Now().Add(15 * time.Minute).Unix(),
					"cucina": map[string][]string{"cas_read": {"main"}},
				}).Serialize()
				require.NoError(t, err)
				return raw
			}
			var url string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/.well-known/cucina-configuration":
					_ = json.NewEncoder(w).Encode(map[string]string{"issuer": url, "token_endpoint": url + "/token", "jwks_uri": url + "/jwks"})
				case "/jwks":
					_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
				case "/token":
					if err := r.ParseForm(); err != nil {
						http.Error(w, "invalid form", http.StatusBadRequest)
						return
					}
					sid := "control-session"
					if r.Form.Get("subject_token") == "new-key" {
						sid = "target-session"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tokenFor(url, sid), "token_type": "Bearer", "expires_in": 900})
				default:
					http.NotFound(w, r)
				}
			}))
			url = srv.URL
			t.Cleanup(srv.Close)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			repb.RegisterContentAddressableStorageServer(server, &revocationCAS{denied: &denied, code: code})
			go func() { _ = server.Serve(ln) }()
			t.Cleanup(server.Stop)
			c.Env.Endpoints = harness.Endpoints{STS: url, RemoteExecution: "grpc://" + ln.Addr().String()}
			c.Env.Secrets.ServiceKeyFile = filepath.Join(t.TempDir(), "writer.key")
			require.NoError(t, os.WriteFile(c.Env.Secrets.ServiceKeyFile, []byte(t.Name()), 0o600))
			ex.Handle("cucinactl", func(_ context.Context, cmd ports.Command) (ports.ExecResult, error) {
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.HasPrefix(args, "keys create --account writer "):
					return ports.ExecResult{Stdout: []byte(`{"key_id":"new-id","account":"writer","key":"new-key"}`)}, nil
				case strings.HasPrefix(args, "keys revoke --sid target-session "):
					denied.Store(true)
					return ports.ExecResult{Stdout: []byte(`{}`)}, nil
				case strings.HasPrefix(args, "keys revoke new-id "):
					return ports.ExecResult{Stdout: []byte(`{}`)}, nil
				default:
					return ports.ExecResult{ExitCode: 2}, nil
				}
			})
			err = c.Scenario.Run(c)
			if code == codes.PermissionDenied {
				require.NoError(t, err)
				require.True(t, denied.Load(), "an existing-token deny-list entry was required")
				require.Contains(t, c.Result.Metrics, "revocation_seconds")
			} else {
				require.Error(t, err, "a server outage must not prove revocation")
			}
		})
	}
}

type revocationCAS struct {
	repb.UnimplementedContentAddressableStorageServer
	denied *atomic.Bool
	code   codes.Code
}

func (s *revocationCAS) FindMissingBlobs(ctx context.Context, _ *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	auth := md.Get("authorization")
	if len(auth) != 1 {
		return nil, status.Error(codes.Unauthenticated, "missing token")
	}
	tok, err := jwt.ParseSigned(strings.TrimPrefix(auth[0], "Bearer "), []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "bad token")
	}
	var claims struct {
		SID string `json:"sid"`
	}
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return nil, err
	}
	if s.denied.Load() && claims.SID == "target-session" {
		return nil, status.Error(s.code, "injected denial or outage")
	}
	return &repb.FindMissingBlobsResponse{}, nil
}

// Guards: T14 and the small-functional resource bound — package operations stay
// in 2-vCPU/8-GiB guests, three distinct enrollments use at most two slots, and a
// failed revoke or outage cannot prove denial.
func TestPackagingLifecycleContract(t *testing.T) {
	for _, fault := range []string{"", "delayed guest RPC", "resize", "revoke", "enrollment unavailable"} {
		t.Run("fault="+fault, func(t *testing.T) {
			// CI 37074297279: Go resolves the private descriptor under
			// USERPROFILE on Windows, not HOME. Isolate both from the host.
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			c, ex := scenarioContext(t, "T14")
			ctx, cancel := context.WithCancel(c.Context)
			c.Context = ctx
			defer cancel()
			var revoked atomic.Bool
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			cucinav1.RegisterEnrollmentServiceServer(server, &packagingEnrollment{revoked: &revoked, unavailable: fault == "enrollment unavailable"})
			go func() { _ = server.Serve(ln) }()
			t.Cleanup(server.Stop)
			dir := t.TempDir()
			for _, name := range []string{"initial.pkg", "upgrade.pkg", "simulate-mdm.sh", "ca.pem", "signer.pem"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
			}
			c.Env.AWS = &harness.AWSEnv{Profile: "default", Region: "us-west-1"}
			c.Env.Endpoints.Enrollment = "grpc://" + ln.Addr().String()
			c.Env.Endpoints.CAFile = filepath.Join(dir, "ca.pem")
			c.Env.DevMac = &harness.DevMacEnv{PkgPath: filepath.Join(dir, "initial.pkg"), UpgradePkgPath: filepath.Join(dir, "upgrade.pkg"),
				MDMKit: dir, BaseImage: "example.invalid/base", SignerCert: filepath.Join(dir, "signer.pem")}
			tart := faketart.New()
			tart.AddImage(faketart.Image{Ref: c.Env.DevMac.BaseImage})
			serials, boots, versions := map[string]string{}, map[string]int{}, map[string]string{}
			approved := map[string]bool{}
			created, installs, uninstalls := 0, 0, 0
			tart.NewGuest = func(name string) *faketart.Guest {
				created++
				serials[name] = fmt.Sprintf("serial-%d", created)
				return faketart.NewGuest()
			}
			jsonResult := func(v any) ports.ExecResult {
				b, err := json.Marshal(v)
				require.NoError(t, err)
				return ports.ExecResult{Stdout: b}
			}
			nextToken := 0
			ex.Handle("cucinactl", func(_ context.Context, cmd ports.Command) (ports.ExecResult, error) {
				a := cmd.Args
				if len(a) < 2 || a[0] != "hosts" {
					return ports.ExecResult{ExitCode: 2}, nil
				}
				switch a[1] {
				case "enroll-token":
					if a[2] == "create" {
						if !strings.Contains(strings.Join(a, " "), "--site e2e-contract") || !strings.Contains(strings.Join(a, " "), "--max-hosts 3") {
							return ports.ExecResult{ExitCode: 2}, nil
						}
						nextToken++
						return jsonResult(map[string]string{"id": fmt.Sprint(nextToken), "token": fmt.Sprintf("site-%d", nextToken)}), nil
					}
					if a[2] == "revoke" {
						if fault == "revoke" {
							return ports.ExecResult{ExitCode: 1}, nil
						}
						if a[3] == "1" {
							revoked.Store(true)
						}
						return jsonResult(map[string]bool{"ok": true}), nil
					}
				case "list":
					var hosts []map[string]any
					for _, serial := range serials {
						phase := v1alpha1.MacHostPending
						if approved[serial] {
							phase = v1alpha1.MacHostOnline
						}
						hosts = append(hosts, map[string]any{"serial": serial, "phase": phase, "last_heartbeat": c.Now().Add(time.Second)})
					}
					return jsonResult(map[string]any{"hosts": hosts}), nil
				case "approve":
					approved[a[2]] = true
					return jsonResult(map[string]bool{"ok": true}), nil
				case "diag":
					require.Contains(t, a, "--file")
					require.NoError(t, os.WriteFile(a[4], []byte(`{"virtualization":{"available":false,"reason":"nested-macos","hv_support":0}}`), 0o600))
					return jsonResult(map[string]bool{"ok": true}), nil
				case "remove":
					return jsonResult(map[string]bool{"ok": true}), nil
				}
				return ports.ExecResult{ExitCode: 2}, nil
			})
			ex.Handle(filepath.Join(dir, "publish-s3-temp.sh"), func(_ context.Context, cmd ports.Command) (ports.ExecResult, error) {
				if cmd.Args[0] == "create" {
					statePath := cmd.Args[len(cmd.Args)-1]
					rel, err := filepath.Rel(home, statePath)
					require.NoError(t, err)
					require.True(t, filepath.IsLocal(rel), "publisher state must stay inside the isolated fixture home")
					require.NoError(t, os.WriteFile(statePath, []byte("PKG_URL=https://example.invalid/package.pkg\n"), 0o600))
				}
				return ports.ExecResult{}, nil
			})
			ex.Handle("ssh-keygen", func(_ context.Context, cmd ports.Command) (ports.ExecResult, error) {
				return ports.ExecResult{}, os.WriteFile(cmd.Args[len(cmd.Args)-1]+".pub", []byte("test-public-key\n"), 0o600)
			})
			planner := &packagingExec{Exec: ex, tart: tart, rebooted: map[string]bool{}, cancel: cancel, resizeFailure: fault == "resize"}
			if fault == "delayed guest RPC" {
				clock := fakes.NewClock(c.Now())
				c.Now = clock.Now
				c.Services.(*infra.Services).Clock = clock
				planner.bootstrapFailures = 1
				advanced := make(chan struct{})
				go func() {
					defer close(advanced)
					if clock.BlockUntilTimers(ctx, 1) == nil {
						clock.Advance(5 * time.Second)
					}
				}()
				defer func() { cancel(); <-advanced }()
			}
			planner.guest = func(name string, args []string) ports.ExecResult {
				script := strings.Join(args, " ")
				switch {
				case strings.Contains(script, "shutdown -r now"):
					boots[name]++
					planner.rebooted[name] = true
				case strings.Contains(script, "kern.boottime"):
					return ports.ExecResult{Stdout: []byte(fmt.Sprint(boots[name]))}
				case strings.Contains(script, "ioreg -rd1"):
					return ports.ExecResult{Stdout: []byte(serials[name])}
				case strings.Contains(script, "installer -pkg"):
					require.NotContains(t, script, "-allowUntrusted")
					installs++
					versions[name] = "1.0"
					if strings.Contains(script, "cucina-upgrade.pkg") {
						versions[name] = "1.1"
					}
				case strings.Contains(script, "pkg-version"):
					return ports.ExecResult{Stdout: []byte(versions[name])}
				case strings.Contains(script, "cucina-host-uninstall --yes"):
					delete(versions, name)
					uninstalls++
				}
				return ports.ExecResult{}
			}
			c.Services.(*infra.Services).Exec = planner
			err = c.Scenario.Run(c)
			if fault == "" || fault == "delayed guest RPC" {
				require.NoError(t, err)
				for _, check := range c.Result.Checks {
					require.True(t, check.Pass, "%s: %s", check.Name, check.Detail)
				}
				require.Equal(t, 3, created)
				require.Len(t, planner.startedShapes, created)
				for _, shape := range planner.startedShapes {
					require.Equal(t, [2]int{2, 8192}, shape, "T14 must resize inherited golden-image CPU/RAM before starting")
				}
				require.Equal(t, 4, installs)
				require.Equal(t, 1, uninstalls)
			} else {
				require.Error(t, err)
				if fault == "revoke" {
					require.Equal(t, 2, created, "a failed revoke cannot proceed to a denial claim")
				}
				if fault == "resize" {
					require.Empty(t, planner.startedShapes, "a failed resize must never boot the oversized clone")
					require.Zero(t, installs)
				}
			}
			require.Zero(t, tart.RunningCount(), "cleanup releases VM slots even on failure")
			for name := range serials {
				require.Nil(t, tart.VM(name), "throwaway VM must be deleted")
			}
		})
	}
}

type packagingExec struct {
	ports.Exec
	tart              *faketart.Tart
	guest             func(string, []string) ports.ExecResult
	rebooted          map[string]bool
	cancel            context.CancelFunc
	bootstrapFailures int
	startedShapes     [][2]int
	resizeFailure     bool
}

func (e *packagingExec) Run(ctx context.Context, cmd ports.Command) (ports.ExecResult, error) {
	if cmd.Path == "ssh" {
		name := strings.TrimSuffix(filepath.Base(cmd.Args[1]), "-ssh")
		if vm := e.tart.VM(name); vm == nil || vm.State != "running" {
			return ports.ExecResult{ExitCode: 255}, nil
		}
		return e.guest(name, cmd.Args[len(cmd.Args)-1:]), nil
	}
	if cmd.Path != "tart" {
		return e.Exec.Run(ctx, cmd)
	}
	if len(cmd.Args) > 0 && cmd.Args[0] == "set" && e.resizeFailure {
		return ports.ExecResult{ExitCode: 1}, nil
	}
	if len(cmd.Args) > 1 && cmd.Args[0] == "exec" {
		if e.bootstrapFailures > 0 {
			e.bootstrapFailures--
			return ports.ExecResult{ExitCode: 1}, nil
		}
		i := 1
		if cmd.Args[i] == "-i" {
			i++
		}
		if e.rebooted[cmd.Args[i]] {
			// Cirrus RPC belongs to admin's GUI session. Cancel this invalid
			// plan immediately instead of spending seven real minutes polling it.
			e.cancel()
			return ports.ExecResult{}, context.Canceled
		}
		return e.guest(cmd.Args[i], cmd.Args[i+1:]), nil
	}
	r, err := e.tart.Run(ctx, cmd)
	if err == nil && r.ExitCode == 0 && len(cmd.Args) > 0 && cmd.Args[0] == "clone" {
		// Model an oversized inherited golden-image configuration, not a
		// conveniently small fake default. The scenario must override both.
		return e.tart.Run(ctx, ports.Command{Path: "tart", Args: []string{"set", cmd.Args[len(cmd.Args)-1], "--cpu", "8", "--memory", "16384"}})
	}
	return r, err
}

func (e *packagingExec) Start(ctx context.Context, cmd ports.Command) (ports.Process, error) {
	if cmd.Path == "tart" {
		if vm := e.tart.VM(cmd.Args[len(cmd.Args)-1]); vm != nil {
			e.startedShapes = append(e.startedShapes, [2]int{vm.CPU, vm.MemoryMiB})
		}
		return e.tart.Start(ctx, cmd)
	}
	return e.Exec.Start(ctx, cmd)
}

type packagingEnrollment struct {
	cucinav1.UnimplementedEnrollmentServiceServer
	revoked     *atomic.Bool
	unavailable bool
}

func (s *packagingEnrollment) EnrollHost(_ context.Context, req *cucinav1.EnrollHostRequest) (*cucinav1.EnrollHostResponse, error) {
	if s.unavailable {
		return nil, status.Error(codes.Unavailable, "injected outage")
	}
	if req.GetSiteToken() == "site-1" && s.revoked.Load() {
		return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID}, nil
	}
	return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_PENDING}, nil
}
