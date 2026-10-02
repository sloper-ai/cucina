// SPDX-License-Identifier: FSL-1.1-ALv2

//! The platform catalog `cucinactl bazelrc` works from:
//!
//! * `platforms/pools.json` (lead-owned): pool platforms and their runners' exact
//!   REAPI property sets — embedded at build time.
//! * `targets.json` (schema in docs/cli.md §Targets schema): Bazel exec platforms
//!   (labels of the `@cucina_platforms` module) and the hermetic-llvm target
//!   platforms with their compile/test placement. Until the cross-platform agent
//!   publishes `platforms/targets.json`, the embedded copy is the fixture in
//!   `cli/cucinactl/data/targets.json`.
//!
//! A discovery document may later carry the deployment's own catalog; the embedded
//! copies are the defaults.

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use serde::Deserialize;

/// Embedded `platforms/pools.json`.
pub const POOLS_JSON: &str = include_str!("../../../platforms/pools.json");
/// Embedded targets catalog (see module docs).
pub const TARGETS_JSON: &str = include_str!("../data/targets.json");

/// `platforms/pools.json`.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PoolCatalog {
    pub schema_version: u32,
    pub default_instance_name: String,
    pub platforms: Vec<PoolPlatform>,
}

/// One pool platform (OS + ISA + toolchain image).
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PoolPlatform {
    pub name: String,
    #[serde(default)]
    pub description: String,
    pub provider: String,
    pub os: String,
    pub arch: String,
    #[serde(default)]
    pub xcode_version: Option<String>,
    pub runners: Vec<Runner>,
}

/// One runner: an exact REAPI platform property set.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Runner {
    pub name: String,
    pub properties: BTreeMap<String, String>,
    #[serde(default)]
    pub emulator: Option<String>,
    #[serde(default)]
    pub generic: bool,
}

/// The targets catalog (docs/cli.md §Targets schema).
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCatalog {
    pub schema_version: u32,
    pub exec_platforms: Vec<ExecPlatform>,
    pub targets: Vec<Target>,
}

/// A Bazel execution platform for compile actions (one per pool runner).
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ExecPlatform {
    pub name: String,
    pub label: String,
    pub pool: String,
    pub runner: String,
    pub os: String,
    pub cpu: String,
}

/// A hermetic-llvm target platform and where its actions run.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Target {
    pub name: String,
    #[serde(default)]
    pub aliases: Vec<String>,
    /// Label passed to `--platforms`.
    pub platform: String,
    pub os: String,
    pub cpu: String,
    /// `gnu`, `musl`, `msvc` or none.
    #[serde(default)]
    pub abi: Option<String>,
    /// Allowed compile exec platforms (names), in default preference order.
    pub exec_platforms: Vec<String>,
    /// The test exec platform ("twin"), or `None` when the target cannot run tests.
    #[serde(default)]
    pub test: Option<TestPlacement>,
}

/// Where a target's tests run.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TestPlacement {
    pub label: String,
    pub pool: String,
    pub runner: String,
    /// `native` or `qemu`.
    pub mode: String,
}

impl Target {
    pub fn is_msvc(&self) -> bool {
        self.abi.as_deref() == Some("msvc")
    }
    pub fn matches(&self, name: &str) -> bool {
        self.name == name
            || self.aliases.iter().any(|a| a == name)
            || self.platform == name
            || self.platform.rsplit(':').next() == Some(name)
    }
}

/// Both catalogs.
#[derive(Debug, Clone)]
pub struct Catalog {
    pub pools: PoolCatalog,
    pub targets: TargetCatalog,
}

impl Catalog {
    /// The catalogs compiled into this binary.
    pub fn embedded() -> Result<Catalog> {
        Catalog::parse(POOLS_JSON, TARGETS_JSON)
    }

    pub fn parse(pools: &str, targets: &str) -> Result<Catalog> {
        let pools: PoolCatalog = serde_json::from_str(pools).context("parsing pools.json")?;
        let targets: TargetCatalog =
            serde_json::from_str(targets).context("parsing targets.json")?;
        anyhow::ensure!(
            pools.schema_version == 1,
            "unsupported pools.json schemaVersion"
        );
        anyhow::ensure!(
            targets.schema_version == 1,
            "unsupported targets.json schemaVersion"
        );
        Ok(Catalog { pools, targets })
    }

    pub fn exec_platform(&self, name: &str) -> Option<&ExecPlatform> {
        self.targets.exec_platforms.iter().find(|e| e.name == name)
    }

    pub fn target(&self, name: &str) -> Option<&Target> {
        self.targets.targets.iter().find(|t| t.matches(name))
    }

    pub fn pool(&self, name: &str) -> Option<&PoolPlatform> {
        self.pools.platforms.iter().find(|p| p.name == name)
    }
}
