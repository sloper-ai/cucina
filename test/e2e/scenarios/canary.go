// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
)

// The canary scenarios run the production canaries (internal/canary) from
// the harness: the same library the controller's 5-minute loop and the
// canary CronJob use (R-TEST-7, "one scenario library, two cadences").

func stsToken(c *harness.Context) (canary.TokenSource, error) {
	if c.Env.Endpoints.STS == "" || c.Env.Secrets.ServiceKeyFile == "" {
		return nil, harness.Skip("needs endpoints.sts and secrets.serviceKeyFile")
	}
	hc, err := stsHTTP(c.Env)
	if err != nil {
		return nil, err
	}
	return &canary.STS{URL: c.Env.Endpoints.STS, KeyFile: c.Env.Secrets.ServiceKeyFile, HTTP: hc}, nil
}

func canaryCache() *harness.Scenario {
	return &harness.Scenario{
		ID: "canary-cache", Title: "Cache canary: token mint/verify + AC/CAS round trip through the client endpoint (no workers)",
		Envs: []harness.EnvKind{harness.EnvAWS, harness.EnvKindCluster, harness.EnvProdSmoke, harness.EnvCanary}, ReadOnly: true,
		Cost: harness.CostNone, Timeout: 2 * time.Minute,
		Run: func(c *harness.Context) error {
			ts, err := stsToken(c)
			if err != nil {
				return err
			}
			r := (&canary.Probe{Kind: canary.KindCache, Endpoint: endpoint(c.Env), Tokens: ts, Timeout: 90 * time.Second}).Run(c)
			c.Record("canary", r)
			c.Metric("canary.cache.seconds", r.Duration.Seconds(), "s")
			hasZstd := false
			for _, x := range r.Compressors {
				hasZstd = hasZstd || x == "ZSTD"
			}
			c.Check(harness.CheckResult{Name: "endpoint advertises ZSTD (R-DATA-3)", Kind: "reapi", Pass: hasZstd, Value: strings.Join(r.Compressors, ",")})
			if !r.Success {
				return harness.Fail("cache canary: %s", r.Error)
			}
			return nil
		},
	}
}

func canaryExec() *harness.Scenario {
	return &harness.Scenario{
		ID: "canary-exec", Title: "Execution canary per pool: a tiny uncached action, scaling each pool from zero",
		Envs:     []harness.EnvKind{harness.EnvAWS, harness.EnvCanary},
		Requires: []harness.Requirement{harness.RequiresAWS},
		Cost:     harness.CostMedium, EstimateUSD: 2, MaxInstances: 4, Timeout: time.Hour, NFRs: []string{"NFR-P1"},
		Run: func(c *harness.Context) error {
			ts, err := stsToken(c)
			if err != nil {
				return err
			}
			props, err := runnerProps(c.Env)
			if err != nil {
				return err
			}
			roles := make([]string, 0, len(c.Env.Pools))
			for r := range c.Env.Pools {
				roles = append(roles, r)
			}
			sort.Strings(roles)
			var failed []string
			for _, role := range roles {
				pool := c.Env.Pools[role]
				runner := "native"
				if strings.HasPrefix(role, "macos") {
					runner = "generic"
				}
				platformName := poolPlatform(c, pool)
				p := props[platformName][runner]
				if p == nil {
					c.Note("canary-exec: no %s runner for pool %s (platform %s)", runner, pool, platformName)
					continue
				}
				r := (&canary.Probe{Kind: canary.KindExec, Endpoint: endpoint(c.Env), Tokens: ts, Pool: pool, Platform: p}).Run(c)
				c.Record("canary."+pool, r)
				if !r.Success {
					failed = append(failed, pool+": "+r.Error)
					continue
				}
				osName := map[string]string{"linux": "linux", "linux-arm64": "linux", "windows": "windows", "macos": "macos"}[role]
				if osName != "" {
					c.NFR(nfr.Reported("NFR-P1", "canary-exec "+pool+" queue time (cold-start sample)", r.QueueTime.Seconds(), "s", "worker "+r.Worker))
				}
			}
			if len(failed) > 0 {
				return harness.Fail("execution canary failed: %s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

// poolPlatform maps a pool to its platforms/pools.json platform: by
// convention the pool name equals the platform name for EC2 pools; macOS pool
// names carry the Xcode version (macos-arm64-xcode27.0 → macos-arm64).
func poolPlatform(c *harness.Context, pool string) string {
	props, _ := runnerProps(c.Env)
	if _, ok := props[pool]; ok {
		return pool
	}
	for name := range props {
		if strings.HasPrefix(pool, name) {
			return name
		}
	}
	return fmt.Sprint(pool)
}
