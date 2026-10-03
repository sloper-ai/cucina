// SPDX-License-Identifier: FSL-1.1-ALv2

package boot_test

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

// renderManifest always uses the explicitly selected, pinned Helm major.
func renderManifest(t testing.TB, helm string, valuesFiles []string, sets ...string) []byte {
	t.Helper()
	out, err := charttest.HelmWithBinary(t, helm, charttest.TemplateArgs(t, valuesFiles, sets...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func renderedValue(t testing.TB, value any, path ...any) any {
	t.Helper()
	for _, key := range path {
		switch key := key.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("rendered path %v is not an object at %q", path, key)
			}
			value, ok = object[key]
			if !ok {
				t.Fatalf("rendered path %v is missing %q", path, key)
			}
		case int:
			list, ok := value.([]any)
			if !ok || key >= len(list) {
				t.Fatalf("rendered path %v is missing index %d", path, key)
			}
			value = list[key]
		}
	}
	return value
}

// Guards R-CP-1/-2: the public, unlocalized chart must declare the two small-profile
// shards consistently in Kubernetes and every frontend/scheduler backing store.
func assertSmallTopology(t testing.TB, manifests []byte) {
	t.Helper()
	wantReplicas := map[string]float64{"frontend": 1, "controller": 1, "scheduler": 1, "storage": 2, "sts": 2}
	for _, object := range charttest.Objects(t, manifests) {
		if object.Kind != "Deployment" && object.Kind != "StatefulSet" {
			continue
		}
		component := strings.TrimPrefix(object.Metadata.Name, "cucina-")
		if want, ok := wantReplicas[component]; ok {
			if got := object.Spec["replicas"]; got != want {
				t.Fatalf("%s replicas: got %v, want %.0f before localization", component, got, want)
			}
			delete(wantReplicas, component)
		}
	}
	if len(wantReplicas) != 0 {
		t.Fatalf("missing rendered workloads: %v", charttest.SortedKeys(wantReplicas))
	}
	configs := configsOf(t, manifests)
	for _, route := range []struct {
		component string
		path      []any
	}{
		{"frontend", []any{"contentAddressableStorage", "backend", "existenceCaching", "backend", "sharding", "shards"}},
		{"frontend", []any{"actionCache", "backend", "completenessChecking", "backend", "sharding", "shards"}},
		{"frontend", []any{"fileSystemAccessCache", "backend", "sharding", "shards"}},
		{"scheduler", []any{"contentAddressableStorage", "sharding", "shards"}},
	} {
		var config map[string]any
		if err := json.Unmarshal([]byte(importstrRE.ReplaceAllString(configs[route.component].text, `"private CA import"`)), &config); err != nil {
			t.Fatal(err)
		}
		shards, ok := renderedValue(t, config, route.path...).(map[string]any)
		if !ok || len(shards) != 2 {
			t.Fatalf("%s %v: want two original shard entries, got %d", route.component, route.path, len(shards))
		}
		for i := range 2 {
			key := fmt.Sprint(i)
			if got := renderedValue(t, shards, key, "weight"); got != float64(1) {
				t.Fatalf("%s shard %s weight: got %v, want 1", route.component, key, got)
			}
			want := fmt.Sprintf("cucina-storage-%d.cucina-storage.cucina.svc.cluster.local:8981", i)
			if got := renderedValue(t, shards, key, "backend", "grpc", "client", "address"); got != want {
				t.Fatalf("%s shard %s does not address its rendered StatefulSet pod", route.component, key)
			}
		}
	}
}

// The effective-map row guards recursive null removal, zero/list/map values and
// inherited resource siblings. costEnabled=false is separately a direct-.Values
// chart nonregression, not evidence about the profile-merge helper.
func assertProfileOverrides(t testing.TB, helm, kindValues string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "overrides.json")
	data := `{"controller":{"costEnabled":false},"frontend":{"replicas":null,"resources":{"requests":{"cpu":0},"limits":{"memory":"3Gi"}},"nodeSelector":{"fixture":"retained"},"tolerations":[{"key":"fixture","operator":"Exists","tolerationSeconds":0}],"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":null},"podAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[]}}}}`
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	objects := charttest.Objects(t, renderManifest(t, helm, []string{kindValues, file}))
	seenFrontend, seenController := false, false
	for _, object := range objects {
		if object.Kind == "Deployment" && object.Metadata.Name == "cucina-frontend" {
			seenFrontend = true
			for _, check := range []struct {
				path []any
				want any
			}{
				{[]any{"template", "spec", "containers", 0, "resources", "requests", "cpu"}, float64(0)},
				{[]any{"template", "spec", "containers", 0, "resources", "requests", "memory"}, "256Mi"},
				{[]any{"template", "spec", "containers", 0, "resources", "limits", "cpu"}, "2"},
				{[]any{"template", "spec", "containers", 0, "resources", "limits", "memory"}, "3Gi"},
				{[]any{"template", "spec", "nodeSelector"}, map[string]any{"fixture": "retained"}},
				{[]any{"template", "spec", "tolerations", 0, "tolerationSeconds"}, float64(0)},
				{[]any{"template", "spec", "affinity", "nodeAffinity"}, map[string]any{}},
				{[]any{"template", "spec", "affinity", "podAffinity", "requiredDuringSchedulingIgnoredDuringExecution"}, []any{}},
			} {
				if got := renderedValue(t, object.Spec, check.path...); !reflect.DeepEqual(got, check.want) {
					t.Fatalf("component override %v: got %v, want %v", check.path, got, check.want)
				}
			}
		}
		if text, ok := object.Data["controller.json"]; ok {
			seenController = true
			var config map[string]any
			if err := json.Unmarshal([]byte(text), &config); err != nil {
				t.Fatal(err)
			}
			if got := renderedValue(t, config, "observability", "costEnabled"); got != false {
				t.Fatalf("explicit false override: got %v", got)
			}
		}
	}
	if !seenFrontend || !seenController {
		t.Fatal("missing public override consumers")
	}
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
			// A drive-qualified Windows path is an opaque UNIX endpoint, not a
			// URI authority. "unix://C:\\..." fails URL parsing and gRPC falls
			// back to DNS, so every readiness probe used to time out on Windows.
			addr := (&url.URL{Scheme: "unix", Opaque: sock}).String()
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
	unreachable := (&url.URL{Scheme: "unix", Opaque: filepath.Join(root, "unreachable.sock")}).String()
	backingFiles := map[string]struct{}{}
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
			if file, ok := n["file"].(map[string]any); ok && file["sizeBytes"] != nil {
				if p, ok := file["path"].(string); ok {
					backingFiles[p] = struct{}{}
				}
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
	for p := range backingFiles {
		prepareSparseBackingFile(t, p)
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
