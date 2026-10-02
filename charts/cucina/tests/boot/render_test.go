// SPDX-License-Identifier: FSL-1.1-ALv2

package boot_test

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_scheduler"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
	"github.com/sloper-ai/cucina/internal/bbtest"
)

// A rendered Buildbarn configuration of one component.
type bbConfig struct {
	component string // frontend, storage, scheduler
	text      string // ConfigMap payload: protojson JSON plus `importstr` for the CA bundle
}

var importstrRE = regexp.MustCompile(`importstr "([^"]+)"`)

// renderProfile renders the chart and returns its Buildbarn configurations.
func renderProfile(t testing.TB, valuesFiles []string, sets ...string) map[string]bbConfig {
	t.Helper()
	return configsOf(t, charttest.Template(t, valuesFiles, sets...))
}

// configsOf extracts the Buildbarn configurations from rendered manifests.
func configsOf(t testing.TB, manifests []byte) map[string]bbConfig {
	t.Helper()
	out := map[string]bbConfig{}
	for _, o := range charttest.Objects(t, manifests) {
		if o.Kind != "ConfigMap" {
			continue
		}
		for _, c := range []string{"frontend", "storage", "scheduler"} {
			if text, ok := o.Data[c+".jsonnet"]; ok {
				out[c] = bbConfig{component: c, text: text}
			}
		}
	}
	if len(out) != 3 {
		t.Fatalf("expected frontend, storage and scheduler configurations, got %v", charttest.SortedKeys(out))
	}
	return out
}

// TestConfigRenderBootsPinnedBinaries is the R-CP-2 config-render check and the gate
// for every Buildbarn bump (R-OPS-1): it renders every configuration profile (size
// profile x storage mode x client exposure x TLS source, with the sample pools so the
// scheduler has predeclared queues), parses the scheduler configuration strictly with
// the pinned bb-remote-execution protos, and starts the pinned bb_storage/bb_scheduler
// release binaries against each distinct configuration: strict protojson (an unknown
// or renamed field aborts startup), every JMESPath test vector, the importstr CA
// bundle, block devices and persistent state are all evaluated at startup.
func TestConfigRenderBootsPinnedBinaries(t *testing.T) {
	bins := map[string]string{
		"frontend":  bbtest.Binary(t, bbtest.EnvStorage),
		"storage":   bbtest.Binary(t, bbtest.EnvStorage),
		"scheduler": bbtest.Binary(t, bbtest.EnvScheduler),
	}
	samples := filepath.Join(charttest.ChartDir(t), "samples", "pools.yaml")

	type variant struct {
		name string
		sets []string
	}
	var variants []variant
	for _, size := range []string{"small", "medium", "large"} {
		for _, mode := range []string{"file", "block"} {
			for _, exposure := range []string{"LoadBalancer", "Ingress"} {
				for _, tlsSource := range []string{"generated", "certManager"} {
					sets := []string{"sizeProfile=" + size, "storage.mode=" + mode, "exposure.client.type=" + exposure,
						"endpoints.client.host=cucina.example.com", "endpoints.worker.host=192.0.2.10",
						"tls.public.source=" + tlsSource, "tls.internal.source=" + tlsSource}
					if mode == "block" {
						sets = append(sets, "storage.storageClassName=ebs-gp3-block")
					}
					if tlsSource == "certManager" {
						sets = append(sets, "tls.public.certManager.issuerRef.name=test", "tls.internal.certManager.issuerRef.name=test")
					}
					variants = append(variants, variant{fmt.Sprintf("%s/%s/%s/%s", size, mode, exposure, tlsSource), sets})
				}
			}
		}
	}

	// Render everything first (concurrently), then boot each distinct configuration once.
	charttest.Tool(t, "HELM", "helm") // resolve (or skip) on the test goroutine
	manifests := make([][]byte, len(variants))
	errs := make([]error, len(variants))
	var wg sync.WaitGroup
	for i, v := range variants {
		wg.Go(func() {
			manifests[i], errs[i] = charttest.Helm(t, charttest.TemplateArgs(t, []string{samples}, v.sets...)...)
		})
	}
	wg.Wait()
	rendered := make([]map[string]bbConfig, len(variants))
	for i := range variants {
		if errs[i] != nil {
			t.Fatalf("%s: %v", variants[i].name, errs[i])
		}
		rendered[i] = configsOf(t, manifests[i])
	}
	distinct := map[string]bbConfig{}
	var order []string
	for i, v := range variants {
		for _, c := range rendered[i] {
			sum := sha256.Sum256([]byte(c.component + "\x00" + c.text))
			key := hex.EncodeToString(sum[:6])
			if _, seen := distinct[key]; !seen {
				distinct[key] = c
				order = append(order, key)
			}
			if c.component == "scheduler" {
				assertSchedulerProtoStrict(t, v.name, c.text)
			}
		}
	}
	t.Logf("%d profiles rendered, %d distinct configurations", len(variants), len(distinct))

	pki := bbtest.NewPKI(t)
	var mu sync.Mutex // bbtest.PKI is not safe for concurrent use
	for _, key := range order {
		c := distinct[key]
		t.Run(c.component+"-"+key, func(t *testing.T) {
			t.Parallel()
			root := bbtest.ShortTempDir(t)
			mu.Lock()
			server := pki.LoopbackServer(t, "server-"+key, "spiffe://cucina/server/"+c.component)
			mu.Unlock()
			w := wire(t, c, root, pki, server, wiring{})
			p := bbtest.Start(t, c.component, bins[c.component], w.config, bbtest.WithDir(root))
			if err := p.WaitReady(t.Context(), bbtest.GRPCReady(w.probe, w.probeTLS)); err != nil {
				t.Fatalf("%s did not start with the rendered configuration: %v\n%s", c.component, err, bbtest.Tail(p.Logs(), 40))
			}
		})
	}
}

// assertSchedulerProtoStrict parses the scheduler configuration with the pinned
// bb-remote-execution Go types (protojson, unknown fields are errors) and checks the
// R-RE-2 timeouts.
func assertSchedulerProtoStrict(t *testing.T, variant, text string) {
	t.Helper()
	jsonText := importstrRE.ReplaceAllString(text, `"-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"`)
	var cfg bb_scheduler.ApplicationConfiguration
	if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(jsonText), &cfg); err != nil {
		t.Errorf("%s: scheduler configuration does not match bb-remote-execution 1a3be95: %v", variant, err)
	}
	if got := cfg.GetActionRouter().GetSimple().GetInitialSizeClassAnalyzer().GetMaximumExecutionTimeout().AsDuration().Seconds(); got < 3600 {
		t.Errorf("%s: maximumExecutionTimeout %vs < 3600s (R-RE-2)", variant, got)
	}
	if got := cfg.GetPlatformQueueWithNoWorkersTimeout().AsDuration().Seconds(); got != 900 {
		t.Errorf("%s: platformQueueWithNoWorkersTimeout %vs != 900s (R-RE-2)", variant, got)
	}
	if len(cfg.GetPredeclaredPlatformQueues()) == 0 {
		t.Errorf("%s: no predeclaredPlatformQueues for the sample pools (R-RE-2)", variant)
	}
}

// wiring connects a localized configuration to its peers on loopback.
type wiring struct {
	shards    []string // storage shard addresses, by shard key "0", "1", …
	scheduler string   // scheduler client address (frontend's schedulers entry)
	denylist  []byte   // deny-list file content (default: empty)
}

type wired struct {
	config    []byte
	listeners map[string]string // "grpcServers[1]", "clientGrpcServers[0]", … → unix:// address
	probe     string            // a gRPC listener to probe for readiness
	probeTLS  *tls.Config
}

// wire rewrites a rendered configuration for a local boot under root: mount points
// (/cucina/…) become directories under root with test TLS material, JWKS, deny-list and
// the persistent-state directories kubelet would create; gRPC servers listen on UNIX
// sockets under root and the diagnostics server on an ephemeral loopback port; shard and
// scheduler client addresses point at the given peers (or a socket nobody serves); a raw
// block device becomes a 64 MiB file (devices cannot be opened on the dev Mac).
// Everything else is booted exactly as rendered.
func wire(t *testing.T, c bbConfig, root string, pki *bbtest.PKI, server bbtest.KeyPair, peers wiring) wired {
	t.Helper()
	text := importstrRE.ReplaceAllString(c.text, `"@@IMPORTSTR@@$1"`)
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("%s configuration is not JSON apart from importstr: %v", c.component, err)
	}
	out := wired{listeners: map[string]string{}}
	probeHasTLS := false
	// gRPC servers listen on UNIX sockets (no TCP port races between parallel boots);
	// peers and test clients dial unix:// addresses.
	for _, list := range []string{"grpcServers", "clientGrpcServers", "workerGrpcServers", "buildQueueStateGrpcServers"} {
		servers, _ := doc[list].([]any)
		for i, s := range servers {
			srv := s.(map[string]any)
			sock := filepath.Join(root, fmt.Sprintf("%s-%d.sock", list, i))
			delete(srv, "listenAddresses")
			srv["listenPaths"] = []any{sock}
			addr := "unix://" + sock
			out.listeners[fmt.Sprintf("%s[%d]", list, i)] = addr
			_, hasTLS := srv["tls"]
			if out.probe == "" || (probeHasTLS && !hasTLS) {
				out.probe, probeHasTLS = addr, hasTLS
			}
		}
	}
	if out.probe == "" {
		t.Fatalf("%s configuration has no gRPC listener", c.component)
	}
	if probeHasTLS {
		out.probeTLS = pki.ClientTLS(nil, "localhost")
	}
	unreachable := "unix://" + filepath.Join(root, "unreachable.sock")
	var walk func(v any) any
	walk = func(v any) any {
		switch n := v.(type) {
		case map[string]any:
			if sharding, ok := n["sharding"].(map[string]any); ok {
				for k, s := range sharding["shards"].(map[string]any) {
					client := s.(map[string]any)["backend"].(map[string]any)["grpc"].(map[string]any)["client"].(map[string]any)
					client["address"] = unreachable
					var i int
					if _, err := fmt.Sscan(k, &i); err == nil && i < len(peers.shards) {
						client["address"] = peers.shards[i]
					}
				}
			}
			if schedulers, ok := n["schedulers"].(map[string]any); ok {
				for _, s := range schedulers {
					endpoint := s.(map[string]any)["endpoint"].(map[string]any)
					endpoint["address"] = unreachable
					if peers.scheduler != "" {
						endpoint["address"] = peers.scheduler
					}
				}
			}
			if dev, ok := n["devicePath"].(string); ok {
				delete(n, "devicePath")
				n["file"] = map[string]any{"path": "/cucina/dev/" + filepath.Base(dev), "sizeBytes": 64 << 20}
			}
			if addrs, ok := n["listenAddresses"].([]any); ok && len(addrs) > 0 {
				n["listenAddresses"] = []any{"127.0.0.1:0"} // diagnostics HTTP server: any free port
			}
			for k, val := range n {
				n[k] = walk(val)
			}
			return n
		case []any:
			for i := range n {
				n[i] = walk(n[i])
			}
			return n
		case string:
			if p, ok := strings.CutPrefix(n, "@@IMPORTSTR@@/cucina/"); ok {
				return "@@IMPORTSTR@@" + filepath.Join(root, p)
			}
			if p, ok := strings.CutPrefix(n, "/cucina/"); ok {
				return filepath.Join(root, p)
			}
		}
		return v
	}
	walk(doc)

	for _, d := range []string{"ca", "tls/clients", "tls/workers", "tls/server", "jwks", "denylist", "storage", "meta", "dev",
		"state/cas", "state/ac", "state/fsac", "state/iscc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, b []byte) {
		if err := os.WriteFile(filepath.Join(root, rel), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ca/ca.crt", []byte(pki.CAPEM))
	for _, d := range []string{"tls/clients", "tls/workers", "tls/server"} {
		write(d+"/tls.crt", server.CertPEM)
		write(d+"/tls.key", server.KeyPEM)
	}
	write("jwks/jwks.json", charttest.JWKS(t))
	denylist := peers.denylist
	if denylist == nil {
		denylist = []byte(`{"version":1,"sids":[],"subs":[]}`)
	}
	write("denylist/denylist.json", denylist)

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out.config = []byte(regexp.MustCompile(`"@@IMPORTSTR@@([^"]+)"`).ReplaceAllString(string(b), `importstr "$1"`))
	return out
}
