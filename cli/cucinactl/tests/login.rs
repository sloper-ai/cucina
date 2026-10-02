// SPDX-License-Identifier: FSL-1.1-ALv2

//! `cucinactl login` (R-AUTH-5, R-AUTH-6, R-AUTH-12) against an in-process fake
//! OIDC provider + STS: authorization code + PKCE S256 over the 127.0.0.1 loopback
//! redirect, `state`/`nonce` mismatch rejection, the `--manual` paste flow,
//! renewal with the stored refresh token, and service-account keys. The real
//! `mock-oauth2-server` runs in the campaign through `--browser-command`.

mod support;

use std::collections::HashMap;
use std::io::{BufRead as _, BufReader, Write as _};
use std::process::{Child, Output, Stdio};
use std::sync::mpsc;
use std::time::Duration;

use cucinactl::auth::secrets::{SecretKind, Secrets};
use cucinactl::config::{CredentialStore, Paths};
use support::fake_auth::{FakeAuth, service_key};
use support::{TempDir, cmd, now, read_token, schema, write_token};

/// Starts `cucinactl -p dev -o json login <url> <extra…>` and returns the child
/// plus the authorization URL it printed on stderr.
fn start_login(
    cfg: &std::path::Path,
    url: &str,
    extra: &[&str],
) -> (Child, String, mpsc::Receiver<String>) {
    let mut child = cmd(cfg)
        .args([
            "-p",
            "dev",
            "-o",
            "json",
            "login",
            url,
            "--login-timeout",
            "60s",
        ])
        .args(extra)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn login");
    let stderr = child.stderr.take().unwrap();
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        for line in BufReader::new(stderr).lines().map_while(Result::ok) {
            let _ = tx.send(line);
        }
    });
    let auth_url = loop {
        let line = rx
            .recv_timeout(Duration::from_secs(30))
            .expect("login prints the authorization URL");
        if let Some(i) = line.find("http") {
            let candidate = line[i..].trim().to_string();
            if candidate.contains("/idp/authorize?") {
                break candidate;
            }
        }
    };
    (child, auth_url, rx)
}

fn http() -> reqwest::Client {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap()
}

/// The non-interactive "browser": the IdP immediately redirects to the loopback URI.
async fn authorize(auth_url: &str) -> String {
    let resp = http().get(auth_url).send().await.unwrap();
    assert_eq!(resp.status(), 302, "IdP accepted the authorization request");
    resp.headers()["location"].to_str().unwrap().to_string()
}

async fn wait(child: Child) -> Output {
    tokio::task::spawn_blocking(move || child.wait_with_output().unwrap())
        .await
        .unwrap()
}

fn secrets(cfg: &std::path::Path) -> Secrets {
    Secrets::new(
        &Paths {
            dir: cfg.to_path_buf(),
        },
        CredentialStore::File,
    )
    .unwrap()
}

// R-AUTH-5 (flow, PKCE S256, loopback 127.0.0.1, offline_access, storage) and
// R-AUTH-6 (renewal: IdP refresh → fresh ID token → STS exchange).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn loopback_login_with_pkce_then_renewal_with_the_refresh_token() {
    let auth = FakeAuth::start().await;
    let cfg = TempDir::new();
    let (child, auth_url, _rx) = start_login(cfg.path(), &auth.url(), &["--no-browser"]);

    let location = authorize(&auth_url).await;
    assert!(
        location.starts_with("http://127.0.0.1:"),
        "loopback redirect: {location}"
    );
    let callback = http().get(&location).send().await.unwrap();
    assert_eq!(callback.status(), 200);
    let out = wait(child).await;
    assert!(
        out.status.success(),
        "login failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    let doc: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    schema::assert_valid("login.v1", &doc);
    assert_eq!(doc["subject"], "mock:user-1");
    assert_eq!(doc["method"], "oidc");

    let q: HashMap<String, String> = auth.state.last_authorize.lock().unwrap().clone().unwrap();
    assert_eq!(q["code_challenge_method"], "S256");
    assert_eq!(
        q["code_challenge"].len(),
        43,
        "base64url(SHA-256) challenge"
    );
    assert!(
        q["state"].len() >= 16 && q["nonce"].len() >= 16,
        "random state and nonce"
    );
    let scopes: Vec<&str> = q["scope"].split(' ').collect();
    for s in ["openid", "email", "profile", "offline_access"] {
        assert!(scopes.contains(&s), "scope {s} requested: {scopes:?}");
    }
    assert!(
        location.starts_with(&q["redirect_uri"]),
        "redirect_uri is the loopback listener"
    );

    // Stored: IdP refresh token in the credential store, Cucina JWT in the cache.
    assert!(
        secrets(cfg.path())
            .get("dev", SecretKind::RefreshToken)
            .unwrap()
            .is_some()
    );
    let token = read_token(cfg.path(), "dev").expect("token cached");
    assert_eq!(token.subject.as_deref(), Some("mock:user-1"));

    // Renewal when < 5 minutes remain (through the credential helper, never prompting).
    write_token(
        cfg.path(),
        "dev",
        &token.access_token,
        now() + 60,
        token.method,
    );
    let dir = cfg.path().to_path_buf();
    let helper = tokio::task::spawn_blocking(move || {
        let mut child = cmd(&dir)
            .args(["credential-helper", "get"])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(br#"{"uri":"https://127.0.0.1/google.bytestream.ByteStream"}"#)
            .unwrap();
        child.wait_with_output().unwrap()
    })
    .await
    .unwrap();
    assert!(
        helper.status.success(),
        "{}",
        String::from_utf8_lossy(&helper.stderr)
    );
    assert_eq!(
        auth.state
            .refreshes
            .load(std::sync::atomic::Ordering::SeqCst),
        1
    );
    assert_eq!(
        *auth.state.exchanges.lock().unwrap(),
        vec!["urn:ietf:params:oauth:token-type:id_token".to_string(); 2]
    );
    assert!(read_token(cfg.path(), "dev").unwrap().exp > now() + 600);
}

// R-AUTH-5: a redirect whose `state` does not match, or an ID token whose `nonce`
// does not match, aborts the login (exit 3) and stores nothing.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn state_or_nonce_mismatch_aborts_login() {
    let auth = FakeAuth::start().await;
    for case in ["state", "nonce"] {
        auth.state.behavior.lock().unwrap().wrong_nonce = case == "nonce";
        let cfg = TempDir::new();
        let (child, auth_url, _rx) = start_login(cfg.path(), &auth.url(), &["--no-browser"]);
        let location = authorize(&auth_url).await;
        let target = if case == "state" {
            let mut u = openidconnect::url::Url::parse(&location).unwrap();
            let code = u
                .query_pairs()
                .find(|(k, _)| k == "code")
                .map(|(_, v)| v.into_owned())
                .unwrap();
            u.query_pairs_mut()
                .clear()
                .append_pair("code", &code)
                .append_pair("state", "forged");
            u.to_string()
        } else {
            location
        };
        let _ = http().get(&target).send().await;
        let out = wait(child).await;
        assert_eq!(
            out.status.code(),
            Some(3),
            "{case}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert!(
            read_token(cfg.path(), "dev").is_none(),
            "{case}: no token stored"
        );
        assert!(
            secrets(cfg.path())
                .get("dev", SecretKind::RefreshToken)
                .unwrap()
                .is_none()
        );
    }
}

// R-AUTH-5 `--manual`: nothing listens; the pasted redirect URL's `state` is checked.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn manual_mode_accepts_the_pasted_redirect_and_checks_state() {
    let auth = FakeAuth::start().await;
    for (case, expected) in [("genuine", 0), ("forged-state", 3)] {
        let cfg = TempDir::new();
        let (mut child, auth_url, _rx) = start_login(cfg.path(), &auth.url(), &["--manual"]);
        let location = authorize(&auth_url).await;
        let pasted = if case == "genuine" {
            location
        } else {
            let mut u = openidconnect::url::Url::parse(&location).unwrap();
            let pairs: Vec<(String, String)> = u
                .query_pairs()
                .map(|(k, v)| {
                    let forged = k == "state";
                    (
                        k.into_owned(),
                        if forged {
                            "forged".into()
                        } else {
                            v.into_owned()
                        },
                    )
                })
                .collect();
            u.query_pairs_mut().clear().extend_pairs(pairs);
            u.to_string()
        };
        let mut stdin = child.stdin.take().unwrap();
        stdin.write_all(format!("{pasted}\n").as_bytes()).unwrap();
        drop(stdin);
        let out = wait(child).await;
        assert_eq!(
            out.status.code(),
            Some(expected),
            "{case}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
    }
}

// R-AUTH-12 / R-AUTH-10: `login --key` exchanges a service-account key at the STS,
// stores the key (not the JWT) as the renewable secret, and `whoami` verifies the
// resulting Cucina JWT against the STS JWKS.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn service_key_login_and_whoami_verification() {
    let auth = FakeAuth::start().await;
    let cfg = TempDir::new();
    let dir = cfg.path().to_path_buf();
    let url = auth.url();
    let (login, whoami) = tokio::task::spawn_blocking(move || {
        let mut child = cmd(&dir)
            .args(["-p", "ci", "-o", "json", "login", &url, "--key", "-"])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(service_key().as_bytes())
            .unwrap();
        let login = child.wait_with_output().unwrap();
        let whoami = cmd(&dir)
            .args(["-p", "ci", "-o", "json", "whoami"])
            .output()
            .unwrap();
        (login, whoami)
    })
    .await
    .unwrap();
    assert!(
        login.status.success(),
        "{}",
        String::from_utf8_lossy(&login.stderr)
    );
    let doc: serde_json::Value = serde_json::from_slice(&login.stdout).unwrap();
    schema::assert_valid("login.v1", &doc);
    assert_eq!(doc["method"], "service-key");
    assert_eq!(
        secrets(cfg.path())
            .get("ci", SecretKind::ServiceKey)
            .unwrap(),
        Some(service_key())
    );
    assert!(
        whoami.status.success(),
        "{}",
        String::from_utf8_lossy(&whoami.stderr)
    );
    let who: serde_json::Value = serde_json::from_slice(&whoami.stdout).unwrap();
    assert_eq!(who["verified"], true, "{who}");
    assert_eq!(who["subject"], "sa:ci-bot");
    assert_eq!(
        *auth.state.exchanges.lock().unwrap(),
        vec!["urn:cucina:params:oauth:token-type:service-key".to_string()]
    );
}
