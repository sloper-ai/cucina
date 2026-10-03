// SPDX-License-Identifier: FSL-1.1-ALv2

package portscan

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"time"
)

// TLSProof is protocol evidence, NOT peer identity or certificate trust. It
// includes no address, certificate, error text, or other environment data.
type TLSProof struct {
	Port                       int    `json:"port"`
	Version                    uint16 `json:"version,omitempty"`
	CipherSuite                uint16 `json:"cipherSuite,omitempty"`
	HandshakeComplete          bool   `json:"handshakeComplete"`
	ServerCertificateObserved  bool   `json:"serverCertificateObserved"`
	ClientCertificateRequested bool   `json:"clientCertificateRequested"`
	CertificateRequiredAlert   bool   `json:"certificateRequiredAlert"`
	SpeaksTLS                  bool   `json:"speaksTLS"`
}

// ProbeTLS performs a cancellation-aware, at-most-ten-second TLS handshake to
// the exact numeric target/port. A certificate_required error is not sufficient:
// crypto/tls must also have parsed the peer certificate, negotiated a cipher and
// version, and received a real CertificateRequest on this connection.
func ProbeTLS(ctx context.Context, target string, port int, serverName string) (TLSProof, error) {
	proof := TLSProof{Port: port}
	addr, err := Target(target)
	if err != nil {
		return proof, err
	}
	if port < 1 || port > Ports {
		return proof, errors.New("invalid TLS port")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // T10i proves TLS-speaking only, not peer identity.
		VerifyConnection: func(cs tls.ConnectionState) error {
			proof.Version, proof.CipherSuite = cs.Version, cs.CipherSuite
			proof.ServerCertificateObserved = len(cs.PeerCertificates) > 0
			return nil
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			proof.ClientCertificateRequested = true
			return &tls.Certificate{}, nil
		},
	}
	raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	if err != nil {
		return proof, err
	}
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		_ = raw.Close()
		return proof, err
	}
	// Keep cancellation active through close_notify too, not just the handshake.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	conn := tls.Client(raw, cfg)
	defer func() { _ = conn.Close(); stop() }()
	err = conn.HandshakeContext(ctx)
	if ctx.Err() != nil {
		return proof, ctx.Err()
	}
	if err == nil {
		cs := conn.ConnectionState()
		proof.HandshakeComplete = cs.HandshakeComplete
		proof.SpeaksTLS = cs.HandshakeComplete && proof.ServerCertificateObserved && cs.Version >= tls.VersionTLS12 && cs.CipherSuite != 0
		if !proof.SpeaksTLS {
			return proof, errors.New("missing TLS handshake evidence")
		}
		return proof, nil
	}
	var remote *net.OpError
	if proof.ServerCertificateObserved && proof.ClientCertificateRequested && proof.Version >= tls.VersionTLS12 && proof.CipherSuite != 0 &&
		errors.As(err, &remote) && remote.Op == "remote error" && remote.Err.Error() == "tls: certificate required" {
		proof.CertificateRequiredAlert, proof.SpeaksTLS = true, true
		return proof, nil
	}
	return proof, err
}
