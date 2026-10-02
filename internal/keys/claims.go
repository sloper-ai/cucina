// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Audience is the fixed `aud` of every Cucina JWT (plain string, contracts §5.1).
const Audience = "buildbarn"

// MaxTokenTTL is the upper bound for Cucina JWTs issued to humans and CI (R-AUTH-3).
const MaxTokenTTL = 15 * time.Minute

// ClockSkew is the skew allowance added whenever a retention period must outlive
// every token that may still be presented (deny-list sid entries, retired keys).
const ClockSkew = 2 * time.Minute

// Scopes is the `cucina` claim: explicit instance-name lists per verb. Buildbarn's
// JMESPath has no `let`, so the lists are always explicit ("*" is expanded by the STS).
// Every list is always present in the JSON (empty as []), sorted and de-duplicated.
type Scopes struct {
	CASRead  []string `json:"cas_read"`
	CASWrite []string `json:"cas_write"`
	ACRead   []string `json:"ac_read"`
	ACWrite  []string `json:"ac_write"`
	Execute  []string `json:"execute"`
	Admin    []string `json:"admin"`
}

// Normalized returns a copy with nil lists replaced by empty ones and every list
// sorted and de-duplicated.
func (s Scopes) Normalized() Scopes {
	n := func(in []string) []string {
		out := make([]string, 0, len(in))
		out = append(out, in...)
		slices.Sort(out)
		return slices.Compact(out)
	}
	return Scopes{
		CASRead: n(s.CASRead), CASWrite: n(s.CASWrite), ACRead: n(s.ACRead),
		ACWrite: n(s.ACWrite), Execute: n(s.Execute), Admin: n(s.Admin),
	}
}

// Claims is the complete claim set of a Cucina JWT (contracts §5.1, R-AUTH-3). There is
// deliberately no `nbf`: Buildbarn applies zero leeway to it.
type Claims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	Expiry   int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	ID       string `json:"jti"`
	Session  string `json:"sid"`
	// Name is the optional display name (TrustPolicy claimMappings.displayName, or the
	// service account) for audit records; Buildbarn ignores it. Never used for decisions.
	Name   string `json:"name,omitempty"`
	Cucina Scopes `json:"cucina"`
}

// MaxNameLen bounds the `name` claim (bytes); longer names are cut at a rune boundary.
const MaxNameLen = 256

// clipName returns name limited to MaxNameLen bytes of valid UTF-8 without control
// characters ("" when nothing printable remains).
func clipName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > MaxNameLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sidPattern is the session id format: 128 bits, base64url without padding.
var sidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// subjectPattern is the subject format shared by the JWT `sub`, the deny-list and
// workload URI SANs (docs/security.md §Deny-list): every byte is one that JSON
// encoders never escape, so a deny-list entry is byte-for-byte its JMESPath needle.
var subjectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[A-Za-z0-9._~:@/+=%-]+$`)

var schemePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// MaxSubjectLen bounds a subject (bytes).
const MaxSubjectLen = 512

// ValidSessionID reports whether s is a well-formed `sid`.
func ValidSessionID(s string) bool { return sidPattern.MatchString(s) }

// ValidSubject reports whether s is a well-formed subject (`<scheme>:<rest>`).
func ValidSubject(s string) bool {
	return len(s) <= MaxSubjectLen && subjectPattern.MatchString(s)
}

// subjectSafe reports whether b may appear unescaped in a subject's rest.
func subjectSafe(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	}
	return strings.IndexByte("._~:@/+=-", b) >= 0
}

// NormalizeSubject turns a mapped principal (for example the result of a TrustPolicy
// subject expression) into the canonical subject: the scheme before the first ':' must
// already be valid; every byte of the rest outside [A-Za-z0-9._~:@/+=-] (including '%')
// is percent-encoded. The mapping is injective. It fails for an empty or over-long result.
func NormalizeSubject(mapped string) (string, error) {
	scheme, rest, ok := strings.Cut(mapped, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("subject must have the form <scheme>:<id>")
	}
	if !schemePattern.MatchString(scheme) {
		return "", fmt.Errorf("subject scheme must match [a-z][a-z0-9-]{0,31}")
	}
	var b strings.Builder
	b.Grow(len(mapped))
	b.WriteString(scheme)
	b.WriteByte(':')
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if subjectSafe(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	out := b.String()
	if !ValidSubject(out) {
		return "", fmt.Errorf("subject longer than %d bytes", MaxSubjectLen)
	}
	return out, nil
}

// NewID returns 128 random bits, base64url without padding (22 characters). It is the
// format of `jti` and of random `sid`s.
func NewID(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
