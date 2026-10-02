// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"strings"
	"testing"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/mgmt"
)

// TestMethodAccessCoversEveryRPC guards R-AUTH-11: every RPC of ManagementService has
// exactly one access rule and the table names no RPC that does not exist, so a new
// RPC cannot ship without authentication or auditing (mgmt.New also refuses to start
// with an incomplete table). RPCs that change state by their name must be Mutate.
func TestMethodAccessCoversEveryRPC(t *testing.T) {
	access := mgmt.MethodAccess()
	desc := cucinav1.ManagementService_ServiceDesc
	var names []string
	for _, m := range desc.Methods {
		names = append(names, m.MethodName)
	}
	for _, s := range desc.Streams {
		names = append(names, s.StreamName)
	}
	seen := map[string]bool{}
	for _, n := range names {
		full := "/" + desc.ServiceName + "/" + n
		seen[full] = true
		a, ok := access[full]
		if !ok {
			t.Errorf("%s has no access rule", full)
			continue
		}
		reads := n == "HostDiagnostics"
		for _, p := range []string{"Get", "List", "Watch", "Stream", "Collect"} {
			reads = reads || strings.HasPrefix(n, p)
		}
		if !reads && a != mgmt.Mutate {
			t.Errorf("%s changes state but has access %v", n, a)
		}
		if reads && a == mgmt.Mutate {
			t.Errorf("%s only reads but is classified as a mutation", n)
		}
	}
	for m := range access {
		if !seen[m] {
			t.Errorf("access table names unknown RPC %s", m)
		}
	}
}
