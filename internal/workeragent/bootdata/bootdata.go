// SPDX-License-Identifier: FSL-1.1-ALv2

// Package bootdata defines the public, non-secret boot data the controller
// puts into an EC2 worker's user data (ports.LaunchRequest.UserData) and that
// `cucina-worker-agent bootstrap` reads from IMDS at every boot (R-POOL-3,
// contracts §5.2).
//
// Boot data tells the agent where the EnrollmentService is and which CA
// verifies it (no trust-on-first-use). It never carries secrets: user data is
// readable by every process on the instance, so Encode refuses any PEM block
// that is not a certificate.
//
// Wire format: compact JSON, at most MaxSize bytes. The version key
// "cucinaBootData" doubles as a type marker (no top-level "version"/"tasks"
// keys, so EC2Launch v2 and cloud-init leave the document alone).
// Compatibility: optional fields may be added under the same version (older
// agents ignore unknown fields); anything an older agent must not ignore
// requires a new version, which older agents refuse.
package bootdata

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Version is the boot data format version written by Encode and the only one
// Decode accepts.
const Version = 1

// MaxSize is the maximum encoded size in bytes.
const MaxSize = 4096

// BootData is the controller → worker boot document.
type BootData struct {
	// Version must equal bootdata.Version (set by Encode when zero).
	Version int `json:"cucinaBootData"`
	// EnrollEndpoint is host:port of the controller's EnrollmentService.
	EnrollEndpoint string `json:"enrollEndpoint"`
	// ServerName is the TLS server name the agent verifies; empty means the
	// host part of EnrollEndpoint.
	ServerName string `json:"serverName,omitempty"`
	// CAPEM is the PEM bundle (one or two CA certificates during rotation) that
	// must verify the EnrollmentService certificate.
	CAPEM string `json:"caPem"`
	// Cluster, Pool and Generation mirror the cucina:cluster, cucina:pool and
	// cucina:generation launch tags (informational; the enrollment response is
	// authoritative).
	Cluster    string `json:"cluster,omitempty"`
	Pool       string `json:"pool,omitempty"`
	Generation string `json:"generation,omitempty"`
}

// ErrTooLarge is returned when the encoded document exceeds MaxSize.
var ErrTooLarge = errors.New("boot data exceeds 4 KiB")

// Encode validates b and returns its compact JSON encoding. A zero Version is
// set to the current Version.
func Encode(b BootData) ([]byte, error) {
	if b.Version == 0 {
		b.Version = Version
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode boot data: %w", err)
	}
	if len(out) > MaxSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(out))
	}
	return out, nil
}

// Decode parses and validates a boot data document (for example the raw EC2
// user data). Surrounding whitespace is ignored; unknown fields are ignored.
func Decode(data []byte) (BootData, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return BootData{}, errors.New("boot data is empty")
	}
	if len(data) > MaxSize {
		return BootData{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(data))
	}
	var b BootData
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&b); err != nil {
		return BootData{}, fmt.Errorf("boot data is not a cucina boot data document: %w", err)
	}
	if dec.More() {
		return BootData{}, errors.New("boot data has trailing content")
	}
	if err := b.Validate(); err != nil {
		return BootData{}, err
	}
	return b, nil
}

// Validate checks the version, the endpoint and that CAPEM holds only
// parseable certificates.
func (b BootData) Validate() error {
	switch {
	case b.Version == 0:
		return errors.New("boot data: missing cucinaBootData version")
	case b.Version != Version:
		return fmt.Errorf("boot data version %d is not supported by this agent (supports %d): rebuild the worker image", b.Version, Version)
	}
	host, port, err := net.SplitHostPort(b.EnrollEndpoint)
	if err != nil || host == "" {
		return fmt.Errorf("boot data: enrollEndpoint %q must be host:port", b.EnrollEndpoint)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("boot data: enrollEndpoint %q has an invalid port", b.EnrollEndpoint)
	}
	if strings.ContainsAny(b.ServerName, " /:") {
		return fmt.Errorf("boot data: serverName %q is not a host name", b.ServerName)
	}
	if _, err := b.CertPool(); err != nil {
		return err
	}
	return nil
}

// TLSServerName returns ServerName, or the host of EnrollEndpoint when empty.
func (b BootData) TLSServerName() string {
	if b.ServerName != "" {
		return b.ServerName
	}
	host, _, err := net.SplitHostPort(b.EnrollEndpoint)
	if err != nil {
		return ""
	}
	return host
}

// CertPool parses CAPEM into a pool. It fails when CAPEM holds no certificate,
// an unparseable certificate, or any non-certificate PEM block (a private key
// in user data would be readable by every process on the instance).
func (b BootData) CertPool() (*x509.CertPool, error) {
	rest := []byte(b.CAPEM)
	pool := x509.NewCertPool()
	n := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("boot data: caPem contains a %q PEM block; only certificates are allowed", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("boot data: caPem certificate %d: %w", n+1, err)
		}
		pool.AddCert(cert)
		n++
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("boot data: caPem has non-PEM content")
	}
	if n == 0 {
		return nil, errors.New("boot data: caPem holds no certificate")
	}
	return pool, nil
}
