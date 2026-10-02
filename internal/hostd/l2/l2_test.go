// SPDX-License-Identifier: FSL-1.1-ALv2

package l2_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/hostd/l2"
)

// TestParseMetrics guards R-OBS-1 / R-DATA-7 "hostd: L2 hit ratio, WAN bytes":
// the counters are derived from bb_storage's blob access metrics (CAS only):
// requests = read_caching Gets, misses = upstream (grpc) Gets, WAN bytes =
// grpc Get/Put blob sizes.
func TestParseMetrics(t *testing.T) {
	text := `# TYPE buildbarn_blob_access_operations_blob_size_bytes histogram
buildbarn_blob_access_operations_blob_size_bytes_sum{backend_type="read_caching",operation="Get",storage_type="CAS"} 9.5e+06
buildbarn_blob_access_operations_blob_size_bytes_count{backend_type="read_caching",operation="Get",storage_type="CAS"} 1000
buildbarn_blob_access_operations_blob_size_bytes_sum{backend_type="grpc",operation="Get",storage_type="CAS"} 2.5e+06
buildbarn_blob_access_operations_blob_size_bytes_count{backend_type="grpc",operation="Get",storage_type="CAS"} 250
buildbarn_blob_access_operations_blob_size_bytes_sum{backend_type="grpc",operation="Put",storage_type="CAS"} 4096
buildbarn_blob_access_operations_blob_size_bytes_count{backend_type="grpc",operation="Put",storage_type="CAS"} 2
buildbarn_blob_access_operations_blob_size_bytes_sum{backend_type="grpc",operation="Get",storage_type="AC"} 777
buildbarn_blob_access_operations_blob_size_bytes_count{backend_type="grpc",operation="Get",storage_type="AC"} 7
`
	st := l2.ParseMetrics(strings.NewReader(text))
	require.Equal(t, l2.Stats{Hits: 750, Misses: 250, WANReceived: 2_500_000, WANSent: 4096}, st)
}
