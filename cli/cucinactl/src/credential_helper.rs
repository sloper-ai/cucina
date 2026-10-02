// SPDX-License-Identifier: FSL-1.1-ALv2

//! Bazel credential helper (R-AUTH-8, R-AUTH-6, R-AUTH-7).
//!
//! Bazel runs `<helper> get` (no other arguments) once per gRPC service, possibly
//! concurrently, writes `{"uri": "https://<host>/<grpc service>"}` to stdin and
//! expects exactly
//! `{"headers":{"Authorization":["Bearer <jwt>"]},"expires":"<RFC 3339>"}` on
//! stdout, with `expires = exp − 2 min` and whole seconds (Bazel honours it).
//!
//! * Reachable as `cucinactl credential-helper get` and, via argv[0] dispatch, as
//!   `cucina-credential-helper` (hardlink/copy of `cucinactl`).
//! * Never prompts: without a valid session it tells the user to run
//!   `cucinactl login` on stderr and exits 3.
//! * Fast path (token valid ≥ 5 min): read config + cache, print — no network, no
//!   async runtime, no keychain access. Otherwise renews under the profile's lock.
//! * GitHub Actions (`ACTIONS_ID_TOKEN_REQUEST_URL`/`_TOKEN` present): fetches a
//!   GitHub OIDC token for `audience=cucina`, exchanges it, stores nothing.
//! * Scope it per host in `.bazelrc`; `cucinactl bazelrc` emits
//!   `--credential_helper=<host>=<path>`.

use std::collections::BTreeMap;
use std::ffi::OsString;
use std::io::{Read as _, Write as _};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

use crate::auth::token::{CachedToken, HELPER_EXPIRY_MARGIN_SECS, RENEW_BEFORE_SECS};
use crate::auth::{self, ProfileCtx};
use crate::config::{self, Config, Paths};
use crate::exit::{CliError, ExitCode, exit_code_for};

/// Overrides the base URL used to find the STS in GitHub Actions mode.
pub const CUCINA_URL_ENV: &str = "CUCINA_URL";
/// Extra CA bundle (PEM) for a private CA (every mode; see [`crate::tls`]).
pub use crate::tls::CUCINA_CA_FILE_ENV;

#[derive(Debug, Deserialize)]
struct Request {
    uri: String,
}

#[derive(Debug, Serialize)]
struct Response<'a> {
    headers: BTreeMap<&'static str, [String; 1]>,
    expires: &'a str,
}

/// The exact stdout document for a token.
pub fn render(token: &CachedToken) -> String {
    let expires = crate::util::rfc3339_seconds(token.exp - HELPER_EXPIRY_MARGIN_SECS);
    let mut headers = BTreeMap::new();
    headers.insert("Authorization", [format!("Bearer {}", token.access_token)]);
    serde_json::to_string(&Response {
        headers,
        expires: &expires,
    })
    .expect("serializable")
}

/// Entry point for `cucina-credential-helper [get]` (argv[0] dispatch).
pub fn main_argv0(args: &[OsString]) -> std::process::ExitCode {
    match args.first().and_then(|a| a.to_str()) {
        None | Some("get") => run_get().into(),
        Some("--version") => {
            println!("cucina-credential-helper {}", crate::VERSION);
            ExitCode::Ok.into()
        }
        Some(other) => {
            eprintln!(
                "cucina-credential-helper: unsupported command {other:?} (Bazel invokes `get`); \
                 this is cucinactl's Bazel credential helper"
            );
            ExitCode::Usage.into()
        }
    }
}

/// Reads the request from stdin, prints the response; returns the exit code.
pub fn run_get() -> ExitCode {
    let mut input = String::new();
    if let Err(e) = std::io::stdin().read_to_string(&mut input) {
        eprintln!("cucina-credential-helper: reading stdin: {e}");
        return ExitCode::Error;
    }
    match get(&input) {
        Ok(doc) => {
            let mut out = std::io::stdout().lock();
            if writeln!(out, "{doc}").and_then(|()| out.flush()).is_err() {
                return ExitCode::Error;
            }
            ExitCode::Ok
        }
        Err(err) => {
            eprintln!("cucina-credential-helper: {err:#}");
            exit_code_for(&err)
        }
    }
}

/// Handles one `get` request and returns the stdout document.
pub fn get(input: &str) -> Result<String> {
    let request: Request = serde_json::from_str(input.trim()).map_err(|e| {
        CliError::usage(format!(
            "expected a JSON request like {{\"uri\": \"https://host/service\"}} on stdin: {e}"
        ))
    })?;
    let host = config::host_of(&request.uri)
        .ok_or_else(|| CliError::usage(format!("request URI {:?} has no host", request.uri)))?;

    if let Some(actions) = auth::github::detect() {
        return runtime()?.block_on(github_actions(&host, &actions));
    }

    let paths = Paths::resolve()?;
    let config = Config::load(&paths)?;
    let selected = match std::env::var("CUCINA_PROFILE")
        .ok()
        .filter(|p| !p.is_empty())
    {
        Some(name) => Some(config.select(Some(&name))?),
        None => config.profile_for_host(&host),
    };
    let (name, profile) = selected
        .filter(|(_, p)| p.remote_host().as_deref() == Some(host.as_str()))
        .ok_or_else(|| {
            CliError::auth_required(format!(
                "no matching cucinactl profile for {host}; run `cucinactl login https://{host}`"
            ))
        })?;
    let ctx = ProfileCtx::new(&paths, &name, &profile);

    // Fast path: no runtime, no network, no keychain.
    let now = crate::util::now_unix();
    if let Some(token) = ctx.cache().read()
        && token.valid_for(now, RENEW_BEFORE_SECS)
    {
        return Ok(render(&token));
    }
    let token = runtime()?
        .block_on(auth::ensure_token(
            &ctx,
            RENEW_BEFORE_SECS,
            HELPER_EXPIRY_MARGIN_SECS + 30,
        ))
        .map_err(|e| {
            if exit_code_for(&e) == ExitCode::AuthRequired {
                e.context("run `cucinactl login` to sign in again")
            } else {
                e
            }
        })?;
    Ok(render(&token))
}

async fn github_actions(host: &str, actions: &auth::github::ActionsEnv) -> Result<String> {
    // A private CA: the matching profile's `ca_file`, plus CUCINA_CA_FILE/SSL_CERT_FILE
    // (crate::tls). The STS: a profile for this host (if `cucinactl login`/`config` ran), else
    // $CUCINA_URL, else https://<host>; resolved through the discovery document.
    let paths = Paths::resolve().ok();
    let profile = paths
        .as_ref()
        .and_then(|p| Config::load(p).ok())
        .and_then(|c| c.profile_for_host(host));
    let ca = profile.as_ref().and_then(|(_, p)| p.ca_file.clone());
    let http = crate::http::Http::new(ca.as_deref(), auth::RENEW_HTTP_TIMEOUT)?;
    let token_endpoint = match profile {
        Some((_, p)) if !p.token_endpoint.is_empty() => p.token_endpoint,
        _ => {
            let base = std::env::var(CUCINA_URL_ENV)
                .ok()
                .filter(|v| !v.is_empty())
                .unwrap_or_else(|| format!("https://{host}"));
            let base = crate::http::parse_base_url(&base)?;
            auth::discovery::fetch(&http, &base)
                .await
                .context("finding the Cucina STS (set CUCINA_URL to the Cucina URL)")?
                .token_endpoint
        }
    };
    let token = auth::github_actions_token(&http, actions, &token_endpoint).await?;
    Ok(render(&token))
}

fn runtime() -> Result<tokio::runtime::Runtime> {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .context("starting the async runtime")
}
