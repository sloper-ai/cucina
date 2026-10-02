// SPDX-License-Identifier: FSL-1.1-ALv2

package report

import (
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Rule is one redaction pattern (§12: "Redact environment identifiers from
// committed reports").
type Rule struct {
	Name string
	re   *regexp.Regexp
	// keep returns true for matches that are not identifiers (allow-list).
	keep func(match string) bool
	// repl renders the replacement (default "<redacted:name>").
	repl func(match string) string
}

func placeholder(name string) func(string) string {
	return func(string) string { return "<redacted:" + name + ">" }
}

// Rules are applied in order. They cover the AWS account ID, IPv4/IPv6
// addresses, EC2 resource IDs, AWS/EC2 host names, local host names, home
// directories, e-mail addresses, presigned-URL signatures and credentials
// (JWTs, service keys) that must never appear in a committed report.
var Rules = []Rule{
	{Name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{Name: "service-key", re: regexp.MustCompile(`\bcuc_sk_[A-Za-z0-9_-]+`)},
	{Name: "presigned-query", re: regexp.MustCompile(`(?i)\?[^\s"'<>]*X-Amz-(?:Credential|Signature|Security-Token)=[^\s"'<>]*`),
		repl: func(string) string { return "?<redacted:presigned-query>" }},
	{Name: "ecr-registry", re: regexp.MustCompile(`\b\d{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com\b`)},
	{Name: "aws-account", re: regexp.MustCompile(`(arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:)\d{12}(:)`),
		repl: func(m string) string {
			sub := regexp.MustCompile(`(arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:)\d{12}(:)`).FindStringSubmatch(m)
			return sub[1] + "<redacted:aws-account>" + sub[2]
		}},
	{Name: "aws-account", re: regexp.MustCompile(`(?i)\b(account(?:[ _-]?id)?["':= ]+)\d{12}\b`),
		repl: func(m string) string { return m[:len(m)-12] + "<redacted:aws-account>" }},
	{Name: "ec2-hostname", re: regexp.MustCompile(`\b(?:ip|ec2)-\d{1,3}-\d{1,3}-\d{1,3}-\d{1,3}\.[a-z0-9.-]*(?:compute\.internal|amazonaws\.com)\b`)},
	{Name: "aws-resource-id", re: regexp.MustCompile(`\b(?:i|vol|eni|sg|subnet|vpc|ami|snap|eipalloc|eipassoc|igw|eigw|rtb|nat|lt|vpce|acl|r)-[0-9a-f]{8,17}\b`)},
	{Name: "email", re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
		keep: func(m string) bool {
			l := strings.ToLower(m)
			return strings.HasSuffix(l, "@example.com") || strings.HasSuffix(l, "@example.test") || strings.HasSuffix(l, ".example.com")
		}},
	{Name: "local-hostname", re: regexp.MustCompile(`\b[A-Za-z0-9][A-Za-z0-9-]*\.local\b`),
		keep: func(m string) bool { return m == "cluster.local" }},
	{Name: "home-dir", re: regexp.MustCompile(`(/Users/|/home/|C:\\Users\\)([A-Za-z0-9._-]+)`),
		keep: func(m string) bool {
			for _, ok := range []string{"/home/ubuntu", "/Users/Shared", `C:\Users\Public`, "/home/runner"} {
				if strings.HasPrefix(m, ok) {
					return true
				}
			}
			return false
		},
		repl: func(m string) string {
			sub := regexp.MustCompile(`(/Users/|/home/|C:\\Users\\)([A-Za-z0-9._-]+)`).FindStringSubmatch(m)
			return sub[1] + "<redacted:user>"
		}},
	{Name: "ipv4", re: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), keep: keepIPv4},
	{Name: "ipv6", re: regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}(?::|[0-9a-f]{1,4})(?:[0-9a-f:]*)`), keep: keepIPv6},
}

// keepIPv4 lets through non-addresses (version strings with an octet > 255)
// and addresses that identify nothing (loopback, unspecified, documentation
// ranges, the k3s pod/service defaults are still redacted).
func keepIPv4(m string) bool {
	parts := strings.Split(m, ".")
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n > 255 || (len(p) > 1 && p[0] == '0') {
			return true
		}
	}
	ip := net.ParseIP(m)
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	for _, doc := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
		_, n, _ := net.ParseCIDR(doc)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// keepIPv6 lets through things that look like addresses but are not (clock
// times like 10:15:30 have no hex letters and no "::").
func keepIPv6(m string) bool {
	m = strings.TrimRight(m, ":")
	if !strings.Contains(m, "::") && !strings.ContainsAny(strings.ToLower(m), "abcdef") {
		return true
	}
	ip := net.ParseIP(m)
	if ip == nil {
		// Not a parseable address (e.g. a hex-looking fragment): redact only
		// clear address shapes with at least 4 groups.
		return strings.Count(m, ":") < 3
	}
	return ip.IsLoopback() || ip.IsUnspecified() || strings.HasPrefix(strings.ToLower(m), "2001:db8:")
}

// Finding is one identifier found in a text.
type Finding struct {
	Rule  string `json:"rule"`
	Match string `json:"match"`
	Line  int    `json:"line"`
}

// Redactor replaces identifiers; Literals adds exact strings known from the
// environment descriptor (instance IDs, IPs, the account, host names).
type Redactor struct {
	Literals map[string]string // literal → name
}

// Apply redacts text and returns the result with the findings it replaced.
func (r Redactor) Apply(text string) (string, []Finding) {
	var findings []Finding
	lits := make([]string, 0, len(r.Literals))
	for l := range r.Literals {
		if len(l) >= 4 {
			lits = append(lits, l)
		}
	}
	sort.Slice(lits, func(i, j int) bool { return len(lits[i]) > len(lits[j]) })
	for _, l := range lits {
		if strings.Contains(text, l) {
			findings = append(findings, Finding{Rule: "literal:" + r.Literals[l], Match: l})
			text = strings.ReplaceAll(text, l, "<redacted:"+r.Literals[l]+">")
		}
	}
	for _, rule := range Rules {
		repl := rule.repl
		if repl == nil {
			repl = placeholder(rule.Name)
		}
		text = rule.re.ReplaceAllStringFunc(text, func(m string) string {
			if rule.keep != nil && rule.keep(m) {
				return m
			}
			findings = append(findings, Finding{Rule: rule.Name, Match: m})
			return repl(m)
		})
	}
	return text, findings
}

// Check reports identifiers left in text, with line numbers (the pre-commit
// gate for reports: `e2e redact-check docs/reports/*.md`).
func Check(text string) []Finding {
	var out []Finding
	for i, line := range strings.Split(text, "\n") {
		_, fs := Redactor{}.Apply(line)
		for _, f := range fs {
			f.Line = i + 1
			out = append(out, f)
		}
	}
	return out
}
