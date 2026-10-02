// SPDX-License-Identifier: FSL-1.1-ALv2

//! Long-lived client secrets: the IdP refresh token (OIDC sessions) or the
//! service-account key (`login --key`). Only these live here; the short-lived Cucina
//! JWT is in the token cache.
//!
//! Lookup order: environment override (`CUCINA_REFRESH_TOKEN`, `CUCINA_SERVICE_KEY`;
//! read-only, for headless hosts and CI) → the profile's store: the OS keychain
//! (`keyring`: macOS Keychain, Windows Credential Manager, Linux Secret Service) or,
//! opt-in, a 0600 JSON file. `CUCINA_CREDENTIAL_STORE=file|keyring` overrides the
//! profile setting. Windows caps a credential at 2,560 bytes, which is why only the
//! refresh token (never the JWT) is stored there.

use std::collections::BTreeMap;
use std::time::Duration;

use anyhow::{Context, Result};

use crate::config::{CredentialStore, Paths};
use crate::exit::CliError;

/// Keychain service name.
pub const KEYRING_SERVICE: &str = "ai.sloper.cucina";
/// Environment variable selecting the store (`file` or `keyring`).
pub const STORE_ENV: &str = "CUCINA_CREDENTIAL_STORE";
const WINDOWS_MAX_SECRET: usize = 2560;

/// The kinds of stored secrets.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SecretKind {
    RefreshToken,
    ServiceKey,
}

impl SecretKind {
    fn slug(self) -> &'static str {
        match self {
            SecretKind::RefreshToken => "refresh-token",
            SecretKind::ServiceKey => "service-key",
        }
    }
    /// Environment variable that overrides the stored value.
    pub fn env_var(self) -> &'static str {
        match self {
            SecretKind::RefreshToken => "CUCINA_REFRESH_TOKEN",
            SecretKind::ServiceKey => "CUCINA_SERVICE_KEY",
        }
    }
}

/// Access to the configured secret store.
pub struct Secrets {
    store: CredentialStore,
    paths: Paths,
}

impl Secrets {
    /// The store for a profile (environment override applied).
    pub fn new(paths: &Paths, profile_store: CredentialStore) -> Result<Secrets> {
        let store = match std::env::var(STORE_ENV).ok().as_deref() {
            None | Some("") => profile_store,
            Some("file") => CredentialStore::File,
            Some("keyring") => CredentialStore::Keyring,
            Some(other) => {
                return Err(CliError::usage(format!(
                    "{STORE_ENV}={other:?} is invalid (use `file` or `keyring`)"
                ))
                .into());
            }
        };
        Ok(Secrets {
            store,
            paths: paths.clone(),
        })
    }

    /// The effective store.
    pub fn store(&self) -> CredentialStore {
        self.store
    }

    fn key(profile: &str, kind: SecretKind) -> String {
        format!("{profile}:{}", kind.slug())
    }

    /// Reads a secret (environment override first).
    pub fn get(&self, profile: &str, kind: SecretKind) -> Result<Option<String>> {
        if let Ok(v) = std::env::var(kind.env_var())
            && !v.is_empty()
        {
            return Ok(Some(v));
        }
        let key = Self::key(profile, kind);
        match self.store {
            CredentialStore::Keyring => {
                let entry = keyring_entry(&key)?;
                match entry.get_password() {
                    Ok(v) => Ok(Some(v)),
                    Err(keyring::Error::NoEntry) => Ok(None),
                    Err(e) => Err(keyring_error(e)),
                }
            }
            CredentialStore::File => Ok(self.read_file()?.remove(&key)),
        }
    }

    /// Stores a secret.
    pub fn set(&self, profile: &str, kind: SecretKind, value: &str) -> Result<()> {
        let key = Self::key(profile, kind);
        match self.store {
            CredentialStore::Keyring => {
                if cfg!(windows) && value.len() > WINDOWS_MAX_SECRET {
                    anyhow::bail!(
                        "the {} is {} bytes; Windows Credential Manager stores at most {WINDOWS_MAX_SECRET}",
                        kind.slug(),
                        value.len()
                    );
                }
                keyring_entry(&key)?
                    .set_password(value)
                    .map_err(keyring_error)
            }
            CredentialStore::File => {
                let _lock = super::lock::lock_exclusive(
                    &self.paths.secrets_lock_file(),
                    Duration::from_secs(9),
                )?;
                let mut map = self.read_file()?;
                map.insert(key, value.to_string());
                crate::config::write_private(
                    &self.paths.secrets_file(),
                    &serde_json::to_vec_pretty(&map)?,
                )
            }
        }
    }

    /// Deletes a secret (missing is fine).
    pub fn delete(&self, profile: &str, kind: SecretKind) -> Result<()> {
        let key = Self::key(profile, kind);
        match self.store {
            CredentialStore::Keyring => match keyring_entry(&key)?.delete_credential() {
                Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
                Err(e) => Err(keyring_error(e)),
            },
            CredentialStore::File => {
                let _lock = super::lock::lock_exclusive(
                    &self.paths.secrets_lock_file(),
                    Duration::from_secs(9),
                )?;
                let mut map = self.read_file()?;
                if map.remove(&key).is_some() {
                    crate::config::write_private(
                        &self.paths.secrets_file(),
                        &serde_json::to_vec_pretty(&map)?,
                    )?;
                }
                Ok(())
            }
        }
    }

    fn read_file(&self) -> Result<BTreeMap<String, String>> {
        let path = self.paths.secrets_file();
        match std::fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .with_context(|| format!("parsing {}", path.display())),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(BTreeMap::new()),
            Err(e) => Err(e).with_context(|| format!("reading {}", path.display())),
        }
    }
}

fn keyring_entry(key: &str) -> Result<keyring::Entry> {
    keyring::Entry::new(KEYRING_SERVICE, key).map_err(keyring_error)
}

fn keyring_error(e: keyring::Error) -> anyhow::Error {
    let hint = "no usable OS keychain; log in with `--credential-store file` (0600 file) \
                or provide the secret through CUCINA_REFRESH_TOKEN / CUCINA_SERVICE_KEY";
    match e {
        keyring::Error::NoDefaultStore
        | keyring::Error::NoStorageAccess(_)
        | keyring::Error::PlatformFailure(_) => anyhow::anyhow!("keychain: {e}; {hint}"),
        other => anyhow::anyhow!("keychain: {other}"),
    }
}
