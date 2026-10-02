// SPDX-License-Identifier: FSL-1.1-ALv2

//! `.bazelrc` generation (UC9, R-DATA-1, §10.2, R-RE-1, R-XPLAT-2(d), R-AUTH-8).
//!
//! Pure: [`generate`] turns a [`Request`] and the platform [`Catalog`] into lines.
//! Invariants (property-tested in `tests/bazelrc_props.rs`):
//!
//! * every flag exists in Bazel 9.2 (allow-list `testdata/bazel-9.2-flags.txt`);
//! * `startup` lines first; flags never repeat;
//! * the credential helper is always host-scoped and `Authorization` is never set
//!   with `--remote_header`; `--experimental_remote_cache_chunking` and
//!   `--experimental_remote_merkle_tree_cache` are never emitted;
//! * `--extra_execution_platforms`, `--host_platform` and `--platforms` are always
//!   set together (Bazel 9's default test toolchain needs `--platforms`);
//!   `--host_platform` is the first execution platform; cross configurations list
//!   compile exec platforms first (the chosen pool first; macOS targets only on
//!   macOS) and then the target's test exec platform;
//! * MSVC targets get the two EULA `--repo_env` flags and nothing else does;
//!   Windows targets from non-Windows clients get the Windows test environment;
//! * read-only principals get `--remote_upload_local_results=false`.

use std::fmt;

use anyhow::{Result, bail};

use crate::catalog::{Catalog, ExecPlatform, Target};
use crate::exit::CliError;

/// The client host OS (Bazel computes the strict test environment from it).
#[derive(Debug, Clone, Copy, PartialEq, Eq, clap::ValueEnum)]
pub enum Os {
    Linux,
    Windows,
    Macos,
}

impl Os {
    /// The OS this binary runs on.
    pub fn current() -> Os {
        if cfg!(windows) {
            Os::Windows
        } else if cfg!(target_os = "macos") {
            Os::Macos
        } else {
            Os::Linux
        }
    }
    pub fn as_str(self) -> &'static str {
        match self {
            Os::Linux => "linux",
            Os::Windows => "windows",
            Os::Macos => "macos",
        }
    }
}

/// What to configure.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Mode {
    /// Build for and execute on one OS's pool (UC1–UC3).
    Native {
        os: Os,
        /// Pool (or exec-platform name) to use instead of the OS default.
        exec_pool: Option<String>,
        /// macOS: select the pool whose image has this Xcode version.
        xcode: Option<String>,
    },
    /// Cross configuration for a hermetic-llvm target (R-XPLAT-2(d)).
    Cross {
        target: String,
        exec_pool: Option<String>,
    },
}

/// Where the platform labels live.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Labels {
    /// The `@cucina_platforms` module (labels from the targets catalog).
    Module,
    /// A package in the user's workspace holding the output of `--emit-build-file`
    /// (native mode only), e.g. `//tools/cucina`.
    Package(String),
}

/// Everything [`generate`] needs.
#[derive(Debug, Clone)]
pub struct Request {
    pub mode: Mode,
    /// `grpcs://host:port`.
    pub endpoint: String,
    pub instance_name: String,
    /// Path of `cucina-credential-helper` (absolute, `%workspace%/…`, or a name on PATH).
    pub helper_path: String,
    /// PEM bundle for `--tls_certificate` (private CA).
    pub ca_file: Option<String>,
    /// Emit `common:<name>` lines (use with `--config=<name>`).
    pub config_name: Option<String>,
    /// CI: `--remote_download_outputs=minimal`.
    pub ci: bool,
    /// Principal without `ac-write`: `--remote_upload_local_results=false`.
    pub read_only: bool,
    /// Remote cache only, local execution (UC4).
    pub cache_only: bool,
    /// `--disk_cache` directory (`None` disables the local disk cache).
    pub disk_cache: Option<String>,
    /// `--experimental_disk_cache_gc_max_size`.
    pub disk_cache_max_size: String,
    pub client_os: Os,
    pub labels: Labels,
    /// Extra comment lines for the header (profile, versions).
    pub header: Vec<String>,
}

/// One `.bazelrc` line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Line {
    Comment(String),
    Blank,
    Flag {
        /// `startup`, `common`, …
        command: &'static str,
        config: Option<String>,
        /// The flag token, e.g. `--jobs=200` (unquoted).
        flag: String,
    },
}

/// A generated `.bazelrc` fragment.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Bazelrc {
    pub lines: Vec<Line>,
}

impl Bazelrc {
    /// The flag tokens (`--name=value`) in order, with their command.
    pub fn flags(&self) -> impl Iterator<Item = (&'static str, &str)> {
        self.lines.iter().filter_map(|l| match l {
            Line::Flag { command, flag, .. } => Some((*command, flag.as_str())),
            _ => None,
        })
    }

    /// The value of `--name=value`, if emitted.
    pub fn value(&self, name: &str) -> Option<&str> {
        let prefix = format!("--{name}=");
        self.flags()
            .find_map(|(_, f)| f.strip_prefix(prefix.as_str()))
    }
}

impl fmt::Display for Bazelrc {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        for line in &self.lines {
            match line {
                Line::Comment(c) if c.is_empty() => writeln!(f, "#")?,
                Line::Comment(c) => writeln!(f, "# {c}")?,
                Line::Blank => writeln!(f)?,
                Line::Flag {
                    command,
                    config,
                    flag,
                } => match config {
                    Some(c) => writeln!(f, "{command}:{c} {}", quote(flag))?,
                    None => writeln!(f, "{command} {}", quote(flag))?,
                },
            }
        }
        Ok(())
    }
}

/// Quotes a token for Bazel's rc tokenizer (backslash escapes outside quotes).
pub fn quote(token: &str) -> String {
    let plain = token
        .chars()
        .all(|c| !c.is_whitespace() && !matches!(c, '\'' | '"' | '\\' | '#'));
    if plain {
        token.to_string()
    } else if !token.contains('\'') {
        format!("'{token}'")
    } else {
        let escaped = token.replace('\\', "\\\\").replace('"', "\\\"");
        format!("\"{escaped}\"")
    }
}

/// Windows test environment for Windows targets driven from non-Windows clients
/// (R-XPLAT-4): Bazel derives the strict action environment from the client OS.
pub const WINDOWS_TEST_ENV: &[(&str, &str)] = &[
    ("SYSTEMROOT", r"C:\Windows"),
    (
        "PATH",
        r"C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0",
    ),
];

/// hermetic-llvm's MSVC CRT / Windows SDK licence acceptance (R-XPLAT-6; accepted
/// by the user on 2026-10-01).
pub const MSVC_EULA_REPO_ENV: &[&str] = &[
    "BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1",
    "BAZEL_WINDOWS_SDK_EULA=1",
];

struct Builder {
    lines: Vec<Line>,
    config: Option<String>,
}

impl Builder {
    fn comment(&mut self, c: impl Into<String>) {
        self.lines.push(Line::Comment(c.into()));
    }
    fn blank(&mut self) {
        self.lines.push(Line::Blank);
    }
    fn startup(&mut self, flag: &str) {
        self.lines.push(Line::Flag {
            command: "startup",
            config: None,
            flag: flag.to_string(),
        });
    }
    fn common(&mut self, flag: impl Into<String>) {
        self.lines.push(Line::Flag {
            command: "common",
            config: self.config.clone(),
            flag: flag.into(),
        });
    }
}

fn exec_label(e: &ExecPlatform, labels: &Labels) -> String {
    match labels {
        Labels::Module => e.label.clone(),
        Labels::Package(pkg) => format!("{}:{}-{}", pkg.trim_end_matches(':'), e.pool, e.runner),
    }
}

/// Resolves `--exec-pool` against exec-platform names and pool names.
fn find_exec<'a>(catalog: &'a Catalog, wanted: &str) -> Option<&'a ExecPlatform> {
    catalog
        .targets
        .exec_platforms
        .iter()
        .find(|e| e.name == wanted)
        .or_else(|| {
            catalog
                .targets
                .exec_platforms
                .iter()
                .find(|e| e.pool == wanted)
        })
}

/// Exec platforms for a native configuration: exactly one.
fn native_exec<'a>(
    catalog: &'a Catalog,
    os: Os,
    exec_pool: Option<&str>,
    xcode: Option<&str>,
) -> Result<&'a ExecPlatform> {
    let candidates: Vec<&ExecPlatform> = catalog
        .targets
        .exec_platforms
        .iter()
        .filter(|e| e.os == os.as_str())
        .collect();
    if let Some(wanted) = exec_pool {
        let e = find_exec(catalog, wanted).ok_or_else(|| {
            CliError::not_found(format!("no execution platform or pool named {wanted:?}"))
        })?;
        if e.os != os.as_str() {
            bail!(CliError::usage(format!(
                "pool {wanted:?} runs {}, not {}",
                e.os,
                os.as_str()
            )));
        }
        return Ok(e);
    }
    if let Some(version) = xcode {
        return candidates
            .into_iter()
            .find(|e| {
                catalog
                    .pool(&e.pool)
                    .and_then(|p| p.xcode_version.as_deref())
                    == Some(version)
            })
            .ok_or_else(|| {
                CliError::not_found(format!("no macOS pool serves Xcode {version}")).into()
            });
    }
    // Default: x86_64 for Linux/Windows (cheapest general pool), the first macOS pool.
    candidates
        .iter()
        .find(|e| e.cpu == "x86_64")
        .or(candidates.first())
        .copied()
        .ok_or_else(|| {
            CliError::not_found(format!("no Cucina pool executes {}", os.as_str())).into()
        })
}

struct CrossPlan<'a> {
    target: &'a Target,
    compile: Vec<&'a ExecPlatform>,
}

fn cross_plan<'a>(
    catalog: &'a Catalog,
    target: &str,
    exec_pool: Option<&str>,
) -> Result<CrossPlan<'a>> {
    let t = catalog.target(target).ok_or_else(|| {
        let names: Vec<&str> = catalog
            .targets
            .targets
            .iter()
            .map(|t| t.name.as_str())
            .collect();
        CliError::not_found(format!(
            "unknown target {target:?}; known targets: {}",
            names.join(", ")
        ))
    })?;
    let mut allowed: Vec<&ExecPlatform> = Vec::new();
    for name in &t.exec_platforms {
        let e = catalog.exec_platform(name).ok_or_else(|| {
            anyhow::anyhow!(
                "targets catalog: {} lists unknown exec platform {name:?}",
                t.name
            )
        })?;
        allowed.push(e);
    }
    anyhow::ensure!(
        !allowed.is_empty(),
        "targets catalog: {} has no exec platforms",
        t.name
    );
    if let Some(wanted) = exec_pool {
        let pos = allowed
            .iter()
            .position(|e| e.name == wanted || e.pool == wanted)
            .ok_or_else(|| {
                let names: Vec<&str> = allowed.iter().map(|e| e.pool.as_str()).collect();
                CliError::usage(format!(
                    "pool {wanted:?} cannot compile {} (allowed: {})",
                    t.name,
                    names.join(", ")
                ))
            })?;
        let chosen = allowed.remove(pos);
        allowed.insert(0, chosen);
    }
    Ok(CrossPlan {
        target: t,
        compile: allowed,
    })
}

fn validate(req: &Request) -> Result<()> {
    let endpoint_ok = req.endpoint.starts_with("grpcs://")
        || (req.endpoint.starts_with("grpc://")
            && crate::config::host_of(&req.endpoint)
                .is_some_and(|h| h == "127.0.0.1" || h == "localhost" || h == "::1"));
    if !endpoint_ok {
        bail!(CliError::usage(format!(
            "the client endpoint must be grpcs://host:port (got {:?})",
            req.endpoint
        )));
    }
    if req.instance_name.is_empty() || req.instance_name.chars().any(char::is_whitespace) {
        bail!(CliError::usage("invalid instance name"));
    }
    if let Some(c) = &req.config_name
        && (c.is_empty()
            || !c
                .chars()
                .all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '-' | '_')))
    {
        bail!(CliError::usage(format!(
            "invalid --config-name {c:?} (use [A-Za-z0-9_-])"
        )));
    }
    if req.helper_path.is_empty() {
        bail!(CliError::usage("empty credential helper path"));
    }
    if matches!(req.labels, Labels::Package(_)) && matches!(req.mode, Mode::Cross { .. }) {
        bail!(CliError::usage(
            "--platforms-package works only for native configurations; cross configurations need the @cucina_platforms module"
        ));
    }
    Ok(())
}

/// Generates the `.bazelrc` fragment.
pub fn generate(catalog: &Catalog, req: &Request) -> Result<Bazelrc> {
    validate(req)?;
    let host = crate::config::host_of(&req.endpoint)
        .ok_or_else(|| CliError::usage(format!("no host in {:?}", req.endpoint)))?;
    let mut b = Builder {
        lines: Vec::new(),
        config: req.config_name.clone(),
    };

    for h in &req.header {
        b.comment(h.clone());
    }
    if let Some(c) = &req.config_name {
        b.comment(format!(
            "Enable with --config={c} (startup options apply to every command)."
        ));
    }
    b.blank();

    b.comment("Remote repository contents cache: toolchains are never downloaded or uploaded per client (R-DATA-2).");
    b.startup("--experimental_remote_repo_contents_cache");
    b.blank();

    b.comment("Endpoint, TLS and the host-scoped credential helper (R-AUTH-8).");
    if !req.cache_only {
        b.common(format!("--remote_executor={}", req.endpoint));
    }
    b.common(format!("--remote_cache={}", req.endpoint));
    b.common(format!("--remote_instance_name={}", req.instance_name));
    if let Some(ca) = &req.ca_file {
        b.common(format!("--tls_certificate={ca}"));
    }
    b.common(format!("--credential_helper={host}={}", req.helper_path));
    b.comment(
        "Never also send Authorization via --remote_header: the helper renews the 15-minute token.",
    );
    b.blank();

    match &req.mode {
        Mode::Native {
            os,
            exec_pool,
            xcode,
        } if !req.cache_only => {
            let e = native_exec(catalog, *os, exec_pool.as_deref(), xcode.as_deref())?;
            let label = exec_label(e, &req.labels);
            b.comment(format!(
                "Platforms (R-RE-1): execute on pool {} ({} runner); Bazel 9's test toolchain needs --platforms too.",
                e.pool, e.runner
            ));
            b.common(format!("--extra_execution_platforms={label}"));
            b.common(format!("--host_platform={label}"));
            b.common(format!("--platforms={label}"));
            b.blank();
        }
        Mode::Native { .. } => {}
        Mode::Cross { target, exec_pool } => {
            let plan = cross_plan(catalog, target, exec_pool.as_deref())?;
            let t = plan.target;
            let mut exec_labels: Vec<String> = plan
                .compile
                .iter()
                .map(|e| exec_label(e, &req.labels))
                .collect();
            match &t.test {
                Some(test) => {
                    b.comment(format!(
                        "Cross configuration {} (R-XPLAT-2): compile on {}, test on {}/{} ({}).",
                        t.name, plan.compile[0].pool, test.pool, test.runner, test.mode
                    ));
                    exec_labels.push(test.label.clone());
                }
                None => b.comment(format!(
                    "Cross configuration {} (R-XPLAT-2): compile on {}; no OS to run tests on (build only).",
                    t.name, plan.compile[0].pool
                )),
            }
            b.comment(
                "Compile exec platforms first (preference order), then the test exec platform.",
            );
            b.common(format!(
                "--extra_execution_platforms={}",
                exec_labels.join(",")
            ));
            b.common(format!("--host_platform={}", exec_labels[0]));
            b.common(format!("--platforms={}", t.platform));
            b.common("--experimental_platform_in_output_dir");
            if t.is_msvc() {
                b.comment("MSVC CRT and Windows SDK licence terms (accepted; R-XPLAT-6).");
                for env in MSVC_EULA_REPO_ENV {
                    b.common(format!("--repo_env={env}"));
                }
            }
            if t.os == "windows" && req.client_os != Os::Windows {
                b.comment("Windows test environment for a non-Windows client (R-XPLAT-4).");
                for (k, v) in WINDOWS_TEST_ENV {
                    b.common(format!("--test_env={k}={v}"));
                }
            }
            b.blank();
        }
    }

    b.comment("Transfer and caching (R-DATA-1).");
    b.common("--remote_cache_compression");
    if req.ci {
        b.common("--remote_download_outputs=minimal");
    } else {
        b.common("--remote_download_outputs=toplevel");
        b.comment("Also download selected intermediate outputs, e.g. for IDE indexing:");
        let prefix = match &req.config_name {
            Some(c) => format!("common:{c}"),
            None => "common".to_string(),
        };
        b.comment(format!(
            r"{prefix} --remote_download_regex='.*\.(h|hpp|inc|pb\.h)$'"
        ));
    }
    if let Some(dir) = &req.disk_cache {
        b.common(format!("--disk_cache={dir}"));
        b.common(format!(
            "--experimental_disk_cache_gc_max_size={}",
            req.disk_cache_max_size
        ));
    }
    b.common("--experimental_remote_cache_eviction_retries=5");
    b.common("--rewind_lost_inputs");
    b.common("--remote_build_event_upload=minimal");
    b.common("--nolegacy_important_outputs");
    if req.read_only {
        b.comment("Read-only principal (no ac-write): never upload local results (avoids deny/refresh loops).");
        b.common("--remote_upload_local_results=false");
    }
    b.comment(
        "Keep --experimental_remote_cache_ttl (default 3h) below the server's CAS retention.",
    );
    b.blank();

    b.comment(
        "Resilience (§10.2): Bazel never times out Execute; retries cover restarts and evictions.",
    );
    if !req.cache_only {
        b.common("--jobs=200");
    }
    b.common("--remote_retries=10");
    b.common("--remote_retry_max_delay=30s");
    b.common("--grpc_keepalive_time=30s");
    if !req.cache_only {
        b.common("--noremote_local_fallback");
    }
    Ok(Bazelrc { lines: b.lines })
}

/// `platform()` definitions for every runner of every pool (`--emit-build-file`),
/// for workspaces that do not depend on the `@cucina_platforms` module.
pub fn platforms_build_file(catalog: &Catalog, header: &[String]) -> String {
    let mut out = String::new();
    for h in header {
        out.push_str(&format!("# {h}\n"));
    }
    out.push_str(
        "# Bazel platforms whose exec_properties match Cucina's runners exactly (R-RE-1).\n\n",
    );
    for pool in &catalog.pools.platforms {
        for runner in &pool.runners {
            let os = match pool.os.as_str() {
                "macos" => "macos",
                "windows" => "windows",
                _ => "linux",
            };
            let cpu = match runner.properties.get("ISA").map(String::as_str) {
                Some("x86-64") => "x86_64",
                Some("arm-a64") => "aarch64",
                Some("arm-a32") => "armv7",
                Some("rv64g") => "riscv64",
                Some("s390x") => "s390x",
                _ => "x86_64",
            };
            let props: Vec<String> = runner
                .properties
                .iter()
                .map(|(k, v)| format!("        {k:?}: {v:?},"))
                .collect();
            out.push_str(&format!(
                "platform(\n    name = \"{}-{}\",\n    constraint_values = [\n        \"@platforms//os:{os}\",\n        \"@platforms//cpu:{cpu}\",\n    ],\n    exec_properties = {{\n{}\n    }},\n    visibility = [\"//visibility:public\"],\n)\n\n",
                pool.name,
                runner.name,
                props.join("\n")
            ));
        }
    }
    out
}
