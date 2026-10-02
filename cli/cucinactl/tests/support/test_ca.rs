// SPDX-License-Identifier: FSL-1.1-ALv2

//! Throwaway private CA/server certificates for T10a. rcgen uses aws-lc-rs only;
//! private keys stay in memory and only the CA's public certificate is written to
//! the test's temporary directory. No certificates or keys are checked in.

use std::net::IpAddr;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use rcgen::{
    BasicConstraints, Certificate, CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa,
    Issuer, KeyPair, KeyUsagePurpose, SanType,
};
use rustls_pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};

pub struct ServerCert {
    pub chain: Vec<CertificateDer<'static>>,
    pub key: PrivateKeyDer<'static>,
}

pub struct TestCa {
    cert: Certificate,
    issuer: Issuer<'static, KeyPair>,
}

fn params() -> CertificateParams {
    let now = SystemTime::now();
    let mut params = CertificateParams::default();
    params.not_before = (now - Duration::from_secs(3600)).into();
    params.not_after = (now + Duration::from_secs(30 * 86_400)).into();
    params
}

impl TestCa {
    pub fn new(common_name: &str) -> TestCa {
        let mut params = params();
        params
            .distinguished_name
            .push(DnType::CommonName, common_name);
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        let key = KeyPair::generate().expect("CA key");
        TestCa {
            cert: params.self_signed(&key).expect("CA certificate"),
            issuer: Issuer::new(params, key),
        }
    }

    pub fn pem(&self) -> String {
        self.cert.pem()
    }

    pub fn server(&self, dns: &[&str], ips: &[IpAddr]) -> ServerCert {
        let mut params = params();
        params.subject_alt_names = dns
            .iter()
            .map(|d| SanType::DnsName((*d).try_into().expect("DNS name")))
            .chain(ips.iter().copied().map(SanType::IpAddress))
            .collect();
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        params.use_authority_key_identifier_extension = true;
        let key = KeyPair::generate().expect("server key");
        let leaf = params
            .signed_by(&key, &self.issuer)
            .expect("server certificate");
        ServerCert {
            chain: vec![leaf.der().clone(), self.cert.der().clone()],
            key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        }
    }
}

pub fn server_config(cert: ServerCert) -> Arc<rustls::ServerConfig> {
    let provider = Arc::new(rustls::crypto::aws_lc_rs::default_provider());
    let mut cfg = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .expect("protocol versions")
        .with_no_client_auth()
        .with_single_cert(cert.chain, cert.key)
        .expect("server certificate");
    cfg.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
    Arc::new(cfg)
}
