// SPDX-License-Identifier: FSL-1.1-ALv2

package keys_test

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/jmespath/go-jmespath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

const issuer = "https://cucina.example.com"

func newKeySet(t testing.TB) *keys.KeySet {
	t.Helper()
	kid, priv, err := keys.GenerateKey(nil)
	require.NoError(t, err)
	ks, err := keys.NewKeySet(keys.Bootstrap(kid, t0), map[string]*ecdsa.PrivateKey{kid: priv})
	require.NoError(t, err)
	return ks
}

func decodeSegment(t testing.TB, seg string) map[string]any {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func sortedKeys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestJWTClaimContract guards the Cucina JWT contract (contracts §5.1, R-AUTH-3): ES256
// with kid, exactly the specified claims, aud a plain string, no nbf, explicit verb lists
// (never null), TTL 15 min.
func TestJWTClaimContract(t *testing.T) {
	ks := newKeySet(t)
	kid, _, _ := ks.Signing()
	m := &keys.Minter{Issuer: issuer, Keys: keys.StaticKeys{Set: ks}, Clock: keystest.NewClock(t0)}
	raw, c, err := m.Mint(keys.MintRequest{Subject: "google:123", Scopes: keys.Scopes{ACRead: []string{"main"}, CASRead: []string{"main", "main"}}, TTL: keys.MaxTokenTTL})
	require.NoError(t, err)

	parts := strings.Split(raw, ".")
	require.Len(t, parts, 3)
	assert.Equal(t, map[string]any{"alg": "ES256", "kid": kid, "typ": "JWT"}, decodeSegment(t, parts[0]))
	p := decodeSegment(t, parts[1])
	assert.Equal(t, []string{"aud", "cucina", "exp", "iat", "iss", "jti", "sid", "sub"}, sortedKeys(p))
	assert.Equal(t, "buildbarn", p["aud"], "aud is a plain string")
	assert.Equal(t, issuer, p["iss"])
	assert.Equal(t, "google:123", p["sub"])
	assert.Equal(t, float64(15*60), p["exp"].(float64)-p["iat"].(float64))
	assert.Equal(t, float64(t0.Unix()), p["iat"])
	assert.True(t, keys.ValidSessionID(p["sid"].(string)))
	assert.Regexp(t, `^[A-Za-z0-9_-]{22}$`, p["jti"])
	cucina := p["cucina"].(map[string]any)
	assert.Equal(t, []string{"ac_read", "ac_write", "admin", "cas_read", "cas_write", "execute"}, sortedKeys(cucina))
	assert.Equal(t, []any{"main"}, cucina["cas_read"], "de-duplicated")
	assert.Equal(t, []any{}, cucina["ac_write"], "empty lists are [], never null")
	assert.Equal(t, c.ID, p["jti"])

	// The optional display name (audit only) is the one additive claim; it is bounded.
	named, _, err := m.Mint(keys.MintRequest{Subject: "google:123", Name: "Alice Example\n" + strings.Repeat("é", 200), TTL: time.Minute})
	require.NoError(t, err)
	np := decodeSegment(t, strings.Split(named, ".")[1])
	assert.Equal(t, []string{"aud", "cucina", "exp", "iat", "iss", "jti", "name", "sid", "sub"}, sortedKeys(np))
	assert.True(t, strings.HasPrefix(np["name"].(string), "Alice Example"))
	assert.NotContains(t, np["name"], "\n")
	assert.LessOrEqual(t, len(np["name"].(string)), keys.MaxNameLen)
	vn, err := (&keys.Verifier{Issuer: issuer, Keys: keys.StaticKeys{Set: ks}, Clock: keystest.NewClock(t0)}).Verify(named)
	require.NoError(t, err)
	assert.Equal(t, np["name"], vn.Name)

	// The verifier accepts it; a second token gets a fresh jti and sid.
	v := &keys.Verifier{Issuer: issuer, Keys: keys.StaticKeys{Set: ks}, Clock: keystest.NewClock(t0)}
	got, err := v.Verify(raw)
	require.NoError(t, err)
	assert.Equal(t, c, *got)
	_, c2, _ := m.Mint(keys.MintRequest{Subject: "google:123", TTL: time.Minute})
	assert.NotEqual(t, c.ID, c2.ID)
	assert.NotEqual(t, c.Session, c2.Session)

	// Invalid mint requests fail closed.
	for _, req := range []keys.MintRequest{
		{Subject: "google:123", TTL: 16 * time.Minute},
		{Subject: "google:123", TTL: 0},
		{Subject: "no-scheme", TTL: time.Minute},
		{Subject: `google:"quote"`, TTL: time.Minute},
		{Subject: "google:1", Session: "short", TTL: time.Minute},
	} {
		_, _, err := m.Mint(req)
		assert.Error(t, err, "%+v", req)
	}
}

// bbFrontend reproduces bb-storage's jwt.AuthorizationHeaderParser and
// JMESPathExpressionAuthorizer at the pinned tag (20260930T153215Z-086b011): header regex,
// kid-demultiplexed signature check from the JWKS file, claims validation and metadata
// extraction over {"payload": …}, exp/nbf with zero leeway, authorizers over
// {"authenticationMetadata", "instanceName", "files"} where files are raw strings.
type bbFrontend struct {
	jwks     jose.JSONWebKeySet
	issuer   string
	now      time.Time
	denyList *string // nil: no files configured
}

var bbHeader = regexp.MustCompile(`^Bearer\s+(([-_a-zA-Z0-9]+)\.([-_a-zA-Z0-9]+))\.([-_a-zA-Z0-9]+)$`)

func (b *bbFrontend) authenticate(t testing.TB, header string) (map[string]any, bool) {
	m := bbHeader.FindStringSubmatch(header)
	if m == nil {
		return nil, false
	}
	var fields [][]byte
	for _, f := range m[2:] {
		d, err := base64.RawURLEncoding.DecodeString(f)
		if err != nil {
			return nil, false
		}
		fields = append(fields, d)
	}
	var h struct {
		Alg string  `json:"alg"`
		Kid *string `json:"kid"`
	}
	if json.Unmarshal(fields[0], &h) != nil || h.Alg != "ES256" || len(fields[2]) != 64 {
		return nil, false
	}
	digest := sha256.Sum256([]byte(m[1]))
	r, s := new(big.Int).SetBytes(fields[2][:32]), new(big.Int).SetBytes(fields[2][32:])
	ok := false
	for _, k := range b.jwks.Keys {
		if h.Kid != nil && k.KeyID != *h.Kid {
			continue
		}
		if pub, isEC := k.Key.(*ecdsa.PublicKey); isEC && ecdsa.Verify(pub, digest[:], r, s) {
			ok = true
		}
	}
	if !ok {
		return nil, false
	}
	var payload any
	if json.Unmarshal(fields[1], &payload) != nil {
		return nil, false
	}
	in := map[string]any{"payload": payload}
	valid, err := jmespath.Search(keys.BuildbarnClaimsValidation(b.issuer), in)
	if err != nil || valid != true {
		return nil, false
	}
	var times struct {
		Exp *json.Number `json:"exp"`
		Nbf *json.Number `json:"nbf"`
	}
	require.NoError(t, json.Unmarshal(fields[1], &times))
	if times.Nbf != nil {
		nbf, _ := times.Nbf.Int64()
		if b.now.Before(time.Unix(nbf, 0)) {
			return nil, false
		}
	}
	if times.Exp != nil {
		exp, _ := times.Exp.Int64()
		if !b.now.Before(time.Unix(exp, 0)) {
			return nil, false
		}
	}
	md, err := jmespath.Search(keys.BuildbarnMetadataExtraction, in)
	if err != nil {
		return nil, false
	}
	// NewAuthenticationMetadataFromRaw: a JSON round trip through the proto (only
	// "public", "private" and "tracingAttributes" are allowed).
	j, _ := json.Marshal(md)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(j, &raw))
	for k := range raw {
		if k != "public" && k != "private" {
			return nil, false
		}
	}
	return raw, true
}

func (b *bbFrontend) authorize(md map[string]any, claimKey, instance string) bool {
	in := map[string]any{"authenticationMetadata": md, "instanceName": instance}
	if b.denyList != nil {
		in["files"] = map[string]any{keys.DenyListFileKey: *b.denyList}
	}
	res, err := jmespath.Search(keys.BuildbarnAuthorizer(claimKey), in)
	return err == nil && res == true
}

// TestBuildbarnJWTPolicy guards R-AUTH-4/-9: real Cucina tokens are accepted or rejected by
// the rendered Buildbarn `jwt` policy and per-operation authorizers exactly as specified,
// including a read-only principal asking for ac-write, and the deny-list (until the
// bbtest harness boots the pinned bb_storage binary with the same expressions).
func TestBuildbarnJWTPolicy(t *testing.T) {
	ks := newKeySet(t)
	other := newKeySet(t)
	clock := keystest.NewClock(t0)
	minter := &keys.Minter{Issuer: issuer, Keys: keys.StaticKeys{Set: ks}, Clock: clock}
	mint := func(m *keys.Minter, sub, sid string, s keys.Scopes, ttl time.Duration) string {
		raw, _, err := m.Mint(keys.MintRequest{Subject: sub, Session: sid, Scopes: s, TTL: ttl})
		require.NoError(t, err)
		return raw
	}
	const sid = "AAAAAAAAAAAAAAAAAAAAAA"
	reader := mint(minter, "github:1401027334:pr", "", keys.Scopes{CASRead: []string{"main"}, ACRead: []string{"main"}}, 15*time.Minute)
	writer := mint(minter, "github:1401027334:CI", sid, keys.Scopes{CASRead: []string{"main"}, ACRead: []string{"main"}, ACWrite: []string{"main"}}, 15*time.Minute)
	short := mint(minter, "google:short", "", keys.Scopes{ACRead: []string{"main"}}, time.Minute)
	foreign := mint(&keys.Minter{Issuer: issuer, Keys: keys.StaticKeys{Set: other}, Clock: clock}, "google:x", "", keys.Scopes{ACWrite: []string{"main"}}, time.Minute)
	wrongIss := mint(&keys.Minter{Issuer: "https://evil.example.com", Keys: keys.StaticKeys{Set: ks}, Clock: clock}, "google:x", "", keys.Scopes{ACWrite: []string{"main"}}, time.Minute)
	tampered := func(raw string) string {
		parts := strings.Split(raw, ".")
		p := decodeSegment(t, parts[1])
		p["cucina"].(map[string]any)["ac_write"] = []string{"main"}
		b, _ := json.Marshal(p)
		parts[1] = base64.RawURLEncoding.EncodeToString(b)
		return strings.Join(parts, ".")
	}(reader)

	empty := keys.EmptyDenyList
	denySid, err := keys.EncodeDenyList([]keys.Revocation{{Kind: keys.RevokeSession, Value: sid}}, t0)
	require.NoError(t, err)
	denySub, err := keys.EncodeDenyList([]keys.Revocation{{Kind: keys.RevokeSubject, Value: "github:1401027334:CI"}}, t0)
	require.NoError(t, err)
	denyPrefix, err := keys.EncodeDenyList([]keys.Revocation{{Kind: keys.RevokeSubject, Value: "github:1401027334:C"}, {Kind: keys.RevokeSubject, Value: "github:1401027334:CI-nightly"}}, t0)
	require.NoError(t, err)
	str := func(b []byte) *string { s := string(b); return &s }

	rows := []struct {
		name      string
		token     string
		advance   time.Duration
		denyList  *string
		claimKey  string
		instance  string
		authn     bool
		permitted bool
	}{
		{"writer may write the AC", writer, 0, &empty, "ac_write", "main", true, true},
		{"read-only principal asking ac-write is denied", reader, 0, &empty, "ac_write", "main", true, false},
		{"read-only principal may read the AC", reader, 0, &empty, "ac_read", "main", true, true},
		{"grant is per instance name", writer, 0, &empty, "ac_write", "team-a", true, false},
		{"expired token", short, time.Minute, &empty, "ac_read", "main", false, false},
		{"unknown signing key (kid)", foreign, 0, &empty, "ac_write", "main", false, false},
		{"wrong issuer", wrongIss, 0, &empty, "ac_write", "main", false, false},
		{"tampered payload", tampered, 0, &empty, "ac_write", "main", false, false},
		{"unauthenticated request", "", 0, &empty, "ac_read", "main", false, false},
		{"deny-listed sid", writer, 0, str(denySid), "ac_write", "main", true, false},
		{"deny-listed sub", writer, 0, str(denySub), "ac_read", "main", true, false},
		{"deny-list matches exactly (no prefix match)", writer, 0, str(denyPrefix), "ac_write", "main", true, true},
		{"other principals are unaffected by a deny entry", reader, 0, str(denySid), "ac_read", "main", true, true},
		{"missing deny-list file denies", writer, 0, nil, "ac_write", "main", true, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			bb := &bbFrontend{jwks: ks.JWKS(), issuer: issuer, now: t0.Add(r.advance), denyList: r.denyList}
			header := ""
			if r.token != "" {
				header = "Bearer " + r.token
			}
			md, ok := bb.authenticate(t, header)
			require.Equal(t, r.authn, ok, "authentication")
			if ok {
				assert.Equal(t, r.permitted, bb.authorize(md, r.claimKey, r.instance), "authorization")
			}
		})
	}

	t.Run("certificate principals share the authorizers", func(t *testing.T) {
		// Metadata of a worker certificate (docs/security.md §Workload identity).
		md := map[string]any{"public": map[string]any{"user": "spiffe://cucina/worker/linux-x64/i-0abc"},
			"private": map[string]any{"sub": "spiffe://cucina/worker/linux-x64/i-0abc", "ac_write": []any{"main"}}}
		bb := &bbFrontend{denyList: &empty}
		assert.True(t, bb.authorize(md, "ac_write", "main"))
		deny, err := keys.EncodeDenyList([]keys.Revocation{{Kind: keys.RevokeSubject, Value: "spiffe://cucina/worker/linux-x64/i-0abc"}}, t0)
		require.NoError(t, err)
		bb.denyList = str(deny)
		assert.False(t, bb.authorize(md, "ac_write", "main"))
	})
}
