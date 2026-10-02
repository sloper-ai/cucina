// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
)

// The conformance suites of internal/ports/porttest run against every fake
// (R-TEST-8b). The same suites run against the real adapters in acceptance;
// when a real adapter fails one, the fake is fixed.

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func TestClockConformance(t *testing.T) {
	t.Run("Manual", func(t *testing.T) {
		porttest.RunClock(t, false, func(t *testing.T) porttest.ClockHarness {
			c := fakes.NewClock(t0)
			return porttest.ClockHarness{Clock: c, Advance: c.Advance}
		})
	})
	t.Run("SystemInSynctest", func(t *testing.T) {
		porttest.RunClock(t, true, func(t *testing.T) porttest.ClockHarness {
			// Inside the bubble a timer-based sleep advances virtual time only.
			clock := fakes.SystemClock{}
			return porttest.ClockHarness{Clock: clock, Advance: func(d time.Duration) { _ = clock.Sleep(t.Context(), d) }}
		})
	})
}

func TestFSConformance(t *testing.T) {
	porttest.RunFS(t, func(t *testing.T) (ports.FS, string) {
		f := fakes.NewFS(fakes.NewClock(t0), fakes.NewRand(1), 0)
		if err := f.MkdirAll("/porttest", 0o755); err != nil {
			t.Fatal(err)
		}
		return f, "/porttest"
	})
}

func TestSecretStoreConformance(t *testing.T) {
	porttest.RunSecretStore(t, func(t *testing.T) ports.SecretStore {
		return fakes.NewSecretStore(fakes.NewClock(t0), fakes.NewRand(1))
	})
}

func TestComputeConformance(t *testing.T) {
	porttest.RunCompute(t, func(t *testing.T) porttest.ComputeHarness {
		clock := fakes.NewClock(t0)
		cfg := fakes.DefaultComputeConfig()
		c := fakes.NewCompute(clock, fakes.NewRand(7), cfg)
		return porttest.ComputeHarness{
			Compute: c,
			Cluster: "porttest",
			Request: ports.LaunchRequest{Pool: "pool-a", ImageID: "ami-1", InstanceTypes: []string{"c8i.2xlarge", "c7i.2xlarge"},
				SubnetIDs: []string{"subnet-a", "subnet-b"}, RootVolume: ports.VolumeSpec{SizeGiB: 30, Type: "gp3"},
				Tags: map[string]string{domain.TagCluster: "porttest", domain.TagManagedBy: domain.ManagedByValue,
					domain.TagPool: "pool-a", domain.TagRole: "worker", domain.TagImageVersion: "1"}},
			Await: porttest.FakeAwait(clock.Advance, c.Tick, time.Second, 2*time.Hour),
			ForceICE: func(t *testing.T, typ string) func() {
				for _, az := range cfg.Subnets {
					c.SetCapacity(typ, az, 0)
				}
				return func() {
					for _, az := range cfg.Subnets {
						c.SetCapacity(typ, az, -1)
					}
				}
			},
			PriceTypes: []string{"c8i.2xlarge", "c7i.2xlarge"},
			ForceThrottle: func(t *testing.T) func() {
				c.SetBucket("Launch", 0, 0)
				return func() { c.SetBucket("Launch", 5, 2) }
			},
		}
	})
}

func TestBuildQueueConformance(t *testing.T) {
	declared := domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: "ISA=x86-64;OSFamily=linux", SizeClass: 1}
	undeclared := domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: "ISA=rv64g;OSFamily=linux", SizeClass: 1}
	porttest.RunBuildQueue(t, func(t *testing.T) porttest.BuildQueueHarness {
		clock := fakes.NewClock(t0)
		bq := fakes.NewBuildQueue(clock, fakes.NewRand(3), fakes.DefaultBuildQueueConfig())
		bq.Declare(declared)
		return porttest.BuildQueueHarness{
			Queue: bq, Declared: declared, Undeclared: undeclared,
			Submit: func(t *testing.T, key domain.QueueKey) string {
				name, err := bq.Submit(key, time.Minute, "porttest")
				if err != nil {
					t.Fatal(err)
				}
				return name
			},
			AddWorker: func(t *testing.T, key domain.QueueKey, id ports.WorkerID) {
				bq.RegisterNode(domain.PoolName(id[domain.LabelPool]), id[domain.LabelNode], map[domain.QueueKey]int{key: 1})
			},
			Await: porttest.FakeAwait(clock.Advance, bq.Tick, time.Second, time.Hour),
		}
	})
}

func TestVMRuntimeConformance(t *testing.T) {
	porttest.RunVMRuntime(t, func(t *testing.T) porttest.VMRuntimeHarness {
		cfg := fakes.DefaultVMRuntimeConfig()
		cfg.BootLatency, cfg.GuestAgentDelay = 0, 0 // IP() must not block on the manual clock
		return porttest.VMRuntimeHarness{
			Runtime:    fakes.NewVMRuntime(fakes.NewClock(t0), fakes.NewRand(5), cfg),
			Image:      "ghcr.io/sloper-ai/cucina-worker-macos:27.0-porttest",
			BootWait:   time.Minute,
			TwoVMLimit: true,
		}
	})
}

func TestHostFleetConformance(t *testing.T) {
	porttest.RunHostFleet(t, func(t *testing.T) porttest.HostFleetHarness {
		clock := fakes.NewClock(t0)
		f := fakes.NewHostFleet(clock, fakes.NewRand(9), fakes.DefaultHostFleetConfig())
		f.AddHost(ports.HostState{Serial: "PORTTEST01", Name: "mini-1", Online: true, Approved: true, Slots: 2})
		return porttest.HostFleetHarness{Fleet: f, Host: "PORTTEST01", Pool: "macos", Image: "ghcr.io/sloper-ai/cucina-worker-macos:27.0",
			Generation: "g1", Await: porttest.FakeAwait(clock.Advance, f.Tick, time.Second, 10*time.Minute)}
	})
}

// TestIdentityProvider guards the fake IdP's contract (R-AUTH tests rely on
// it): Google- and GitHub-shaped tokens verify for their issuer and audience,
// and every mismatch fails closed.
func TestIdentityProvider(t *testing.T) {
	clock := fakes.NewClock(t0)
	idp := fakes.NewIdentityProvider(clock, fakes.NewRand(11))
	ctx := t.Context()
	google := idp.Issue(fakes.GoogleIssuer, fakes.GoogleClaims("1234", "dev@example.com", "example.com", "client-id", t0.Add(time.Hour)))
	github := idp.Issue(fakes.GitHubIssuer, fakes.GitHubClaims("sloper-ai/cucina", "refs/heads/main", "ci", "push", "cucina", t0.Add(10*time.Minute)))

	c, err := idp.Verify(ctx, fakes.GoogleIssuer, []string{"client-id"}, google)
	if err != nil || c["hd"] != "example.com" || c["email"] != "dev@example.com" {
		t.Fatalf("google token: %v %v", c, err)
	}
	c, err = idp.Verify(ctx, fakes.GitHubIssuer, []string{"other", "cucina"}, github)
	if err != nil || c["repository"] != "sloper-ai/cucina" || c["sub"] != "repo:sloper-ai/cucina:ref:refs/heads/main" {
		t.Fatalf("github token: %v %v", c, err)
	}
	expired := idp.Issue(fakes.GoogleIssuer, fakes.GoogleClaims("1", "a@example.com", "", "client-id", t0.Add(-time.Second)))
	for name, tc := range map[string]struct {
		issuer string
		aud    []string
		raw    string
		want   error
	}{
		"wrong audience":     {fakes.GoogleIssuer, []string{"someone-else"}, google, fakes.ErrTokenAudience},
		"wrong issuer":       {fakes.GitHubIssuer, []string{"client-id"}, google, fakes.ErrTokenSignature},
		"expired":            {fakes.GoogleIssuer, []string{"client-id"}, expired, fakes.ErrTokenExpired},
		"tampered signature": {fakes.GoogleIssuer, []string{"client-id"}, google[:len(google)-2] + "AA", fakes.ErrTokenSignature},
		"malformed":          {fakes.GoogleIssuer, []string{"client-id"}, "not.a-jwt", fakes.ErrTokenMalformed},
	} {
		if _, err := idp.Verify(ctx, tc.issuer, tc.aud, tc.raw); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	// Key rotation: old tokens verify until the old key is retired.
	idp.RotateKey(fakes.GoogleIssuer)
	if _, err := idp.Verify(ctx, fakes.GoogleIssuer, []string{"client-id"}, google); err != nil {
		t.Fatalf("token signed with the previous key: %v", err)
	}
	idp.RetireOldKeys(fakes.GoogleIssuer)
	if _, err := idp.Verify(ctx, fakes.GoogleIssuer, []string{"client-id"}, google); !errors.Is(err, fakes.ErrTokenSignature) {
		t.Fatalf("token of a retired key must fail: %v", err)
	}
}
