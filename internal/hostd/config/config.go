// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config is cucina-hostd's configuration: the managed-preferences
// schema of preference domain ai.sloper.cucina.hostd (R-MAC-10,
// docs/dev/hostd.md §3), strict parsing and validation (R-TEST-7 "fail fast on
// configuration") and the overlay of controller-provided HostSettings.
package config

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Domain is the preference domain (and LaunchDaemon label) of hostd.
const Domain = "ai.sloper.cucina.hostd"

// ManagedPlistPath is where MDM custom-settings payloads for Domain land.
const ManagedPlistPath = "/Library/Managed Preferences/" + Domain + ".plist"

// Defaults (docs/dev/hostd.md §3).
const (
	DefaultVMSlots       = 2
	DefaultL2SizeGiB     = 200
	DefaultLogLevel      = "info"
	DefaultTartPath      = "/usr/local/cucina/tart.app/Contents/MacOS/tart"
	DefaultRunAsUser     = "cucina"
	DefaultVMMaxAge      = 7 * 24 * time.Hour
	DefaultMetricsListen = "127.0.0.1:9470"
	DefaultVMNamePrefix  = "cucina-vm-"
)

// Config is the validated hostd configuration.
type Config struct {
	ControllerURL        string
	ControllerServerName string
	// CACertificates are the parsed CA certificates (from CACertificate).
	CACertificates []*x509.Certificate
	// CAPins are SHA-256 digests of DER SubjectPublicKeyInfo (from CAPinSHA256).
	CAPins [][sha256.Size]byte
	// SiteEnrollmentToken is secret: never log it (String redacts it).
	SiteEnrollmentToken string
	IdentityLabel       string
	Site                string
	Labels              map[string]string
	VMSlots             int
	VMCPUCount          int
	VMMemoryGiB         int
	L2SizeGiB           int
	LogLevel            string
	TartPath            string
	RunAsUser           string
	VMMaxAge            time.Duration
	MetricsListen       string
	VMNamePrefix        string
	// Forced records the keys whose value came from a forced (managed) source.
	Forced map[string]bool
}

// Source yields raw preference values. Values use plist's Go representation:
// string, uint64/int64/float64, bool, []byte, []any, map[string]any.
type Source interface {
	// Value returns the value of key, whether it is forced (managed) and whether it exists.
	Value(key string) (v any, forced, ok bool)
	// Keys lists every key present (used to reject unknown keys); nil if unknown.
	Keys() []string
}

// Error is a configuration error naming the offending key.
type Error struct {
	Key string
	Msg string
}

func (e *Error) Error() string {
	if e.Key == "" {
		return "hostd configuration: " + e.Msg
	}
	return fmt.Sprintf("hostd configuration: key %q (domain %s): %s", e.Key, Domain, e.Msg)
}

func keyErr(key, format string, a ...any) error {
	return &Error{Key: key, Msg: fmt.Sprintf(format, a...)}
}

type keySpec struct {
	name  string
	apply func(c *Config, v any) error
}

var schema = []keySpec{
	{"ControllerURL", func(c *Config, v any) (err error) { c.ControllerURL, err = str("ControllerURL", v); return }},
	{"ControllerServerName", func(c *Config, v any) (err error) {
		c.ControllerServerName, err = str("ControllerServerName", v)
		return
	}},
	{"CACertificate", applyCA},
	{"CAPinSHA256", applyPins},
	{"SiteEnrollmentToken", func(c *Config, v any) (err error) {
		c.SiteEnrollmentToken, err = str("SiteEnrollmentToken", v)
		return
	}},
	{"IdentityLabel", func(c *Config, v any) (err error) { c.IdentityLabel, err = str("IdentityLabel", v); return }},
	{"Site", func(c *Config, v any) (err error) { c.Site, err = str("Site", v); return }},
	{"Labels", applyLabels},
	{"VMSlots", intIn("VMSlots", 1, 2, func(c *Config, n int) { c.VMSlots = n })},
	{"VMCPUCount", intIn("VMCPUCount", 0, 1024, func(c *Config, n int) { c.VMCPUCount = n })},
	{"VMMemoryGiB", intIn("VMMemoryGiB", 0, 4096, func(c *Config, n int) { c.VMMemoryGiB = n })},
	{"L2SizeGiB", intIn("L2SizeGiB", 10, 4096, func(c *Config, n int) { c.L2SizeGiB = n })},
	{"LogLevel", func(c *Config, v any) error {
		s, err := str("LogLevel", v)
		if err != nil {
			return err
		}
		switch s {
		case "debug", "info", "warn", "error":
			c.LogLevel = s
			return nil
		}
		return keyErr("LogLevel", "must be one of debug, info, warn, error (got %q)", s)
	}},
	{"TartPath", func(c *Config, v any) error {
		s, err := str("TartPath", v)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(s) {
			return keyErr("TartPath", "must be an absolute path (got %q)", s)
		}
		c.TartPath = s
		return nil
	}},
	{"RunAsUser", func(c *Config, v any) error {
		s, err := str("RunAsUser", v)
		if err != nil {
			return err
		}
		if s == "root" || !userRe.MatchString(s) {
			return keyErr("RunAsUser", "must be a non-root short user name (got %q)", s)
		}
		c.RunAsUser = s
		return nil
	}},
	{"VMMaxAgeHours", intIn("VMMaxAgeHours", 1, 2160, func(c *Config, n int) { c.VMMaxAge = time.Duration(n) * time.Hour })},
	{"MetricsListen", func(c *Config, v any) error {
		s, err := str("MetricsListen", v)
		if err != nil {
			return err
		}
		if s == "" {
			c.MetricsListen = "" // disabled
			return nil
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			return keyErr("MetricsListen", "must be host:port (%v)", err)
		}
		c.MetricsListen = s
		return nil
	}},
	{"VMNamePrefix", func(c *Config, v any) error {
		s, err := str("VMNamePrefix", v)
		if err != nil {
			return err
		}
		if !prefixRe.MatchString(s) {
			return keyErr("VMNamePrefix", "must match %s (got %q)", prefixRe, s)
		}
		c.VMNamePrefix = s
		return nil
	}},
}

var (
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}-$`)
	labelRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._/-]{0,61}[A-Za-z0-9])?$`)
)

// Keys returns the schema's key names (sorted).
func Keys() []string {
	out := make([]string, 0, len(schema))
	for _, k := range schema {
		out = append(out, k.name)
	}
	sort.Strings(out)
	return out
}

// Default returns the configuration with every default applied and nothing required set.
func Default() Config {
	return Config{
		Labels:        map[string]string{},
		VMSlots:       DefaultVMSlots,
		L2SizeGiB:     DefaultL2SizeGiB,
		LogLevel:      DefaultLogLevel,
		TartPath:      DefaultTartPath,
		RunAsUser:     DefaultRunAsUser,
		VMMaxAge:      DefaultVMMaxAge,
		MetricsListen: DefaultMetricsListen,
		VMNamePrefix:  DefaultVMNamePrefix,
		Forced:        map[string]bool{},
	}
}

// Load reads every schema key from src over the defaults, rejects unknown keys
// (keys starting with "Payload" are tolerated: some MDMs copy payload metadata)
// and validates the result. Errors name the key.
func Load(src Source) (Config, error) {
	c := Default()
	known := map[string]bool{}
	for _, k := range schema {
		known[k.name] = true
	}
	var errs []error
	for _, k := range src.Keys() {
		if !known[k] && !strings.HasPrefix(k, "Payload") {
			errs = append(errs, keyErr(k, "unknown key (known keys: %s)", strings.Join(Keys(), ", ")))
		}
	}
	for _, k := range schema {
		v, forced, ok := src.Value(k.name)
		if !ok {
			continue
		}
		if err := k.apply(&c, v); err != nil {
			errs = append(errs, err)
			continue
		}
		if forced {
			c.Forced[k.name] = true
		}
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks cross-key rules.
func (c Config) Validate() error {
	if c.ControllerURL == "" {
		return keyErr("ControllerURL", "is required")
	}
	u, err := url.Parse(c.ControllerURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") {
		return keyErr("ControllerURL", "must be https://host:port (got %q)", c.ControllerURL)
	}
	if len(c.CACertificates) == 0 && len(c.CAPins) == 0 {
		return keyErr("CACertificate", "one of CACertificate or CAPinSHA256 is required to verify the controller")
	}
	if c.VMSlots < 1 || c.VMSlots > 2 {
		return keyErr("VMSlots", "must be 1 or 2")
	}
	return nil
}

// EnrollAddress is host:port of the enrollment endpoint.
func (c Config) EnrollAddress() string {
	u, _ := url.Parse(c.ControllerURL)
	return u.Host
}

// ServerName is the TLS server name for the controller.
func (c Config) ServerName() string {
	if c.ControllerServerName != "" {
		return c.ControllerServerName
	}
	u, _ := url.Parse(c.ControllerURL)
	return u.Hostname()
}

// String renders the configuration without secrets.
func (c Config) String() string {
	tok := "unset"
	if c.SiteEnrollmentToken != "" {
		tok = "set (redacted)"
	}
	return fmt.Sprintf("controller=%s serverName=%s ca=%d pins=%d token=%s site=%q slots=%d cpu=%d memGiB=%d l2GiB=%d log=%s tart=%s user=%s",
		c.ControllerURL, c.ServerName(), len(c.CACertificates), len(c.CAPins), tok, c.Site, c.VMSlots, c.VMCPUCount,
		c.VMMemoryGiB, c.L2SizeGiB, c.LogLevel, c.TartPath, c.RunAsUser)
}

func str(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", keyErr(key, "must be a string (got %T)", v)
	}
	return strings.TrimSpace(s), nil
}

func toInt(key string, v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case uint64:
		if n > 1<<31 {
			return 0, keyErr(key, "out of range")
		}
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, keyErr(key, "must be an integer (got %v)", n)
		}
		return int(n), nil
	}
	return 0, keyErr(key, "must be an integer (got %T)", v)
}

func intIn(key string, lo, hi int, set func(*Config, int)) func(*Config, any) error {
	return func(c *Config, v any) error {
		n, err := toInt(key, v)
		if err != nil {
			return err
		}
		if n < lo || n > hi {
			return keyErr(key, "must be between %d and %d (got %d)", lo, hi, n)
		}
		set(c, n)
		return nil
	}
}

func applyCA(c *Config, v any) error {
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		return keyErr("CACertificate", "must be a string (PEM) or data (PEM/DER) (got %T)", v)
	}
	certs, err := ParseCertificates(raw)
	if err != nil {
		return keyErr("CACertificate", "%v", err)
	}
	for _, cert := range certs {
		if !cert.IsCA {
			return keyErr("CACertificate", "certificate %q is not a CA", cert.Subject.String())
		}
	}
	c.CACertificates = certs
	return nil
}

// ParseCertificates parses PEM (one or more CERTIFICATE blocks) or a single DER certificate.
func ParseCertificates(raw []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := raw
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %q", b.Type)
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, errors.New("neither PEM certificates nor a DER certificate")
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

func applyPins(c *Config, v any) error {
	var items []string
	switch t := v.(type) {
	case string:
		items = []string{t}
	case []any:
		for _, it := range t {
			s, ok := it.(string)
			if !ok {
				return keyErr("CAPinSHA256", "array elements must be strings (got %T)", it)
			}
			items = append(items, s)
		}
	default:
		return keyErr("CAPinSHA256", "must be a string or an array of strings (got %T)", v)
	}
	for _, s := range items {
		s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, ":", "")))
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != sha256.Size {
			return keyErr("CAPinSHA256", "must be 64 hex characters (SHA-256 of the DER SubjectPublicKeyInfo)")
		}
		var pin [sha256.Size]byte
		copy(pin[:], b)
		c.CAPins = append(c.CAPins, pin)
	}
	return nil
}

func applyLabels(c *Config, v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return keyErr("Labels", "must be a dictionary of strings (got %T)", v)
	}
	labels := map[string]string{}
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return keyErr("Labels", "value of %q must be a string (got %T)", k, raw)
		}
		if !labelRe.MatchString(k) || len(s) > 63 {
			return keyErr("Labels", "invalid label %q=%q", k, s)
		}
		labels[k] = s
	}
	c.Labels = labels
	return nil
}

// SPKIPin returns the pin (SHA-256 of the DER SubjectPublicKeyInfo) of cert.
func SPKIPin(cert *x509.Certificate) [sha256.Size]byte {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}
