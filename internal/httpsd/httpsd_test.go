// SPDX-License-Identifier: FSL-1.1-ALv2

package httpsd_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/httpsd"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Guards R-OBS-1 (EC2 workers are discovered through Prometheus HTTP SD with
// pool/node/generation/instance_type labels) and R-SCALE-6 (the endpoint does
// not call EC2 per scrape).
func TestWorkersEndpoint(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	calls := 0
	fleet := []ports.Instance{
		{ID: "i-b", Pool: "linux", Generation: "v2", State: ports.InstanceRunning, Type: "c8i.8xlarge", PrivateIP: netip.MustParseAddr("10.0.1.12")},
		{ID: "i-a", Pool: "linux", Generation: "v1", State: ports.InstanceRunning, Type: "c7i.8xlarge", PrivateIP: netip.MustParseAddr("10.0.1.11")},
		{ID: "i-c", Pool: "linux", Generation: "v2", State: ports.InstancePending, Type: "c8i.8xlarge", PrivateIP: netip.MustParseAddr("10.0.1.13")},
		{ID: "i-d", Pool: "windows", Generation: "w1", State: ports.InstanceShuttingDown, Type: "c7a.8xlarge", PrivateIP: netip.MustParseAddr("10.0.1.14")},
	}
	cache := httpsd.NewCache(func(context.Context) ([]ports.Instance, error) { calls++; return fleet, nil }, 30*time.Second, func() time.Time { return now })
	h := &httpsd.Handler{Source: cache, Port: 9987}
	get := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, httpsd.Path, nil))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
		return rec.Body.String()
	}
	assert.JSONEq(t, `[
	  {"targets":["10.0.1.11:9987"],"labels":{"pool":"linux","node":"i-a","generation":"v1","instance_type":"c7i.8xlarge"}},
	  {"targets":["10.0.1.12:9987"],"labels":{"pool":"linux","node":"i-b","generation":"v2","instance_type":"c8i.8xlarge"}}
	]`, get())
	get()
	assert.Equal(t, 1, calls, "a fresh cache must not call EC2 again")

	now = now.Add(31 * time.Second)
	cache.Publish(fleet[:1]) // the leader's loop publishes a fresh observation
	assert.JSONEq(t, `[{"targets":["10.0.1.12:9987"],"labels":{"pool":"linux","node":"i-b","generation":"v2","instance_type":"c8i.8xlarge"}}]`, get())
	assert.Equal(t, 1, calls)

	now = now.Add(31 * time.Second)
	get()
	assert.Equal(t, 2, calls, "a stale cache refreshes")
}
