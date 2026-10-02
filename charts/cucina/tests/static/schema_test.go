// SPDX-License-Identifier: FSL-1.1-ALv2

package static_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
)

// TestInvalidValuesFailFast proves that values.schema.json and the chart's
// cross-field checks reject bad configuration at `helm install/upgrade` time instead
// of producing objects that fail later in the cluster (R-CP-7, R-TEST-7 "fail fast on
// configuration"). Each row changes one thing on top of the valid small profile.
func TestInvalidValuesFailFast(t *testing.T) {
	chart := charttest.ChartDir(t)
	base := filepath.Join(chart, "ci", "values-small.yaml")
	if _, err := charttest.Helm(t, charttest.TemplateArgs(t, []string{base})...); err != nil {
		t.Fatalf("the valid baseline does not render: %v", err)
	}
	ec2 := "{name: p, platform: linux-x86-64, provider: ec2, capacity: {minRunning: 0, max: 1}, image: {ami: ami-0123456789abcdef0}, ec2: {instanceTypes: [c7i.large], subnetIDs: [subnet-1], securityGroupIDs: [sg-1], instanceProfile: w}}"
	cases := []struct{ name, values string }{
		{"unknown top-level key", "frontnd: {replicas: 2}"},
		{"unknown nested key", "storage: {mode: file, stores: {cas: {sise: 100Gi}}}"},
		{"unknown size profile", "sizeProfile: huge"},
		{"Buildbarn image without digest", "images: {bbStorage: {tag: latest, digest: ''}}"},
		{"malformed size", "storage: {mode: file, stores: {cas: {size: 100GB}}}"},
		{"CAS blocks below 512 MiB", "storage: {mode: file, stores: {cas: {size: 10Gi}}}"},
		{"maximum execution timeout below 3600s", "buildbarn: {scheduler: {maximumExecutionTimeout: 1800s}}"},
		{"existence cache longer than an hour", "buildbarn: {frontend: {existenceCacheDuration: 7200s}}"},
		{"Go duration where protobuf wants seconds", "tls: {refreshInterval: 5m}"},
		{"no instance names", "instanceNames: []"},
		{"pool without its provider block", "pools: [{name: p, platform: linux-x86-64, provider: ec2, capacity: {minRunning: 0, max: 1}, image: {ami: ami-1}}]"},
		{"pool with an unknown field", "pools: [" + ec2[:len(ec2)-1] + ", maxSize: 3}]"},
		{"pool on an unknown platform", "pools: [{name: p, platform: linux-sparc, provider: ec2, capacity: {minRunning: 0, max: 1}, image: {ami: ami-1}, ec2: {instanceTypes: [x], subnetIDs: [s], securityGroupIDs: [g], instanceProfile: w}}]"},
		{"pool provider does not match its platform", "pools: [{name: p, platform: macos-arm64-xcode27.0, provider: ec2, capacity: {minRunning: 0, max: 1}, image: {ami: ami-1}, ec2: {instanceTypes: [x], subnetIDs: [s], securityGroupIDs: [g], instanceProfile: w}}]"},
		{"pool on an unconfigured instance name", "pools: [" + ec2[:len(ec2)-1] + ", instanceNames: [tenant-x]}]"},
		{"pool minRunning above max", "pools: [{name: p, platform: linux-x86-64, provider: ec2, capacity: {minRunning: 2, max: 1}, image: {ami: ami-1}, ec2: {instanceTypes: [x], subnetIDs: [s], securityGroupIDs: [g], instanceProfile: w}}]"},
		{"duplicate pool names", "pools: [" + ec2 + ", " + ec2 + "]"},
		{"extra platform clashing with the catalog", "platforms: {extra: [{name: linux-x86-64, provider: ec2, os: linux, arch: x86_64, defaults: {idleTimeout: 5m, drainTimeout: 30m, startupTimeout: 5m, buildDirectory: fuse, l1Placement: auto}, sizeClasses: [{name: default, sizeClass: 1}], runners: [{name: native, properties: {OSFamily: linux, ISA: x86-64}, concurrency: {fixed: 1}}]}]}"},
		{"trust policy without grants", "trustPolicies: [{name: t, spec: {type: oidc, issuer: {url: 'https://issuer.example.com', audiences: [a]}, claimMappings: {subject: {expression: claims.sub}}}}]"},
		{"OIDC trust policy without an issuer", "trustPolicies: [{name: t, spec: {type: oidc, grants: [{instanceNames: [main], verbs: [cas-read]}]}}]"},
		{"existing-Secret TLS without a Secret", "tls: {public: {source: existingSecret}}"},
		{"cert-manager TLS without an issuer", "tls: {internal: {source: certManager}}"},
		{"worker endpoint behind an Ingress", "exposure: {worker: {type: Ingress}}"},
		{"EC2 provider without an account", "controller: {aws: {enabled: true, region: us-west-1, accountId: ''}}"},
		{"raw block storage without a StorageClass", "storage: {mode: block, storageClassName: ''}"},
		{"storage PVC smaller than the stores", "storage: {mode: file, persistence: {size: 10Gi}}"},
	}
	dir := t.TempDir()
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := filepath.Join(dir, filepath.Base(t.Name())+".yaml")
			if err := os.WriteFile(f, []byte(c.values+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := charttest.Helm(t, charttest.TemplateArgs(t, []string{base, f})...); err == nil {
				t.Errorf("case %d (%s) rendered without error:\n%.400s", i, c.values, out)
			}
		})
	}
}
