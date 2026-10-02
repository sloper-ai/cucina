// SPDX-License-Identifier: FSL-1.1-ALv2

//! Bazel credential-helper protocol (R-AUTH-8, R-AUTH-6, R-AUTH-7): exact stdout,
//! `expires = exp − 2 min` in whole-second RFC 3339, argv[0] dispatch, never
//! prompting, renewals serialized across concurrent helpers, and the GitHub
//! Actions path that stores nothing.

mod support;

use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};

use cucinactl::auth::secrets::SecretKind;
use cucinactl::config::AuthMethod;
use support::fake_auth::{FakeAuth, service_key};
use support::{
    TempDir, bin, command_at, fake_jwt, now, read_token, write_profile, write_secret, write_token,
};

const REQUEST: &str =
    r#"{"uri":"https://cucina.test.invalid/build.bazel.remote.execution.v2.Execution"}"#;

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
#[test]
fn without_a_session_it_exits_3_and_never_prompts() {
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
    for dir in [empty.path(), expired.path()] {
        let mut c = command_at(&bin(), dir);
        c.args(["credential-helper", "get"]);
        let out = run(c, REQUEST);
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
    let auth = FakeAuth::start().await;
    let cfg = TempDir::new();
    let base = auth.url();
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
        "{}",
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
    let stored: Vec<_> = std::fs::read_dir(cfg.path()).unwrap().collect();
    assert!(stored.is_empty(), "nothing written to the config directory");
}
