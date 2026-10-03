// SPDX-License-Identifier: FSL-1.1-ALv2

//! Client authentication (R-AUTH-5..8, R-AUTH-10..12): OIDC login, service-account
//! keys, GitHub Actions OIDC, the RFC 8693 exchange at the Cucina STS, renewal, and
//! local storage (OS keychain for the IdP refresh token, 0600 locked cache for the
//! 15-minute Cucina JWT).

pub mod discovery;
pub mod github;
pub mod jwt;
pub mod lock;
pub mod oidc;
pub mod secrets;
pub mod sts;
pub mod token;

use std::time::Duration;

use anyhow::{Context, Result};

use crate::config::{AuthMethod, Paths, Profile};
use crate::exit::CliError;
use crate::http::Http;
use discovery::{CucinaConfiguration, IdentityProvider};
use secrets::{SecretKind, Secrets};
use token::{CachedToken, TokenCache};

/// Timeout for each HTTP call made while renewing (fits Bazel's 10 s helper timeout
/// for the common single-round-trip case).
pub const RENEW_HTTP_TIMEOUT: Duration = Duration::from_secs(8);

/// One profile and the locations of its local state.
#[derive(Debug, Clone)]
pub struct ProfileCtx {
    pub paths: Paths,
    pub name: String,
    pub profile: Profile,
}

impl ProfileCtx {
    pub fn new(paths: &Paths, name: &str, profile: &Profile) -> ProfileCtx {
        ProfileCtx {
            paths: paths.clone(),
            name: name.to_string(),
            profile: profile.clone(),
        }
    }

    pub fn cache(&self) -> TokenCache {
        TokenCache::new(&self.paths, &self.name)
    }

    pub fn secrets(&self) -> Result<Secrets> {
        Secrets::new(&self.paths, self.profile.credential_store)
    }

    pub fn http(&self, timeout: Duration) -> Result<Http> {
        Http::new(self.profile.ca_file.as_deref(), timeout)
    }

    /// The discovery document cached at login.
    pub fn load_discovery(&self) -> Result<CucinaConfiguration> {
        let path = self.paths.discovery_file(&self.name);
        let bytes = std::fs::read(&path).map_err(|_| {
            CliError::auth_required(format!(
                "no cached discovery document for profile {:?}; run `cucinactl login {}`",
                self.name, self.profile.url
            ))
        })?;
        serde_json::from_slice(&bytes).with_context(|| format!("parsing {}", path.display()))
    }

    pub fn save_discovery(&self, doc: &CucinaConfiguration) -> Result<()> {
        crate::config::write_private(
            &self.paths.discovery_file(&self.name),
            &serde_json::to_vec_pretty(doc)?,
        )
    }

    /// The identity provider this profile logged in with.
    pub fn identity_provider(&self) -> Result<IdentityProvider> {
        let doc = self.load_discovery()?;
        let wanted = self.profile.provider.as_deref();
        doc.identity_providers
            .into_iter()
            .find(|p| wanted.is_none_or(|w| p.name == w))
            .ok_or_else(|| {
                CliError::auth_required(format!(
                    "identity provider {:?} is no longer offered by {}; run `cucinactl login`",
                    wanted.unwrap_or("(any)"),
                    self.profile.url
                ))
                .into()
            })
    }
}

/// Returns a Cucina JWT valid for at least `min_secs` seconds, renewing it when
/// needed. Renewals are serialized across processes by the profile's lock file; a
/// process that waited for the lock re-reads the cache and reuses a token another
/// process just renewed. If renewal fails while the cached token is still valid
/// for `fallback_secs`, the cached token is returned (with a warning).
pub async fn ensure_token(
    ctx: &ProfileCtx,
    min_secs: i64,
    fallback_secs: i64,
) -> Result<CachedToken> {
    ensure_token_with_clock(ctx, min_secs, fallback_secs, crate::util::now_unix).await
}

pub(crate) async fn ensure_token_with_clock(
    ctx: &ProfileCtx,
    min_secs: i64,
    fallback_secs: i64,
    clock: fn() -> i64,
) -> Result<CachedToken> {
    let cache = ctx.cache();
    let now = clock();
    if let Some(t) = cache.read()
        && t.valid_for(now, min_secs)
    {
        return Ok(t);
    }
    let lock_cache = cache.clone();
    let _lock = tokio::task::spawn_blocking(move || lock_cache.lock()).await??;
    let now = clock();
    let current = cache.read();
    if let Some(t) = &current
        && t.valid_for(now, min_secs)
    {
        return Ok(t.clone());
    }
    match renew_with_clock(ctx, clock).await {
        Ok(fresh) => {
            cache.write(&fresh)?;
            Ok(fresh)
        }
        Err(err) => match current {
            Some(t) if t.valid_for(now, fallback_secs) => {
                tracing::warn!("session renewal failed, using the cached token: {err:#}");
                Ok(t)
            }
            _ => Err(err),
        },
    }
}

/// Renews the session per the profile's method (never prompts).
pub async fn renew(ctx: &ProfileCtx) -> Result<CachedToken> {
    renew_with_clock(ctx, crate::util::now_unix).await
}

async fn renew_with_clock(ctx: &ProfileCtx, clock: fn() -> i64) -> Result<CachedToken> {
    let http = ctx.http(RENEW_HTTP_TIMEOUT)?;
    let secrets = ctx.secrets()?;
    let now = clock();
    let not_logged_in = || {
        CliError::auth_required(format!(
            "not logged in to {} (profile {:?}); run `cucinactl login {}`",
            ctx.profile.url, ctx.name, ctx.profile.url
        ))
    };
    let response = match ctx.profile.auth {
        AuthMethod::Oidc => {
            let refresh_token = secrets
                .get(&ctx.name, SecretKind::RefreshToken)?
                .ok_or_else(not_logged_in)?;
            let idp = ctx.identity_provider()?;
            let tokens = oidc::refresh(&http, &idp, &refresh_token).await?;
            if let Some(rotated) = &tokens.refresh_token
                && rotated != &refresh_token
            {
                secrets.set(&ctx.name, SecretKind::RefreshToken, rotated)?;
            }
            sts::exchange(
                &http,
                &ctx.profile.token_endpoint,
                &tokens.id_token,
                sts::TOKEN_TYPE_ID_TOKEN,
                None,
            )
            .await?
        }
        AuthMethod::ServiceKey => {
            let key = secrets
                .get(&ctx.name, SecretKind::ServiceKey)?
                .ok_or_else(not_logged_in)?;
            sts::exchange(
                &http,
                &ctx.profile.token_endpoint,
                &key,
                sts::TOKEN_TYPE_SERVICE_KEY,
                None,
            )
            .await?
        }
    };
    Ok(CachedToken::from_access_token(
        response.access_token,
        response.expires_in,
        now,
        ctx.profile.auth,
    ))
}

/// GitHub Actions: fetch a fresh GitHub OIDC token and exchange it (nothing stored).
pub async fn github_actions_token(
    http: &Http,
    env: &github::ActionsEnv,
    token_endpoint: &str,
) -> Result<CachedToken> {
    let now = crate::util::now_unix();
    let gh = github::fetch_id_token(http, env).await?;
    let response = sts::exchange(http, token_endpoint, &gh, sts::TOKEN_TYPE_JWT, None).await?;
    Ok(CachedToken::from_access_token(
        response.access_token,
        response.expires_in,
        now,
        AuthMethod::Oidc,
    ))
}
