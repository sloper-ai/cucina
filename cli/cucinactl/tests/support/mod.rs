// SPDX-License-Identifier: FSL-1.1-ALv2

//! Shared test fixtures: temporary config directories, the `cucinactl` binary,
//! session seeding, and in-process fakes (OIDC IdP + Cucina STS + GitHub OIDC,
//! management API, REAPI). Fakes only — no interaction mocks (R-TEST-5), loopback
//! only, no sleeps.

#![allow(dead_code)]

pub mod fake_auth;
pub mod fake_mgmt;
pub mod fake_reapi;
pub mod net;
pub mod schema;
pub mod test_ca;

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};

use base64::Engine as _;
use cucinactl::auth::token::{CachedToken, TokenCache};
use cucinactl::config::{AuthMethod, Config, CredentialStore, Paths, Profile};

/// Repository root (Bazel runfiles or the Cargo workspace).
pub fn repo_root() -> PathBuf {
    if let Some(srcdir) = std::env::var_os("TEST_SRCDIR") {
        let ws = std::env::var("TEST_WORKSPACE").unwrap_or_else(|_| "_main".into());
        return Path::new(&srcdir).join(ws);
    }
    // Cargo sets CARGO_MANIFEST_DIR for test processes; read it at run time (Bazel
    // rejects binaries that embed the absolute build directory via env!()).
    let manifest =
        std::env::var_os("CARGO_MANIFEST_DIR").expect("run under cargo test or bazel test");
    Path::new(&manifest)
        .ancestors()
        .nth(2)
        .expect("crate lives two levels below the repository root")
        .to_path_buf()
}

/// The `cucinactl` binary under test.
pub fn bin() -> PathBuf {
    if let Some(p) = std::env::var_os("CUCINACTL_BIN") {
        return PathBuf::from(p);
    }
    match option_env!("CARGO_BIN_EXE_cucinactl") {
        Some(p) => PathBuf::from(p),
        None => panic!("set CUCINACTL_BIN to the cucinactl binary"),
    }
}

/// A unique temporary directory, removed on drop.
pub struct TempDir(PathBuf);

impl TempDir {
    pub fn new() -> TempDir {
        static N: AtomicU64 = AtomicU64::new(0);
        let base = std::env::var_os("TEST_TMPDIR")
            .map(PathBuf::from)
            .or_else(|| option_env!("CARGO_TARGET_TMPDIR").map(PathBuf::from))
            .unwrap_or_else(std::env::temp_dir);
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0);
        let dir = base.join(format!(
            "cucinactl-{}-{}-{nanos}",
            std::process::id(),
            N.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&dir).expect("create temp dir");
        TempDir(dir)
    }
    pub fn path(&self) -> &Path {
        &self.0
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Environment variables a test must never inherit.
const SCRUB: &[&str] = &[
    "CUCINA_PROFILE",
    "CUCINA_OUTPUT",
    "CUCINA_REFRESH_TOKEN",
    "CUCINA_SERVICE_KEY",
    "CUCINA_BROWSER_COMMAND",
    "CUCINA_URL",
    "CUCINA_LOG",
    "XDG_CONFIG_HOME",
    "ACTIONS_ID_TOKEN_REQUEST_URL",
    "ACTIONS_ID_TOKEN_REQUEST_TOKEN",
    "CLICOLOR_FORCE",
    // Trust and proxies come from the test, never from the developer's shell.
    "CUCINA_CA_FILE",
    "SSL_CERT_FILE",
    "CUCINA_ALLOW_INSECURE_HTTP",
    "HTTPS_PROXY",
    "https_proxy",
    "HTTP_PROXY",
    "http_proxy",
    "ALL_PROXY",
    "all_proxy",
    "NO_PROXY",
    "no_proxy",
];

/// A `cucinactl` command isolated to `config_dir` (file credential store, no colors).
pub fn cmd(config_dir: &Path) -> Command {
    command_at(&bin(), config_dir)
}

/// Like [`cmd`] for another path to the binary (argv[0] tests).
pub fn command_at(program: &Path, config_dir: &Path) -> Command {
    let mut c = Command::new(program);
    for v in SCRUB {
        c.env_remove(v);
    }
    c.env("CUCINA_CONFIG_DIR", config_dir)
        .env("CUCINA_CREDENTIAL_STORE", "file")
        .env("NO_COLOR", "1");
    c
}

/// Explicitly bypass system proxies for owned loopback fakes. The pinned
/// hyper-util matcher handles numeric IPs separately from the `*` domain rule.
pub const LOOPBACK_NO_PROXY: &str = "127.0.0.1,::1,localhost";

/// Verifies [`command_at`]'s isolation plus the explicit loopback proxy bypass,
/// before a library-test child creates fixtures or clients. Never print values.
pub fn assert_sanitized_environment() {
    for name in SCRUB {
        if *name == "NO_PROXY" || (cfg!(windows) && name.eq_ignore_ascii_case("NO_PROXY")) {
            assert!(
                std::env::var_os(name).as_deref() == Some(std::ffi::OsStr::new(LOOPBACK_NO_PROXY)),
                "test child requires the owned loopback proxy bypass"
            );
        } else {
            assert!(
                std::env::var_os(name).is_none(),
                "test child inherited {name}"
            );
        }
    }
    assert!(
        std::env::var_os("CUCINA_CREDENTIAL_STORE").as_deref()
            == Some(std::ffi::OsStr::new("file")),
        "test child requires the file credential store"
    );
}

/// An (unsigned for local purposes) JWT with the given claims.
pub fn fake_jwt(claims: &serde_json::Value) -> String {
    let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
    format!(
        "{}.{}.{}",
        b64.encode(br#"{"alg":"ES256","kid":"test","typ":"JWT"}"#),
        b64.encode(claims.to_string()),
        b64.encode(b"not-a-signature")
    )
}

pub fn now() -> i64 {
    cucinactl::util::now_unix()
}

/// A profile pointing at local fakes.
pub fn profile(url: &str, management: &str, remote_executor: &str, auth: AuthMethod) -> Profile {
    Profile {
        url: url.to_string(),
        token_endpoint: format!("{url}/token"),
        jwks_uri: format!("{url}/jwks.json"),
        remote_executor: remote_executor.to_string(),
        instance_name: "main".into(),
        management: management.to_string(),
        auth,
        provider: (auth == AuthMethod::Oidc).then(|| "mock".to_string()),
        ca_file: None,
        credential_store: CredentialStore::File,
    }
}

/// Writes `config.toml` with one current profile.
pub fn write_profile(dir: &Path, name: &str, p: &Profile) {
    let paths = Paths {
        dir: dir.to_path_buf(),
    };
    let mut cfg = Config::load(&paths).expect("load config");
    cfg.profiles.insert(name.to_string(), p.clone());
    cfg.current_profile = Some(name.to_string());
    cfg.save(&paths).expect("save config");
}

/// Seeds the token cache of `profile` with `token` expiring at `exp`.
pub fn write_token(dir: &Path, profile: &str, token: &str, exp: i64, method: AuthMethod) {
    let paths = Paths {
        dir: dir.to_path_buf(),
    };
    TokenCache::new(&paths, profile)
        .write(&CachedToken {
            version: 1,
            access_token: token.to_string(),
            exp,
            subject: Some("test:user".into()),
            obtained_at: now(),
            method,
        })
        .expect("write token");
}

/// Reads the cached token of `profile`.
pub fn read_token(dir: &Path, profile: &str) -> Option<CachedToken> {
    TokenCache::new(
        &Paths {
            dir: dir.to_path_buf(),
        },
        profile,
    )
    .read()
}

/// Writes a secret into the file credential store.
pub fn write_secret(
    dir: &Path,
    profile: &str,
    kind: cucinactl::auth::secrets::SecretKind,
    value: &str,
) {
    let paths = Paths {
        dir: dir.to_path_buf(),
    };
    let secrets =
        cucinactl::auth::secrets::Secrets::new(&paths, CredentialStore::File).expect("secrets");
    assert!(
        secrets.store() == CredentialStore::File,
        "test fixture refuses to write to a non-file credential store"
    );
    secrets.set(profile, kind, value).expect("write secret");
}

/// Parses stdout as JSON.
pub fn stdout_json(out: &std::process::Output) -> serde_json::Value {
    serde_json::from_slice(&out.stdout).unwrap_or_else(|e| {
        panic!(
            "stdout is not JSON ({e}): {}\nstderr: {}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        )
    })
}
