// SPDX-License-Identifier: FSL-1.1-ALv2

package static_test

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/pki"
)

// TestRenderedAuthMatchesGoContracts keeps the Buildbarn authentication and
// authorization the chart renders identical to the expressions the auth and PKI
// packages specify and test against real tokens and certificates (docs/security.md,
// R-AUTH-4, R-AUTH-9, R-SEC-2, R-SEC-4): any drift on either side fails here.
func TestRenderedAuthMatchesGoContracts(t *testing.T) {
	const issuer = "https://sts.example.com:8443"
	manifests := charttest.Template(t, []string{filepath.Join(charttest.ChartDir(t), "ci", "values-small.yaml")}, "endpoints.sts.url="+issuer)
	configs := map[string]map[string]any{}
	for _, o := range charttest.Objects(t, manifests) {
		for _, c := range []string{"frontend", "scheduler"} {
			if text, ok := o.Data[c+".jsonnet"]; ok {
				var doc map[string]any
				jsonText := regexp.MustCompile(`importstr "[^"]+"`).ReplaceAllString(text, `"CA"`)
				if err := json.Unmarshal([]byte(jsonText), &doc); err != nil {
					t.Fatal(err)
				}
				configs[c] = doc
			}
		}
	}
	instanceNames := []string{"main"}
	deny := keys.BuildbarnDenyFragment
	cases := []struct {
		name, component string
		path            []any
		want            string
	}{
		{"frontend JWT claims", "frontend", []any{"grpcServers", 0, "authenticationPolicy", "jwt", "claimsValidationJmespathExpression", "expression"}, keys.BuildbarnClaimsValidation(issuer)},
		{"frontend JWT metadata", "frontend", []any{"grpcServers", 0, "authenticationPolicy", "jwt", "metadataExtractionJmespathExpression", "expression"}, keys.BuildbarnMetadataExtraction},
		{"frontend worker listener validation", "frontend", []any{"grpcServers", 1, "authenticationPolicy", "tlsClientCertificate", "validationJmespathExpression", "expression"}, pki.BuildbarnWorkerListenerValidation},
		{"frontend worker listener metadata", "frontend", []any{"grpcServers", 1, "authenticationPolicy", "tlsClientCertificate", "metadataExtractionJmespathExpression", "expression"}, pki.BuildbarnWorkerListenerMetadata(instanceNames)},
		{"CAS get", "frontend", []any{"contentAddressableStorage", "getAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("cas_read")},
		{"CAS findMissing", "frontend", []any{"contentAddressableStorage", "findMissingAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("cas_read")},
		{"CAS put", "frontend", []any{"contentAddressableStorage", "putAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("cas_write")},
		{"AC get", "frontend", []any{"actionCache", "getAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("ac_read")},
		{"AC put", "frontend", []any{"actionCache", "putAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("ac_write")},
		{"frontend execute", "frontend", []any{"executeAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("execute")},
		{"scheduler JWT claims", "scheduler", []any{"clientGrpcServers", 0, "authenticationPolicy", "jwt", "claimsValidationJmespathExpression", "expression"}, keys.BuildbarnClaimsValidation(issuer)},
		{"scheduler JWT metadata", "scheduler", []any{"clientGrpcServers", 0, "authenticationPolicy", "jwt", "metadataExtractionJmespathExpression", "expression"}, keys.BuildbarnMetadataExtraction},
		{"scheduler execute", "scheduler", []any{"executeAuthorizer", "jmespathExpression", "expression"}, keys.BuildbarnAuthorizer("execute")},
		{"scheduler worker validation", "scheduler", []any{"workerGrpcServers", 0, "authenticationPolicy", "tlsClientCertificate", "validationJmespathExpression", "expression"}, pki.BuildbarnSchedulerWorkerValidation},
		{"scheduler worker metadata", "scheduler", []any{"workerGrpcServers", 0, "authenticationPolicy", "tlsClientCertificate", "metadataExtractionJmespathExpression", "expression"}, pki.BuildbarnSubjectMetadata},
		{"synchronize", "scheduler", []any{"synchronizeAuthorizer", "jmespathExpression", "expression"}, pki.BuildbarnSynchronizeAuthorizer + " && " + deny},
		{"BuildQueueState validation", "scheduler", []any{"buildQueueStateGrpcServers", 0, "authenticationPolicy", "tlsClientCertificate", "validationJmespathExpression", "expression"}, pki.BuildbarnBuildQueueStateValidation},
		{"BuildQueueState metadata", "scheduler", []any{"buildQueueStateGrpcServers", 0, "authenticationPolicy", "tlsClientCertificate", "metadataExtractionJmespathExpression", "expression"}, pki.BuildbarnSubjectMetadata},
		{"modifyDrains", "scheduler", []any{"modifyDrainsAuthorizer", "jmespathExpression", "expression"}, pki.BuildbarnBuildQueueStateAuthorizer + " && " + deny},
		{"killOperations", "scheduler", []any{"killOperationsAuthorizer", "jmespathExpression", "expression"}, pki.BuildbarnBuildQueueStateAuthorizer + " && " + deny},
	}
	for _, c := range cases {
		got, err := lookup(configs[c.component], c.path)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	// Every authorizer reads the deny-list file under the key the auth package writes.
	for _, path := range [][]any{
		{"contentAddressableStorage", "getAuthorizer", "jmespathExpression", "files", 0, "key"},
		{"actionCache", "putAuthorizer", "jmespathExpression", "files", 0, "key"},
	} {
		if got, err := lookup(configs["frontend"], path); err != nil || got != keys.DenyListFileKey {
			t.Errorf("deny-list file key at %v: %q, %v (want %q)", path, got, err, keys.DenyListFileKey)
		}
	}
}

func lookup(v any, path []any) (string, error) {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return "", &pathError{path}
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				return "", &pathError{path}
			}
			v = l[k]
		}
	}
	s, ok := v.(string)
	if !ok {
		return "", &pathError{path}
	}
	return s, nil
}

type pathError struct{ path []any }

func (e *pathError) Error() string {
	b, _ := json.Marshal(e.path)
	return "no string at " + string(b)
}
