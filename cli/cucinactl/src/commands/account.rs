// SPDX-License-Identifier: FSL-1.1-ALv2

//! `login`, `logout`, `whoami` and `keys` (R-AUTH-5, R-AUTH-10..12).

use std::collections::BTreeMap;
use std::io::{self, Read as _, Write};

use anyhow::{Context, Result};
use cucina_api::proto::cucina::v1 as pb;
use serde::Serialize;

use super::Ctx;
use crate::auth::discovery::{self, IdentityProvider};
use crate::auth::oidc::{self, Browser, LoginOptions};
use crate::auth::secrets::SecretKind;
use crate::auth::token::{CachedToken, unverified_claims};
use crate::auth::{ProfileCtx, sts};
use crate::cli::{KeysCmd, LoginArgs, LogoutArgs};
use crate::config::{self, AuthMethod, Profile};
use crate::exit::CliError;
use crate::http::Http;
use crate::output::{Render, dash, emit, write_fields};
use crate::util::{now_unix, proto_time, rfc3339_seconds, to_proto_duration};
use crate::views::{
    ResultView, RevocationEntryView, RevocationListView, RevocationView, ServiceKeyListView,
    ServiceKeyView,
};

/// `login.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct LoginView {
    pub profile: String,
    pub url: String,
    pub method: AuthMethod,
    pub provider: Option<String>,
    pub subject: Option<String>,
    pub expires_at: String,
    /// The stored IdP session was renewed instead of running the browser flow.
    pub reused_session: bool,
}

impl Render for LoginView {
    const SCHEMA: &'static str = "login.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        writeln!(
            out,
            "Logged in to {} as {} (profile {}; token valid until {}){}",
            self.url,
            self.subject.as_deref().unwrap_or("?"),
            self.profile,
            self.expires_at,
            if self.reused_session {
                " — renewed the stored session; use --force to log in again"
            } else {
                ""
            }
        )
    }
}

/// `whoami.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct WhoamiView {
    pub profile: String,
    pub url: String,
    pub method: AuthMethod,
    pub subject: Option<String>,
    pub issuer: Option<String>,
    pub session_id: Option<String>,
    pub expires_at: String,
    pub expires_in_seconds: i64,
    /// Verb → instance names (`cas_read`, `cas_write`, `ac_read`, `ac_write`, `execute`, `admin`).
    pub grants: BTreeMap<String, Vec<String>>,
    /// Signature, issuer, audience and expiry checked against the STS JWKS.
    pub verified: bool,
    pub verification_error: Option<String>,
}

impl Render for WhoamiView {
    const SCHEMA: &'static str = "whoami.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut fields = vec![
            ("Profile", self.profile.clone()),
            ("URL", self.url.clone()),
            (
                "Subject",
                self.subject.clone().unwrap_or_else(|| "-".into()),
            ),
            (
                "Session",
                self.session_id.clone().unwrap_or_else(|| "-".into()),
            ),
            (
                "Expires",
                format!("{} (in {}s)", self.expires_at, self.expires_in_seconds),
            ),
            (
                "Verified",
                match &self.verification_error {
                    None if self.verified => "yes (STS JWKS)".into(),
                    Some(e) => format!("no: {e}"),
                    None => "not checked".into(),
                },
            ),
        ];
        for (verb, names) in &self.grants {
            fields.push(("Grant", format!("{verb}: {}", dash(&names.join(", ")))));
        }
        write_fields(out, &fields)
    }
}

fn read_key(args: &LoginArgs) -> Result<Option<String>> {
    let raw = if let Some(var) = &args.key_env {
        Some(
            std::env::var(var)
                .map_err(|_| CliError::usage(format!("environment variable {var} is not set")))?,
        )
    } else if let Some(path) = &args.key {
        if path.as_os_str() == "-" {
            let mut s = String::new();
            std::io::stdin().read_to_string(&mut s)?;
            Some(s)
        } else {
            Some(
                std::fs::read_to_string(path)
                    .with_context(|| format!("reading the key file {}", path.display()))?,
            )
        }
    } else {
        None
    };
    Ok(raw.map(|k| k.trim().to_string()).filter(|k| !k.is_empty()))
}

fn choose_provider(
    providers: &[IdentityProvider],
    wanted: Option<&str>,
    url: &str,
) -> Result<IdentityProvider> {
    let oidc: Vec<&IdentityProvider> = providers.iter().filter(|p| p.kind == "oidc").collect();
    match wanted {
        Some(w) => oidc
            .iter()
            .find(|p| p.name == w)
            .map(|p| (*p).clone())
            .ok_or_else(|| {
                let names: Vec<&str> = oidc.iter().map(|p| p.name.as_str()).collect();
                CliError::usage(format!(
                    "{url} offers no identity provider {w:?} (available: {})",
                    names.join(", ")
                ))
                .into()
            }),
        None => match oidc.as_slice() {
            [one] => Ok((*one).clone()),
            [] => Err(CliError::usage(format!(
                "{url} offers no interactive identity provider; log in with --key"
            ))
            .into()),
            many => {
                let names: Vec<&str> = many.iter().map(|p| p.name.as_str()).collect();
                Err(CliError::usage(format!(
                    "{url} offers several identity providers; pass --provider ({})",
                    names.join(", ")
                ))
                .into())
            }
        },
    }
}

fn default_profile_name(url: &openidconnect::url::Url) -> String {
    let host = url.host_str().unwrap_or("cucina");
    let mut name: String = host
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '.' {
                c
            } else {
                '-'
            }
        })
        .collect();
    if let Some(port) = url.port() {
        name.push_str(&format!("-{port}"));
    }
    name.truncate(64);
    name
}

pub async fn login(ctx: &Ctx, args: &LoginArgs) -> Result<()> {
    let mut config = ctx.config()?;
    let existing_selected = ctx
        .global
        .profile
        .as_ref()
        .and_then(|n| config.profiles.get(n).map(|p| (n.clone(), p.clone())))
        .or_else(|| {
            args.url
                .is_none()
                .then(|| config.select(None).ok())
                .flatten()
        });
    let url_input = match (&args.url, &existing_selected) {
        (Some(u), _) => u.clone(),
        (None, Some((_, p))) => p.url.clone(),
        (None, None) => {
            return Err(CliError::usage(
                "usage: cucinactl login <url> (e.g. https://cucina.example.com)",
            )
            .into());
        }
    };
    let base = crate::http::parse_base_url(&url_input)?;
    let url = base.as_str().trim_end_matches('/').to_string();
    let name = match (&ctx.global.profile, &existing_selected) {
        (Some(n), _) => n.clone(),
        (None, Some((n, p))) if p.url == url => n.clone(),
        _ => config
            .profiles
            .iter()
            .find(|(_, p)| p.url == url)
            .map(|(n, _)| n.clone())
            .unwrap_or_else(|| default_profile_name(&base)),
    };
    config::validate_profile_name(&name)?;
    let existing = config.profiles.get(&name).cloned();

    let ca_file = args
        .ca_file
        .clone()
        .or_else(|| existing.as_ref().and_then(|p| p.ca_file.clone()));
    let http = Http::new(ca_file.as_deref(), ctx.global.timeout)?;
    let doc = discovery::fetch(&http, &base).await?;

    let mut profile = Profile {
        url: url.clone(),
        token_endpoint: doc.token_endpoint.clone(),
        jwks_uri: doc.jwks_uri.clone(),
        remote_executor: doc.endpoints.remote_execution.clone(),
        instance_name: doc.endpoints.instance_name.clone(),
        management: doc.endpoints.management.clone(),
        auth: AuthMethod::Oidc,
        provider: None,
        ca_file,
        credential_store: args
            .credential_store
            .or_else(|| existing.as_ref().map(|p| p.credential_store))
            .unwrap_or_default(),
    };
    let pctx = ProfileCtx::new(&ctx.paths, &name, &profile);
    let secrets = pctx.secrets()?;
    let now = now_unix();

    let (response, reused) = if let Some(key) = read_key(args)? {
        profile.auth = AuthMethod::ServiceKey;
        let resp = sts::exchange(
            &http,
            &profile.token_endpoint,
            &key,
            sts::TOKEN_TYPE_SERVICE_KEY,
            None,
        )
        .await?;
        secrets.set(&name, SecretKind::ServiceKey, &key)?;
        let _ = secrets.delete(&name, SecretKind::RefreshToken);
        (resp, false)
    } else {
        let wanted = args
            .provider
            .as_deref()
            .or_else(|| existing.as_ref().and_then(|p| p.provider.as_deref()));
        let idp = choose_provider(&doc.identity_providers, wanted, &url)?;
        profile.provider = Some(idp.name.clone());
        // Reuse this machine's refresh token (Google caps refresh tokens per account
        // and client) unless --force.
        let mut renewed = None;
        if !args.force
            && existing.as_ref().is_some_and(|p| {
                p.auth == AuthMethod::Oidc && p.provider.as_deref() == Some(idp.name.as_str())
            })
            && let Ok(Some(rt)) = secrets.get(&name, SecretKind::RefreshToken)
            && let Ok(tokens) = oidc::refresh(&http, &idp, &rt).await
        {
            if let Some(rotated) = tokens.refresh_token.as_ref().filter(|r| **r != rt) {
                secrets.set(&name, SecretKind::RefreshToken, rotated)?;
            }
            renewed = Some(tokens);
        }
        let reused = renewed.is_some();
        let tokens = match renewed {
            Some(t) => t,
            None => {
                let opts = LoginOptions {
                    port: args.port,
                    redirect_path: args.redirect_path.clone(),
                    browser: match (&args.browser_command, args.no_browser) {
                        (_, true) => Browser::PrintOnly,
                        (Some(cmd), false) => Browser::Command(cmd.clone()),
                        (None, false) => Browser::System,
                    },
                    manual: args.manual,
                    hosted_domain: args.hd.clone(),
                    timeout: args.login_timeout,
                };
                let t = oidc::login(&http, &idp, &opts).await?;
                match &t.refresh_token {
                    Some(rt) => secrets.set(&name, SecretKind::RefreshToken, rt)?,
                    None => eprintln!(
                        "warning: {} issued no refresh token; run `cucinactl login` again when the token expires",
                        idp.name
                    ),
                }
                t
            }
        };
        let _ = secrets.delete(&name, SecretKind::ServiceKey);
        let resp = sts::exchange(
            &http,
            &profile.token_endpoint,
            &tokens.id_token,
            sts::TOKEN_TYPE_ID_TOKEN,
            None,
        )
        .await?;
        (resp, reused)
    };

    let pctx = ProfileCtx::new(&ctx.paths, &name, &profile);
    pctx.save_discovery(&doc)?;
    let token = CachedToken::from_access_token(
        response.access_token,
        response.expires_in,
        now,
        profile.auth,
    );
    pctx.cache().write(&token)?;
    config.profiles.insert(name.clone(), profile.clone());
    config.current_profile = Some(name.clone());
    config.save(&ctx.paths)?;

    emit(
        ctx.global.output,
        &LoginView {
            profile: name,
            url,
            method: profile.auth,
            provider: profile.provider.clone(),
            subject: token.subject.clone(),
            expires_at: rfc3339_seconds(token.exp),
            reused_session: reused,
        },
    )
}

pub fn logout(ctx: &Ctx, args: &LogoutArgs) -> Result<()> {
    let mut config = ctx.config()?;
    let names: Vec<String> = if args.all {
        config.profiles.keys().cloned().collect()
    } else {
        vec![config.select(ctx.global.profile.as_deref())?.0]
    };
    for name in &names {
        let profile = config.profiles.get(name).cloned().unwrap_or_default();
        let pctx = ProfileCtx::new(&ctx.paths, name, &profile);
        pctx.cache().remove()?;
        if let Ok(secrets) = pctx.secrets() {
            for kind in [SecretKind::RefreshToken, SecretKind::ServiceKey] {
                if let Err(e) = secrets.delete(name, kind) {
                    eprintln!("warning: could not delete the stored secret of {name}: {e:#}");
                }
            }
        }
        if args.forget {
            config::remove_if_exists(&ctx.paths.discovery_file(name))?;
            config.profiles.remove(name);
            if config.current_profile.as_deref() == Some(name.as_str()) {
                config.current_profile = None;
            }
        }
    }
    if args.forget {
        config.save(&ctx.paths)?;
    }
    emit(
        ctx.global.output,
        &ResultView::new(
            "logout",
            &names.join(","),
            "local session deleted (the identity provider's grant is untouched)",
        ),
    )
}

pub async fn whoami(ctx: &Ctx) -> Result<()> {
    let pctx = ctx.profile()?;
    let token = crate::auth::ensure_token(&pctx, 60, 30).await?;
    let claims = unverified_claims(&token.access_token).unwrap_or_default();
    let str_claim = |k: &str| claims.get(k).and_then(|v| v.as_str()).map(str::to_string);
    let mut grants = BTreeMap::new();
    if let Some(serde_json::Value::Object(map)) = claims.get("cucina") {
        for (verb, v) in map {
            let names = v
                .as_array()
                .map(|a| {
                    a.iter()
                        .filter_map(|n| n.as_str().map(str::to_string))
                        .collect()
                })
                .unwrap_or_default();
            grants.insert(verb.clone(), names);
        }
    }
    let issuer = pctx
        .load_discovery()
        .map(|d| d.issuer)
        .unwrap_or_else(|_| pctx.profile.url.clone());
    let (verified, verification_error) = if pctx.profile.jwks_uri.is_empty() {
        (false, Some("no jwks_uri in the profile".to_string()))
    } else {
        let http = pctx.http(ctx.global.timeout)?;
        match crate::auth::jwt::verify(&http, &pctx.profile.jwks_uri, &issuer, &token.access_token)
            .await
        {
            Ok(_) => (true, None),
            Err(e) => (false, Some(format!("{e:#}"))),
        }
    };
    let now = now_unix();
    emit(
        ctx.global.output,
        &WhoamiView {
            profile: pctx.name.clone(),
            url: pctx.profile.url.clone(),
            method: pctx.profile.auth,
            subject: str_claim("sub"),
            issuer: str_claim("iss"),
            session_id: str_claim("sid"),
            expires_at: rfc3339_seconds(token.exp),
            expires_in_seconds: token.remaining(now),
            grants,
            verified,
            verification_error,
        },
    )
}

pub async fn keys(ctx: &Ctx, cmd: KeysCmd) -> Result<()> {
    let s = ctx.session().await?;
    let m = &s.management;
    let out = ctx.global.output;
    match cmd {
        KeysCmd::Create {
            account,
            description,
            ttl,
        } => {
            let r = m
                .create_service_key(pb::CreateServiceKeyRequest {
                    account: account.clone(),
                    description,
                    ttl: ttl.map(to_proto_duration).into(),
                    ..Default::default()
                })
                .await?;
            emit(
                out,
                &ServiceKeyView {
                    key_id: r.key_id,
                    account,
                    key: r.key,
                },
            )
        }
        KeysCmd::List { account } => {
            let r = m
                .list_service_keys(pb::ListServiceKeysRequest {
                    account,
                    ..Default::default()
                })
                .await?;
            emit(out, &ServiceKeyListView::from(&r))
        }
        KeysCmd::Revoke {
            key_id,
            sub,
            sid,
            reason,
        } => match key_id {
            Some(id) => {
                ctx.confirm(&format!("revoke service key {id}"))?;
                m.revoke_service_key(pb::RevokeServiceKeyRequest {
                    key_id: id.clone(),
                    ..Default::default()
                })
                .await?;
                emit(
                    out,
                    &ResultView::new(
                        "revoke service key",
                        &id,
                        "exchanges with it are refused immediately",
                    ),
                )
            }
            None => {
                let (sub, sid) = (sub.unwrap_or_default(), sid.unwrap_or_default());
                let who = if sid.is_empty() {
                    format!("principal {sub}")
                } else {
                    format!("session {sid}")
                };
                ctx.confirm(&format!("revoke {who}"))?;
                let r = m
                    .revoke_principal(pb::RevokePrincipalRequest {
                        sub: sub.clone(),
                        sid: sid.clone(),
                        reason,
                        ..Default::default()
                    })
                    .await?;
                emit(
                    out,
                    &RevocationView {
                        sub,
                        sid,
                        effective_by: proto_time(r.effective_by.as_option()),
                    },
                )
            }
        },
        KeysCmd::Revocations => {
            let r = m
                .list_revocations(pb::ListRevocationsRequest::default())
                .await?;
            emit(
                out,
                &RevocationListView {
                    revocations: r
                        .revocations
                        .iter()
                        .map(|v| RevocationEntryView {
                            sub: v.sub.clone(),
                            sid: v.sid.clone(),
                            reason: v.reason.clone(),
                            created: proto_time(v.created.as_option()),
                            created_by: v.created_by.clone(),
                        })
                        .collect(),
                },
            )
        }
    }
}
