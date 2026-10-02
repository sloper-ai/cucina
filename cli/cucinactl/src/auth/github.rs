// SPDX-License-Identifier: FSL-1.1-ALv2

//! GitHub Actions OIDC (R-AUTH-7). A job with `permissions: {id-token: write}` has
//! `ACTIONS_ID_TOKEN_REQUEST_URL`/`ACTIONS_ID_TOKEN_REQUEST_TOKEN` in its environment
//! (Bazel passes the client environment to the credential helper). The helper fetches
//! a token for the audience `cucina` (used only by Cucina) and exchanges it at the STS
//! as `subject_token_type=…:jwt`. GitHub tokens live ~5 minutes, so every renewal
//! fetches a new one; nothing is stored.

use anyhow::{Context, Result};
use openidconnect::url::Url;
use serde::Deserialize;

use crate::exit::CliError;
use crate::http::Http;

pub const REQUEST_URL_ENV: &str = "ACTIONS_ID_TOKEN_REQUEST_URL";
pub const REQUEST_TOKEN_ENV: &str = "ACTIONS_ID_TOKEN_REQUEST_TOKEN";
/// The audience requested from GitHub; Cucina's GitHub TrustPolicy accepts exactly this.
pub const AUDIENCE: &str = "cucina";

/// The GitHub Actions OIDC request parameters, when running inside a job.
#[derive(Debug, Clone)]
pub struct ActionsEnv {
    pub request_url: String,
    pub request_token: String,
}

/// Detects the GitHub Actions OIDC environment.
pub fn detect() -> Option<ActionsEnv> {
    let url = std::env::var(REQUEST_URL_ENV)
        .ok()
        .filter(|v| !v.is_empty())?;
    let token = std::env::var(REQUEST_TOKEN_ENV)
        .ok()
        .filter(|v| !v.is_empty())?;
    Some(ActionsEnv {
        request_url: url,
        request_token: token,
    })
}

#[derive(Deserialize)]
struct IdTokenResponse {
    value: String,
}

/// Fetches a GitHub OIDC token for `audience=cucina`.
pub async fn fetch_id_token(http: &Http, env: &ActionsEnv) -> Result<String> {
    let mut url = Url::parse(&env.request_url).context("invalid ACTIONS_ID_TOKEN_REQUEST_URL")?;
    url.query_pairs_mut().append_pair("audience", AUDIENCE);
    http.check_url(&url)?;
    let resp = http
        .client()
        .get(url)
        .bearer_auth(&env.request_token)
        .header("accept", "application/json; api-version=2.0")
        .send()
        .await
        .context("requesting the GitHub Actions OIDC token")?;
    let status = resp.status();
    if !status.is_success() {
        let msg = format!(
            "GitHub refused the OIDC token request (HTTP {status}); does the job have `permissions: id-token: write`?"
        );
        return Err(if status.as_u16() == 401 || status.as_u16() == 403 {
            CliError::auth_required(msg).into()
        } else {
            CliError::unavailable(msg).into()
        });
    }
    let body: IdTokenResponse = resp
        .json()
        .await
        .context("decoding the GitHub OIDC token response")?;
    anyhow::ensure!(
        !body.value.is_empty(),
        "GitHub returned an empty OIDC token"
    );
    Ok(body.value)
}
