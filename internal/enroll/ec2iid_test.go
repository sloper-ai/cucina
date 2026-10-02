// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/ports"
)

// TestAWSCertificates guards the embedded AWS certificates (R-SEC-3): us-west-1
// and every other commercial Region, all RSA-2048 (enforced by the
// constructor), none from the China, GovCloud or Sovereign Cloud partitions.
func TestAWSCertificates(t *testing.T) {
	v, err := enroll.NewIdentityVerifier()
	require.NoError(t, err)
	regions := v.Regions()
	require.Contains(t, regions, "us-west-1")
	require.GreaterOrEqual(t, len(regions), 34)
	for _, r := range regions {
		require.False(t, strings.HasPrefix(r, "cn-") || strings.HasPrefix(r, "us-gov-") || strings.HasPrefix(r, "eusc-"), r)
	}
}

// FuzzIdentityVerify guards the parser at the trust boundary: arbitrary
// documents and signatures never panic, and nothing but the exact signed
// document ever verifies.
func FuzzIdentityVerify(f *testing.F) {
	aws := testAWS(f)
	inst := ports.Instance{ID: "i-0123456789abcdef0", AZ: "us-west-1a", ImageID: testImage, Type: "c8i.2xlarge",
		LaunchTime: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)}
	doc := identityDocument(f, inst, nil)
	for _, o := range []sigOpts{{}, {ber: true}, {noAttrs: true}, {detached: true}} {
		f.Add(doc, aws.sign(f, testRegion, doc, o))
	}
	f.Add([]byte(nil), aws.sign(f, testRegion, doc, sigOpts{ber: true}))
	f.Add([]byte(`{"region":"us-west-1"}`), "MIAGCSqGSIb3DQEHAqCAMIACAQEx")
	v := aws.verifier(f)
	f.Fuzz(func(t *testing.T, document []byte, signature string) {
		got, err := v.Verify(document, signature)
		if err != nil {
			return
		}
		if len(document) > 0 && !bytes.Equal(document, doc) {
			t.Fatalf("a document that was never signed verified: %q", document)
		}
		if got.InstanceID != inst.ID {
			t.Fatalf("verified content names %q", got.InstanceID)
		}
	})
}
