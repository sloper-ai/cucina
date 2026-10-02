// SPDX-License-Identifier: FSL-1.1-ALv2

//! The platform catalogs `cucinactl bazelrc` works from, both embedded at build time:
//!
//! * `platforms/pools.json`: pool platforms and their runners' exact REAPI property
//!   sets.
//! * `platforms/targets.json` (schema: docs/cross-compilation.md §Schema, ADR 0900):
//!   the compile exec platforms of the `@cucina_platforms` module and every
//!   hermetic-llvm target with its compile and test placement, including the rows the
//!   user excluded (rejected with their `reason`).
//!
//! Schema version 1 only grows: unknown fields are ignored, optional fields default.
//! `cli/cucinactl/data/targets.json` is a minimal schema-1 fixture for tests only.

use std::collections::BTreeMap;

use anyhow::{Context, Result, ensure};
use serde::Deserialize;

/// Embedded `platforms/pools.json`.
pub const POOLS_JSON: &str = include_str!("../../../platforms/pools.json");
/// Embedded `platforms/targets.json`.
pub const TARGETS_JSON: &str = include_str!("../../../platforms/targets.json");

/// Bazel's default test timeouts (short, moderate, long, eternal), in seconds.
pub const DEFAULT_TEST_TIMEOUTS: [u64; 4] = [60, 300, 900, 3600];

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

/// `platforms/targets.json`.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCatalog {
    pub schema_version: u32,
    /// OS/CPU pairs that are neither targets nor exec platforms.
    #[serde(default)]
    pub excluded_platforms: Vec<ExcludedPlatform>,
    pub exec_platforms: Vec<ExecPlatform>,
    pub targets: Vec<Target>,
}

/// An OS/CPU pair the user excluded.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ExcludedPlatform {
    pub os: String,
    pub cpu: String,
    #[serde(default)]
    pub reason: String,
}

/// A Bazel execution platform for compile actions (one per compile-capable runner).
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ExecPlatform {
    /// Unique id; equals the pool name (`--exec-pool`).
    pub name: String,
    pub label: String,
    pub pool: String,
    pub runner: String,
    pub os: String,
    pub cpu: String,
    /// Extra constraint values (informational for cucinactl).
    #[serde(default)]
    pub constraints: Vec<String>,
    /// Bazel flags required whenever this platform compiles (e.g. the macOS SDK
    /// version), emitted by `cucinactl bazelrc`.
    #[serde(default)]
    pub flags: Vec<String>,
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
    /// `gnu.<version>`, `musl` or none.
    #[serde(default)]
    pub libc: Option<String>,
    /// `gnu`, `musl`, `msvc` or none.
    #[serde(default)]
    pub abi: Option<String>,
    /// Allowed compile exec platforms (names), default first.
    #[serde(default)]
    pub exec_platforms: Vec<String>,
    /// The test exec platform ("twin"), or `None` when the target cannot run tests.
    #[serde(default)]
    pub test: Option<TestPlacement>,
    /// `native`, `qemu-user` or `none`.
    #[serde(default)]
    pub test_mode: Option<String>,
    /// Multiplier for Bazel's test timeouts under emulation (absent = 1).
    #[serde(default)]
    pub test_timeout_scale: Option<f64>,
    #[serde(default)]
    pub notes: String,
    /// Excluded by the user: rejected everywhere with `reason`.
    #[serde(default)]
    pub excluded: bool,
    #[serde(default)]
    pub reason: Option<String>,
}

/// Where a target's tests run.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TestPlacement {
    pub label: String,
    pub pool: String,
    pub runner: String,
    /// `native` or `qemu-user`.
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

    /// Why the target is excluded (`None` for supported targets).
    pub fn exclusion(&self) -> Option<String> {
        self.excluded.then(|| {
            self.reason
                .clone()
                .filter(|r| !r.is_empty())
                .unwrap_or_else(|| format!("{} is excluded from Cucina's targets.", self.name))
        })
    }

    /// The scaled `--test_timeout` values (short, moderate, long, eternal), when the
    /// target has tests and a `testTimeoutScale` other than 1.
    pub fn test_timeouts(&self) -> Option<[u64; 4]> {
        let k = self.test_timeout_scale?;
        if self.test.is_none() || (k - 1.0).abs() < f64::EPSILON {
            return None;
        }
        // Validated finite and positive; whole seconds, at least 1.
        Some(DEFAULT_TEST_TIMEOUTS.map(|b| ((b as f64) * k).round().max(1.0) as u64))
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

    /// Parses and validates both catalogs.
    pub fn parse(pools: &str, targets: &str) -> Result<Catalog> {
        let pools: PoolCatalog = serde_json::from_str(pools).context("parsing pools.json")?;
        let targets: TargetCatalog =
            serde_json::from_str(targets).context("parsing targets.json")?;
        ensure!(
            pools.schema_version == 1,
            "unsupported pools.json schemaVersion"
        );
        ensure!(
            targets.schema_version == 1,
            "unsupported targets.json schemaVersion"
        );
        let catalog = Catalog { pools, targets };
        catalog.validate()?;
        Ok(catalog)
    }

    /// The invariants cucinactl relies on (a subset of `//tools/xplat`'s checks,
    /// docs/cross-compilation.md §Invariants).
    pub fn validate(&self) -> Result<()> {
        let excluded = |os: &str, cpu: &str| {
            self.targets
                .excluded_platforms
                .iter()
                .any(|x| x.os == os && x.cpu == cpu)
        };
        for e in &self.targets.exec_platforms {
            ensure!(
                !excluded(&e.os, &e.cpu),
                "targets.json: exec platform {} is an excluded OS/CPU pair",
                e.name
            );
            ensure!(
                self.runner(&e.pool, &e.runner).is_some(),
                "targets.json: exec platform {} names unknown runner {}/{}",
                e.name,
                e.pool,
                e.runner
            );
            for f in &e.flags {
                ensure!(
                    f.starts_with("--") && f.len() > 2 && !f.chars().any(char::is_control),
                    "targets.json: exec platform {} has an invalid flag {f:?}",
                    e.name
                );
            }
        }
        for t in &self.targets.targets {
            if t.excluded {
                ensure!(
                    t.exec_platforms.is_empty() && t.test.is_none(),
                    "targets.json: excluded target {} must have no placement",
                    t.name
                );
                continue;
            }
            ensure!(
                !excluded(&t.os, &t.cpu),
                "targets.json: target {} uses an excluded OS/CPU pair without `excluded: true`",
                t.name
            );
            ensure!(
                !t.exec_platforms.is_empty(),
                "targets.json: target {} has no exec platforms",
                t.name
            );
            for name in &t.exec_platforms {
                let e = self.exec_platform(name).with_context(|| {
                    format!(
                        "targets.json: target {} lists unknown exec platform {name:?}",
                        t.name
                    )
                })?;
                ensure!(
                    t.os != "macos" || e.os == "macos",
                    "targets.json: macOS target {} lists non-macOS exec platform {name}",
                    t.name
                );
            }
            if let Some(test) = &t.test {
                ensure!(
                    self.runner(&test.pool, &test.runner).is_some(),
                    "targets.json: target {} tests on unknown runner {}/{}",
                    t.name,
                    test.pool,
                    test.runner
                );
            }
            if let Some(k) = t.test_timeout_scale {
                ensure!(
                    k.is_finite() && k > 0.0 && k <= 1000.0,
                    "targets.json: target {} has an invalid testTimeoutScale {k}",
                    t.name
                );
            }
        }
        Ok(())
    }

    pub fn exec_platform(&self, name: &str) -> Option<&ExecPlatform> {
        self.targets.exec_platforms.iter().find(|e| e.name == name)
    }

    /// The target row (supported or excluded) named `name` (name, alias, platform
    /// label or the label's target name).
    pub fn target(&self, name: &str) -> Option<&Target> {
        self.targets.targets.iter().find(|t| t.matches(name))
    }

    pub fn pool(&self, name: &str) -> Option<&PoolPlatform> {
        self.pools.platforms.iter().find(|p| p.name == name)
    }

    pub fn runner(&self, pool: &str, runner: &str) -> Option<&Runner> {
        self.pool(pool)?.runners.iter().find(|r| r.name == runner)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Minimal schema-1 document (only the fields of the first version, plus an unknown
    /// one): newer readers must keep accepting it.
    const FIXTURE: &str = include_str!("../data/targets.json");

    #[test]
    fn embedded_catalogs_are_consistent() {
        let c = Catalog::embedded().expect("embedded catalogs");
        assert!(c.targets.targets.iter().any(|t| t.excluded));
        assert!(c.targets.targets.iter().any(|t| t.test_timeouts().is_some()));
        let mac = c.target("aarch64-apple-darwin").expect("macOS target");
        assert!(!mac.excluded);
        assert_eq!(
            c.exec_platform(&mac.exec_platforms[0]).unwrap().os,
            "macos",
            "macOS compiles on macOS"
        );
    }

    #[test]
    fn minimal_schema_one_fixture_still_parses() {
        let c = Catalog::parse(POOLS_JSON, FIXTURE).expect("fixture");
        assert!(c.targets.excluded_platforms.is_empty());
        for t in &c.targets.targets {
            assert!(!t.excluded && t.test_timeouts().is_none(), "{}", t.name);
        }
        assert!(
            c.targets
                .exec_platforms
                .iter()
                .all(|e| e.flags.is_empty() && e.constraints.is_empty())
        );
    }

    #[test]
    fn excluded_rows_are_found_and_explained() {
        let c = Catalog::embedded().unwrap();
        for alias in ["x86_64-apple-darwin", "macos_x86_64", "windows_aarch64_msvc"] {
            let t = c.target(alias).expect(alias);
            let why = t.exclusion().expect("excluded");
            assert!(!why.is_empty());
        }
    }

    #[test]
    fn timeouts_scale_bazels_defaults() {
        let c = Catalog::embedded().unwrap();
        let rv = c.target("riscv64-linux-gnu").unwrap();
        assert_eq!(rv.test_timeout_scale, Some(10.0));
        assert_eq!(rv.test_timeouts(), Some([600, 3000, 9000, 36000]));
        let mut half = rv.clone();
        half.test_timeout_scale = Some(2.5);
        assert_eq!(half.test_timeouts(), Some([150, 750, 2250, 9000]));
        half.test_timeout_scale = Some(1.0);
        assert_eq!(half.test_timeouts(), None);
        let wasm = c.target("wasm32-unknown-unknown").unwrap();
        assert_eq!(wasm.test_timeouts(), None, "build-only targets have no tests");
    }

    #[test]
    fn inconsistent_catalogs_are_rejected() {
        let base: serde_json::Value = serde_json::from_str(TARGETS_JSON).unwrap();
        let broken = |f: &dyn Fn(&mut serde_json::Value)| {
            let mut v = base.clone();
            f(&mut v);
            Catalog::parse(POOLS_JSON, &v.to_string())
        };
        assert!(broken(&|v| v["execPlatforms"][0]["runner"] = "nope".into()).is_err());
        assert!(broken(&|v| v["execPlatforms"][0]["flags"] = serde_json::json!(["x"])).is_err());
        assert!(broken(&|v| v["targets"][0]["execPlatforms"] = serde_json::json!(["linux-x86-64"])).is_err());
        assert!(broken(&|v| v["targets"][1]["testTimeoutScale"] = serde_json::json!(-1)).is_err());
        assert!(broken(&|v| v["schemaVersion"] = 2.into()).is_err());
    }
}
