// SPDX-License-Identifier: FSL-1.1-ALv2

//! Guards R-BUILD-1 "Crates with C code": aws-lc-rs runs on AWS-LC built from the BCR
//! `aws-lc` module through the rules_rs overlay (third_party/rust), and zstd on BCR `zstd`.

use aws_lc_rs::rand::SystemRandom;
use aws_lc_rs::signature::{self, EcdsaKeyPair, KeyPair};

#[test]
fn sha256_known_answer() {
    let got = aws_lc_rs::digest::digest(&aws_lc_rs::digest::SHA256, b"abc");
    let want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
    let hex: String = got.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    assert_eq!(hex, want);
}

#[test]
fn ecdsa_p256_sign_verify_round_trip() {
    let rng = SystemRandom::new();
    let alg = &signature::ECDSA_P256_SHA256_FIXED_SIGNING;
    let pkcs8 = EcdsaKeyPair::generate_pkcs8(alg, &rng).expect("generate");
    let key = EcdsaKeyPair::from_pkcs8(alg, pkcs8.as_ref()).expect("parse");
    let sig = key.sign(&rng, b"cucina").expect("sign");
    let public = signature::UnparsedPublicKey::new(
        &signature::ECDSA_P256_SHA256_FIXED,
        key.public_key().as_ref(),
    );
    public.verify(b"cucina", sig.as_ref()).expect("verify");
    assert!(public.verify(b"tampered", sig.as_ref()).is_err());
}

#[test]
fn rustls_aws_lc_provider_installs() {
    assert!(
        rustls::crypto::aws_lc_rs::default_provider()
            .install_default()
            .is_ok()
    );
}

#[test]
fn zstd_round_trip() {
    let data = b"cucina cucina cucina cucina".repeat(64);
    let compressed = zstd::encode_all(&data[..], 3).expect("compress");
    assert!(compressed.len() < data.len());
    assert_eq!(zstd::decode_all(&compressed[..]).expect("decompress"), data);
}
