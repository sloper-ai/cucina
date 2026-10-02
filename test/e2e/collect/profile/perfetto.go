// SPDX-License-Identifier: FSL-1.1-ALv2

package profile

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// PerfettoSQL is the query set run through trace_processor when it is
// available. It mirrors the Go summariser: the critical path, the slowest
// actions and per-mnemonic totals.
const PerfettoSQL = `
SELECT 'critical_path' AS kind, name, dur / 1e6 AS ms FROM slice
  WHERE category = 'critical path component' ORDER BY ts;
SELECT 'top_action' AS kind, name, dur / 1e6 AS ms,
       EXTRACT_ARG(arg_set_id, 'args.mnemonic') AS mnemonic FROM slice
  WHERE category = 'action processing' ORDER BY dur DESC LIMIT 25;
SELECT 'mnemonic' AS kind, EXTRACT_ARG(arg_set_id, 'args.mnemonic') AS mnemonic,
       COUNT(*) AS n, SUM(dur) / 1e6 AS total_ms, MAX(dur) / 1e6 AS max_ms FROM slice
  WHERE category = 'action processing' GROUP BY mnemonic ORDER BY total_ms DESC;
`

// FindTraceProcessor returns the trace_processor binary from
// $PERFETTO_TRACE_PROCESSOR or PATH ("trace_processor_shell",
// "trace_processor"), or "" if none is installed.
func FindTraceProcessor() string {
	if p := os.Getenv("PERFETTO_TRACE_PROCESSOR"); p != "" {
		return p
	}
	for _, n := range []string{"trace_processor_shell", "trace_processor"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// TraceProcessor runs PerfettoSQL against a trace with the given binary and
// returns its textual output (recorded verbatim in the scenario result).
func TraceProcessor(ctx context.Context, bin, trace, scratchDir string) (string, error) {
	q := filepath.Join(scratchDir, "perfetto-queries.sql")
	if err := os.WriteFile(q, []byte(PerfettoSQL), 0o600); err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-q", q, trace)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("trace_processor: %w: %s", err, tail(errb.String(), 2000))
	}
	return out.String(), nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
