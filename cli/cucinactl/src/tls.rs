// SPDX-License-Identifier: FSL-1.1-ALv2

//! TLS: rustls with the aws-lc-rs provider only (R-LIB-3) and the OS trust store via
//! `rustls-platform-verifier` (enterprise CAs included), optionally extended with an
//! extra PEM CA bundle (Cucina's private CA). Shared by the HTTP client (STS, IdP)
//! and the gRPC channels (management API, REAPI).

use std::path::Path;
use std::sync::Arc;

use anyhow::{Context, Result};
use rustls::client::danger::ServerCertVerifier;
use rustls::crypto::CryptoProvider;
use rustls_pki_types::CertificateDer;
use rustls_pki_types::pem::PemObject as _;

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

/// The server-certificate verifier: OS trust plus the optional extra bundle.
pub fn verifier(ca_file: Option<&Path>) -> Result<Arc<dyn ServerCertVerifier>> {
    let extra = match ca_file {
        Some(p) => load_ca_bundle(p)?,
        None => Vec::new(),
    };
    let v = rustls_platform_verifier::Verifier::new_with_extra_roots(extra, provider())
        .context("initialising the platform certificate verifier")?;
    Ok(Arc::new(v))
}

/// A rustls client configuration (HTTP/1.1 + HTTP/2 ALPN) for the HTTP client.
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
