// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/sloper-ai/cucina/internal/mgmt"
)

// TestRedactionRemovesCredentialShapes guards the redaction behind audit records,
// streamed logs and support bundles (property test, R-TEST-3): credentials of every
// shape Cucina handles, generated at random and embedded at random positions in
// text and in JSON (as a value of a secret-named key and inside free text), never
// survive, while ordinary text and non-secret JSON fields do.
func TestRedactionRemovesCredentialShapes(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const (
		b64    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		alnum  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		digits = "0123456789"
	)
	rnd := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(b)
	}
	// Each shape returns the text to embed and the part that must disappear.
	shapes := map[string]func() (string, string){
		"jwt": func() (string, string) {
			s := "eyJ" + rnd(b64, 20) + "." + rnd(b64, 30) + "." + rnd(b64, 40)
			return s, s
		},
		"service key":      func() (string, string) { s := "cuc_sk_" + rnd(alnum, 6) + "_" + rnd(b64, 43); return s, s },
		"enrollment token": func() (string, string) { s := "cuc_et_" + rnd(alnum, 6) + "_" + rnd(b64, 43); return s, s },
		"github token":     func() (string, string) { s := "ghp_" + rnd(alnum, 36); return s, s },
		"aws access key":   func() (string, string) { s := "AKIA" + rnd(upper, 16); return s, s },
		"bearer":           func() (string, string) { s := rnd(b64, 32); return "Authorization: Bearer " + s, s },
		"pem": func() (string, string) {
			s := rnd(b64, 64)
			return "-----BEGIN PRIVATE KEY-----\n" + s + "\n-----END PRIVATE KEY-----", s
		},
		"url user-info": func() (string, string) {
			s := rnd(alnum, 12)
			return "https://deploy:" + s + "@registry.example.com/v2", s
		},
		"password=":     func() (string, string) { s := rnd(alnum, 14); return "password=" + s, s },
		"client_secret": func() (string, string) { s := rnd(alnum, 24); return `"client_secret": "` + s + `"`, s },
		"arn account": func() (string, string) {
			s := rnd(digits, 12)
			return "arn:aws:iam::" + s + ":role/cucina-controller", s
		},
		"aws secret key=": func() (string, string) { s := rnd(b64, 40); return "aws_secret_access_key = " + s, s },
	}
	words := []string{"worker", "started", "queue", "main", "{", "}", ",", ":", "\n", "\t", "pool=linux-x86-64", "exit 0", "ok"}
	sentence := func() string {
		n := r.IntN(6)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = words[r.IntN(len(words))]
		}
		return strings.Join(parts, " ")
	}
	for range 200 {
		for name, gen := range shapes {
			embed, secret := gen()
			text := sentence() + " " + embed + " " + sentence()
			if out := mgmt.RedactText(text); strings.Contains(out, secret) {
				t.Fatalf("%s survived RedactText: %q", name, out)
			}
			doc, _ := json.Marshal(map[string]any{
				"message": text,
				"nested":  []any{map[string]any{"apiKey": secret, "count": 3, "pool": "linux-x86-64"}},
			})
			out, err := mgmt.RedactJSON(doc)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), secret) {
				t.Fatalf("%s survived RedactJSON: %s", name, out)
			}
			if !strings.Contains(string(out), `"count":3`) || !strings.Contains(string(out), `"pool":"linux-x86-64"`) {
				t.Fatalf("RedactJSON dropped non-secret fields: %s", out)
			}
		}
	}

	// Ordinary text and non-secret keys are left alone.
	for _, s := range []string{
		"basic authentication enabled", "token exchange ok for sa:ci", "queue main has 3 workers",
		`{"tokenTTL":"15m","keyFile":"/etc/cucina/tls.key","key_id":"k1","clientID":"1234.apps"}`,
	} {
		if got := mgmt.RedactText(s); got != s {
			t.Errorf("RedactText(%q) = %q, want unchanged", s, got)
		}
	}
	for key, secret := range map[string]bool{
		"password": true, "clientSecret": true, "client_secret": true, "key": true, "token": true, "subjectToken": true,
		"privateKey": true, "Authorization": true, "accountId": true,
		"key_id": false, "keyFile": false, "tokenTTL": false, "tokens": false, "keys": false, "clientID": false, "subjectTokenType": false,
	} {
		if got := mgmt.SecretKey(key); got != secret {
			t.Errorf("SecretKey(%q) = %v, want %v", key, got, secret)
		}
	}
}
