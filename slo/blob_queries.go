// SPDX-License-Identifier: FSL-1.1-ALv2

package slo

import (
	"fmt"
	"strings"
	"time"
)

// CASBytesIncrease is the shared acceptance query for logical CAS blob bytes.
// Selectors are the caller's job/pool matchers. backend is "grpc", "local"
// (both supported local implementations), or empty for all instrumented
// layers. The all-layer sum is diagnostic, not network/compressed byte usage.
func CASBytesIncrease(selectors, backend, operation string, window time.Duration) string {
	parts := []string{}
	if selectors = strings.Trim(strings.TrimSpace(selectors), ","); selectors != "" {
		parts = append(parts, selectors)
	}
	parts = append(parts, `storage_type="cas"`)
	if backend == "local" {
		parts = append(parts, `backend_type=~"local_block_device|local_in_memory"`)
	} else if backend != "" {
		parts = append(parts, fmt.Sprintf("backend_type=%q", backend))
	}
	parts = append(parts, fmt.Sprintf("operation=%q", operation))
	return fmt.Sprintf("sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s}[%ds]))", strings.Join(parts, ","), max(1, int(window/time.Second)))
}

// CASBytesRate selects the storage/operation dimensions of the shared byte
// recording rule. Like CASBytesIncrease, this is logical blob observation.
func CASBytesRate(operation string) string {
	return fmt.Sprintf(`sum(%s{storage_type="cas",operation=%q})`, BlobBytesRate, operation)
}
