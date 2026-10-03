// SPDX-License-Identifier: FSL-1.1-ALv2

//! Bazel credential-helper protocol (R-AUTH-8, R-AUTH-6, R-AUTH-7): exact stdout,
//! `expires = exp − 2 min` in whole-second RFC 3339, argv[0] dispatch, never
//! prompting, renewals serialized across concurrent helpers, and the GitHub
//! Actions path that stores nothing.

mod support;

use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};

use cucinactl::auth::ProfileCtx;
use cucinactl::auth::secrets::SecretKind;
use cucinactl::auth::token::CachedToken;
use cucinactl::config::{AuthMethod, Paths};
use cucinactl::credential_helper::get_for_profile;
use cucinactl::exit::{ExitCode, exit_code_for};
use support::fake_auth::{FakeAuth, service_key};
use support::{
    TempDir, bin, command_at as isolated_command_at, fake_jwt, now, read_token, write_profile,
    write_secret, write_token,
};

const REQUEST: &str =
    r#"{"uri":"https://cucina.test.invalid/build.bazel.remote.execution.v2.Execution"}"#;

/// Every child in this file talks directly to owned loopback fakes, including
/// CLI subprocesses; leave shared proxy-test command construction unchanged.
fn command_at(program: &Path, config_dir: &Path) -> Command {
    let mut command = isolated_command_at(program, config_dir);
    command.env("NO_PROXY", support::LOOPBACK_NO_PROXY);
    command
}

fn run(mut c: Command, stdin: &str) -> Output {
    let mut child = c
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn helper");
    child
        .stdin
        .take()
        .unwrap()
        .write_all(stdin.as_bytes())
        .unwrap();
    child.wait_with_output().unwrap()
}

/// `cucina-credential-helper` as a hardlink (or copy) of the binary.
fn helper_link(dir: &Path) -> PathBuf {
    let name = if cfg!(windows) {
        "cucina-credential-helper.exe"
    } else {
        "cucina-credential-helper"
    };
    let link = dir.join(name);
    if std::fs::hard_link(bin(), &link).is_err() {
        std::fs::copy(bin(), &link).expect("copy helper");
    }
    link
}

fn session_profile(dir: &Path, token_endpoint_base: &str, auth: AuthMethod) {
    let mut p = support::profile(
        token_endpoint_base,
        "http://127.0.0.1:1",
        "grpcs://cucina.test.invalid:443",
        auth,
    );
    p.provider = None;
    write_profile(dir, "prod", &p);
}

// R-AUTH-8: the exact response document, from `cucinactl credential-helper get` and
// from the `cucina-credential-helper` personality, with and without `get`.
#[test]
fn protocol_output_is_exact_for_every_invocation_form() {
    let cfg = TempDir::new();
    let links = TempDir::new();
    session_profile(cfg.path(), "https://cucina.test.invalid", AuthMethod::Oidc);
    let exp = now() + 600;
    let jwt = fake_jwt(&serde_json::json!({"sub": "google:42", "exp": exp}));
    write_token(cfg.path(), "prod", &jwt, exp, AuthMethod::Oidc);

    let expires = cucinactl::util::rfc3339_seconds(exp - 120);
    let want =
        format!(r#"{{"headers":{{"Authorization":["Bearer {jwt}"]}},"expires":"{expires}"}}"#)
            + "\n";
    let link = helper_link(links.path());
    let forms: Vec<(PathBuf, Vec<&str>)> = vec![
        (bin(), vec!["credential-helper", "get"]),
        (link.clone(), vec!["get"]),
        (link, vec![]),
    ];
    for (program, args) in forms {
        let mut c = command_at(&program, cfg.path());
        c.args(&args);
        let out = run(c, REQUEST);
        assert!(
            out.status.success(),
            "{program:?} {args:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert_eq!(
            String::from_utf8(out.stdout).unwrap(),
            want,
            "{program:?} {args:?}"
        );
        assert!(out.stderr.is_empty(), "nothing on stderr on success");
    }
    // Whole seconds, UTC, no fraction (Bazel's yyyy-MM-dd'T'HH:mm:ssXXX).
    assert!(expires.len() == 20 && expires.ends_with('Z') && !expires.contains('.'));

    // Guards: R-AUTH-8 profile selection — an empty override falls back; a named
    // non-current profile for the same host must return its own credential.
    let other = support::profile(
        "https://cucina.test.invalid",
        "http://127.0.0.1:1",
        "grpcs://cucina.test.invalid:443",
        AuthMethod::Oidc,
    );
    write_profile(cfg.path(), "other", &other);
    session_profile(cfg.path(), "https://cucina.test.invalid", AuthMethod::Oidc);
    let other_jwt = fake_jwt(&serde_json::json!({"sub": "google:other", "exp": exp}));
    write_token(cfg.path(), "other", &other_jwt, exp, AuthMethod::Oidc);
    for (selected, expected) in [("", &jwt), ("other", &other_jwt)] {
        let mut c = command_at(&bin(), cfg.path());
        c.args(["credential-helper", "get"])
            .env("CUCINA_PROFILE", selected);
        let out = run(c, REQUEST);
        assert!(
            out.status.success(),
            "profile={selected:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert_eq!(
            String::from_utf8(out.stdout).unwrap(),
            format!(
                r#"{{"headers":{{"Authorization":["Bearer {expected}"]}},"expires":"{expires}"}}"#
            ) + "\n",
            "profile={selected:?}"
        );
        assert!(out.stderr.is_empty(), "nothing on stderr on success");
    }

    // Guards: R-AUTH-8 host scoping — explicitly choosing a profile must not release
    // its token for an unrelated host (e.g. another repository's download or BES).
    for selected in [None, Some("prod")] {
        let mut c = command_at(&bin(), cfg.path());
        c.args(["credential-helper", "get"]);
        if let Some(profile) = selected {
            c.env("CUCINA_PROFILE", profile);
        }
        let out = run(c, r#"{"uri":"https://unrelated.example/download"}"#);
        assert_eq!(out.status.code(), Some(3), "profile={selected:?}");
        assert!(
            out.stdout.is_empty(),
            "never disclose a token to an unrelated host"
        );
    }
}

// R-AUTH-8: without a valid session the helper never prompts; it tells the user to
// run `cucinactl login` and exits 3 (auth required), with nothing on stdout.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_a_session_it_exits_3_and_never_prompts() {
    let auth = FakeAuth::start().await;
    auth.state.behavior.lock().unwrap().sts_error = Some((400, "invalid_grant".into()));
    let rejected = TempDir::new();
    session_profile(rejected.path(), &auth.url(), AuthMethod::ServiceKey);
    write_secret(
        rejected.path(),
        "prod",
        SecretKind::ServiceKey,
        &service_key(),
    );
    let empty = TempDir::new();
    let expired = TempDir::new();
    session_profile(
        expired.path(),
        "https://cucina.test.invalid",
        AuthMethod::Oidc,
    );
    // Expired token and no refresh token stored.
    write_token(
        expired.path(),
        "prod",
        &fake_jwt(&serde_json::json!({"exp": now() - 10})),
        now() - 10,
        AuthMethod::Oidc,
    );
    // Guards: R-AUTH-8 — an STS rejection also needs actionable login guidance,
    // even when the underlying error does not already mention cucinactl login.
    for dir in [empty.path(), expired.path(), rejected.path()] {
        let mut c = command_at(&bin(), dir);
        c.args(["credential-helper", "get"]);
        let out = tokio::task::spawn_blocking(move || run(c, REQUEST))
            .await
            .unwrap();
        assert_eq!(
            out.status.code(),
            Some(3),
            "stderr: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert!(out.stdout.is_empty());
        assert!(String::from_utf8_lossy(&out.stderr).contains("cucinactl login"));
    }
}

// Guards: R-AUTH-6/8 — after failed renewal, a cached credential must retain at
// least 30 seconds of advertised validity (JWT expiry minus the helper's margin).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn failed_renewal_requires_usable_advertised_expiry() {
    const CHILD: &str = "CUCINA_TEST_CREDENTIAL_FALLBACK_CHILD";
    const COMPLETED: &str = "fallback-completed.json";
    if std::env::var_os(CHILD).is_none() {
        // Only the scrubbed child constructs the fake, secret store and public
        // library request. Unlike a CLI subprocess, spawn_blocking alone would
        // inherit the developer's credentials, trust and proxy overrides.
        let cfg = TempDir::new();
        let mut command = command_at(&std::env::current_exe().unwrap(), cfg.path());
        command
            .args([
                "--exact",
                "failed_renewal_requires_usable_advertised_expiry",
            ])
            .env(CHILD, "1");
        let out = tokio::task::spawn_blocking(move || {
            // assert_cmd kills and reaps this owned child on timeout. There are
            // no nested processes in the child's fixed-clock library flow.
            assert_cmd::Command::from_std(command)
                .timeout(std::time::Duration::from_secs(20))
                .output()
                .expect("run isolated credential-helper test child")
        })
        .await
        .unwrap();
        assert!(
            out.status.success(),
            "isolated fallback table failed: stdout={} stderr={}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        );
        let completed: Vec<i64> = serde_json::from_slice(
            &std::fs::read(cfg.path().join(COMPLETED))
                .expect("child must prove both fallback rows completed"),
        )
        .unwrap();
        assert_eq!(completed, [1149, 1150]);
        return;
    }

    // An inherited marker cannot bypass isolation: before fixture or HTTP I/O,
    // require absent inherited overrides, our exact loopback bypass and file store.
    support::assert_sanitized_environment();
    assert!(
        std::env::var_os(CHILD).as_deref() == Some(std::ffi::OsStr::new("1")),
        "invalid test-child marker"
    );
    let completion_dir = PathBuf::from(
        std::env::var_os("CUCINA_CONFIG_DIR").expect("test child requires its config directory"),
    );
    let mut completed = Vec::new();
    for (exp, reusable) in [(1149, false), (1150, true)] {
        let auth = FakeAuth::start().await;
        auth.state.behavior.lock().unwrap().sts_error = Some((503, "server_error".into()));
        let cfg = TempDir::new();
        let profile = support::profile(
            &auth.url(),
            "http://127.0.0.1:1",
            "grpcs://cucina.test.invalid:443",
            AuthMethod::ServiceKey,
        );
        write_profile(cfg.path(), "prod", &profile);
        write_secret(cfg.path(), "prod", SecretKind::ServiceKey, &service_key());
        let paths = Paths {
            dir: cfg.path().to_path_buf(),
        };
        let ctx = ProfileCtx::new(&paths, "prod", &profile);
        let jwt = fake_jwt(&serde_json::json!({"sub": "sa:cached", "exp": exp}));
        let cached = CachedToken::from_access_token(jwt.clone(), None, 1000, profile.auth);
        ctx.cache().write(&cached).unwrap();
        let result = tokio::task::spawn_blocking(move || {
            get_for_profile("https://cucina.test.invalid/service", &ctx, || 1000)
        })
        .await
        .unwrap();
        if reusable {
            let doc: serde_json::Value = serde_json::from_str(&result.unwrap()).unwrap();
            assert_eq!(
                doc,
                serde_json::json!({
                    "headers": {"Authorization": [format!("Bearer {jwt}")]},
                    "expires": "1970-01-01T00:17:10Z",
                })
            );
        } else {
            let err = result.expect_err("no credential document below the fallback margin");
            assert_eq!(exit_code_for(&err), ExitCode::Unavailable);
        }
        assert_eq!(read_token(cfg.path(), "prod"), Some(cached));
        assert!(
            auth.state
                .exchanges
                .lock()
                .unwrap()
                .iter()
                .any(|kind| kind == "urn:cucina:params:oauth:token-type:service-key"),
            "expiry={exp}: the fake must validate the synthetic service key before refusing renewal"
        );
        completed.push(exp);
    }
    let report = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(completion_dir.join(COMPLETED))
        .expect("create the test-child completion report");
    serde_json::to_writer(report, &completed).unwrap();
}

// R-AUTH-6 + "concurrency-safe (file lock)": Bazel starts one helper per gRPC
// service at once; with < 5 min left they must renew exactly once and all return
// the renewed token.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn concurrent_helpers_renew_once() {
    let auth = FakeAuth::start().await;
    let cfg = TempDir::new();
    session_profile(cfg.path(), &auth.url(), AuthMethod::ServiceKey);
    write_secret(cfg.path(), "prod", SecretKind::ServiceKey, &service_key());
    let stale = fake_jwt(&serde_json::json!({"exp": now() + 60}));
    write_token(
        cfg.path(),
        "prod",
        &stale,
        now() + 60,
        AuthMethod::ServiceKey,
    );

    let dir = cfg.path().to_path_buf();
    let outputs = tokio::task::spawn_blocking(move || {
        let children: Vec<_> = (0..8)
            .map(|_| {
                let mut c = command_at(&bin(), &dir);
                c.args(["credential-helper", "get"])
                    .stdin(Stdio::piped())
                    .stdout(Stdio::piped())
                    .stderr(Stdio::piped());
                let mut child = c.spawn().expect("spawn");
                child
                    .stdin
                    .take()
                    .unwrap()
                    .write_all(REQUEST.as_bytes())
                    .unwrap();
                child
            })
            .collect();
        children
            .into_iter()
            .map(|c| c.wait_with_output().unwrap())
            .collect::<Vec<_>>()
    })
    .await
    .unwrap();

    let renewed = read_token(cfg.path(), "prod").expect("renewed token cached");
    assert_ne!(renewed.access_token, stale);
    assert!(renewed.exp > now() + 600);
    for out in &outputs {
        assert!(
            out.status.success(),
            "{}",
            String::from_utf8_lossy(&out.stderr)
        );
        let doc: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
        assert_eq!(
            doc["headers"]["Authorization"][0],
            format!("Bearer {}", renewed.access_token)
        );
    }
    assert_eq!(
        auth.exchange_count(),
        1,
        "exactly one STS exchange for 8 concurrent helpers"
    );
}

// R-AUTH-7: in GitHub Actions the helper fetches an OIDC token for audience
// `cucina`, exchanges it as `…:jwt`, and stores nothing.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn github_actions_token_is_exchanged_and_nothing_is_stored() {
    // Guards: R-AUTH-7 — a saved endpoint works even when discovery is unavailable;
    // a profile without an endpoint falls back to discovery, without storing secrets.
    for endpoint in [None, Some("saved"), Some("empty")] {
        let auth = FakeAuth::start().await;
        let cfg = TempDir::new();
        let base = auth.url();
        if let Some(endpoint) = endpoint {
            let mut profile = support::profile(
                &base,
                "http://127.0.0.1:1",
                "grpcs://cucina.test.invalid:443",
                AuthMethod::Oidc,
            );
            if endpoint == "empty" {
                profile.token_endpoint.clear();
            } else {
                auth.state.behavior.lock().unwrap().discovery_error = Some(503);
            }
            write_profile(cfg.path(), "prod", &profile);
        }
        let config_file = cfg.path().join("config.toml");
        let config_before = std::fs::read(&config_file).ok();
        let dir = cfg.path().to_path_buf();
        let out = tokio::task::spawn_blocking(move || {
            let mut c = command_at(&bin(), &dir);
            c.args(["credential-helper", "get"])
                .env(
                    "ACTIONS_ID_TOKEN_REQUEST_URL",
                    format!("{base}/gha/token?api-version=2.0"),
                )
                .env("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "gha-request-token")
                .env("CUCINA_URL", &base);
            run(c, REQUEST)
        })
        .await
        .unwrap();
        assert!(
            out.status.success(),
            "endpoint={endpoint:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        let doc: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
        let header = doc["headers"]["Authorization"][0].as_str().unwrap();
        let jwt = header.strip_prefix("Bearer ").unwrap();
        let claims = cucinactl::auth::token::unverified_claims(jwt).unwrap();
        assert_eq!(claims["sub"], "github:1401027334:ci");
        assert_eq!(
            *auth.state.gha_audiences.lock().unwrap(),
            vec!["cucina".to_string()]
        );
        assert_eq!(
            *auth.state.exchanges.lock().unwrap(),
            vec!["urn:ietf:params:oauth:token-type:jwt".to_string()]
        );
        let stored: Vec<_> = std::fs::read_dir(cfg.path())
            .unwrap()
            .map(|entry| entry.unwrap().file_name())
            .collect();
        if let Some(before) = config_before {
            assert_eq!(stored, vec![std::ffi::OsString::from("config.toml")]);
            assert_eq!(std::fs::read(config_file).unwrap(), before);
        } else {
            assert!(stored.is_empty(), "nothing written to the config directory");
        }
    }
}
