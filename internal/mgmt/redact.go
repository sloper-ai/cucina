// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Redacted replaces every secret the redactor finds.
const Redacted = "[REDACTED]"

// Redaction is applied to every audit summary and to everything that enters a
// support bundle or a streamed worker log. It works in two layers:
//
//  1. JSON objects: the value of any key that names a secret (token, secret,
//     password, key, credential, authorization, private key, account id, …) is
//     replaced, whatever its type.
//  2. Every string (and every text file): credential-shaped substrings are
//     replaced wherever they appear: JWTs, Cucina keys and tokens (cuc_<kind>_…),
//     PEM private keys, GitHub and AWS keys, HTTP Bearer/Basic credentials, URL
//     user-info, `secret=…`/`"password": "…"` assignments, and account IDs in ARNs.

var (
	secretKeyExact = map[string]bool{
		"key": true, "token": true, "secret": true, "password": true, "passwd": true, "pwd": true,
		"pass": true, "passphrase": true, "authorization": true, "cookie": true, "setcookie": true,
		"credential": true, "credentials": true, "privatekey": true, "accountid": true,
		"awsaccountid": true, "signature": true,
	}
	secretKeySuffixes = []string{
		"token", "secret", "password", "passwd", "passphrase", "privatekey", "apikey", "accesskey",
		"secretkey", "sessionkey", "credential", "credentials", "authorization", "cookie", "keypem",
	}

	secretPatterns = []*regexp.Regexp{
		// PEM private keys of any type (also a truncated block at the end of the input).
		regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`),
		// JWS/JWT compact serialisation: Cucina JWTs, OIDC ID tokens.
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`),
		// Cucina opaque credentials: service keys (cuc_sk_…), enrollment tokens and any cuc_<kind>_… secret.
		regexp.MustCompile(`cuc_[a-z]{2,8}_[A-Za-z0-9_-]{6,}`),
		// GitHub tokens.
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`),
		// AWS access key IDs.
		regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		// HTTP authorization schemes (credentials are long; prose like "basic auth" is not).
		regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`),
	}
	// URL user-info: scheme://user:password@host.
	urlUserInfo = regexp.MustCompile(`://[^/\s:@"']+:[^/\s@"']+@`)
	// Assignments of secret-named keys in text, JSON-in-text, env files and query strings.
	secretAssign = regexp.MustCompile(`(?i)("?(?:password|passwd|passphrase|secret|client[_-]?secret|token|access[_-]?token|refresh[_-]?token|id[_-]?token|subject[_-]?token|session[_-]?token|api[_-]?key|private[_-]?key|aws[_-]?secret[_-]?access[_-]?key|authorization|enroll(?:ment)?[_-]?token|site[_-]?enrollment[_-]?token)"?\s*[:=]\s*)("(?:[^"\\]|\\.)*"|[^\s,;&}"]+)`)
	// The account ID inside an AWS ARN.
	arnAccount = regexp.MustCompile(`(arn:aws[a-zA-Z-]*:[a-z0-9-]*:[a-z0-9-]*:)\d{12}(:)`)
)

// normalizeKey lower-cases a JSON key and drops separators ("client_secret" → "clientsecret").
func normalizeKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		switch r {
		case '_', '-', '.', ' ':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SecretKey reports whether a JSON object key names a secret.
func SecretKey(k string) bool {
	n := normalizeKey(k)
	if secretKeyExact[n] {
		return true
	}
	for _, s := range secretKeySuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// RedactText replaces credential-shaped substrings of s.
func RedactText(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	s = urlUserInfo.ReplaceAllString(s, "://"+Redacted+"@")
	s = secretAssign.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretAssign.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], `"`) {
			return sub[1] + `"` + Redacted + `"`
		}
		return sub[1] + Redacted
	})
	return arnAccount.ReplaceAllString(s, "${1}"+Redacted+"${2}")
}

// RedactBytes is RedactText for byte slices.
func RedactBytes(b []byte) []byte { return []byte(RedactText(string(b))) }

// RedactJSON parses a JSON document and returns it with every secret redacted
// (both layers). Numbers keep their exact text.
func RedactJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(redactValue(v))
}

// redactValue walks decoded JSON, redacting in place.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if SecretKey(k) {
				if x != nil && x != "" {
					t[k] = Redacted
				}
				continue
			}
			t[k] = redactValue(x)
		}
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
	case string:
		return RedactText(t)
	}
	return v
}

// RedactedJSON marshals v to JSON and redacts it.
func RedactedJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return RedactJSON(b)
}
