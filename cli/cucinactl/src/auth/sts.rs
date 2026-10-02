// SPDX-License-Identifier: FSL-1.1-ALv2

//! RFC 8693 token exchange at the Cucina STS (`POST /token`, docs/contracts.md §5.1).

use anyhow::{Context, Result};
use openidconnect::url::Url;
use serde::Deserialize;

use crate::exit::CliError;
use crate::http::Http;

pub const GRANT_TYPE_TOKEN_EXCHANGE: &str = "urn:ietf:params:oauth:grant-type:token-exchange";
/// OIDC ID tokens (interactive login, renewal).
pub const TOKEN_TYPE_ID_TOKEN: &str = "urn:ietf:params:oauth:token-type:id_token";
/// Plain JWTs (GitHub Actions OIDC tokens).
pub const TOKEN_TYPE_JWT: &str = "urn:ietf:params:oauth:token-type:jwt";
/// Opaque service-account keys (`cuc_sk_…`, R-AUTH-10); ADR 0603.
pub const TOKEN_TYPE_SERVICE_KEY: &str = "urn:cucina:params:oauth:token-type:service-key";

/// Successful exchange response.
#[derive(Debug, Clone, Deserialize)]
pub struct ExchangeResponse {
    pub access_token: String,
    #[serde(default)]
    pub issued_token_type: String,
    #[serde(default)]
    pub token_type: String,
    #[serde(default)]
    pub expires_in: Option<u64>,
}

#[derive(Debug, Deserialize)]
struct ErrorBody {
    error: String,
    #[serde(default)]
    error_description: Option<String>,
}

/// Exchanges `subject_token` for a Cucina JWT. Errors never echo token material.
pub async fn exchange(
    http: &Http,
    token_endpoint: &str,
    subject_token: &str,
    subject_token_type: &str,
    audience: Option<&str>,
) -> Result<ExchangeResponse> {
    let url = Url::parse(token_endpoint)
        .with_context(|| format!("invalid STS token endpoint {token_endpoint:?}"))?;
    http.check_url(&url)?;
    let mut form = vec![
        ("grant_type", GRANT_TYPE_TOKEN_EXCHANGE),
        ("subject_token", subject_token),
        ("subject_token_type", subject_token_type),
    ];
    if let Some(aud) = audience {
        form.push(("audience", aud));
    }
    let resp = http
        .client()
        .post(url.clone())
        .header("accept", "application/json")
        .form(&form)
        .send()
        .await
        .with_context(|| format!("STS token exchange at {url}"))?;
    let status = resp.status();
    let body = resp.bytes().await.context("reading the STS response")?;
    if status.is_success() {
        let parsed: ExchangeResponse =
            serde_json::from_slice(&body).context("decoding the STS response")?;
        anyhow::ensure!(
            !parsed.access_token.is_empty(),
            "the STS returned an empty access_token"
        );
        if !parsed.token_type.is_empty() && !parsed.token_type.eq_ignore_ascii_case("bearer") {
            anyhow::bail!("unexpected STS token_type {:?}", parsed.token_type);
        }
        return Ok(parsed);
    }
    let detail = serde_json::from_slice::<ErrorBody>(&body)
        .map(|e| match e.error_description {
            Some(d) if !d.is_empty() => format!("{}: {d}", e.error),
            _ => e.error,
        })
        .unwrap_or_else(|_| format!("HTTP {status}"));
    let message = format!("the Cucina STS rejected the token exchange ({detail})");
    let err = match (
        status.as_u16(),
        detail.split(':').next().unwrap_or_default(),
    ) {
        (_, "access_denied") | (403, _) => CliError::permission_denied(message),
        (_, "invalid_grant") | (401, _) => CliError::auth_required(message),
        (429, _) | (500..=599, _) => CliError::unavailable(message),
        _ => CliError::new(crate::exit::ExitCode::Error, message),
    };
    Err(err.into())
}
