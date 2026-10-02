// SPDX-License-Identifier: FSL-1.1-ALv2

package profile

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fixture is a real Bazel 9.2.0 --profile trace of a genrule + sh_test
// build, reduced to the critical path, action, phase and counter events (and
// the output base path scrubbed). Guards §10.4 "the --profile trace,
// analyzed … (bazel analyze-profile was removed in Bazel 9)".
func TestAnalyzeBazel92Profile(t *testing.T) {
	s, err := ReadFile(filepath.Join("testdata", "local-cold.json"), 2)
	require.NoError(t, err)

	require.Equal(t, "9.2.0", s.BazelVersion)
	require.Equal(t, []Component{
		{Name: "action 'Executing genrule //:gen'", Duration: 274567 * time.Microsecond},
		{Name: "runfiles for //:t", Duration: 1914 * time.Microsecond},
		{Name: "action 'Testing //:t'", Duration: 357014 * time.Microsecond},
	}, s.CriticalPath)
	require.Equal(t, 633495*time.Microsecond, s.CriticalPathTotal, "matches BEP critical_path_time 0.633496418s")

	require.Len(t, s.TopActions, 2)
	require.Equal(t, Action{Name: "Testing //:t", Mnemonic: "TestRunner", Duration: 355985 * time.Microsecond}, s.TopActions[0])
	require.Equal(t, "Genrule", s.TopActions[1].Mnemonic)
	require.Equal(t, 8, sumCounts(s.Mnemonics))

	require.Equal(t, []Component{
		{Name: "Initialize command", Duration: 4131779 * time.Microsecond},
		{Name: "Evaluate target patterns", Duration: 603849 * time.Microsecond},
		{Name: "Load, analyze dependencies and build artifacts", Duration: 4790136 * time.Microsecond},
	}, s.Phases)

	require.InDelta(t, 216.355, s.PeakBazelMemoryMB, 1e-9)
	// Up: 4.0518 Mbps and 3.6398 Mbps over two 4 s intervals; down: 116.57 Mbps over 4 s.
	require.InDelta(t, 3845808.46, s.NetworkUpBytes, 1)
	require.InDelta(t, 58284047.41, s.NetworkDownBytes, 1)
}

func sumCounts(m map[string]MnemonicStat) int {
	n := 0
	for _, v := range m {
		n += v.Count
	}
	return n
}
