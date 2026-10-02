// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/mgmt"
)

// TestSupportBundleRedactsSecrets guards `cucinactl diag` (R-CLI-3): the bundle is a
// tar.gz with version info, the controller configuration, the Cucina resources,
// state dumps, the controller log tail and a metrics snapshot, and no secret planted
// in any of those sources survives (redaction by construction), while the
// diagnostic content around the secrets does.
func TestSupportBundleRedactsSecrets(t *testing.T) {
	secrets := map[string]string{
		"registry password": "hunter2-registry-password",
		"account id":        "123456789012",
		"url credentials":   "pa55w0rd-in-url",
		"client secret":     "GOCSPX-planted-client-secret",
		"jwt":               "eyJhbGciOiJFUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJzYTpjaSJ9.c2lnLXZhbHVlLXBsYW50ZWQ",
		"service key":       "cuc_sk_k7_cGxhbnRlZC1zZXJ2aWNlLWtleQ",
		"enroll token":      "cuc_et_e1_cGxhbnRlZC1lbnJvbGwtdG9rZW4",
		"private key":       "MHcCAQEEIPlantedPrivateKeyMaterial",
		"aws access key":    "AKIAPLANTEDKEY123456",
		"aws secret":        "wJalrXUtnFEMI/K7MDENG/bPxRfiCYPLANTEDKEY",
		"bearer":            "opaque-session-token-0123456789abcdef",
	}
	ring := mgmt.NewLogRing(1 << 20)
	for _, line := range []string{
		`{"level":"INFO","msg":"exchange ok","subject":"sa:ci","token":"` + secrets["service key"] + `"}`,
		`{"level":"WARN","msg":"bad header","authorization":"Bearer ` + secrets["bearer"] + `"}`,
		"-----BEGIN EC PRIVATE KEY-----\n" + secrets["private key"] + "\n-----END EC PRIVATE KEY-----",
		"host enrolled with " + secrets["enroll token"],
		"aws_secret_access_key=" + secrets["aws secret"],
	} {
		_, _ = ring.Write([]byte(line + "\n"))
	}
	f := newFixture(t, func(d *mgmt.Deps, o *mgmt.Options) {
		o.Config = map[string]any{
			"registry":  map[string]any{"host": "ghcr.io", "password": secrets["registry password"]},
			"aws":       map[string]any{"region": "us-west-1", "accountId": secrets["account id"]},
			"endpoints": map[string]any{"stsUrl": "https://admin:" + secrets["url credentials"] + "@sts.example.com/"},
			"auth":      map[string]any{"tokenTTL": "15m"},
		}
		d.LogTail = ring
		d.Support = fakeSupport{
			policies: []v1alpha1.TrustPolicy{{
				ObjectMeta: metav1.ObjectMeta{Name: "google"},
				Spec: v1alpha1.TrustPolicySpec{Type: "oidc", Login: &v1alpha1.LoginSpec{
					Name: "google", ClientID: "1234.apps.googleusercontent.com", ClientSecret: secrets["client secret"],
				}},
			}},
			metrics: "cucina_sts_exchanges_total{issuer=\"google\",result=\"ok\"} 3\n# leaked " + secrets["aws access key"] + "\n",
		}
	})
	f.fleet.pools[0].Resource.Annotations = map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"clientSecret":"` + secrets["client secret"] + `"}}`,
		"cucina.sloper.ai/note":                            "rotated " + secrets["jwt"],
	}

	st := newStream[cucinav1.LogChunk](as(admin))
	if err := f.srv.CollectSupportBundle(&cucinav1.CollectSupportBundleRequest{IncludeLogs: true}, st); err != nil {
		t.Fatalf("CollectSupportBundle: %v", err)
	}
	var archive bytes.Buffer
	chunks := st.drain()
	for _, c := range chunks {
		archive.Write(c.GetData())
	}
	if len(chunks) == 0 || !chunks[len(chunks)-1].GetLast() {
		t.Fatal("bundle stream does not end with a last chunk")
	}
	files := untar(t, archive.Bytes())

	var got []string
	for name := range files {
		got = append(got, name)
	}
	for _, want := range []string{
		"version.json", "config/controller.json", "resources/workerpools.json", "resources/machosts.json",
		"resources/trustpolicies.json", "state/status.json", "state/pools.json", "state/workers.json", "state/hosts.json",
		"state/queues.json", "state/operations.json", "logs/controller.log", "metrics/controller.prom",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("bundle lacks %s (has %v)", want, got)
		}
	}
	for name, data := range files {
		for what, secret := range secrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s contains the %s", name, what)
			}
		}
		if strings.HasSuffix(name, ".json") && !json.Valid(data) {
			t.Errorf("%s is not valid JSON", name)
		}
	}
	for name, want := range map[string]string{
		"config/controller.json":       "us-west-1",
		"resources/workerpools.json":   "linux-x86-64",
		"resources/trustpolicies.json": "1234.apps.googleusercontent.com",
		"logs/controller.log":          "exchange ok",
		"metrics/controller.prom":      "cucina_sts_exchanges_total",
		"state/workers.json":           "i-0aaaaaaaaaaaaaaa1",
	} {
		if !bytes.Contains(files[name], []byte(want)) {
			t.Errorf("%s lost its diagnostic content %q", name, want)
		}
	}
}

func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("bad tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimPrefix(h.Name, "cucina-support/")] = data
	}
}
