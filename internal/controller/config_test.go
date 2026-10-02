// SPDX-License-Identifier: FSL-1.1-ALv2

package controller_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/controller"
)

// Guards R-TEST-7 "fail fast on configuration": the controller parses its
// configuration strictly into config.Controller and refuses to start with a
// message that names the offending field.
func TestConfigIsStrict(t *testing.T) {
	sample, err := os.ReadFile("testdata/controller.json")
	require.NoError(t, err)
	for _, mode := range []controller.Mode{controller.ModeController, controller.ModeSTS, controller.ModeTool} {
		c, err := controller.ParseConfig(sample, mode)
		require.NoError(t, err, mode)
		assert.Equal(t, time.Second, c.Scheduler.PollInterval.Duration)
	}

	edit := func(old, new string) []byte {
		s := strings.Replace(string(sample), old, new, 1)
		require.NotEqual(t, string(sample), s, "edit %q did not apply", old)
		return []byte(s)
	}
	cases := []struct {
		name string
		doc  []byte
		mode controller.Mode
		want string
	}{
		{"unknown nested field", edit(`"pollInterval": "1s",`, `"pollInterval": "1s", "pollIntervall": "2s",`), controller.ModeController, `"pollIntervall" within "/scheduler"`},
		{"field name case", edit(`"clusterId"`, `"clusterID"`), controller.ModeTool, `"clusterID"`},
		{"wrong type", edit(`"metricsPort": 9987`, `"metricsPort": "9987"`), controller.ModeController, `"/worker/metricsPort"`},
		{"duration without unit", edit(`"tokenTTL": "15m"`, `"tokenTTL": 900`), controller.ModeSTS, "duration must be a string"},
		{"trailing data", append(append([]byte{}, sample...), []byte(`{}`)...), controller.ModeTool, "after top-level value"},
		{"duplicate member", edit(`"namespace": "cucina",`, `"namespace": "cucina", "namespace": "other",`), controller.ModeTool, "duplicate"},
		{"poll interval out of range", edit(`"pollInterval": "1s"`, `"pollInterval": "5s"`), controller.ModeController, "/scheduler/pollInterval must be between 1s and 2s"},
		{"scheduler without mTLS", edit(`"clientKeyFile": "/etc/cucina/client/tls.key", `, ``), controller.ModeController, "/scheduler/clientCertFile"},
		{"certificate TTL above 7 days", edit(`"workerCertTTL": "24h"`, `"workerCertTTL": "200h"`), controller.ModeController, "/pki/workerCertTTL"},
		{"bad cluster id", edit(`"clusterId": "cucina-test"`, `"clusterId": "Cucina Test"`), controller.ModeTool, "/clusterId"},
		{"sts needs https issuer", edit(`"stsUrl": "https://`, `"stsUrl": "http://`), controller.ModeSTS, "/endpoints/stsUrl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := controller.ParseConfig(tc.doc, tc.mode)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
