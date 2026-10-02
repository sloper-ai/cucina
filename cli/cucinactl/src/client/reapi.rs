// SPDX-License-Identifier: FSL-1.1-ALv2

//! Read-only REAPI access through the client endpoint with the user's own JWT
//! (R-CLI-2): blobs via ByteStream — `compressed-blobs/zstd` (decoded with the
//! `zstd` crate) when the server advertises ZSTD, plain `blobs/` otherwise — with
//! size and SHA-256 verification, ActionCache lookups, and input-tree walking.

use std::collections::{HashMap, VecDeque};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, bail};
use connectrpc::client::SharedHttp2Connection;
use connectrpc::{ConnectError, ErrorCode};
use cucina_api::connect::build::bazel::remote::execution::v2::{
    ActionCacheClient, CapabilitiesClient,
};
use cucina_api::connect::google::bytestream::ByteStreamClient;
use cucina_api::proto::build::bazel::remote::execution::v2 as re;
use cucina_api::proto::google::bytestream as bs;
use futures::StreamExt as _;
use serde::Serialize;
use sha2::{Digest as _, Sha256};
use tokio::sync::OnceCell;

use super::{Channel, TokenHandle};
use crate::exit::CliError;

/// Largest blob `action inspect` downloads (stdout/stderr, protos).
pub const MAX_BLOB: u64 = 64 << 20;

/// Parses `<hash>/<size>` or `<hash>-<size>`.
pub fn parse_digest(s: &str) -> Option<re::Digest> {
    let (hash, size) = s.split_once('/').or_else(|| s.rsplit_once('-'))?;
    let hash = hash.trim().to_ascii_lowercase();
    if hash.is_empty() || !hash.chars().all(|c| c.is_ascii_hexdigit()) {
        return None;
    }
    let size_bytes = size.trim().trim_end_matches('/').parse::<i64>().ok()?;
    (size_bytes >= 0).then(|| re::Digest {
        hash,
        size_bytes,
        ..Default::default()
    })
}

/// `<hash>/<size>`.
pub fn digest_string(d: &re::Digest) -> String {
    format!("{}/{}", d.hash, d.size_bytes)
}

/// REAPI client for one instance name.
#[derive(Clone)]
pub struct Reapi {
    channel: Channel,
    token: TokenHandle,
    instance: String,
    timeout: Duration,
    zstd: Arc<OnceCell<bool>>,
}

/// One entry of an input tree.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct TreeEntry {
    pub path: String,
    /// `file`, `directory` or `symlink`.
    pub kind: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub digest: Option<String>,
    pub size_bytes: u64,
    pub executable: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub target: Option<String>,
}

/// A (possibly truncated) listing of an input tree.
#[derive(Debug, Clone, Serialize, Default, PartialEq)]
pub struct TreeListing {
    pub root_digest: String,
    pub files: u64,
    pub directories: u64,
    pub symlinks: u64,
    pub total_file_bytes: u64,
    /// The walk stopped at the entry limit; counts are lower bounds.
    pub truncated: bool,
    pub entries: Vec<TreeEntry>,
}

impl Reapi {
    pub fn new(channel: Channel, token: TokenHandle, instance: &str, timeout: Duration) -> Reapi {
        Reapi {
            channel,
            token,
            instance: instance.to_string(),
            timeout,
            zstd: Arc::new(OnceCell::new()),
        }
    }

    fn transport(&self) -> SharedHttp2Connection {
        self.channel.transport.clone()
    }

    /// Whether the server advertises ZSTD for ByteStream (`compressed-blobs/zstd`).
    pub async fn supports_zstd(&self) -> bool {
        *self
            .zstd
            .get_or_init(|| async {
                let client = CapabilitiesClient::new(self.transport(), self.channel.config.clone());
                let Ok(options) = self.token.options(Some(self.timeout)) else {
                    return false;
                };
                let request = re::GetCapabilitiesRequest {
                    instance_name: self.instance.clone(),
                    ..Default::default()
                };
                match client.get_capabilities_with_options(request, options).await {
                    Ok(resp) => resp
                        .into_owned()
                        .cache_capabilities
                        .as_option()
                        .is_some_and(|c| {
                            c.supported_compressors
                                .iter()
                                .any(|v| v.as_known() == Some(re::compressor::Value::ZSTD))
                        }),
                    Err(_) => false,
                }
            })
            .await
    }

    fn resource_name(&self, d: &re::Digest, zstd: bool) -> String {
        let prefix = if self.instance.is_empty() {
            String::new()
        } else {
            format!("{}/", self.instance)
        };
        if zstd {
            format!("{prefix}compressed-blobs/zstd/{}/{}", d.hash, d.size_bytes)
        } else {
            format!("{prefix}blobs/{}/{}", d.hash, d.size_bytes)
        }
    }

    /// Downloads and verifies one blob.
    pub async fn read_blob(&self, d: &re::Digest) -> Result<Vec<u8>> {
        if d.size_bytes == 0 {
            return Ok(Vec::new());
        }
        let size = u64::try_from(d.size_bytes).unwrap_or(u64::MAX);
        if size > MAX_BLOB {
            bail!(
                "blob {} is larger than {} MiB",
                digest_string(d),
                MAX_BLOB >> 20
            );
        }
        let zstd = self.supports_zstd().await;
        let client = ByteStreamClient::new(self.transport(), self.channel.config.clone());
        let resource_name = self.resource_name(d, zstd);
        let request = bs::ReadRequest {
            resource_name: resource_name.clone(),
            ..Default::default()
        };
        let options = self.token.options(Some(self.timeout))?;
        let mut stream = client
            .read_with_options(request, options)
            .await
            .map_err(|e| blob_error(e, d))?;
        let mut data = Vec::new();
        while let Some(chunk) = stream.message().await.map_err(|e| blob_error(e, d))? {
            data.extend_from_slice(&chunk.to_owned_message().data);
            if data.len() as u64 > MAX_BLOB + (MAX_BLOB >> 4) {
                bail!("blob {} exceeds the size limit", digest_string(d));
            }
        }
        if zstd {
            data = zstd::bulk::decompress(&data, usize::try_from(size).unwrap_or(usize::MAX))
                .with_context(|| format!("zstd-decoding {resource_name}"))?;
        }
        if data.len() as u64 != size {
            bail!(
                "blob {} has {} bytes, expected {size}",
                digest_string(d),
                data.len()
            );
        }
        if d.hash.len() == 64 {
            let got = crate::util::hex(&Sha256::digest(&data));
            if got != d.hash {
                bail!("blob {} failed SHA-256 verification", digest_string(d));
            }
        }
        Ok(data)
    }

    /// Downloads and decodes a protobuf blob.
    pub async fn read_proto<M: buffa::Message>(&self, d: &re::Digest) -> Result<M> {
        let bytes = self.read_blob(d).await?;
        M::decode_from_slice(&bytes)
            .map_err(|e| anyhow::anyhow!("decoding blob {}: {e}", digest_string(d)))
    }

    /// Looks up an action result in the AC (with inline stdout/stderr); `None` if absent.
    pub async fn get_action_result(&self, action: &re::Digest) -> Result<Option<re::ActionResult>> {
        let client = ActionCacheClient::new(self.transport(), self.channel.config.clone());
        let request = re::GetActionResultRequest {
            instance_name: self.instance.clone(),
            action_digest: action.clone().into(),
            inline_stdout: true,
            inline_stderr: true,
            ..Default::default()
        };
        let options = self.token.options(Some(self.timeout))?;
        match client
            .get_action_result_with_options(request, options)
            .await
        {
            Ok(r) => Ok(Some(r.into_owned())),
            Err(e) if e.code == ErrorCode::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    /// Walks an input tree breadth-first (Directory blobs fetched concurrently,
    /// deduplicated by digest), stopping after `max_entries` entries.
    pub async fn walk_tree(&self, root: &re::Digest, max_entries: usize) -> Result<TreeListing> {
        let mut listing = TreeListing {
            root_digest: digest_string(root),
            ..TreeListing::default()
        };
        let mut cache: HashMap<String, re::Directory> = HashMap::new();
        let mut level: VecDeque<(String, re::Digest)> =
            VecDeque::from([(String::new(), root.clone())]);
        while !level.is_empty() {
            let mut missing: Vec<re::Digest> = level
                .iter()
                .filter(|(_, d)| !cache.contains_key(&digest_string(d)))
                .map(|(_, d)| d.clone())
                .collect();
            missing.dedup_by(|a, b| a.hash == b.hash && a.size_bytes == b.size_bytes);
            let fetched: Vec<Result<(String, re::Directory)>> = futures::stream::iter(missing)
                .map(|d| async move {
                    let dir: re::Directory = self.read_proto(&d).await?;
                    Ok((digest_string(&d), dir))
                })
                .buffer_unordered(16)
                .collect()
                .await;
            for f in fetched {
                let (k, dir) = f?;
                cache.insert(k, dir);
            }
            let mut next = VecDeque::new();
            for (prefix, d) in level.drain(..) {
                let dir = cache
                    .get(&digest_string(&d))
                    .context("directory missing after fetch")?
                    .clone();
                let join = |name: &str| {
                    if prefix.is_empty() {
                        name.to_string()
                    } else {
                        format!("{prefix}/{name}")
                    }
                };
                for f in &dir.files {
                    let digest = f.digest.as_option();
                    let size = digest.map_or(0, |d| d.size_bytes.max(0) as u64);
                    listing.files += 1;
                    listing.total_file_bytes += size;
                    listing.entries.push(TreeEntry {
                        path: join(&f.name),
                        kind: "file",
                        digest: digest.map(digest_string),
                        size_bytes: size,
                        executable: f.is_executable,
                        target: None,
                    });
                }
                for s in &dir.symlinks {
                    listing.symlinks += 1;
                    listing.entries.push(TreeEntry {
                        path: join(&s.name),
                        kind: "symlink",
                        digest: None,
                        size_bytes: 0,
                        executable: false,
                        target: Some(s.target.clone()),
                    });
                }
                for sub in &dir.directories {
                    listing.directories += 1;
                    let path = join(&sub.name);
                    let digest = sub.digest.as_option();
                    listing.entries.push(TreeEntry {
                        path: path.clone(),
                        kind: "directory",
                        digest: digest.map(digest_string),
                        size_bytes: 0,
                        executable: false,
                        target: None,
                    });
                    if let Some(sd) = digest {
                        next.push_back((path, sd.clone()));
                    }
                }
                if listing.entries.len() >= max_entries {
                    listing.truncated = true;
                    listing.entries.truncate(max_entries);
                    listing.entries.sort_by(|a, b| a.path.cmp(&b.path));
                    return Ok(listing);
                }
            }
            level = next;
        }
        listing.entries.sort_by(|a, b| a.path.cmp(&b.path));
        Ok(listing)
    }
}

fn blob_error(e: ConnectError, d: &re::Digest) -> anyhow::Error {
    if e.code == ErrorCode::NotFound {
        CliError::not_found(format!(
            "blob {} is not in the CAS (expired or never uploaded)",
            digest_string(d)
        ))
        .into()
    } else {
        anyhow::Error::from(e).context(format!("reading blob {}", digest_string(d)))
    }
}
