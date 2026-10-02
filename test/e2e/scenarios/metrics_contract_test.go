// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/slo"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/scenarios"
)

// Guards R-TEST-8f/T1: a missing L3 series, a query error or insufficient
// observed retention cannot pass. Rule-label selection is executed separately
// by //slo:metrics_contract_test; this tests the public acceptance oracle.
func TestRetentionOracleRequiresMeasuredL3(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		queryError  bool
		pass        bool
	}{
		{"measured L3 at the unchanged four-hour margin", "14400", false, true},
		{"three hours lacks the required margin", "10800", false, false},
		{"fresh L3 cannot prove retention yet", "300", false, false},
		{"absent L3 is not zero or infinity", "", false, false},
		{"query failure is not retention evidence", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var v string
				switch r.URL.Query().Get("query") {
				case `max(up{job=~".*controller.*"})`:
					v = "1" // Independent, healthy controller-scrape control.
				case slo.RetentionAboveBazelTTL.Query:
					if tc.queryError {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = fmt.Fprint(w, `{"status":"error","errorType":"unavailable","error":"synthetic query failure"}`)
						return
					}
					v = tc.value
				default:
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"unknown fixture query"}`)
					return
				}
				if v == "" {
					_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[18000,%q]}]}}`, v)
			}))
			defer server.Close()
			e := &harness.Env{Endpoints: harness.Endpoints{Prometheus: server.URL}}
			c := &harness.Context{Context: context.Background(), Env: e, Result: &harness.Result{}, Services: &infra.Services{Env: e}}
			check := scenarios.PromCheck("L3 retention", slo.RetentionAboveBazelTTL)
			result := check.Evaluate(context.Background(), c)
			require.Equal(t, tc.pass, result.Pass)
			require.Empty(t, result.Skipped)
			if tc.value == "" {
				require.Empty(t, result.Value, "missing evidence must not become a measured zero")
				if tc.queryError {
					require.Contains(t, result.Detail, "query failed:")
				} else {
					require.Equal(t, "required Prometheus query returned no data", result.Detail)
				}
			} else {
				require.Equal(t, tc.value, result.Value)
			}
		})
	}
}
