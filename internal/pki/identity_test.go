// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/pki"
)

// TestURISANFormats guards the normative URI SAN strings of docs/security.md
// §Workload identity (R-SEC-2, R-AUTH-4): the chart's Buildbarn expressions and
// the deny-list match these bytes exactly.
func TestURISANFormats(t *testing.T) {
	must := func(id pki.Identity, err error) pki.Identity {
		require.NoError(t, err)
		return id
	}
	for _, tc := range []struct {
		id   pki.Identity
		want string
		node string
	}{
		{must(pki.WorkerIdentity("linux-x86-64", "i-0123456789abcdef0")), "spiffe://cucina/worker/linux-x86-64/i-0123456789abcdef0", "i-0123456789abcdef0"},
		{must(pki.WorkerIdentity("linux-aarch64", "i-0a1b2c3d")), "spiffe://cucina/worker/linux-aarch64/i-0a1b2c3d", "i-0a1b2c3d"},
		{must(pki.VMIdentity("macos-arm64-xcode27.0", "H4X9K2LM7Q", "vm-1")), "spiffe://cucina/worker/macos-arm64-xcode27.0/H4X9K2LM7Q/vm-1", "H4X9K2LM7Q/vm-1"},
		{must(pki.HostIdentity("C02XK0AAJGH6")), "spiffe://cucina/host/C02XK0AAJGH6", ""},
		{pki.ControllerIdentity(), "spiffe://cucina/controller", ""},
		{must(pki.ServerIdentity("frontend")), "spiffe://cucina/server/frontend", ""},
	} {
		require.Equal(t, tc.want, tc.id.String())
		require.Equal(t, tc.node, tc.id.Node())
		parsed, err := pki.ParseIdentity(tc.want)
		require.NoError(t, err)
		require.Equal(t, tc.id, parsed)
	}

	for _, bad := range []string{
		"spiffe://cucina/worker/linux-x86-64",                      // too few segments
		"spiffe://cucina/worker/linux-x86-64/i-0123456789abcdef0/", // trailing slash
		"spiffe://cucina/worker/Linux/i-0123456789abcdef0",         // pool grammar
		"spiffe://cucina/worker/linux/i-0123",                      // instance id grammar
		"spiffe://cucina/worker/linux/i-0123456789ABCDEF0",         // instance id is lower-case hex
		"spiffe://cucina/host/c02xk0aajgh6",                        // serial must be canonical (upper case)
		"spiffe://cucina/host/C02%58K0AAJGH6",                      // percent-encoding
		"spiffe://cucina/host/C02XK0AAJGH6?x=1",                    // query
		"spiffe://cucina/host/C02XK0AAJGH6#f",                      // fragment
		"spiffe://cucina:443/host/C02XK0AAJGH6",                    // port
		"spiffe://evil@cucina/host/C02XK0AAJGH6",                   // user info
		"spiffe://other/host/C02XK0AAJGH6",                         // trust domain
		"SPIFFE://cucina/host/C02XK0AAJGH6",                        // scheme case
		"spiffe://cucina/worker/pool/H4X9K2LM7Q/..",                // dot segment
		"spiffe://cucina/controller/x",                             // controller has no path
		"spiffe://cucina/admin/x",                                  // unknown role
		"spiffe://cucina/server/Frontend",                          // component grammar
		"spiffe://cucina/worker/linux/H4X9K2LM7Q/vm-1/extra",       // too many segments
	} {
		_, err := pki.ParseIdentity(bad)
		require.ErrorIs(t, err, pki.ErrInvalidIdentity, bad)
	}

	for _, s := range []string{"spiffe://cucina:443/host/C02XK0AAJGH6", "spiffe://u@cucina/host/C02XK0AAJGH6", "spiffe://cucina/host/C02XK0AAJGH6?"} {
		u, err := url.Parse(s)
		require.NoError(t, err)
		_, err = pki.ParseIdentityURL(u)
		require.ErrorIs(t, err, pki.ErrInvalidIdentity, s)
	}

	serial, err := pki.CanonicalSerial("  c02xk0aajgh6 \n")
	require.NoError(t, err)
	require.Equal(t, "C02XK0AAJGH6", serial)
	for _, bad := range []string{"", "ABC", "C02 XK0AAJGH6", "C02-XK0AAJGH6", "ÄBCDEFG"} {
		_, err := pki.CanonicalSerial(bad)
		require.ErrorIs(t, err, pki.ErrInvalidIdentity, bad)
	}
}

// TestIdentityRoundTrip is a property test (R-TEST-3: large input space): every
// valid identity survives String/URL -> Parse unchanged, so what the issuer
// writes is exactly what Cucina's verifiers and Buildbarn read back.
func TestIdentityRoundTrip(t *testing.T) {
	alnum := rapid.SampledFrom([]rune("abcdefghijklmnopqrstuvwxyz0123456789"))
	upper := rapid.SampledFrom([]rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"))
	hex := rapid.SampledFrom([]rune("0123456789abcdef"))
	str := func(t *rapid.T, label string, g *rapid.Generator[rune], minLen, maxLen int) string {
		return string(rapid.SliceOfN(g, minLen, maxLen).Draw(t, label))
	}
	rapid.Check(t, func(t *rapid.T) {
		pool := str(t, "pool", alnum, 1, 1) + str(t, "poolMid", rapid.SampledFrom([]rune("abc019.-")), 0, 20) + str(t, "poolEnd", alnum, 1, 1)
		serial := str(t, "serial", upper, 6, 32)
		var id pki.Identity
		var err error
		switch rapid.IntRange(0, 3).Draw(t, "role") {
		case 0:
			n := rapid.SampledFrom([]int{8, 17}).Draw(t, "idLen")
			id, err = pki.WorkerIdentity(pool, "i-"+str(t, "iid", hex, n, n))
		case 1:
			vm := str(t, "vm", alnum, 1, 1) + str(t, "vmMid", rapid.SampledFrom([]rune("aZ09._-")), 0, 30) + str(t, "vmEnd", upper, 1, 1)
			id, err = pki.VMIdentity(pool, serial, vm)
		case 2:
			id, err = pki.HostIdentity(serial)
		case 3:
			id = pki.ControllerIdentity()
		}
		if err != nil {
			t.Fatalf("generator produced an invalid identity: %v", err)
		}
		got, err := pki.ParseIdentity(id.String())
		if err != nil || got != id {
			t.Fatalf("ParseIdentity(%q) = %+v, %v", id.String(), got, err)
		}
		got, err = pki.ParseIdentityURL(id.URL())
		if err != nil || got != id {
			t.Fatalf("ParseIdentityURL(%q) = %+v, %v", id.String(), got, err)
		}
	})
}
