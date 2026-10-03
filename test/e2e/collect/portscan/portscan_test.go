// SPDX-License-Identifier: FSL-1.1-ALv2

package portscan_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/collect/portscan"
)

// Guards T10i's observed false-pass boundary: only a successful, complete scan
// of one intended host plus actual TLS protocol evidence qualifies. No nmap is
// run: scan rows consume captured-format data; TLS rows use localhost fixtures.
func TestT10iEvidence(t *testing.T) {
	const valid = `<nmaprun scanner="nmap" version="7.991">
<scaninfo type="connect" protocol="tcp" numservices="65535" services="1-65535"/>
<host><status state="up"/><address addr="203.0.113.7" addrtype="ipv4"/>
<ports><extraports state="filtered" count="65534"><extrareasons reason="no-responses" count="65534" proto="tcp" ports="1-442,444-65535"/></extraports>
<port protocol="tcp" portid="443"><state state="open" reason="syn-ack"/></port></ports></host>
<runstats><finished time="2" elapsed="1" exit="success"/><hosts up="1" down="0" total="1"/></runstats></nmaprun>`
	type testCase struct {
		name string
		run  func(*testing.T)
	}
	change := func(old, next string) func(*portscan.Evidence) {
		return func(e *portscan.Evidence) { e.XML = []byte(strings.Replace(string(e.XML), old, next, 1)) }
	}
	scan := func(name string, edit func(*portscan.Evidence), want bool) testCase {
		return testCase{name, func(t *testing.T) {
			e := portscan.Evidence{Target: "203.0.113.7", PositivePort: 443, XML: []byte(valid)}
			if edit != nil {
				edit(&e)
			}
			r, err := portscan.Read(e)
			if !want {
				require.Error(t, err, "partial or ambiguous output cannot certify T10i")
				require.Zero(t, r.PortsScanned)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 65535, r.PortsScanned)
			require.Equal(t, []int{443}, r.OpenPorts)
		}}
	}
	tlsCase := func(name string, clientCert, plaintext, cancelled bool) testCase {
		return testCase{name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			addr := "127.0.0.1:1"
			if cancelled {
				cancel()
			} else if plaintext {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				t.Cleanup(func() { _ = ln.Close() })
				addr = ln.Addr().String()
				go func() {
					c, err := ln.Accept()
					if err == nil {
						_, _ = io.WriteString(c, "tls: certificate required\r\n")
						_ = c.Close()
					}
				}()
			} else {
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
				if clientCert {
					srv.TLS.ClientAuth = tls.RequireAnyClientCert
				}
				srv.StartTLS()
				t.Cleanup(srv.Close)
				addr = srv.Listener.Addr().String()
			}
			host, number, err := net.SplitHostPort(addr)
			require.NoError(t, err)
			port, err := strconv.Atoi(number)
			require.NoError(t, err)
			proof, err := portscan.ProbeTLS(ctx, host, port, "localhost")
			if plaintext || cancelled {
				require.Error(t, err)
				require.False(t, proof.SpeaksTLS)
				if cancelled {
					require.ErrorIs(t, err, context.Canceled)
				}
				return
			}
			require.NoError(t, err)
			require.True(t, proof.SpeaksTLS)
			require.True(t, proof.ServerCertificateObserved)
			require.NotZero(t, proof.CipherSuite)
			if clientCert {
				require.True(t, proof.ClientCertificateRequested)
			}
		}}
	}
	for _, tc := range []testCase{
		scan("complete", nil, true),
		scan("aggregate without reason ranges", change(` ports="1-442,444-65535"`, ""), true),
		scan("split full declared range", change(`services="1-65535"`, `services="1-1024,1025-65535"`), true),
		scan("hostname is not one numeric target", func(e *portscan.Evidence) { e.Target = "example.invalid" }, false),
		scan("range is not one numeric target", func(e *portscan.Evidence) { e.Target = "203.0.113.0/24" }, false),
		scan("IPv6 is not the declared scope", func(e *portscan.Evidence) { e.Target = "2001:db8::7" }, false),
		scan("scanner nonzero exit", func(e *portscan.Evidence) { e.ExitCode = 1 }, false),
		scan("local socket exhaustion with exit zero", func(e *portscan.Evidence) { e.Stderr = []byte("socket: Too many open files") }, false),
		scan("truncated after an open port", change(`</runstats></nmaprun>`, ""), false),
		scan("trailing second document", func(e *portscan.Evidence) { e.XML = append(e.XML, []byte(valid)...) }, false),
		scan("partial declared range", change(`numservices="65535" services="1-65535"`, `numservices="1024" services="1-1024"`), false),
		scan("duplicated declared port", change(`services="1-65535"`, `services="1-65535,443"`), false),
		scan("missing scan declaration", change(`<scaninfo type="connect" protocol="tcp" numservices="65535" services="1-65535"/>`, ""), false),
		scan("wrong host", change(`addr="203.0.113.7"`, `addr="203.0.113.8"`), false),
		scan("duplicate host", change(`</host>`, `</host><host><status state="up"/><address addr="203.0.113.7" addrtype="ipv4"/></host>`), false),
		scan("timed-out host", change(`<host>`, `<host timedout="true">`), false),
		scan("host down", change(`state="up"`, `state="down"`), false),
		scan("missing completion", change(`<finished time="2" elapsed="1" exit="success"/>`, ""), false),
		scan("failed completion", change(`exit="success"`, `exit="error" errormsg="socket error"`), false),
		scan("duplicate completion", change(`<finished time="2" elapsed="1" exit="success"/>`, `<finished time="2" elapsed="1" exit="success"/><finished time="2" elapsed="1" exit="success"/>`), false),
		scan("unaccounted ports", change(`count="65534"`, `count="65533"`), false),
		scan("missing positive endpoint", change(`state="open" reason="syn-ack"`, `state="closed" reason="reset"`), false),
		scan("different required endpoint", func(e *portscan.Evidence) { e.PositivePort = 8443 }, false),
		scan("duplicate explicit port", change(`</ports>`, `<port protocol="tcp" portid="443"><state state="open"/></port></ports>`), false),
		scan("invalid port zero", change(`portid="443"`, `portid="0"`), false),
		scan("mixed transport", change(`protocol="tcp" portid`, `protocol="udp" portid`), false),
		scan("duplicate aggregate", change(`</ports>`, `<extraports state="filtered" count="1"/></ports>`), false),
		scan("aggregate overlaps explicit", change(`ports="1-442,444-65535"`, `ports="1-65534"`), false),
		scan("missing host totals", change(`<hosts up="1" down="0" total="1"/>`, ""), false),
		scan("multiple-host totals", change(`total="1"`, `total="2"`), false),
		scan("duplicate state attribute", change(`state="open"`, `state="closed" state="open"`), false),
		scan("ambiguous aggregate state", change(`state="filtered"`, `state="open|filtered"`), false),
		tlsCase("TLS speaking self-signed endpoint is not an identity claim", false, false, false),
		tlsCase("mTLS needs real protocol evidence", true, false, false),
		tlsCase("plaintext certificate-required text is not TLS", false, true, false),
		tlsCase("cancelled TLS observation cannot pass", false, false, true),
	} {
		t.Run(tc.name, tc.run)
	}
}
