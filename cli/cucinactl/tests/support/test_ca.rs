// SPDX-License-Identifier: FSL-1.1-ALv2

//! A throwaway private CA for TLS tests (T10a: Cucina's private CA must be trusted
//! for discovery, the STS and the management API). Keys and certificates are made per
//! run with aws-lc-rs and a minimal DER writer (nothing is checked in): an ECDSA P-256
//! CA (`basicConstraints` CA, `keyCertSign`) and server certificates with DNS/IP
//! `subjectAltName` and `serverAuth`, valid for 30 days, which webpki and the macOS
//! and Windows verifiers accept.

use std::net::IpAddr;
use std::sync::Arc;

use aws_lc_rs::rand::{SecureRandom as _, SystemRandom};
use aws_lc_rs::signature::{ECDSA_P256_SHA256_ASN1_SIGNING, EcdsaKeyPair, KeyPair as _};
use base64::Engine as _;
use rustls_pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use sha2::Digest as _;

// ---- DER ---------------------------------------------------------------------------

fn tlv(tag: u8, content: &[u8]) -> Vec<u8> {
    let mut out = vec![tag];
    let n = content.len();
    if n < 0x80 {
        out.push(n as u8);
    } else {
        let bytes = n.to_be_bytes();
        let first = bytes.iter().position(|b| *b != 0).unwrap();
        out.push(0x80 | (bytes.len() - first) as u8);
        out.extend_from_slice(&bytes[first..]);
    }
    out.extend_from_slice(content);
    out
}

fn seq(parts: &[Vec<u8>]) -> Vec<u8> {
    tlv(0x30, &parts.concat())
}

fn oid(arcs: &[u64]) -> Vec<u8> {
    let mut body = Vec::new();
    let mut push = |mut v: u64| {
        let mut tmp = vec![(v & 0x7f) as u8];
        v >>= 7;
        while v > 0 {
            tmp.push(0x80 | (v & 0x7f) as u8);
            v >>= 7;
        }
        tmp.reverse();
        body.extend(tmp);
    };
    push(arcs[0] * 40 + arcs[1]);
    for a in &arcs[2..] {
        push(*a);
    }
    tlv(0x06, &body)
}

fn uint(bytes: &[u8]) -> Vec<u8> {
    let first = bytes.iter().position(|b| *b != 0).unwrap_or(bytes.len() - 1);
    let mut body = bytes[first..].to_vec();
    if body[0] & 0x80 != 0 {
        body.insert(0, 0);
    }
    tlv(0x02, &body)
}

fn bits(bytes: &[u8]) -> Vec<u8> {
    let mut body = vec![0u8];
    body.extend_from_slice(bytes);
    tlv(0x03, &body)
}

fn time(unix: i64) -> Vec<u8> {
    let t = jiff::Timestamp::from_second(unix).expect("timestamp");
    let s = t.strftime("%y%m%d%H%M%SZ").to_string();
    tlv(0x17, s.as_bytes()) // UTCTime (years 1950-2049)
}

fn name(cn: &str) -> Vec<u8> {
    let attr = seq(&[oid(&[2, 5, 4, 3]), tlv(0x0c, cn.as_bytes())]);
    seq(&[tlv(0x31, &attr)])
}

fn extension(id: &[u64], critical: bool, value: Vec<u8>) -> Vec<u8> {
    let mut parts = vec![oid(id)];
    if critical {
        parts.push(tlv(0x01, &[0xff]));
    }
    parts.push(tlv(0x04, &value));
    seq(&parts)
}

fn ecdsa_with_sha256() -> Vec<u8> {
    seq(&[oid(&[1, 2, 840, 10045, 4, 3, 2])])
}

fn spki(point: &[u8]) -> Vec<u8> {
    seq(&[
        seq(&[
            oid(&[1, 2, 840, 10045, 2, 1]),
            oid(&[1, 2, 840, 10045, 3, 1, 7]),
        ]),
        bits(point),
    ])
}

fn key_id(point: &[u8]) -> Vec<u8> {
    sha2::Sha256::digest(point)[..20].to_vec()
}

// ---- CA ----------------------------------------------------------------------------

/// A server certificate chain and its key, ready for a rustls server.
pub struct ServerCert {
    pub chain: Vec<CertificateDer<'static>>,
    pub key: PrivateKeyDer<'static>,
}

pub struct TestCa {
    key: EcdsaKeyPair,
    name: Vec<u8>,
    der: Vec<u8>,
    rng: SystemRandom,
}

fn keypair() -> (EcdsaKeyPair, Vec<u8>) {
    let pair = EcdsaKeyPair::generate(&ECDSA_P256_SHA256_ASN1_SIGNING).expect("keygen");
    let pkcs8 = pair.to_pkcs8v1().expect("pkcs8").as_ref().to_vec();
    (pair, pkcs8)
}

impl TestCa {
    pub fn new(common_name: &str) -> TestCa {
        let rng = SystemRandom::new();
        let (key, _) = keypair();
        let name = name(common_name);
        let point = key.public_key().as_ref().to_vec();
        let exts = vec![
            extension(&[2, 5, 29, 19], true, seq(&[tlv(0x01, &[0xff])])),
            // keyCertSign | cRLSign
            extension(&[2, 5, 29, 15], true, tlv(0x03, &[0x01, 0x06])),
            extension(&[2, 5, 29, 14], false, tlv(0x04, &key_id(&point))),
        ];
        let mut ca = TestCa {
            key,
            name: name.clone(),
            der: Vec::new(),
            rng,
        };
        ca.der = ca.sign(name, &point, exts);
        ca
    }

    fn sign(&self, subject: Vec<u8>, point: &[u8], exts: Vec<Vec<u8>>) -> Vec<u8> {
        let mut serial = [0u8; 16];
        self.rng.fill(&mut serial).expect("rng");
        serial[0] = (serial[0] & 0x7f) | 0x01;
        let now = cucinactl::util::now_unix();
        let tbs = seq(&[
            tlv(0xa0, &uint(&[2])),
            uint(&serial),
            ecdsa_with_sha256(),
            self.name.clone(),
            seq(&[time(now - 3600), time(now + 30 * 86_400)]),
            subject,
            spki(point),
            tlv(0xa3, &seq(&exts)),
        ]);
        let sig = self.key.sign(&self.rng, &tbs).expect("sign");
        seq(&[tbs, ecdsa_with_sha256(), bits(sig.as_ref())])
    }

    /// The CA certificate in PEM (what `--ca-file`/`CUCINA_CA_FILE` point at).
    pub fn pem(&self) -> String {
        let b64 = base64::engine::general_purpose::STANDARD.encode(&self.der);
        let mut out = String::from("-----BEGIN CERTIFICATE-----\n");
        for chunk in b64.as_bytes().chunks(64) {
            out.push_str(std::str::from_utf8(chunk).unwrap());
            out.push('\n');
        }
        out.push_str("-----END CERTIFICATE-----\n");
        out
    }

    /// A server certificate for these DNS names and IP addresses.
    pub fn server(&self, dns: &[&str], ips: &[IpAddr]) -> ServerCert {
        let (key, pkcs8) = keypair();
        let point = key.public_key().as_ref().to_vec();
        let mut names = Vec::new();
        for d in dns {
            names.push(tlv(0x82, d.as_bytes()));
        }
        for ip in ips {
            match ip {
                IpAddr::V4(v4) => names.push(tlv(0x87, &v4.octets())),
                IpAddr::V6(v6) => names.push(tlv(0x87, &v6.octets())),
            }
        }
        let ca_point = self.key.public_key().as_ref().to_vec();
        let exts = vec![
            extension(&[2, 5, 29, 19], true, seq(&[])),
            // digitalSignature
            extension(&[2, 5, 29, 15], true, tlv(0x03, &[0x07, 0x80])),
            extension(&[2, 5, 29, 37], false, seq(&[oid(&[1, 3, 6, 1, 5, 5, 7, 3, 1])])),
            extension(&[2, 5, 29, 17], false, seq(&names)),
            extension(&[2, 5, 29, 14], false, tlv(0x04, &key_id(&point))),
            extension(
                &[2, 5, 29, 35],
                false,
                seq(&[tlv(0x80, &key_id(&ca_point))]),
            ),
        ];
        let leaf = self.sign(name(dns.first().copied().unwrap_or("cucina test")), &point, exts);
        ServerCert {
            chain: vec![
                CertificateDer::from(leaf),
                CertificateDer::from(self.der.clone()),
            ],
            key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(pkcs8)),
        }
    }
}

/// A rustls server configuration (ALPN h2 and http/1.1) for `cert`.
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
