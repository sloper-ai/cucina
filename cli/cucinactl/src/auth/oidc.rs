// SPDX-License-Identifier: FSL-1.1-ALv2

//! Interactive OIDC login (R-AUTH-5): OAuth 2.0 authorization code + PKCE (S256)
//! with a loopback redirect (RFC 8252), and silent renewal with the IdP refresh
//! token (R-AUTH-6). No device flow, no out-of-band flow.
//!
//! * The redirect listener binds `127.0.0.1` only (never `localhost`, never all
//!   interfaces), on `--port`, else the provider's registered ports in order, else
//!   an ephemeral port. It serves exactly one callback, then shuts down; it times out.
//! * `state` and `nonce` are random per attempt; a callback whose `state` does not
//!   match aborts the login (fail closed). The ID token's signature, `iss`, `aud`,
//!   `nonce` and `exp` are verified with `openidconnect`.
//! * `--manual`: nothing listens; the user pastes the final
//!   `http://127.0.0.1:<port>/?state=…&code=…` URL; `state` is checked the same way.
//! * The browser is opened with `webbrowser` (hardened; `open -u` on macOS, ADR 0806)
//!   or `--browser-command`, and the URL is always printed.

use std::collections::HashMap;
use std::io::Write as _;
use std::net::{Ipv4Addr, SocketAddr, TcpListener};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result, anyhow};
use axum::extract::{Query, State};
use axum::http::{StatusCode, Uri};
use axum::response::{Html, IntoResponse};
use openidconnect::core::{
    CoreAuthenticationFlow, CoreClient, CoreErrorResponseType, CoreIdTokenClaims,
    CoreProviderMetadata, CoreTokenResponse,
};
use openidconnect::url::Url;
use openidconnect::{
    AuthType, AuthorizationCode, ClientId, ClientSecret, CsrfToken, EndpointMaybeSet,
    EndpointNotSet, EndpointSet, IssuerUrl, Nonce, OAuth2TokenResponse as _, PkceCodeChallenge,
    RedirectUrl, RefreshToken, RequestTokenError, Scope, TokenResponse as _,
};
use sha2::{Digest as _, Sha256};
use tokio::sync::oneshot;

use super::discovery::IdentityProvider;
use crate::exit::CliError;
use crate::http::{Http, HttpError};

type OidcClient = CoreClient<
    EndpointSet,
    EndpointNotSet,
    EndpointNotSet,
    EndpointNotSet,
    EndpointMaybeSet,
    EndpointMaybeSet,
>;

/// How to show the authorization URL to the user.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Browser {
    /// Open the system browser and print the URL.
    System,
    /// Only print the URL (`--no-browser`, e.g. over `ssh -L`).
    PrintOnly,
    /// Run `<program> [args…] <url>` (`--browser-command`; test/scenario hook).
    Command(String),
}

/// Interactive login options.
#[derive(Debug, Clone)]
pub struct LoginOptions {
    /// Fixed loopback port (`--port`).
    pub port: Option<u16>,
    /// Redirect path (default `/`).
    pub redirect_path: String,
    pub browser: Browser,
    /// Paste-the-URL mode (`--manual`).
    pub manual: bool,
    /// `hd` hint override (`--hd`).
    pub hosted_domain: Option<String>,
    /// How long to wait for the redirect.
    pub timeout: Duration,
}

impl Default for LoginOptions {
    fn default() -> Self {
        LoginOptions {
            port: None,
            redirect_path: "/".into(),
            browser: Browser::System,
            manual: false,
            hosted_domain: None,
            timeout: Duration::from_secs(300),
        }
    }
}

/// A verified ID token (and the refresh token, if the IdP issued one).
#[derive(Debug, Clone)]
pub struct IdTokens {
    pub id_token: String,
    pub refresh_token: Option<String>,
    pub subject: String,
    pub email: Option<String>,
}

fn http_fn(
    http: &Http,
) -> impl Fn(
    openidconnect::HttpRequest,
) -> std::pin::Pin<
    Box<dyn Future<Output = Result<openidconnect::HttpResponse, HttpError>> + Send>,
> + use<> {
    let http = http.clone();
    move |req| {
        let http = http.clone();
        Box::pin(async move { http.oauth_request(req).await })
    }
}

async fn provider_client(http: &Http, idp: &IdentityProvider) -> Result<OidcClient> {
    let client = http_fn(http);
    let metadata = match &idp.discovery_url {
        Some(url) => {
            let url = Url::parse(url).context("invalid discovery_url")?;
            let metadata: CoreProviderMetadata = http.get_json(&url).await?;
            let jwks = openidconnect::JsonWebKeySet::fetch_async(metadata.jwks_uri(), &client)
                .await
                .map_err(|e| anyhow!("fetching the provider JWKS: {e}"))?;
            metadata.set_jwks(jwks)
        }
        None => {
            let issuer = IssuerUrl::new(idp.issuer.clone())
                .with_context(|| format!("invalid issuer URL {:?}", idp.issuer))?;
            CoreProviderMetadata::discover_async(issuer, &client)
                .await
                .map_err(|e| anyhow!("OIDC discovery for {}: {}", idp.issuer, error_chain(&e)))?
        }
    };
    let mut client = CoreClient::from_provider_metadata(
        metadata,
        ClientId::new(idp.client_id.clone()),
        idp.client_secret.clone().map(ClientSecret::new),
    );
    if idp.client_secret.is_some() {
        // Google desktop clients: the (non-confidential) secret goes in the body.
        client = client.set_auth_type(AuthType::RequestBody);
    }
    Ok(client)
}

fn error_chain(e: &dyn std::error::Error) -> String {
    let mut s = e.to_string();
    let mut cur = e.source();
    while let Some(c) = cur {
        s.push_str(": ");
        s.push_str(&c.to_string());
        cur = c.source();
    }
    s
}

fn token_error(
    what: &str,
    e: RequestTokenError<HttpError, openidconnect::StandardErrorResponse<CoreErrorResponseType>>,
) -> anyhow::Error {
    match e {
        RequestTokenError::ServerResponse(resp) => {
            let detail = match resp.error_description() {
                Some(d) => format!("{}: {d}", resp.error()),
                None => resp.error().to_string(),
            };
            let msg = format!("{what}: the identity provider refused ({detail})");
            match resp.error() {
                CoreErrorResponseType::InvalidGrant => {
                    CliError::auth_required(format!("{msg}; run `cucinactl login` again")).into()
                }
                CoreErrorResponseType::UnauthorizedClient => {
                    CliError::permission_denied(msg).into()
                }
                CoreErrorResponseType::Extension(e) if e == "access_denied" => {
                    CliError::permission_denied(msg).into()
                }
                _ => anyhow!(msg),
            }
        }
        RequestTokenError::Request(err) => CliError::unavailable(format!("{what}: {err}")).into(),
        RequestTokenError::Parse(err, _) => anyhow!("{what}: unparseable response: {err}"),
        RequestTokenError::Other(msg) => anyhow!("{what}: {msg}"),
    }
}

fn verify_id_token(
    client: &OidcClient,
    idp: &IdentityProvider,
    response: &CoreTokenResponse,
    nonce: Option<&Nonce>,
) -> Result<IdTokens> {
    let id_token = response.id_token().ok_or_else(|| {
        CliError::auth_required(
            "the identity provider returned no ID token (is the `openid` scope granted?)",
        )
    })?;
    // `iss` is compared below against the provider's accepted issuers (Google emits
    // both `https://accounts.google.com` and `accounts.google.com`).
    let verifier = client.id_token_verifier().require_issuer_match(false);
    let claims: &CoreIdTokenClaims = match nonce {
        Some(n) => id_token.claims(&verifier, n),
        // Refresh responses carry no fresh nonce.
        None => id_token.claims(&verifier, |_: Option<&Nonce>| Ok(())),
    }
    .map_err(|e| CliError::auth_required(format!("ID token rejected: {e}")))?;
    let iss = claims.issuer().as_str();
    if !idp
        .accepted_issuers()
        .iter()
        .any(|i| i.trim_end_matches('/') == iss.trim_end_matches('/'))
    {
        return Err(CliError::auth_required(format!(
            "ID token rejected: unexpected issuer {iss:?} (expected {:?})",
            idp.issuer
        ))
        .into());
    }
    Ok(IdTokens {
        id_token: id_token.to_string(),
        refresh_token: response.refresh_token().map(|t| t.secret().clone()),
        subject: claims.subject().to_string(),
        email: claims.email().map(|e| e.to_string()),
    })
}

/// Requested scopes: the provider's (default `openid email profile`), plus
/// `offline_access` for non-Google providers (Google issues refresh tokens to
/// installed apps anyway). `openid` itself is added by `openidconnect`.
pub fn scopes_for(idp: &IdentityProvider) -> Vec<String> {
    let mut scopes: Vec<String> = if idp.scopes.is_empty() {
        vec!["openid".into(), "email".into(), "profile".into()]
    } else {
        idp.scopes.clone()
    };
    if !idp.is_google() && !scopes.iter().any(|s| s == "offline_access") {
        scopes.push("offline_access".into());
    }
    scopes.retain(|s| s != "openid");
    scopes
}

/// Binds the loopback redirect listener: `--port`, else the provider's registered
/// ports in order, else an ephemeral port. Always `127.0.0.1`.
pub fn bind_loopback(port: Option<u16>, registered: &[u16]) -> Result<TcpListener> {
    let candidates: Vec<u16> = match port {
        Some(p) => vec![p],
        None if !registered.is_empty() => registered.to_vec(),
        None => vec![0],
    };
    let mut last_err = None;
    for p in candidates {
        match TcpListener::bind(SocketAddr::from((Ipv4Addr::LOCALHOST, p))) {
            Ok(l) => return Ok(l),
            Err(e) => last_err = Some((p, e)),
        }
    }
    let (p, e) = last_err.expect("at least one candidate port");
    Err(CliError::unavailable(format!(
        "cannot listen on 127.0.0.1:{p} for the login redirect: {e}"
    ))
    .into())
}

fn constant_time_eq(a: &str, b: &str) -> bool {
    Sha256::digest(a.as_bytes()) == Sha256::digest(b.as_bytes())
}

/// Outcome of one redirect.
#[derive(Debug)]
struct Callback {
    params: HashMap<String, String>,
}

impl Callback {
    /// Validates `state` and returns the authorization code.
    fn into_code(self, expected_state: &CsrfToken) -> Result<String> {
        if let Some(err) = self.params.get("error") {
            let desc = self
                .params
                .get("error_description")
                .map(|d| format!(": {d}"))
                .unwrap_or_default();
            let msg = format!("the identity provider returned an error ({err}{desc})");
            return Err(if err == "access_denied" {
                CliError::permission_denied(msg).into()
            } else {
                CliError::auth_required(msg).into()
            });
        }
        let state = self
            .params
            .get("state")
            .map(String::as_str)
            .unwrap_or_default();
        if !constant_time_eq(state, expected_state.secret()) {
            return Err(CliError::auth_required(
                "login aborted: the redirect's `state` does not match this login attempt",
            )
            .into());
        }
        self.params
            .get("code")
            .filter(|c| !c.is_empty())
            .cloned()
            .ok_or_else(|| {
                CliError::auth_required("the redirect carried no authorization code").into()
            })
    }
}

#[derive(Clone)]
struct CallbackState {
    path: String,
    tx: Arc<Mutex<Option<oneshot::Sender<Callback>>>>,
}

async fn callback_handler(
    State(state): State<CallbackState>,
    uri: Uri,
    Query(params): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    if uri.path() != state.path || !(params.contains_key("code") || params.contains_key("error")) {
        return (StatusCode::NOT_FOUND, Html("Not found".to_string()));
    }
    let tx = state.tx.lock().ok().and_then(|mut g| g.take());
    match tx {
        Some(tx) => {
            let _ = tx.send(Callback { params });
            (
                StatusCode::OK,
                Html(
                    "<!doctype html><title>cucinactl</title><p>Login finished. You can close this \
                     window and return to the terminal.</p>"
                        .to_string(),
                ),
            )
        }
        // Single use: later requests are refused.
        None => (StatusCode::GONE, Html("Already used".to_string())),
    }
}

async fn wait_for_callback(
    listener: TcpListener,
    path: &str,
    timeout: Duration,
) -> Result<Callback> {
    listener.set_nonblocking(true)?;
    let listener = tokio::net::TcpListener::from_std(listener)?;
    let (tx, rx) = oneshot::channel();
    let state = CallbackState {
        path: path.to_string(),
        tx: Arc::new(Mutex::new(Some(tx))),
    };
    let app = axum::Router::new()
        .fallback(axum::routing::get(callback_handler))
        .with_state(state);
    let (stop_tx, stop_rx) = oneshot::channel::<()>();
    let server = tokio::spawn(async move {
        let _ = axum::serve(listener, app)
            .with_graceful_shutdown(async {
                let _ = stop_rx.await;
            })
            .await;
    });
    let result = tokio::time::timeout(timeout, rx).await;
    let _ = stop_tx.send(());
    let _ = server.await;
    match result {
        Ok(Ok(cb)) => Ok(cb),
        Ok(Err(_)) => Err(anyhow!("the login callback server stopped unexpectedly")),
        Err(_) => Err(CliError::auth_required(format!(
            "timed out after {} waiting for the browser to finish the login",
            crate::util::format_duration(timeout)
        ))
        .into()),
    }
}

fn parse_pasted(input: &str, redirect: &Url) -> Result<Callback> {
    let pasted = Url::parse(input.trim()).map_err(|_| {
        CliError::usage("that is not a URL; paste the full address from the browser")
    })?;
    let same_target = pasted.scheme() == redirect.scheme()
        && pasted.host() == redirect.host()
        && pasted.port_or_known_default() == redirect.port_or_known_default()
        && pasted.path() == redirect.path();
    if !same_target {
        return Err(CliError::auth_required(format!(
            "the pasted URL does not start with the redirect URI {redirect}"
        ))
        .into());
    }
    Ok(Callback {
        params: pasted.query_pairs().into_owned().collect(),
    })
}

fn show_url(browser: &Browser, url: &Url, manual: bool) {
    let mut err = std::io::stderr().lock();
    let _ = writeln!(
        err,
        "To log in, open this URL in a browser{}:\n\n    {url}\n",
        if matches!(browser, Browser::System) && !manual {
            " (opening it for you)"
        } else {
            ""
        }
    );
    let _ = err.flush();
    match browser {
        Browser::System if !manual => {
            if let Err(e) = open_browser(url) {
                let _ = writeln!(
                    err,
                    "(could not open a browser: {e}; open the URL manually)"
                );
            }
        }
        Browser::Command(cmd) if !manual => {
            let mut parts = cmd.split_whitespace();
            if let Some(program) = parts.next() {
                let spawned = std::process::Command::new(program)
                    .args(parts)
                    .arg(url.as_str())
                    .stdin(std::process::Stdio::null())
                    .stdout(std::process::Stdio::null())
                    .spawn();
                if let Err(e) = spawned {
                    let _ = writeln!(
                        err,
                        "(could not run {program:?}: {e}; open the URL manually)"
                    );
                }
            }
        }
        _ => {}
    }
}

/// macOS: `open -u` hands the http(s) URL to the scheme's default handler through
/// LaunchServices, like `webbrowser` does through NSWorkspace, without linking AppKit
/// (absent from the hermetic macOS SDK Bazel links against; ADR 0806).
#[cfg(target_os = "macos")]
fn open_browser(url: &Url) -> std::io::Result<()> {
    if !matches!(url.scheme(), "http" | "https") {
        return Err(std::io::Error::other("refusing to open a non-http(s) URL"));
    }
    let status = std::process::Command::new("/usr/bin/open")
        .arg("-u")
        .arg(url.as_str())
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()?;
    if status.success() {
        Ok(())
    } else {
        Err(std::io::Error::other(format!("open exited with {status}")))
    }
}

/// Other platforms: `webbrowser` (hardened: http(s) URLs only).
#[cfg(not(target_os = "macos"))]
fn open_browser(url: &Url) -> std::io::Result<()> {
    webbrowser::open(url.as_str())
}

/// Runs the interactive login against one identity provider and returns verified tokens.
pub async fn login(http: &Http, idp: &IdentityProvider, opts: &LoginOptions) -> Result<IdTokens> {
    let client = provider_client(http, idp).await?;

    let listener = bind_loopback(opts.port, &idp.redirect_ports)?;
    let port = listener.local_addr()?.port();
    let path = if opts.redirect_path.starts_with('/') {
        opts.redirect_path.clone()
    } else {
        format!("/{}", opts.redirect_path)
    };
    let redirect = Url::parse(&format!("http://127.0.0.1:{port}{path}"))?;
    let client = client.set_redirect_uri(RedirectUrl::from_url(redirect.clone()));

    let (pkce_challenge, pkce_verifier) = PkceCodeChallenge::new_random_sha256();
    let mut request = client.authorize_url(
        CoreAuthenticationFlow::AuthorizationCode,
        CsrfToken::new_random,
        Nonce::new_random,
    );
    for scope in scopes_for(idp) {
        request = request.add_scope(Scope::new(scope));
    }
    if let Some(hd) = opts
        .hosted_domain
        .as_ref()
        .or(idp.hosted_domain_hint.as_ref())
    {
        request = request.add_extra_param("hd", hd.clone());
    }
    let (auth_url, csrf, nonce) = request.set_pkce_challenge(pkce_challenge).url();

    let callback = if opts.manual {
        drop(listener);
        show_url(&opts.browser, &auth_url, true);
        eprint!(
            "After signing in, your browser is redirected to {redirect} (the page may fail to load).\n\
             Paste that full address here and press Enter:\n> "
        );
        let line = tokio::task::spawn_blocking(|| {
            let mut s = String::new();
            std::io::stdin().read_line(&mut s).map(|_| s)
        })
        .await??;
        parse_pasted(&line, &redirect)?
    } else {
        show_url(&opts.browser, &auth_url, false);
        eprintln!("Waiting for the identity provider to redirect to {redirect} …");
        wait_for_callback(listener, &path, opts.timeout).await?
    };
    let code = callback.into_code(&csrf)?;

    let http_client = http_fn(http);
    let response = client
        .exchange_code(AuthorizationCode::new(code))
        .context("the identity provider has no token endpoint")?
        .set_pkce_verifier(pkce_verifier)
        .request_async(&http_client)
        .await
        .map_err(|e| token_error("redeeming the authorization code", e))?;
    verify_id_token(&client, idp, &response, Some(&nonce))
}

/// Obtains a fresh ID token with the stored refresh token (renewal, R-AUTH-6).
pub async fn refresh(http: &Http, idp: &IdentityProvider, refresh_token: &str) -> Result<IdTokens> {
    let client = provider_client(http, idp).await?;
    let http_client = http_fn(http);
    let rt = RefreshToken::new(refresh_token.to_string());
    let response = client
        .exchange_refresh_token(&rt)
        .context("the identity provider has no token endpoint")?
        .request_async(&http_client)
        .await
        .map_err(|e| token_error("refreshing the identity-provider session", e))?;
    let mut tokens = verify_id_token(&client, idp, &response, None)?;
    if tokens.refresh_token.is_none() {
        tokens.refresh_token = Some(refresh_token.to_string());
    }
    Ok(tokens)
}

#[cfg(test)]
mod tests {
    use super::*;

    // R-AUTH-5: the redirect listener is bound to 127.0.0.1 only (never all
    // interfaces, never a name), on the requested port or an ephemeral one.
    #[test]
    fn loopback_listener_binds_ipv4_loopback_only() {
        let l = bind_loopback(None, &[]).expect("bind");
        let addr = l.local_addr().unwrap();
        assert_eq!(addr.ip(), std::net::IpAddr::from(Ipv4Addr::LOCALHOST));
        assert_ne!(addr.port(), 0);
        // A registered-port list is honoured in order (first free one wins).
        let busy = l.local_addr().unwrap().port();
        let l2 = bind_loopback(None, &[busy, 0]).expect("falls through to the next port");
        assert_eq!(
            l2.local_addr().unwrap().ip(),
            std::net::IpAddr::from(Ipv4Addr::LOCALHOST)
        );
        assert_ne!(l2.local_addr().unwrap().port(), busy);
    }
}
