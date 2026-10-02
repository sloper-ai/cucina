// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
)

// Site enrollment tokens (R-SEC-3) look like
//
//	cuc_et_<id>_<secret>
//
// with <id> 16 lower-case hex digits (64 bits, the lookup key, not secret) and
// <secret> 43 base64url characters (256 bits). Only SHA-256(secret) is stored;
// a 256-bit random secret needs no slow hash (ADR 0651). The token is
// world-readable on enrolled Macs (managed preferences), which is why its use is
// bounded by expiry, revocation, site binding, a host count and serial-number
// admission.
const (
	tokenPrefix    = "cuc_et_"
	tokenIDLen     = 16
	tokenSecretLen = 43
)

var (
	tokenIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)
	siteRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// newToken returns a fresh token id and secret.
func newToken(r io.Reader) (id, secret string, err error) {
	b := make([]byte, 8+32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(b[:8]), base64.RawURLEncoding.EncodeToString(b[8:]), nil
}

func formatToken(id, secret string) string { return tokenPrefix + id + "_" + secret }

// parseToken splits a token string; ok is false for anything malformed.
func parseToken(s string) (id, secret string, ok bool) {
	if len(s) != len(tokenPrefix)+tokenIDLen+1+tokenSecretLen || s[:len(tokenPrefix)] != tokenPrefix {
		return "", "", false
	}
	rest := s[len(tokenPrefix):]
	id, sep, secret := rest[:tokenIDLen], rest[tokenIDLen], rest[tokenIDLen+1:]
	if sep != '_' || !tokenIDRE.MatchString(id) {
		return "", "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(secret); err != nil {
		return "", "", false
	}
	return id, secret, true
}

// hashSecret is the stored form of a token secret.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte("cucina-site-enrollment-token\x00" + secret))
	return hex.EncodeToString(sum[:])
}

// secretMatches compares in constant time.
func secretMatches(rec TokenRecord, secret string) bool {
	want, err := hex.DecodeString(rec.SecretHash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got, _ := hex.DecodeString(hashSecret(secret))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func validateSite(site string) error {
	if !siteRE.MatchString(site) {
		return fmt.Errorf("site %q must be a lower-case DNS label", site)
	}
	return nil
}
