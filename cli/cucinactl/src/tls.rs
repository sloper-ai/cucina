// SPDX-License-Identifier: FSL-1.1-ALv2

//! TLS: rustls with the aws-lc-rs provider only (R-LIB-3) and the OS trust store via
//! `rustls-platform-verifier` (enterprise CAs included), extended with extra PEM CA
//! bundles (Cucina's private CA, T10a): `--ca-file`/the profile's `ca_file`,
//! `CUCINA_CA_FILE` and `SSL_CERT_FILE`. Shared by the HTTP client (discovery, STS,
//! identity providers) and the gRPC channels (management API, REAPI).

use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context, Result};
use rustls::client::danger::ServerCertVerifier;
use rustls::crypto::CryptoProvider;
use rustls_pki_types::CertificateDer;
use rustls_pki_types::pem::PemObject as _;

/// Extra CA bundle (PEM) for Cucina's private CA, trusted besides the OS store.
pub const CUCINA_CA_FILE_ENV: &str = "CUCINA_CA_FILE";
/// OpenSSL's CA bundle variable; trusted besides the OS store too (rustls does not
/// read it on macOS and Windows).
pub const SSL_CERT_FILE_ENV: &str = "SSL_CERT_FILE";

fn env_path(name: &str) -> Option<PathBuf> {
    std::env::var_os(name)
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
}

/// `CUCINA_CA_FILE`, if set.
pub fn cucina_ca_file_env() -> Option<PathBuf> {
    env_path(CUCINA_CA_FILE_ENV)
}

/// Every extra CA bundle to trust besides the OS store, in order: `explicit`
/// (`--ca-file` or the profile's `ca_file`), `CUCINA_CA_FILE`, `SSL_CERT_FILE`.
pub fn extra_ca_files(explicit: Option<&Path>) -> Vec<PathBuf> {
    let mut out: Vec<PathBuf> = Vec::new();
    for p in [
        explicit.map(Path::to_path_buf),
        cucina_ca_file_env(),
        env_path(SSL_CERT_FILE_ENV),
    ]
    .into_iter()
    .flatten()
    {
        if !out.contains(&p) {
            out.push(p);
        }
    }
    out
}

/// Installs aws-lc-rs as the process-wide rustls provider (idempotent).
pub fn install_crypto_provider() {
    let _ = rustls::crypto::aws_lc_rs::default_provider().install_default();
}

fn provider() -> Arc<CryptoProvider> {
    CryptoProvider::get_default()
        .cloned()
        .unwrap_or_else(|| Arc::new(rustls::crypto::aws_lc_rs::default_provider()))
}

/// Loads every certificate of a PEM bundle.
pub fn load_ca_bundle(path: &Path) -> Result<Vec<CertificateDer<'static>>> {
    let certs = CertificateDer::pem_file_iter(path)
        .with_context(|| format!("reading CA bundle {}", path.display()))?
        .collect::<Result<Vec<_>, _>>()
        .with_context(|| format!("parsing CA bundle {}", path.display()))?;
    anyhow::ensure!(
        !certs.is_empty(),
        "{} contains no certificates",
        path.display()
    );
    Ok(certs)
}

/// The server-certificate verifier: OS trust plus the extra bundles
/// ([`extra_ca_files`]).
pub fn verifier(ca_file: Option<&Path>) -> Result<Arc<dyn ServerCertVerifier>> {
    let mut extra = Vec::new();
    for p in extra_ca_files(ca_file) {
        extra.extend(load_ca_bundle(&p)?);
    }
    let v = rustls_platform_verifier::Verifier::new_with_extra_roots(extra, provider())
        .context("initialising the platform certificate verifier")?;
    Ok(Arc::new(v))
}

/// A rustls client configuration (HTTP/1.1 + HTTP/2 ALPN; gRPC channels narrow it to
/// `h2`) trusting the OS store and the extra bundles.
pub fn client_config(ca_file: Option<&Path>) -> Result<rustls::ClientConfig> {
    let mut cfg = rustls::ClientConfig::builder_with_provider(provider())
        .with_safe_default_protocol_versions()
        .context("TLS protocol versions")?
        .dangerous()
        .with_custom_certificate_verifier(verifier(ca_file)?)
        .with_no_client_auth();
    cfg.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
    Ok(cfg)
}
