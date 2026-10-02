// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/workeragent"
)

// Guards: R-POOL-7 idleness is measured from bb_worker's own metrics: an
// executing action holds file-pool files open (created > closed) and every
// completed action increments the build executor histogram.
func TestParseActivity(t *testing.T) {
	const idle = `# HELP buildbarn_filesystem_file_pool_files_created_total Number of times a file was created.
# TYPE buildbarn_filesystem_file_pool_files_created_total counter
buildbarn_filesystem_file_pool_files_created_total 1204
# HELP buildbarn_filesystem_file_pool_files_closed_total Number of times a file was closed.
# TYPE buildbarn_filesystem_file_pool_files_closed_total counter
buildbarn_filesystem_file_pool_files_closed_total 1204
# HELP buildbarn_builder_build_executor_duration_seconds Amount of time spent per build execution stage, in seconds.
# TYPE buildbarn_builder_build_executor_duration_seconds histogram
buildbarn_builder_build_executor_duration_seconds_bucket{grpc_code="OK",result="Success",stage="Running",le="+Inf"} 300
buildbarn_builder_build_executor_duration_seconds_sum{grpc_code="OK",result="Success",stage="Running"} 512.5
buildbarn_builder_build_executor_duration_seconds_count{grpc_code="OK",result="Success",stage="Running"} 300
buildbarn_builder_build_executor_duration_seconds_bucket{grpc_code="OK",result="Success",stage="UploadingOutputs",le="+Inf"} 300
buildbarn_builder_build_executor_duration_seconds_sum{grpc_code="OK",result="Success",stage="UploadingOutputs"} 12.5
buildbarn_builder_build_executor_duration_seconds_count{grpc_code="OK",result="Success",stage="UploadingOutputs"} 300
# TYPE go_goroutines gauge
go_goroutines 87
`
	a, err := workeragent.ParseActivity(strings.NewReader(idle))
	require.NoError(t, err)
	require.Equal(t, workeragent.Activity{Busy: false, Completed: 600}, a)

	busy := strings.Replace(idle, "files_created_total 1204", "files_created_total 1206", 1)
	a, err = workeragent.ParseActivity(strings.NewReader(busy))
	require.NoError(t, err)
	require.True(t, a.Busy, "two pool files (stdout, stderr) open while an action runs")

	a, err = workeragent.ParseActivity(strings.NewReader("# freshly started bb_worker\n"))
	require.NoError(t, err)
	require.Equal(t, workeragent.Activity{}, a)

	_, err = workeragent.ParseActivity(strings.NewReader("<html>not metrics</html>"))
	require.Error(t, err)
}
