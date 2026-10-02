// SPDX-License-Identifier: FSL-1.1-ALv2

// Package redact removes secrets from everything hostd sends off the host as
// diagnostics or logs (R-SEC, docs/dev/hostd.md): PEM blocks (keys and
// certificates), JWTs, Cucina tokens (site enrollment tokens cuc_et_…),
// GitHub/AWS credentials, bearer/basic authorization values, password-like
// assignments (TART_REGISTRY_PASSWORD=…) and any exact secret hostd knows.
package redact

import (
	"bytes"
	"regexp"
)

// Marker replaces every redacted value.
const Marker = "[REDACTED]"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----[\s\S]*?(?:-----END [A-Z0-9 ]+-----|$)`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`),
	regexp.MustCompile(`cuc_[a-z]{2,8}_[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`://[^/\s:@"']+:[^/\s@"']+@`),
}

var assign = regexp.MustCompile(`(?i)("?[A-Za-z_]*(?:password|passwd|secret|token|private[_-]?key|api[_-]?key|authorization)"?\s*[:=]\s*)("(?:[^"\\]|\\.)*"|[^\s,;&}"]+)`)

// Bytes returns b with secrets replaced by Marker. known are exact secret
// values (e.g. the configured site token); empty values are ignored.
func Bytes(b []byte, known ...string) []byte {
	out := b
	for _, k := range known {
		if len(k) >= 6 {
			out = bytes.ReplaceAll(out, []byte(k), []byte(Marker))
		}
	}
	for _, p := range patterns {
		out = p.ReplaceAll(out, []byte(Marker))
	}
	return assign.ReplaceAll(out, []byte("${1}"+Marker))
}
