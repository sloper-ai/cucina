// SPDX-License-Identifier: FSL-1.1-ALv2

package hostos_test

import (
	"context"
	"testing"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
	"github.com/sloper-ai/cucina/internal/workeragent/hostos"
)

// Guards: R-TEST-8b — the agent's production FS adapter honours the ports.FS
// contract the fakes are held to (atomic writes, NotExist errors, listing).
func TestFSConformance(t *testing.T) {
	porttest.RunFS(t, func(t *testing.T) (ports.FS, string) { return hostos.FS{}, t.TempDir() })
}

// Guards: R-TEST-8b — the system clock adapter honours ports.Clock (checked
// inside a testing/synctest bubble, so no real waiting).
func TestClockConformance(t *testing.T) {
	porttest.RunClock(t, true, func(*testing.T) porttest.ClockHarness {
		c := hostos.Clock{}
		// Inside the synctest bubble the timer behind Sleep is virtual.
		return porttest.ClockHarness{Clock: c, Advance: func(d time.Duration) { _ = c.Sleep(context.Background(), d) }}
	})
}
