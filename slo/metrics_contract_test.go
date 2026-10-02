// SPDX-License-Identifier: FSL-1.1-ALv2

package slo_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/slo"
)

// Guards the actual T1 no-data failure (R-DATA-7/R-TEST-8f): execute OUR
// query/rule definitions against pinned producer labels, including misleading
// uppercase and ephemeral-cache samples. Missing L3 stays empty, not zero.
func TestPinnedBlobAndRetentionQueries(t *testing.T) {
	tool := os.Getenv("SLO_PROMTOOL")
	if tool == "" {
		var err error
		tool, err = exec.LookPath("promtool")
		if err != nil {
			if os.Getenv("TEST_SRCDIR") != "" {
				t.Fatal("Bazel must provide pinned SLO_PROMTOOL")
			}
			t.Skip("promtool required; run //slo:metrics_contract_test or set SLO_PROMTOOL")
		}
	}
	tool, err := filepath.Abs(tool)
	require.NoError(t, err)
	dir := t.TempDir()
	rules, err := slo.RulesFile()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rules.json"), rules, 0o600))
	type series struct {
		Series string `json:"series"`
		Values string `json:"values"`
	}
	type sample struct {
		Labels string  `json:"labels"`
		Value  float64 `json:"value"`
	}
	type assertion struct {
		Expr    string   `json:"expr"`
		Eval    string   `json:"eval_time"`
		Samples []sample `json:"exp_samples"`
	}
	type testCase struct {
		Name     string      `json:"name"`
		Interval string      `json:"interval"`
		Input    []series    `json:"input_series"`
		Tests    []assertion `json:"promql_expr_test"`
	}
	value := func(q, at string, v float64) assertion {
		return assertion{Expr: q, Eval: at, Samples: []sample{{Labels: "{}", Value: v}}}
	}
	empty := func(q, at string) assertion { return assertion{Expr: q, Eval: at, Samples: []sample{}} }
	bytesMetric := "buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum"
	blob := func(labels, values string) series {
		return series{Series: bytesMetric + "{" + labels + "}", Values: values}
	}
	retMetric := "buildbarn_blobstore_old_current_new_location_blob_map_last_removed_old_block_insertion_time_seconds"
	ret := func(labels, values string) series {
		return series{Series: retMetric + "{" + labels + "}", Values: values}
	}
	threshold := slo.RetentionAboveBazelTTL.Query + " >= " + strconv.FormatFloat(slo.RetentionAboveBazelTTL.Value, 'f', 0, 64)
	l1 := ret(`namespace="contract",cucina_component="worker",storage_type="cas"`, `_ _ _ _ _ 17999`)
	l2 := ret(`namespace="contract",cucina_component="host-l2",storage_type="cas"`, `_ _ _ _ _ 17990`)
	uppercase := ret(`namespace="contract",pod="wrong-case",cucina_component="storage",storage_type="CAS"`, `_ _ _ _ _ 17999`)
	tests := []testCase{
		{Name: "CAS bytes use cas and actual local backend names", Interval: "1m", Input: []series{
			blob(`job="contract-worker",storage_type="cas",backend_type="grpc",operation="Get"`, `0+180x5`),
			blob(`job="contract-worker",storage_type="cas",backend_type="grpc",operation="Put"`, `0+240x5`),
			blob(`job="contract-worker",storage_type="cas",backend_type="local_block_device",operation="Get"`, `0+60x5`),
			blob(`job="contract-worker",storage_type="cas",backend_type="local_in_memory",operation="Get"`, `0+120x5`),
			blob(`job="contract-worker",storage_type="CAS",backend_type="grpc",operation="Get"`, `0+60000x5`),
			blob(`job="contract-worker",storage_type="ac",backend_type="grpc",operation="Get"`, `0+60000x5`),
			blob(`job="contract-worker",storage_type="cas",backend_type="grpc",operation="GetFromComposite"`, `0+60000x5`),
		}, Tests: []assertion{
			value(slo.CASBytesIncrease(`job="contract-worker"`, "grpc", "Get", 5*time.Minute), "5m", 900),
			value(slo.CASBytesIncrease(`job="contract-worker"`, "local", "Get", 5*time.Minute), "5m", 900),
			value(slo.CASBytesIncrease(`job="contract-worker"`, "grpc", "Put", 5*time.Minute), "5m", 1200),
			value(slo.CASBytesRate("Get"), "5m", 6), value(slo.CASBytesRate("Put"), "5m", 4),
		}},
		{Name: "only authoritative lowercase L3 controls retention", Interval: "1h", Input: []series{
			ret(`namespace="contract",pod="l3-a",cucina_component="storage",storage_type="cas"`, `0+0x5`),
			ret(`namespace="contract",pod="l3-b",cucina_component="storage",storage_type="cas"`, `_ 3600+0x4`), l1, l2, uppercase,
		}, Tests: []assertion{value(slo.RetentionAboveBazelTTL.Query, "5h", 14400), value(threshold, "5h", 14400)}},
		{Name: "L1 L2 and wrong-case L3 cannot replace absent L3", Interval: "1h", Input: []series{l1, l2, uppercase}, Tests: []assertion{empty(slo.RetentionAboveBazelTTL.Query, "5h"), empty(threshold, "5h")}},
		{Name: "genuine missing L3 remains no data", Interval: "1h", Input: []series{}, Tests: []assertion{empty(slo.RetentionAboveBazelTTL.Query, "5h"), empty(threshold, "5h")}},
		{Name: "fresh map age is finite and does not satisfy TTL guard", Interval: "1m", Input: []series{
			ret(`namespace="contract",pod="fresh-l3",cucina_component="storage",storage_type="cas"`, `0+0x5`),
		}, Tests: []assertion{value(slo.RetentionAboveBazelTTL.Query, "5m", 300), empty(threshold, "5m")}},
	}
	fixture := struct {
		Files    []string   `json:"rule_files"`
		Interval string     `json:"evaluation_interval"`
		Tests    []testCase `json:"tests"`
	}{[]string{"rules.json"}, "30s", tests}
	b, err := json.Marshal(fixture)
	require.NoError(t, err)
	path := filepath.Join(dir, "tests.json")
	require.NoError(t, os.WriteFile(path, b, 0o600))
	cmd := exec.Command(tool, "test", "rules", path)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
