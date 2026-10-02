// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig_test

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_runner"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/sloper-ai/cucina/internal/bbconfig/bbconfigtest"
)

// -update rewrites testdata/ for `go test` users; under Bazel the goldens are
// write_source_files targets (`bazel run //:update_goldens`, diff test
// //internal/bbconfig:goldens_*_test). CI never writes goldens (R-TEST-8e), and
// changing one needs a Test-Change trailer.
var update = flag.Bool("update", false, "regenerate golden files")

// TestGoldens guards the rendered worker, runner and host L2 configurations of
// every shipped profile against unintended change, and round-trips each worker
// and runner configuration through the pinned Buildbarn Go types with strict
// protojson (R-CP-2, R-TEST-6).
func TestGoldens(t *testing.T) {
	goldens, err := bbconfigtest.Goldens()
	require.NoError(t, err)
	var names []string
	for name, got := range goldens {
		names = append(names, name)
		t.Run(name, func(t *testing.T) {
			switch {
			case strings.HasPrefix(name, "worker_"):
				roundTrip(t, got, &bb_worker.ApplicationConfiguration{})
			case strings.HasPrefix(name, "runner_"):
				roundTrip(t, got, &bb_runner.ApplicationConfiguration{})
			}
			golden(t, name, got)
		})
	}
	slices.Sort(names)
	assert.Equal(t, bbconfigtest.GoldenFiles, names, "keep bbconfigtest.GoldenFiles and BUILD.bazel's genrule in sync")
}

// roundTrip parses b strictly into msg (unknown fields fail, like Buildbarn's
// own loader) and checks that re-marshalling loses nothing.
func roundTrip(t *testing.T, b []byte, msg proto.Message) {
	t.Helper()
	require.NoError(t, protojson.Unmarshal(b, msg))
	again, err := protojson.Marshal(msg)
	require.NoError(t, err)
	fresh := msg.ProtoReflect().New().Interface()
	require.NoError(t, protojson.Unmarshal(again, fresh))
	assert.True(t, proto.Equal(msg, fresh), "protojson round trip changed the configuration")
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	if os.Getenv("TEST_SRCDIR") != "" && !*update {
		return // under Bazel, //internal/bbconfig:goldens' diff tests own drift detection
	}
	path := filepath.Join("testdata", name)
	if *update {
		if os.Getenv("CI") != "" {
			t.Fatal("refusing to regenerate goldens in CI")
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden; run: go test ./internal/bbconfig -run TestGoldens -update")
	assert.Equal(t, string(want), string(got), "golden %s differs; inspect the diff, then rerun with -update", name)
}
