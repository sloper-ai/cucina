// SPDX-License-Identifier: FSL-1.1-ALv2

//! `GET <url>/.well-known/cucina-configuration` (docs/contracts.md §5.1, R-AUTH-1):
//! everything `cucinactl login <url>` needs (STS token endpoint, client endpoints,
//! identity providers with client IDs and scopes).

use anyhow::{Context, Result};
use openidconnect::url::Url;
use serde::{Deserialize, Serialize};

use crate::http::Http;

/// Path of the discovery document below the Cucina URL.
pub const WELL_KNOWN_PATH: &str = ".well-known/cucina-configuration";

/// The discovery document. Unknown fields are ignored (forward compatible).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CucinaConfiguration {
    pub version: u32,
    pub issuer: String,
    pub token_endpoint: String,
    #[serde(default)]
    pub jwks_uri: String,
    pub endpoints: Endpoints,
    #[serde(default)]
    pub identity_providers: Vec<IdentityProvider>,
}

/// Client-facing endpoints.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Endpoints {
    /// e.g. `grpcs://cucina.example.com:443`
    pub remote_execution: String,
    /// default instance name, e.g. `main`
    pub instance_name: String,
    /// management API, e.g. `cucina.example.com:8444`
    pub management: String,
}

/// One interactive identity provider (published from `TrustPolicy.spec.login`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct IdentityProvider {
    pub name: String,
    #[serde(rename = "type", default = "default_type")]
    pub kind: String,
    pub issuer: String,
    pub client_id: String,
    /// Non-confidential for Google "Desktop app" clients (sent on token requests).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub client_secret: Option<String>,
    #[serde(default)]
    pub scopes: Vec<String>,
    /// `hd` hint for Google (trust is enforced by the STS, never by this hint).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hosted_domain_hint: Option<String>,
    /// Registered loopback ports for providers that need them (empty = any port).
    #[serde(default)]
    pub redirect_ports: Vec<u16>,
    /// Override of `{issuer}/.well-known/openid-configuration`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub discovery_url: Option<String>,
}

fn default_type() -> String {
    "oidc".into()
}

impl IdentityProvider {
    /// Google issues refresh tokens to installed apps without `offline_access`.
    pub fn is_google(&self) -> bool {
        let issuer = self.issuer.trim_end_matches('/');
        issuer == "https://accounts.google.com" || issuer == "accounts.google.com"
    }

    /// `iss` values accepted in ID tokens (Google also emits the scheme-less form).
    pub fn accepted_issuers(&self) -> Vec<String> {
        let mut v = vec![self.issuer.clone()];
        if self.is_google() {
            v.push("https://accounts.google.com".into());
            v.push("accounts.google.com".into());
        }
        v
    }
}

/// Fetches and validates the discovery document.
pub async fn fetch(http: &Http, base: &Url) -> Result<CucinaConfiguration> {
    let url = base
        .join(WELL_KNOWN_PATH)
        .context("building the discovery URL")?;
    let doc: CucinaConfiguration = http
        .get_json(&url)
        .await
        .with_context(|| format!("Cucina discovery at {url}"))?;
    anyhow::ensure!(
        doc.version == 1,
        "unsupported discovery document version {} (this cucinactl understands version 1)",
        doc.version
    );
    anyhow::ensure!(
        !doc.token_endpoint.is_empty(),
        "discovery document has no token_endpoint"
    );
    Ok(doc)
}
