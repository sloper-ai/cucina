// SPDX-License-Identifier: FSL-1.1-ALv2

package charttest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// A throwaway ES256 signing key standing in for the STS's (test only).
var (
	testKeyOnce sync.Once
	testKey     *ecdsa.PrivateKey
)

const testKID = "chart-test-key"

func signingKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

// JWKS is the JWKS file Buildbarn validates the test JWTs against.
func JWKS(t testing.TB) []byte {
	t.Helper()
	b, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &signingKey(t).PublicKey, KeyID: testKID, Algorithm: string(jose.ES256), Use: "sig",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// CucinaJWT mints a token with the claims of docs/contracts.md §5.1, signed by the
// throwaway key in JWKS.
func CucinaJWT(t testing.TB, issuer, sub, sid string, grants map[string][]string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: signingKey(t)},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", testKID))
	if err != nil {
		t.Fatal(err)
	}
	cucina := map[string][]string{}
	for _, verb := range []string{"cas_read", "cas_write", "ac_read", "ac_write", "execute", "admin"} {
		cucina[verb] = append([]string{}, grants[verb]...)
	}
	now := time.Now()
	token, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": issuer, "aud": "buildbarn", "sub": sub, "sid": sid, "jti": sid + "-jti",
		"iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(), "cucina": cucina,
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Bearer is per-RPC credentials carrying a Cucina JWT (what the credential helper gives Bazel).
type Bearer string

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (b Bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials.
func (Bearer) RequireTransportSecurity() bool { return true }
