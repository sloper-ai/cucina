// SPDX-License-Identifier: FSL-1.1-ALv2

package sts_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/bbtest"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

// storageWithJWT renders the bbtest bb_storage fixture with the client listener switched
// to the Cucina `jwt` policy and every CAS/AC authorizer set to the rendered expression
// with the deny-list file, plus one startup test vector per authorizer.
func storageWithJWT(t *testing.T, addr, jwksPath, denyPath string) []byte {
	t.Helper()
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(bbtest.StorageConfig(bbtest.StorageOptions{ClientListen: addr}), &cfg))
	cfg["grpcServers"].([]any)[0].(map[string]any)["authenticationPolicy"] = map[string]any{"jwt": map[string]any{
		"jwksFile":                             jwksPath,
		"maximumCacheSize":                     1000,
		"cacheReplacementPolicy":               "LEAST_RECENTLY_USED",
		"claimsValidationJmespathExpression":   map[string]any{"expression": keys.BuildbarnClaimsValidation(stsURL)},
		"metadataExtractionJmespathExpression": map[string]any{"expression": keys.BuildbarnMetadataExtraction},
	}}
	const vectorSID = "VVVVVVVVVVVVVVVVVVVVVV"
	vectorFile, err := keys.EncodeDenyList([]keys.Revocation{{Kind: keys.RevokeSession, Value: vectorSID}}, time.Now())
	require.NoError(t, err)
	all := []any{"main"}
	authz := func(claimKey string) map[string]any {
		return map[string]any{"jmespathExpression": map[string]any{
			"expression": keys.BuildbarnAuthorizer(claimKey),
			"files":      []any{map[string]any{"key": keys.DenyListFileKey, "path": denyPath}},
			"testVectors": []any{map[string]any{
				"input": map[string]any{
					"authenticationMetadata": map[string]any{"private": map[string]any{
						"sid": vectorSID, "sub": "google:1", "cas_read": all, "cas_write": all, "ac_read": all, "ac_write": all, "execute": all, "admin": all}},
					"instanceName": "main",
					"files":        map[string]any{keys.DenyListFileKey: string(vectorFile)},
				},
				"expectedOutput": false,
			}},
		}}
	}
	cas := cfg["contentAddressableStorage"].(map[string]any)
	cas["getAuthorizer"], cas["putAuthorizer"], cas["findMissingAuthorizer"] = authz("cas_read"), authz("cas_write"), authz("cas_read")
	ac := cfg["actionCache"].(map[string]any)
	ac["getAuthorizer"], ac["putAuthorizer"] = authz("ac_read"), authz("ac_write")
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	return b
}

// TestBuildbarnAcceptsCucinaTokens is the R-TEST-6 "STS/auth" integration test with the
// real frontend: the pinned bb_storage, configured with the rendered `jwt` policy
// (R-AUTH-4) and deny-list authorizers (R-AUTH-9), accepts and rejects tokens minted by
// the STS: unauthenticated, expired, foreign-key and tampered tokens are refused, a
// read-only principal cannot write the action cache, grants are per instance name, and a
// deny-listed sid or sub is refused while other principals are not.
func TestBuildbarnAcceptsCucinaTokens(t *testing.T) {
	e := newEnv(t, 1000)
	token := func(r result) string {
		require.Equal(t, http.StatusOK, r.code, "%v", r.body)
		return r.body["access_token"].(string)
	}
	writer := token(e.exchange(e.github.Token(e.githubClaims()), auth.TokenTypeJWT))
	reader := token(e.exchange(e.github.Token(e.githubClaims("jti", "bb-pr", "event_name", "pull_request", "ref", "refs/pull/2/merge", "workflow", "PR")), auth.TokenTypeJWT))
	developer := token(e.exchange(e.google.Token(e.googleClaims()), auth.TokenTypeIDToken))
	expired, _, err := (&keys.Minter{Issuer: stsURL, Keys: e.mgr.Ring, Clock: keystest.NewClock(time.Now().Add(-time.Hour))}).Mint(
		keys.MintRequest{Subject: "google:old", Scopes: keys.Scopes{ACRead: []string{"main"}, ACWrite: []string{"main"}}, TTL: 15 * time.Minute})
	require.NoError(t, err)
	kid, priv, err := keys.GenerateKey(nil)
	require.NoError(t, err)
	foreignSet, err := keys.NewKeySet(keys.Bootstrap(kid, time.Now()), map[string]*ecdsa.PrivateKey{kid: priv})
	require.NoError(t, err)
	foreign, _, err := (&keys.Minter{Issuer: stsURL, Keys: keys.StaticKeys{Set: foreignSet}, Clock: keys.SystemClock}).Mint(
		keys.MintRequest{Subject: "google:mallory", Scopes: keys.Scopes{ACWrite: []string{"main"}}, TTL: time.Minute})
	require.NoError(t, err)
	tampered := func() string { // the reader grants itself ac_write
		parts := strings.Split(reader, ".")
		p, _ := base64.RawURLEncoding.DecodeString(parts[1])
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(p), `"ac_write":[]`, `"ac_write":["main"]`, 1)))
		return strings.Join(parts, ".")
	}()
	writerClaims, err := e.mgr.Verifier.Verify(writer)
	require.NoError(t, err)
	readerClaims, err := e.mgr.Verifier.Verify(reader)
	require.NoError(t, err)

	dir := bbtest.ShortTempDir(t)
	jwks, err := e.mgr.Ring.KeySet().JWKSJSON()
	require.NoError(t, err)
	jwksPath := filepath.Join(dir, "jwks.json")
	require.NoError(t, os.WriteFile(jwksPath, jwks, 0o600))

	boot := func(deny string) remoteexecution.ActionCacheClient {
		denyPath := filepath.Join(dir, "denylist-"+strings.ReplaceAll(t.Name(), "/", "_")+time.Now().Format("150405.000000000")+".json")
		require.NoError(t, os.WriteFile(denyPath, []byte(deny), 0o600))
		addr := bbtest.FreeAddr(t)
		bbtest.BootStorage(t, storageWithJWT(t, addr, jwksPath, denyPath), bbtest.GRPCReady(addr, nil))
		conn, err := bbtest.Dial(addr, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return remoteexecution.NewActionCacheClient(conn)
	}
	digest := bbtest.DigestOf([]byte("cucina auth test action"))
	call := func(ac remoteexecution.ActionCacheClient, tok, instance string, write bool) codes.Code {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if tok != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		}
		if write {
			_, err := ac.UpdateActionResult(ctx, &remoteexecution.UpdateActionResultRequest{
				InstanceName: instance, ActionDigest: digest, ActionResult: &remoteexecution.ActionResult{}})
			return status.Code(err)
		}
		_, err := ac.GetActionResult(ctx, &remoteexecution.GetActionResultRequest{InstanceName: instance, ActionDigest: digest})
		return status.Code(err)
	}

	ac := boot(keys.EmptyDenyList)
	for _, r := range []struct {
		name     string
		token    string
		instance string
		write    bool
		want     codes.Code
	}{
		{"unauthenticated request", "", "main", false, codes.Unauthenticated},
		{"expired token", expired, "main", false, codes.Unauthenticated},
		{"token signed by an unpublished key", foreign, "main", true, codes.Unauthenticated},
		{"tampered token", tampered, "main", true, codes.Unauthenticated},
		{"read-only principal asking ac-write", reader, "main", true, codes.PermissionDenied},
		{"read-only principal reads (not yet written)", reader, "main", false, codes.NotFound},
		{"CI on main writes the action cache", writer, "main", true, codes.OK},
		{"read-only principal reads the result", reader, "main", false, codes.OK},
		{"grants are per instance name", writer, "team-a", true, codes.PermissionDenied},
	} {
		t.Run(r.name, func(t *testing.T) {
			assert.Equal(t, r.want, call(ac, r.token, r.instance, r.write))
		})
	}

	t.Run("deny-listed sid and sub", func(t *testing.T) {
		deny, err := keys.EncodeDenyList([]keys.Revocation{
			{Kind: keys.RevokeSession, Value: writerClaims.Session},
			{Kind: keys.RevokeSubject, Value: readerClaims.Subject},
		}, time.Now())
		require.NoError(t, err)
		ac := boot(string(deny))
		assert.Equal(t, codes.PermissionDenied, call(ac, writer, "main", true), "sid deny-listed")
		assert.Equal(t, codes.PermissionDenied, call(ac, reader, "main", false), "sub deny-listed")
		assert.Equal(t, codes.NotFound, call(ac, developer, "main", false), "other principals unaffected")
	})
}
