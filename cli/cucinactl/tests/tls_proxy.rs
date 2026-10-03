// SPDX-License-Identifier: FSL-1.1-ALv2

//! T10a: Cucina's private CA and HTTP proxies. `cucinactl login` (discovery and the
//! STS token exchange, over reqwest) and `cucinactl status` (the management API, over
//! connect-rust) against fakes behind a TLS terminator whose certificate comes from a
//! per-run private CA:
//!
//! * refused without the CA, trusted with `login --ca-file` (stored in the profile and
//!   reused by later commands), `CUCINA_CA_FILE` or `SSL_CERT_FILE`;
//! * tunnelled through an HTTP `CONNECT` proxy from `HTTPS_PROXY` (with credentials) to
//!   a name only the proxy can resolve (`cucina.test`).
//!
//! Loopback only, no sleeps, exit codes and recorded requests only (no log text).

mod support;

use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::path::{Path, PathBuf};
use std::process::Output;

use cucinactl::config::AuthMethod;
use support::fake_auth::{FakeAuth, service_key};
use support::fake_mgmt::FakeMgmt;
use support::net::{FakeProxy, serve_tls};
use support::test_ca::{TestCa, server_config};
use support::{TempDir, cmd, fake_jwt, now, read_token, stdout_json, write_token};
use tokio::net::TcpListener;

const KEY_ENV: &str = "CUCINA_TEST_SERVICE_KEY";

/// The fakes of one deployment, each behind TLS with a certificate for `cucina.test`
/// and 127.0.0.1 issued by `ca`.
struct Deployment {
    /// `https://<host>:<port>` (login URL).
    url: String,
    sts_front: SocketAddr,
    mgmt_front: SocketAddr,
    mgmt: FakeMgmt,
    mgmt_token: String,
    _auth: FakeAuth,
}

async fn deployment(host: &str, ca: &TestCa) -> Deployment {
    let tls = server_config(ca.server(&["cucina.test"], &[IpAddr::V4(Ipv4Addr::LOCALHOST)]));

    let sts_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let sts_front = sts_listener.local_addr().unwrap();
    let url = format!("https://{host}:{}", sts_front.port());
    let auth = FakeAuth::start_with(|_| url.clone()).await;
    serve_tls(sts_listener, auth.addr, tls.clone());

    let mgmt_token = fake_jwt(&serde_json::json!({
        "iss": url, "aud": "buildbarn", "sub": "test:admin", "sid": "s1",
        "exp": now() + 900, "cucina": {"admin": ["main"]}
    }));
    let mgmt = FakeMgmt::new(&mgmt_token);
    let backend: SocketAddr = mgmt
        .serve()
        .await
        .trim_start_matches("http://")
        .parse()
        .unwrap();
    let mgmt_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let mgmt_front = mgmt_listener.local_addr().unwrap();
    serve_tls(mgmt_listener, backend, tls);
    *auth.state.management.lock().unwrap() = format!("https://{host}:{}", mgmt_front.port());

    Deployment {
        url,
        sts_front,
        mgmt_front,
        mgmt,
        mgmt_token,
        _auth: auth,
    }
}

/// Runs `cucinactl -p tls <args>` with `env` off the async runtime.
async fn run(dir: &Path, args: &[&str], env: &[(&str, String)]) -> Output {
    let mut c = cmd(dir);
    c.args(["-p", "tls", "--output", "json", "--timeout", "5s"])
        .args(args);
    for (k, v) in env {
        c.env(k, v);
    }
    tokio::task::spawn_blocking(move || c.output().expect("run cucinactl"))
        .await
        .unwrap()
}

async fn login(dir: &Path, d: &Deployment, extra: &[&str], env: &[(&str, String)]) -> Output {
    let mut args = vec!["login", d.url.as_str(), "--key-env", KEY_ENV];
    args.extend_from_slice(extra);
    let mut env = env.to_vec();
    env.push((KEY_ENV, service_key()));
    run(dir, &args, &env).await
}

fn assert_ok(out: &Output, what: &str) {
    assert!(
        out.status.success(),
        "{what} failed ({}): {}",
        out.status,
        String::from_utf8_lossy(&out.stderr)
    );
}

fn write_ca(ca: &TestCa) -> (TempDir, PathBuf) {
    let files = TempDir::new();
    let path = files.path().join("cucina-ca.pem");
    std::fs::write(&path, ca.pem()).unwrap();
    (files, path)
}

// T10a: the private CA is refused until trusted, then trusted for discovery, the STS
// and the management API from --ca-file (kept in the profile), CUCINA_CA_FILE or
// SSL_CERT_FILE.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn private_ca_is_trusted_for_discovery_sts_and_management() {
    let ca = TestCa::new("Cucina Test Private CA");
    let (_files, ca_path) = write_ca(&ca);
    let d = deployment("127.0.0.1", &ca).await;

    // Unknown issuer: the login fails before any token is minted.
    let dir = TempDir::new();
    let out = login(dir.path(), &d, &[], &[]).await;
    assert!(
        !out.status.success(),
        "a private CA must not be trusted by default"
    );
    assert!(read_token(dir.path(), "tls").is_none());
    // Guards: R-CLI-3 error.v1 — a TLS failure is one JSON error, not interleaved
    // with a dependency's diagnostic log lines.
    let error: serde_json::Value =
        serde_json::from_slice(&out.stderr).expect("a single error.v1 JSON document on stderr");
    support::schema::assert_valid("error.v1", &error);
    assert_eq!(error["error"]["exit_code"], 6);

    // --ca-file: trusted and stored in the profile.
    let ca_arg = ca_path.display().to_string();
    let out = login(dir.path(), &d, &["--ca-file", &ca_arg], &[]).await;
    assert_ok(&out, "login --ca-file");
    let token = read_token(dir.path(), "tls").expect("token cached");
    assert_eq!(token.subject.as_deref(), Some("sa:ci-bot"));
    let config = stdout_json(&run(dir.path(), &["config", "view"], &[]).await);
    let stored_ca = Path::new(
        config["profiles"][0]["ca_file"]
            .as_str()
            .expect("stored CA path"),
    );
    // The profile promises an absolute path to the same file, not a particular
    // spelling: Windows absolute() normalizes mixed slash/backslash inputs.
    assert!(
        stored_ca.is_absolute(),
        "profile CA path must remain absolute"
    );
    assert_eq!(
        std::fs::canonicalize(stored_ca).expect("resolve stored CA"),
        std::fs::canonicalize(&ca_path).expect("resolve supplied CA"),
        "login must retain the supplied CA file"
    );

    // The management API with the profile's CA (no flag, no environment).
    write_token(
        dir.path(),
        "tls",
        &d.mgmt_token,
        now() + 900,
        AuthMethod::ServiceKey,
    );
    let out = run(dir.path(), &["status"], &[]).await;
    assert_ok(&out, "status with the profile's CA");
    assert!(
        d.mgmt
            .state
            .calls
            .lock()
            .unwrap()
            .iter()
            .any(|c| c == "get_status")
    );

    // The environment instead of the flag, each in a fresh configuration.
    for var in ["CUCINA_CA_FILE", "SSL_CERT_FILE"] {
        let dir = TempDir::new();
        let out = login(dir.path(), &d, &[], &[(var, ca_arg.clone())]).await;
        assert_ok(&out, &format!("login with {var}"));
        write_token(
            dir.path(),
            "tls",
            &d.mgmt_token,
            now() + 900,
            AuthMethod::ServiceKey,
        );
        let out = run(dir.path(), &["status"], &[(var, ca_arg.clone())]).await;
        assert_ok(&out, &format!("status with {var}"));
    }
}

// T10a: HTTPS_PROXY tunnels discovery, the STS and the management API through CONNECT
// (with Proxy-Authorization from the proxy URL), and the client never resolves the
// deployment's name itself.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn https_proxy_tunnels_discovery_sts_and_management() {
    let ca = TestCa::new("Cucina Test Private CA");
    let (_files, ca_path) = write_ca(&ca);
    let d = deployment("cucina.test", &ca).await;
    let sts = format!("cucina.test:{}", d.sts_front.port());
    let mgmt = format!("cucina.test:{}", d.mgmt_front.port());
    let plaintext = d.mgmt.serve().await;
    let plaintext_addr: SocketAddr = plaintext.trim_start_matches("http://").parse().unwrap();
    let proxy = FakeProxy::start(HashMap::from([
        (sts.clone(), d.sts_front),
        (mgmt.clone(), d.mgmt_front),
        (plaintext_addr.to_string(), plaintext_addr),
    ]))
    .await;
    // The same CONNECT proxy behind TLS: HTTPS_PROXY may name either scheme.
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let proxy_tls_addr = listener.local_addr().unwrap();
    let mut proxy_tls = server_config(ca.server(&[], &[Ipv4Addr::LOCALHOST.into()]));
    std::sync::Arc::make_mut(&mut proxy_tls).alpn_protocols = vec![b"http/1.1".to_vec()];
    serve_tls(listener, proxy.addr, proxy_tls);
    for (var, url) in [
        ("HTTPS_PROXY", proxy.url_with_credentials("alice", "s3cret")),
        (
            "https_proxy",
            format!("https://alice:s3cret@{proxy_tls_addr}"),
        ),
    ] {
        let env = [
            (var, url),
            ("CUCINA_CA_FILE", ca_path.display().to_string()),
        ];
        proxy.connects.lock().unwrap().clear();
        let dir = TempDir::new();
        let out = login(dir.path(), &d, &[], &env).await;
        assert_ok(&out, "login through the proxy");
        assert!(read_token(dir.path(), "tls").is_some());
        write_token(
            dir.path(),
            "tls",
            &d.mgmt_token,
            now() + 900,
            AuthMethod::ServiceKey,
        );
        let out = run(dir.path(), &["status"], &env).await;
        assert_ok(&out, "status through the proxy");

        let connects = proxy.connects();
        // base64("alice:s3cret"): the proxy's credentials, not Cucina's token.
        let basic = Some("Basic YWxpY2U6czNjcmV0".to_string());
        for authority in [&sts, &mgmt] {
            let seen: Vec<_> = connects
                .iter()
                .filter(|c| &c.authority == authority)
                .collect();
            assert!(!seen.is_empty(), "no CONNECT {authority}: {connects:?}");
            assert!(
                seen.iter().all(|c| c.proxy_authorization == basic),
                "{connects:?}"
            );
        }
        assert!(
            connects
                .iter()
                .all(|c| c.authority == sts || c.authority == mgmt),
            "{connects:?}"
        );
    }

    // A plaintext loopback endpoint still needs the profile CA to authenticate an
    // HTTPS proxy. No CA environment fallback is set in this row.
    let dir = TempDir::new();
    let mut profile = support::profile(&d.url, &plaintext, &plaintext, AuthMethod::ServiceKey);
    profile.ca_file = Some(ca_path.clone());
    support::write_profile(dir.path(), "tls", &profile);
    write_token(
        dir.path(),
        "tls",
        &d.mgmt_token,
        now() + 900,
        AuthMethod::ServiceKey,
    );
    let env = [("HTTP_PROXY", format!("https://{proxy_tls_addr}"))];
    assert_ok(
        &run(dir.path(), &["status"], &env).await,
        "profile CA for HTTPS proxy to plaintext target",
    );

    // NO_PROXY is respected by both transports (a proxy that cannot route anything
    // must receive no CONNECT). No global environment changes in the test process.
    let direct = deployment("127.0.0.1", &ca).await;
    let reject_all = FakeProxy::start(HashMap::new()).await;
    let env = [
        ("HTTPS_PROXY", format!("http://{}", reject_all.addr)),
        ("NO_PROXY", "127.0.0.1".into()),
        ("CUCINA_CA_FILE", ca_path.display().to_string()),
    ];
    let dir = TempDir::new();
    assert_ok(
        &login(dir.path(), &direct, &[], &env).await,
        "NO_PROXY login",
    );
    write_token(
        dir.path(),
        "tls",
        &direct.mgmt_token,
        now() + 900,
        AuthMethod::ServiceKey,
    );
    assert_ok(&run(dir.path(), &["status"], &env).await, "NO_PROXY status");
    assert!(
        reject_all.connects().is_empty(),
        "NO_PROXY must bypass the proxy"
    );
}
