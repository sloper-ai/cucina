// SPDX-License-Identifier: FSL-1.1-ALv2

//! `.bazelrc` generation (UC9, R-DATA-1, §10.2, R-RE-1, R-XPLAT-2(d), R-XPLAT-4,
//! R-AUTH-8).
//!
//! Pure: [`generate`] turns a [`Request`] and the platform [`Catalog`] into lines.
//! Invariants (property-tested in `tests/bazelrc_props.rs`):
//!
//! * every flag exists in Bazel 9.2 (allow-list `testdata/bazel-9.2-flags.txt`), or
//!   is a build setting taken verbatim from an exec platform's `flags`;
//! * only `startup` and `build[:<config>]` lines, `startup` first; flags never repeat
//!   (except `--repo_env`/`--test_env`). `build`, not `common`: Bazel expands a
//!   config's `common:` lines before its `build:` lines, so `common:` lines would lose
//!   against `build:` lines of the same config in other rc files (ADR 0807);
//! * the credential helper is always host-scoped and `Authorization` is never set
//!   with `--remote_header`; `--experimental_remote_cache_chunking` and
//!   `--experimental_remote_merkle_tree_cache` are never emitted;
//! * `--extra_execution_platforms`, `--host_platform` and `--platforms` are always
//!   set together (Bazel 9's default test toolchain needs `--platforms`);
//!   `--host_platform` is the first execution platform; cross configurations list
//!   compile exec platforms first (the chosen pool first; macOS targets only on
//!   macOS) and then the target's test exec platform, plus the exec platforms'
//!   `flags`;
//! * excluded targets are refused with their `reason`;
//! * MSVC targets get the two EULA `--repo_env` flags and nothing else does; Windows
//!   targets get the Windows test environment from every client OS; emulated test
//!   runners get scaled `--test_timeout`s; Windows clients get the Linux/macOS client
//!   action environment and runfiles for cross configurations;
//! * read-only principals get `--remote_upload_local_results=false`.

use std::fmt;

use anyhow::{Result, bail};

use crate::catalog::{Catalog, ExecPlatform, Target};
use crate::exit::CliError;

/// The client host OS (Bazel computes the strict action environment from it).
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
    /// Emit `build:<name>` lines (use with `--config=<name>`).
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
        /// `startup` or `build`.
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
    /// Things the user must do besides pasting the lines (also present as comments),
    /// e.g. installing the `@bazel_tools` overlay for Windows tests.
    pub notes: Vec<String>,
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

/// Test environment of Windows targets (R-XPLAT-4, ADR 0904): Bazel's strict action
/// environment is the *client's*. Emitted from every client OS so test actions are
/// identical (cache sharing). Never `TMP`/`TEMP`: bb_runner sets them per action.
pub const WINDOWS_TEST_ENV: &[(&str, &str)] = &[
    ("SYSTEMROOT", r"C:\Windows"),
    (
        "PATH",
        r"C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0",
    ),
];

/// The strict action `PATH` of Linux/macOS clients, set by Windows clients of cross
/// configurations so their actions match (docs/cross-compilation.md, Hermeticity).
pub const UNIX_CLIENT_ACTION_PATH: &str = "/bin:/usr/bin:/usr/local/bin";

/// Builds the `@bazel_tools` overlay that lets non-Windows Bazel run Windows tests
/// (ADR 0904); path in the Cucina repository.
pub const WINDOWS_TEST_OVERLAY_SCRIPT: &str = "tools/xplat/windows-test-overlay.sh";

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
    fn build(&mut self, flag: impl Into<String>) {
        self.lines.push(Line::Flag {
            command: "build",
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

/// The `flags` of `platforms` in order, the first occurrence of each flag name
/// winning (the chosen exec platform comes first).
fn exec_flags(platforms: &[&ExecPlatform]) -> Vec<String> {
    let mut seen: Vec<&str> = Vec::new();
    let mut out = Vec::new();
    for e in platforms {
        for f in &e.flags {
            let name = f.split_once('=').map_or(f.as_str(), |(n, _)| n);
            if !seen.contains(&name) {
                seen.push(name);
                out.push(f.clone());
            }
        }
    }
    out
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
            .filter(|t| !t.excluded)
            .map(|t| t.name.as_str())
            .collect();
        CliError::not_found(format!(
            "unknown target {target:?}; known targets: {}",
            names.join(", ")
        ))
    })?;
    if let Some(reason) = t.exclusion() {
        bail!(CliError::usage(format!(
            "target {} is not supported: {reason}",
            t.name
        )));
    }
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
    if matches!(req.mode, Mode::Cross { .. }) {
        if matches!(req.labels, Labels::Package(_)) {
            bail!(CliError::usage(
                "--platforms-package works only for native configurations; cross configurations need the @cucina_platforms module"
            ));
        }
        if req.cache_only {
            bail!(CliError::usage(
                "--cache-only cannot be combined with --cross: cross configurations execute on Cucina's pools"
            ));
        }
    }
    Ok(())
}

/// What the configuration executes on.
enum Placement<'a> {
    /// `--cache-only`: local execution.
    Local,
    Native(&'a ExecPlatform),
    Cross(CrossPlan<'a>),
}

/// Generates the `.bazelrc` fragment.
pub fn generate(catalog: &Catalog, req: &Request) -> Result<Bazelrc> {
    validate(req)?;
    let host = crate::config::host_of(&req.endpoint)
        .ok_or_else(|| CliError::usage(format!("no host in {:?}", req.endpoint)))?;
    let placement = match &req.mode {
        Mode::Native { .. } if req.cache_only => Placement::Local,
        Mode::Native {
            os,
            exec_pool,
            xcode,
        } => Placement::Native(native_exec(
            catalog,
            *os,
            exec_pool.as_deref(),
            xcode.as_deref(),
        )?),
        Mode::Cross { target, exec_pool } => {
            Placement::Cross(cross_plan(catalog, target, exec_pool.as_deref())?)
        }
    };
    let windows_client_cross =
        matches!(placement, Placement::Cross(_)) && req.client_os == Os::Windows;
    let mut notes = Vec::new();
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
    if windows_client_cross {
        b.comment(
            "Windows client: runfiles as symlinks (with --enable_runfiles below; needs Developer Mode or admin).",
        );
        b.startup("--windows_enable_symlinks");
    }
    b.blank();

    b.comment("Endpoint, TLS and the host-scoped credential helper (R-AUTH-8).");
    if !req.cache_only {
        b.build(format!("--remote_executor={}", req.endpoint));
    }
    b.build(format!("--remote_cache={}", req.endpoint));
    b.build(format!("--remote_instance_name={}", req.instance_name));
    if let Some(ca) = &req.ca_file {
        b.build(format!("--tls_certificate={ca}"));
    }
    b.build(format!("--credential_helper={host}={}", req.helper_path));
    b.comment(
        "Never also send Authorization via --remote_header: the helper renews the 15-minute token.",
    );
    b.blank();

    match &placement {
        Placement::Local => {}
        Placement::Native(e) => {
            let label = exec_label(e, &req.labels);
            b.comment(format!(
                "Platforms (R-RE-1): execute on pool {} ({} runner); Bazel 9's test toolchain needs --platforms too.",
                e.pool, e.runner
            ));
            b.build(format!("--extra_execution_platforms={label}"));
            b.build(format!("--host_platform={label}"));
            b.build(format!("--platforms={label}"));
            // The flags name @cucina_platforms settings: only with the module's labels.
            if req.labels == Labels::Module && !e.flags.is_empty() {
                b.comment(format!(
                    "Required whenever {} compiles (e.g. the macOS SDK version, R-XPLAT-8).",
                    e.name
                ));
                for f in &e.flags {
                    b.build(f.clone());
                }
            }
            b.blank();
        }
        Placement::Cross(plan) => cross_section(&mut b, &mut notes, req, plan),
    }

    if windows_client_cross {
        b.comment(
            "Windows client, cross configuration: the action environment and runfiles of Linux/macOS",
        );
        b.comment(
            "clients, so actions are identical and share their cache (hermetic-llvm's tools ignore PATH).",
        );
        b.build(format!("--action_env=PATH={UNIX_CLIENT_ACTION_PATH}"));
        b.build(format!("--host_action_env=PATH={UNIX_CLIENT_ACTION_PATH}"));
        b.build("--enable_runfiles");
        b.blank();
    }

    b.comment("Transfer and caching (R-DATA-1).");
    b.build("--remote_cache_compression");
    if req.ci {
        b.build("--remote_download_outputs=minimal");
    } else {
        b.build("--remote_download_outputs=toplevel");
        b.comment("Also download selected intermediate outputs, e.g. for IDE indexing:");
        let prefix = match &req.config_name {
            Some(c) => format!("build:{c}"),
            None => "build".to_string(),
        };
        b.comment(format!(
            r"{prefix} --remote_download_regex='.*\.(h|hpp|inc|pb\.h)$'"
        ));
    }
    if let Some(dir) = &req.disk_cache {
        b.build(format!("--disk_cache={dir}"));
        b.build(format!(
            "--experimental_disk_cache_gc_max_size={}",
            req.disk_cache_max_size
        ));
    }
    b.build("--experimental_remote_cache_eviction_retries=5");
    b.build("--rewind_lost_inputs");
    b.build("--remote_build_event_upload=minimal");
    b.build("--nolegacy_important_outputs");
    if req.read_only {
        b.comment("Read-only principal (no ac-write): never upload local results (avoids deny/refresh loops).");
        b.build("--remote_upload_local_results=false");
    }
    b.comment(
        "Keep --experimental_remote_cache_ttl (default 3h) below the server's CAS retention.",
    );
    b.blank();

    b.comment(
        "Resilience (§10.2): Bazel never times out Execute; retries cover restarts and evictions.",
    );
    if !req.cache_only {
        b.build("--jobs=200");
    }
    b.build("--remote_retries=10");
    b.build("--remote_retry_max_delay=30s");
    b.build("--grpc_keepalive_time=30s");
    if !req.cache_only {
        b.build("--noremote_local_fallback");
    }
    Ok(Bazelrc {
        lines: b.lines,
        notes,
    })
}

/// The platform part of a cross configuration (R-XPLAT-2(d), docs/cross-compilation.md).
fn cross_section(b: &mut Builder, notes: &mut Vec<String>, req: &Request, plan: &CrossPlan<'_>) {
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
    b.comment("Compile exec platforms first (preference order), then the test exec platform.");
    b.build(format!(
        "--extra_execution_platforms={}",
        exec_labels.join(",")
    ));
    b.build(format!("--host_platform={}", exec_labels[0]));
    b.build(format!("--platforms={}", t.platform));
    b.build("--experimental_platform_in_output_dir");
    let flags = exec_flags(&plan.compile);
    if !flags.is_empty() {
        b.comment("Required whenever these exec platforms compile (e.g. the macOS SDK version, R-XPLAT-8).");
        for f in flags {
            b.build(f);
        }
    }
    if t.is_msvc() {
        b.comment("MSVC CRT and Windows SDK licence terms (accepted; R-XPLAT-6).");
        for env in MSVC_EULA_REPO_ENV {
            b.build(format!("--repo_env={env}"));
        }
    }
    if t.os == "windows" && t.test.is_some() {
        b.comment(
            "Windows test environment (R-XPLAT-4): Bazel's strict action environment is the client's;",
        );
        b.comment("the same lines from every client OS keep test actions identical.");
        for (k, v) in WINDOWS_TEST_ENV {
            b.build(format!("--test_env={k}={v}"));
        }
        if req.client_os != Os::Windows {
            let note = format!(
                "Windows tests from a {} client need the @bazel_tools overlay (R-XPLAT-4, ADR 0904): run {WINDOWS_TEST_OVERLAY_SCRIPT} (Cucina repository) in this workspace and add the line it prints (common --override_repository=bazel_tools=…) to the machine-local user.bazelrc; no patched Bazel is needed.",
                match req.client_os {
                    Os::Macos => "macOS",
                    _ => "Linux",
                }
            );
            for chunk in wrap(&note, 96) {
                b.comment(chunk);
            }
            notes.push(note);
        }
    }
    if let Some([short, moderate, long, eternal]) = t.test_timeouts() {
        b.comment(format!(
            "Emulated test runner ({}): Bazel's test timeouts x{} (testTimeoutScale).",
            t.test.as_ref().map_or("emulated", |x| x.mode.as_str()),
            t.test_timeout_scale.unwrap_or(1.0)
        ));
        b.build(format!(
            "--test_timeout={short},{moderate},{long},{eternal}"
        ));
    }
    b.blank();
}

/// Splits `text` into lines of at most `width` characters at spaces.
fn wrap(text: &str, width: usize) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    let mut line = String::new();
    for word in text.split_whitespace() {
        if !line.is_empty() && line.len() + 1 + word.len() > width {
            out.push(std::mem::take(&mut line));
        }
        if !line.is_empty() {
            line.push(' ');
        }
        line.push_str(word);
    }
    if !line.is_empty() {
        out.push(line);
    }
    out
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

#[cfg(test)]
mod tests {
    use super::*;

    fn request(mode: Mode, client_os: Os) -> Request {
        Request {
            mode,
            endpoint: "grpcs://cucina.example.com".into(),
            instance_name: "main".into(),
            helper_path: "cucina-credential-helper".into(),
            ca_file: None,
            config_name: None,
            ci: false,
            read_only: false,
            cache_only: false,
            disk_cache: None,
            disk_cache_max_size: "50G".into(),
            client_os,
            labels: Labels::Module,
            header: vec![],
        }
    }

    fn cross(target: &str, exec_pool: Option<&str>) -> Mode {
        Mode::Cross {
            target: target.into(),
            exec_pool: exec_pool.map(String::from),
        }
    }

    /// The documented example (docs/cross-compilation.md, Quick start): MSVC from Linux.
    #[test]
    fn msvc_from_linux_matches_the_documented_configuration() {
        let c = Catalog::embedded().unwrap();
        let rc = generate(&c, &request(cross("x86_64-windows-msvc", None), Os::Linux)).unwrap();
        let text = rc.to_string();
        for line in [
            "build --extra_execution_platforms=@cucina_platforms//exec:linux-x86-64-native,@cucina_platforms//exec:linux-aarch64-native,@cucina_platforms//exec:windows-x86-64-native,@cucina_platforms//exec:macos-arm64-xcode27.0-xcode,@cucina_platforms//test:test_on_windows_x86_64_msvc",
            "build --host_platform=@cucina_platforms//exec:linux-x86-64-native",
            "build --platforms=@llvm//platforms:windows_x86_64_msvc",
            "build --experimental_platform_in_output_dir",
            "build --@cucina_platforms//apple:sdk_version=27.0",
            "build --repo_env=BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1",
            "build --repo_env=BAZEL_WINDOWS_SDK_EULA=1",
            r"build '--test_env=SYSTEMROOT=C:\Windows'",
            r"build '--test_env=PATH=C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0'",
        ] {
            assert!(text.lines().any(|l| l == line), "missing {line:?}:\n{text}");
        }
        assert!(!text.lines().any(|l| l.starts_with("common")), "{text}");
        assert_eq!(rc.notes.len(), 1, "the @bazel_tools overlay note");
        assert!(rc.notes[0].contains(WINDOWS_TEST_OVERLAY_SCRIPT));
        assert!(text.contains(WINDOWS_TEST_OVERLAY_SCRIPT));
        assert_eq!(rc.value("test_timeout"), None);
    }

    #[test]
    fn windows_clients_get_the_unix_client_environment_and_no_overlay_note() {
        let c = Catalog::embedded().unwrap();
        let rc = generate(&c, &request(cross("x86_64-windows-gnu", None), Os::Windows)).unwrap();
        assert!(rc.notes.is_empty());
        assert_eq!(rc.value("action_env"), Some("PATH=/bin:/usr/bin:/usr/local/bin"));
        assert_eq!(
            rc.value("host_action_env"),
            Some("PATH=/bin:/usr/bin:/usr/local/bin")
        );
        assert!(rc.flags().any(|f| f == ("build", "--enable_runfiles")));
        assert!(
            rc.flags()
                .any(|f| f == ("startup", "--windows_enable_symlinks"))
        );
        // Same Windows test environment as from Linux/macOS clients.
        let from_linux =
            generate(&c, &request(cross("x86_64-windows-gnu", None), Os::Linux)).unwrap();
        let env = |rc: &Bazelrc| -> Vec<String> {
            rc.flags()
                .filter(|(_, f)| f.starts_with("--test_env="))
                .map(|(_, f)| f.to_string())
                .collect()
        };
        assert_eq!(env(&rc), env(&from_linux));
        assert_eq!(env(&rc).len(), 2);
    }

    #[test]
    fn emulated_runners_scale_test_timeouts() {
        let c = Catalog::embedded().unwrap();
        for t in ["riscv64-linux-gnu", "s390x-linux-musl", "armv7-linux-gnueabihf"] {
            let rc = generate(&c, &request(cross(t, None), Os::Macos)).unwrap();
            assert_eq!(rc.value("test_timeout"), Some("600,3000,9000,36000"), "{t}");
        }
        let rc = generate(&c, &request(cross("x86_64-linux-gnu", None), Os::Macos)).unwrap();
        assert_eq!(rc.value("test_timeout"), None);
    }

    #[test]
    fn excluded_targets_are_refused_with_their_reason() {
        let c = Catalog::embedded().unwrap();
        for t in c.targets.targets.iter().filter(|t| t.excluded) {
            let err = generate(&c, &request(cross(&t.name, None), Os::Linux)).unwrap_err();
            assert_eq!(
                crate::exit::exit_code_for(&err),
                crate::exit::ExitCode::Usage
            );
            assert!(
                format!("{err:#}").contains(t.reason.as_deref().unwrap()),
                "{err:#}"
            );
        }
    }

    #[test]
    fn exec_platform_flags_follow_the_chosen_pool() {
        let c = Catalog::embedded().unwrap();
        // macOS target: only the macOS pool, whose flags pin the SDK version.
        let rc = generate(&c, &request(cross("aarch64-apple-darwin", None), Os::Linux)).unwrap();
        assert_eq!(
            rc.value("@cucina_platforms//apple:sdk_version"),
            Some("27.0")
        );
        // Native macOS lane: the same flag with the module's labels, none with a package.
        let native = Mode::Native {
            os: Os::Macos,
            exec_pool: None,
            xcode: None,
        };
        let rc = generate(&c, &request(native.clone(), Os::Macos)).unwrap();
        assert_eq!(
            rc.value("@cucina_platforms//apple:sdk_version"),
            Some("27.0")
        );
        let mut pkg = request(native, Os::Macos);
        pkg.labels = Labels::Package("//tools/cucina".into());
        let rc = generate(&c, &pkg).unwrap();
        assert_eq!(rc.value("@cucina_platforms//apple:sdk_version"), None);
    }

    #[test]
    fn first_flag_of_a_name_wins() {
        let mk = |name: &str, flags: &[&str]| ExecPlatform {
            name: name.into(),
            label: format!("@x//:{name}"),
            pool: name.into(),
            runner: "xcode".into(),
            os: "macos".into(),
            cpu: "aarch64".into(),
            constraints: vec![],
            flags: flags.iter().map(|s| s.to_string()).collect(),
        };
        let a = mk("a", &["--@p//apple:sdk_version=26.0", "--x"]);
        let b = mk("b", &["--@p//apple:sdk_version=27.0", "--y=1"]);
        assert_eq!(
            exec_flags(&[&a, &b]),
            ["--@p//apple:sdk_version=26.0", "--x", "--y=1"]
        );
        assert_eq!(
            exec_flags(&[&b, &a]),
            ["--@p//apple:sdk_version=27.0", "--y=1", "--x"]
        );
    }

    #[test]
    fn wrap_keeps_words_whole() {
        let lines = wrap("aaa bbb ccc ddd", 7);
        assert_eq!(lines, ["aaa bbb", "ccc ddd"]);
        assert_eq!(wrap("", 10), Vec::<String>::new());
    }
}
