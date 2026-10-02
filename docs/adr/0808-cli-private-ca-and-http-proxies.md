<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0808 — Private CA bundles and HTTP proxies for every cucinactl connection

* Status: accepted (2026-10-02)

## Context
T10a: Cucina's private CA must be trusted for discovery, the STS token endpoint and the
management API, and clients may sit behind an HTTP proxy. reqwest (discovery, STS, identity
providers) already follows `HTTPS_PROXY`/`NO_PROXY` and the macOS/Windows system proxy
settings through hyper-util's matcher; connect-rust's `Http2Connection` has no proxy
support. The extra CA was only taken from `login --ca-file`/the profile, and from
`CUCINA_CA_FILE` only in the GitHub Actions path of the credential helper.

## Decision
* Trust = OS store (rustls-platform-verifier) + every configured PEM bundle: the profile's
  `ca_file` (`login --ca-file`, `config set ca-file`, stored as an absolute path),
  `CUCINA_CA_FILE` and `SSL_CERT_FILE`. A union, not a precedence chain, so a corporate
  bundle and Cucina's CA can be combined. `login` stores `--ca-file`, else `CUCINA_CA_FILE`;
  `bazelrc` emits `--tls_certificate` from the profile, else `CUCINA_CA_FILE`. A missing or
  empty bundle is an error naming the file.
* gRPC channels use the same matcher (`hyper_util::client::proxy::matcher::Matcher::
  from_system`). When it returns a proxy, `client/connector.rs` is plugged into
  `Http2Connection::lazy_with_connector`: TCP (or TLS for `https://` proxies) to the proxy,
  `CONNECT host:port` with Basic `Proxy-Authorization` from the proxy URL, the response head
  read byte by byte (2xx accepted, 407 explained), then rustls with ALPN `h2` to the target.
  The proxy resolves the target name. SOCKS proxies are refused with a clear error. Direct
  connections keep connect-rust's own transport.
* New direct dependencies, all already in the graph through reqwest and connectrpc (no new
  crates in Cargo.lock): `hyper-util` (features `client-proxy`, `client-proxy-system`,
  `tokio`), `tokio-rustls` (no default features; aws-lc-rs via rustls), `tower-service`.
* Tests generate a private CA and server certificates per run with aws-lc-rs and a ~100-line
  DER writer (`tests/support/test_ca.rs`) instead of adding rcgen or checking in
  certificates (§12: no generated certs or keys in the repository); a TLS terminator and a
  `CONNECT` proxy fake run on loopback (`tests/tls_proxy.rs`).

## Consequences
Behind a proxy, cucinactl and the credential helper reach Cucina without extra flags; Bazel's
own connections are configured in Bazel. The test certificates satisfy webpki and Apple's
TLS server certificate rules (SAN, `serverAuth`, 30-day validity, P-256/SHA-256). Revisit if
connect-rust gains proxy support.

## Implementation follow-up (2026-10-02)
The test-fixture bullet above is superseded: `tests/support/test_ca.rs` now uses dev-only
`rcgen = 0.14.10` (MIT/Apache-2.0; `default-features = false`, `aws_lc_rs`, `pem`) rather
than maintaining DER/X.509 code. It still generates keys/certificates per run, retains keys
only in memory and tests the same trust boundary. `Cargo.lock` includes rcgen's dependency
closure; the production binary does not depend on rcgen and `ring` remains absent. The
integration table covers both HTTP and HTTPS proxy URLs and `NO_PROXY` bypass.
