// SPDX-License-Identifier: FSL-1.1-ALv2

//! Cluster profiles and on-disk layout.
//!
//! Directory: `$CUCINA_CONFIG_DIR`, else `$XDG_CONFIG_HOME/cucina`, else
//! `~/.config/cucina` (Windows: `%APPDATA%\cucina`). Created 0700.
//!
//! ```text
//! config.toml              profiles (TOML), 0600
//! tokens/<profile>.json    cached Cucina JWT, 0600, written atomically
//! tokens/<profile>.lock    advisory lock serializing renewals (credential helper)
//! discovery/<profile>.json the cached /.well-known/cucina-configuration document
//! secrets.json             opt-in file credential store (headless hosts), 0600
//! ```

use std::collections::BTreeMap;
use std::fs;
use std::io::Write as _;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

use crate::exit::CliError;

/// Environment variable overriding the configuration directory.
pub const CONFIG_DIR_ENV: &str = "CUCINA_CONFIG_DIR";

/// Resolved on-disk locations.
#[derive(Debug, Clone)]
pub struct Paths {
    pub dir: PathBuf,
}

impl Paths {
    /// Resolves the configuration directory (see module docs).
    pub fn resolve() -> Result<Paths> {
        if let Some(dir) = std::env::var_os(CONFIG_DIR_ENV).filter(|v| !v.is_empty()) {
            return Ok(Paths { dir: dir.into() });
        }
        if let Some(xdg) = std::env::var_os("XDG_CONFIG_HOME").filter(|v| !v.is_empty()) {
            return Ok(Paths {
                dir: Path::new(&xdg).join("cucina"),
            });
        }
        let base = if cfg!(windows) {
            dirs::config_dir()
        } else {
            dirs::home_dir().map(|h| h.join(".config"))
        };
        let base = base.context("cannot determine the home directory; set CUCINA_CONFIG_DIR")?;
        Ok(Paths {
            dir: base.join("cucina"),
        })
    }

    pub fn config_file(&self) -> PathBuf {
        self.dir.join("config.toml")
    }
    pub fn token_file(&self, profile: &str) -> PathBuf {
        self.dir.join("tokens").join(format!("{profile}.json"))
    }
    pub fn lock_file(&self, profile: &str) -> PathBuf {
        self.dir.join("tokens").join(format!("{profile}.lock"))
    }
    pub fn discovery_file(&self, profile: &str) -> PathBuf {
        self.dir.join("discovery").join(format!("{profile}.json"))
    }
    pub fn secrets_file(&self) -> PathBuf {
        self.dir.join("secrets.json")
    }
    pub fn secrets_lock_file(&self) -> PathBuf {
        self.dir.join("secrets.lock")
    }
}

/// How a profile authenticates (decides how sessions are renewed).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "kebab-case")]
pub enum AuthMethod {
    /// Interactive OIDC login; renewed with the IdP refresh token.
    #[default]
    Oidc,
    /// Service-account key (`login --key`); renewed by exchanging the key again.
    ServiceKey,
}

/// Where long-lived secrets (IdP refresh token, service key) are stored.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default, clap::ValueEnum)]
#[serde(rename_all = "kebab-case")]
pub enum CredentialStore {
    /// OS keychain: macOS Keychain, Windows Credential Manager, Linux Secret Service.
    #[default]
    Keyring,
    /// Opt-in 0600 file in the config directory (headless hosts without a keychain).
    File,
}

/// One Cucina deployment.
#[derive(Debug, Clone, Serialize, Deserialize, Default, PartialEq)]
pub struct Profile {
    /// Base URL given to `login` (the STS issuer), e.g. `https://cucina.example.com`.
    pub url: String,
    /// RFC 8693 token endpoint of the STS.
    pub token_endpoint: String,
    /// JWKS of the STS (verifies Cucina JWTs in `whoami`).
    #[serde(default)]
    pub jwks_uri: String,
    /// Client endpoint for Bazel, e.g. `grpcs://cucina.example.com:443`.
    pub remote_executor: String,
    /// Default Buildbarn instance name.
    pub instance_name: String,
    /// Management API endpoint (`host:port`, or a URL with scheme).
    pub management: String,
    /// Authentication method of the stored session.
    #[serde(default)]
    pub auth: AuthMethod,
    /// Identity provider name (from discovery) used by `login`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    /// Extra CA bundle (PEM) trusted for this deployment (private CA).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ca_file: Option<PathBuf>,
    /// Secret store for the refresh token / service key.
    #[serde(default)]
    pub credential_store: CredentialStore,
}

impl Profile {
    /// Host (without port) of the client endpoint: the credential-helper scope.
    pub fn remote_host(&self) -> Option<String> {
        host_of(&self.remote_executor)
    }
}

/// Extracts the host from `scheme://host:port/...` or `host:port`.
pub fn host_of(endpoint: &str) -> Option<String> {
    let rest = endpoint.split_once("://").map_or(endpoint, |(_, r)| r);
    let authority = rest.split(['/', '?', '#']).next()?;
    let authority = authority.rsplit_once('@').map_or(authority, |(_, a)| a);
    let host = if let Some(stripped) = authority.strip_prefix('[') {
        stripped.split(']').next()?
    } else {
        authority.split(':').next()?
    };
    (!host.is_empty()).then(|| host.to_ascii_lowercase())
}

/// The whole `config.toml`.
#[derive(Debug, Clone, Serialize, Deserialize, Default, PartialEq)]
pub struct Config {
    /// Profile used when `--profile` is not given.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub current_profile: Option<String>,
    #[serde(default)]
    pub profiles: BTreeMap<String, Profile>,
}

impl Config {
    /// Loads `config.toml`; a missing file is an empty configuration.
    pub fn load(paths: &Paths) -> Result<Config> {
        let path = paths.config_file();
        match fs::read_to_string(&path) {
            Ok(text) => {
                toml::from_str(&text).with_context(|| format!("parsing {}", path.display()))
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Config::default()),
            Err(e) => Err(e).with_context(|| format!("reading {}", path.display())),
        }
    }

    /// Writes `config.toml` atomically (0600).
    pub fn save(&self, paths: &Paths) -> Result<()> {
        let text = toml::to_string_pretty(self)?;
        let body = format!(
            "# cucinactl configuration (profiles). Managed by `cucinactl login` / `cucinactl config`.\n{text}"
        );
        write_private(&paths.config_file(), body.as_bytes())
    }

    /// Resolves the profile to use: explicit name, else the current profile, else the
    /// only profile. Errors with exit code 3 when nothing is configured.
    pub fn select(&self, explicit: Option<&str>) -> Result<(String, Profile)> {
        let name = match explicit {
            Some(n) => n.to_string(),
            None => match (&self.current_profile, self.profiles.len()) {
                (Some(n), _) => n.clone(),
                (None, 1) => self.profiles.keys().next().cloned().unwrap_or_default(),
                (None, 0) => {
                    return Err(CliError::auth_required(
                        "no Cucina profile configured; run `cucinactl login <url>`",
                    )
                    .into());
                }
                (None, _) => {
                    return Err(CliError::usage(
                        "several profiles are configured; pass --profile or run `cucinactl config use <name>`",
                    )
                    .into());
                }
            },
        };
        match self.profiles.get(&name) {
            Some(p) => Ok((name, p.clone())),
            None => Err(CliError::auth_required(format!(
                "profile {name:?} does not exist; run `cucinactl login <url> --profile {name}`"
            ))
            .into()),
        }
    }

    /// Finds the profile whose client endpoint host is `host` (credential-helper scope).
    /// Prefers the current profile when several match.
    pub fn profile_for_host(&self, host: &str) -> Option<(String, Profile)> {
        let host = host.to_ascii_lowercase();
        let matches = |p: &Profile| p.remote_host().as_deref() == Some(host.as_str());
        if let Some(name) = &self.current_profile
            && let Some(p) = self.profiles.get(name)
            && matches(p)
        {
            return Some((name.clone(), p.clone()));
        }
        self.profiles
            .iter()
            .find(|(_, p)| matches(p))
            .map(|(n, p)| (n.clone(), p.clone()))
    }
}

/// Validates a profile name (used in file names).
pub fn validate_profile_name(name: &str) -> Result<()> {
    let ok = !name.is_empty()
        && name.len() <= 64
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
        && !name.starts_with('.');
    if ok {
        Ok(())
    } else {
        Err(CliError::usage(format!(
            "invalid profile name {name:?}: use 1-64 characters from [A-Za-z0-9._-]"
        ))
        .into())
    }
}

/// Creates `dir` (and parents) with 0700 permissions on Unix.
pub fn ensure_private_dir(dir: &Path) -> Result<()> {
    if dir.is_dir() {
        return Ok(());
    }
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt as _;
        builder.mode(0o700);
    }
    builder
        .create(dir)
        .with_context(|| format!("creating {}", dir.display()))
}

/// Opens a new file for writing with 0600 permissions on Unix (fails if it exists).
pub fn create_private_file(path: &Path) -> std::io::Result<fs::File> {
    let mut opts = fs::OpenOptions::new();
    opts.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        opts.mode(0o600);
    }
    opts.open(path)
}

/// Writes `data` to `path` atomically (temporary file + rename) with 0600 permissions,
/// so concurrent readers see either the old or the new content, never a torn file.
pub fn write_private(path: &Path, data: &[u8]) -> Result<()> {
    let dir = path.parent().context("path has no parent directory")?;
    ensure_private_dir(dir)?;
    static COUNTER: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let n = COUNTER.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
    let file_name = path.file_name().unwrap_or_default().to_string_lossy();
    let tmp = dir.join(format!(".{file_name}.{}.{n}.tmp", std::process::id()));
    let result = (|| -> Result<()> {
        let mut f =
            create_private_file(&tmp).with_context(|| format!("creating {}", tmp.display()))?;
        f.write_all(data)?;
        f.sync_all()?;
        drop(f);
        fs::rename(&tmp, path).with_context(|| format!("replacing {}", path.display()))?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

/// Removes a file, ignoring "not found".
pub fn remove_if_exists(path: &Path) -> Result<()> {
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e).with_context(|| format!("removing {}", path.display())),
    }
}
