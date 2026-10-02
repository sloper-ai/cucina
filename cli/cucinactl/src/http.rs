// SPDX-License-Identifier: FSL-1.1-ALv2

//! HTTP client for the STS, the identity providers and GitHub's OIDC endpoint.
//!
//! * Never follows redirects (SSRF guard for OIDC discovery/JWKS fetches, R-LIB-3).
//! * HTTPS only, except to loopback addresses (local test servers) or when
//!   `CUCINA_ALLOW_INSECURE_HTTP=1` is set explicitly (test identity providers).
//! * rustls + aws-lc-rs with the platform verifier and an optional extra CA.
//! * Response bodies are capped.

use std::fmt;
use std::path::Path;
use std::time::Duration;

use anyhow::{Context, Result};
use openidconnect::http as oauth_http;
use openidconnect::url::Url;

/// Environment switch allowing `http://` to non-loopback hosts (test IdPs only).
pub const ALLOW_INSECURE_HTTP_ENV: &str = "CUCINA_ALLOW_INSECURE_HTTP";

const MAX_BODY: usize = 4 << 20;

/// Shared HTTP client.
#[derive(Clone)]
pub struct Http {
    client: reqwest::Client,
    allow_insecure: bool,
}

/// Error type handed to `openidconnect`/`oauth2` (they require `std::error::Error`).
#[derive(Debug)]
pub struct HttpError(pub String);

impl fmt::Display for HttpError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}
impl std::error::Error for HttpError {}

impl Http {
    /// Builds a client; `ca_file` adds a PEM bundle to the OS trust store.
    pub fn new(ca_file: Option<&Path>, timeout: Duration) -> Result<Http> {
        let tls = crate::tls::client_config(ca_file)?;
        let client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(timeout)
            .connect_timeout(Duration::from_secs(10).min(timeout))
            .user_agent(format!("cucinactl/{}", crate::VERSION))
            .tls_backend_preconfigured(tls)
            .build()
            .context("building the HTTP client")?;
        let allow_insecure = std::env::var(ALLOW_INSECURE_HTTP_ENV).is_ok_and(|v| v == "1");
        Ok(Http {
            client,
            allow_insecure,
        })
    }

    /// Rejects non-HTTPS URLs unless they point at loopback (or insecure is allowed).
    pub fn check_url(&self, url: &Url) -> Result<(), HttpError> {
        match url.scheme() {
            "https" => Ok(()),
            "http" if self.allow_insecure || is_loopback(url) => Ok(()),
            other => Err(HttpError(format!(
                "refusing {other}:// URL {url}: only https (or http to 127.0.0.1/[::1]) is allowed"
            ))),
        }
    }

    /// The underlying reqwest client (for JSON/form helpers).
    pub fn client(&self) -> &reqwest::Client {
        &self.client
    }

    /// GET a JSON document.
    pub async fn get_json<T: serde::de::DeserializeOwned>(&self, url: &Url) -> Result<T> {
        self.check_url(url)?;
        let resp = self
            .client
            .get(url.clone())
            .header("accept", "application/json")
            .send()
            .await
            .with_context(|| format!("GET {url}"))?;
        let status = resp.status();
        let body = read_capped(resp).await?;
        anyhow::ensure!(
            status.is_success(),
            "GET {url}: HTTP {status}: {}",
            String::from_utf8_lossy(&body[..body.len().min(512)])
        );
        serde_json::from_slice(&body).with_context(|| format!("decoding JSON from {url}"))
    }

    /// Executes an `oauth2`/`openidconnect` request (the "small reqwest HTTP closure").
    pub async fn oauth_request(
        &self,
        request: oauth_http::Request<Vec<u8>>,
    ) -> Result<oauth_http::Response<Vec<u8>>, HttpError> {
        let (parts, body) = request.into_parts();
        let url = Url::parse(&parts.uri.to_string()).map_err(|e| HttpError(e.to_string()))?;
        self.check_url(&url)?;
        let mut builder = self.client.request(parts.method, url.clone()).body(body);
        for (name, value) in &parts.headers {
            builder = builder.header(name, value);
        }
        let resp = builder
            .send()
            .await
            .map_err(|e| HttpError(format!("{} {url}: {e}", "request")))?;
        let mut out = oauth_http::Response::builder().status(resp.status());
        for (name, value) in resp.headers() {
            out = out.header(name, value);
        }
        let body = read_capped(resp)
            .await
            .map_err(|e| HttpError(format!("{e:#}")))?;
        out.body(body).map_err(|e| HttpError(e.to_string()))
    }
}

async fn read_capped(resp: reqwest::Response) -> Result<Vec<u8>> {
    if resp.content_length().is_some_and(|n| n > MAX_BODY as u64) {
        anyhow::bail!("response body too large");
    }
    let bytes = resp.bytes().await.context("reading response body")?;
    anyhow::ensure!(bytes.len() <= MAX_BODY, "response body too large");
    Ok(bytes.to_vec())
}

fn is_loopback(url: &Url) -> bool {
    match url.host() {
        Some(openidconnect::url::Host::Ipv4(ip)) => ip.is_loopback(),
        Some(openidconnect::url::Host::Ipv6(ip)) => ip.is_loopback(),
        Some(openidconnect::url::Host::Domain(d)) => d.eq_ignore_ascii_case("localhost"),
        None => false,
    }
}

/// Parses a base URL given on the command line, adding `https://` when missing.
pub fn parse_base_url(input: &str) -> Result<Url> {
    let with_scheme = if input.contains("://") {
        input.to_string()
    } else {
        format!("https://{input}")
    };
    let mut url = Url::parse(&with_scheme).with_context(|| format!("invalid URL {input:?}"))?;
    anyhow::ensure!(url.host_str().is_some(), "invalid URL {input:?}: no host");
    if !url.path().ends_with('/') {
        let path = format!("{}/", url.path());
        url.set_path(&path);
    }
    url.set_query(None);
    url.set_fragment(None);
    Ok(url)
}
