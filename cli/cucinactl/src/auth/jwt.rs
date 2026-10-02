// SPDX-License-Identifier: FSL-1.1-ALv2

//! Cucina JWT inspection for `whoami`: claims decoding and (best effort) signature
//! verification against the STS JWKS with `jsonwebtoken` (aws-lc-rs backend).

use anyhow::{Context, Result};
use jsonwebtoken::jwk::JwkSet;
use jsonwebtoken::{Algorithm, DecodingKey, Validation};
use openidconnect::url::Url;

use crate::http::Http;

/// Audience of every Cucina JWT (docs/contracts.md §5.1).
pub const AUDIENCE: &str = "buildbarn";

/// Verifies `token`'s signature (ES256/EdDSA, `kid` from the JWKS), `iss`, `aud` and
/// `exp`, returning the claims.
pub async fn verify(
    http: &Http,
    jwks_uri: &str,
    issuer: &str,
    token: &str,
) -> Result<serde_json::Map<String, serde_json::Value>> {
    let header = jsonwebtoken::decode_header(token).context("malformed JWT header")?;
    let kid = header.kid.clone().context("the JWT has no `kid`")?;
    let url = Url::parse(jwks_uri).with_context(|| format!("invalid jwks_uri {jwks_uri:?}"))?;
    let set: JwkSet = http.get_json(&url).await.context("fetching the STS JWKS")?;
    let jwk = set
        .find(&kid)
        .with_context(|| format!("signing key {kid:?} is not published in the STS JWKS"))?;
    let key = DecodingKey::from_jwk(jwk).context("unusable JWK")?;
    let alg = match header.alg {
        Algorithm::ES256 | Algorithm::EdDSA => header.alg,
        other => anyhow::bail!("unexpected JWT algorithm {other:?}"),
    };
    let mut validation = Validation::new(alg);
    validation.set_audience(&[AUDIENCE]);
    validation.set_issuer(&[issuer.trim_end_matches('/'), issuer]);
    validation.set_required_spec_claims(&["exp", "iss", "aud", "sub"]);
    validation.leeway = 0;
    let data = jsonwebtoken::decode::<serde_json::Map<String, serde_json::Value>>(
        token,
        &key,
        &validation,
    )
    .context("JWT verification failed")?;
    Ok(data.claims)
}
