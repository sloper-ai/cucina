// SPDX-License-Identifier: FSL-1.1-ALv2

//! The Cucina JWT cache: one 0600 JSON file per profile, replaced atomically, with
//! renewals serialized by an advisory lock file (see [`super::lock`]).

use std::fs;
use std::path::PathBuf;
use std::time::Duration;

use anyhow::{Context, Result};
use base64::Engine as _;
use serde::{Deserialize, Serialize};

use crate::config::{AuthMethod, Paths};

/// Renew when fewer than this many seconds of validity remain (R-AUTH-6).
pub const RENEW_BEFORE_SECS: i64 = 5 * 60;
/// Bazel is told the credential expires this much before the JWT does (R-AUTH-8).
pub const HELPER_EXPIRY_MARGIN_SECS: i64 = 2 * 60;

/// A cached Cucina JWT.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CachedToken {
    pub version: u32,
    /// The Cucina JWT (bearer token).
    pub access_token: String,
    /// JWT `exp` claim (Unix seconds).
    pub exp: i64,
    /// JWT `sub` claim, for display.
    #[serde(default)]
    pub subject: Option<String>,
    /// When the token was obtained (Unix seconds).
    pub obtained_at: i64,
    /// How the session renews.
    pub method: AuthMethod,
}

impl CachedToken {
    /// Builds a cache entry from an STS response (exp from the JWT, else `expires_in`).
    pub fn from_access_token(
        access_token: String,
        expires_in: Option<u64>,
        now: i64,
        method: AuthMethod,
    ) -> CachedToken {
        let claims = unverified_claims(&access_token);
        let exp = claims
            .as_ref()
            .and_then(|c| c.get("exp"))
            .and_then(serde_json::Value::as_i64)
            .or_else(|| expires_in.map(|s| now + i64::try_from(s).unwrap_or(0)))
            .unwrap_or(now);
        let subject = claims
            .as_ref()
            .and_then(|c| c.get("sub"))
            .and_then(|v| v.as_str())
            .map(str::to_string);
        CachedToken {
            version: 1,
            access_token,
            exp,
            subject,
            obtained_at: now,
            method,
        }
    }

    /// Seconds of validity left at `now`.
    pub fn remaining(&self, now: i64) -> i64 {
        self.exp - now
    }

    /// Valid for at least `min_secs` more seconds.
    pub fn valid_for(&self, now: i64, min_secs: i64) -> bool {
        self.remaining(now) >= min_secs
    }
}

/// Decodes a JWT payload without verifying it (local display/expiry only; the
/// servers verify every token).
pub fn unverified_claims(jwt: &str) -> Option<serde_json::Map<String, serde_json::Value>> {
    let payload = jwt.split('.').nth(1)?;
    let bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(payload.trim_end_matches('='))
        .ok()?;
    match serde_json::from_slice(&bytes).ok()? {
        serde_json::Value::Object(map) => Some(map),
        _ => None,
    }
}

/// File-backed cache for one profile.
#[derive(Debug, Clone)]
pub struct TokenCache {
    path: PathBuf,
    lock_path: PathBuf,
}

/// How long a credential helper waits for a concurrent renewal (Bazel's default
/// helper timeout is 10 s).
pub const LOCK_TIMEOUT: Duration = Duration::from_secs(9);

impl TokenCache {
    pub fn new(paths: &Paths, profile: &str) -> TokenCache {
        TokenCache {
            path: paths.token_file(profile),
            lock_path: paths.lock_file(profile),
        }
    }

    /// Reads the cached token; missing or unreadable caches are `None`.
    pub fn read(&self) -> Option<CachedToken> {
        let bytes = fs::read(&self.path).ok()?;
        let token: CachedToken = serde_json::from_slice(&bytes).ok()?;
        (token.version == 1 && !token.access_token.is_empty()).then_some(token)
    }

    /// Replaces the cached token atomically (0600).
    pub fn write(&self, token: &CachedToken) -> Result<()> {
        let data = serde_json::to_vec_pretty(token)?;
        crate::config::write_private(&self.path, &data)
            .with_context(|| format!("writing token cache {}", self.path.display()))
    }

    /// Deletes the cached token (logout).
    pub fn remove(&self) -> Result<()> {
        crate::config::remove_if_exists(&self.path)?;
        crate::config::remove_if_exists(&self.lock_path)
    }

    /// Takes the renewal lock.
    pub fn lock(&self) -> Result<super::lock::FileLock> {
        super::lock::lock_exclusive(&self.lock_path, LOCK_TIMEOUT)
    }
}
