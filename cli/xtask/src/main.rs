// SPDX-License-Identifier: FSL-1.1-ALv2

//! `cargo xtask <task>` — developer tasks for the Rust workspace.
//!
//! * `codegen [--check]`: regenerates the checked-in buffa messages and
//!   connect-rust stubs in `cli/cucina-api/src/gen/{buffa,connect}/` (ADR 0003) from
//!   `api/proto/cucina/v1/*.proto` and the vendored REAPI/googleapis/Buildbarn
//!   protos in `cli/cucina-api/proto/`, with `buf generate` and the
//!   `protoc-gen-buffa`, `protoc-gen-buffa-packaging` (twice) and
//!   `protoc-gen-connect-rust` plugins. It records the SHA-256 of every input in
//!   `src/gen/inputs.sha256` (checked by the `codegen_drift` test, which needs no
//!   tools). `--check` regenerates into a scratch directory and fails if anything
//!   differs (CI).
//!   Tools: `buf` comes from `$BUF`, else mise (`buf@1.73.0`), else `PATH`; the
//!   plugins from `$PROTOC_GEN_BUFFA`, `$PROTOC_GEN_BUFFA_PACKAGING`,
//!   `$PROTOC_GEN_CONNECT_RUST`, else the pinned release binaries (the same ones
//!   Bazel uses, tools/pinned.bzl), fetched once with `curl` and SHA-256-verified.
//! * `docs [--check]`: regenerates the command reference section of `docs/cli.md`
//!   from the clap definitions (`--check` fails if it is stale).

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use anyhow::{Context, Result, bail, ensure};
use sha2::{Digest, Sha256};

/// Import roots (repository-relative) and the buf module directory each becomes.
const INPUT_ROOTS: &[(&str, &str)] = &[("api/proto", "cucina"), ("cli/cucina-api/proto", "vendor")];

/// Pinned plugins (ADR 0003, ADR 0106; the same release binaries as tools/pinned.bzl):
/// (tool, version, override env var, release URL prefix).
const PLUGINS: &[(&str, &str, &str, &str)] = &[
    (
        "protoc-gen-buffa",
        "0.9.2",
        "PROTOC_GEN_BUFFA",
        "https://github.com/anthropics/buffa/releases/download/v0.9.2",
    ),
    (
        "protoc-gen-buffa-packaging",
        "0.9.2",
        "PROTOC_GEN_BUFFA_PACKAGING",
        "https://github.com/anthropics/buffa/releases/download/v0.9.2",
    ),
    (
        "protoc-gen-connect-rust",
        "0.9.1",
        "PROTOC_GEN_CONNECT_RUST",
        "https://github.com/connectrpc/connect-rust/releases/download/v0.9.1",
    ),
];

/// SHA-256 of each release binary: (file name, sha256). Mirrors tools/pinned.bzl.
const PLUGIN_SHA256: &[(&str, &str)] = &[
    (
        "protoc-gen-buffa-v0.9.2-darwin-aarch64",
        "654f4a5d58212afbdb3dec7475d99d6bc6cba2c10807c3751652cd3806cf5a5d",
    ),
    (
        "protoc-gen-buffa-v0.9.2-linux-x86_64",
        "e8d87b0cc34beb5524835888da694901deeeccaefd834fd5c03300375b629688",
    ),
    (
        "protoc-gen-buffa-v0.9.2-linux-aarch64",
        "14f8285cc558f136aaa8a401e802f0e9e7a655e86dbf85ded7841f35fdf62b42",
    ),
    (
        "protoc-gen-buffa-v0.9.2-windows-x86_64.exe",
        "d6346bda11fa0845f46e43935a2c17beb5dcfe56b96db9109a4db88ea0f47814",
    ),
    (
        "protoc-gen-buffa-packaging-v0.9.2-darwin-aarch64",
        "7f2213ebb3de60361b5ef9c91aa76e092f61c70bd9b0cb631302fecf677f78ec",
    ),
    (
        "protoc-gen-buffa-packaging-v0.9.2-linux-x86_64",
        "965e5b7b2ef6847f6597d32565468a093d70ab82ee74c326ff5397b04d419161",
    ),
    (
        "protoc-gen-buffa-packaging-v0.9.2-linux-aarch64",
        "37a51a9dc9d62db6070f202f12e9a0bd9a8bf37cfa4adbfeb603cfabe0f9ff58",
    ),
    (
        "protoc-gen-buffa-packaging-v0.9.2-windows-x86_64.exe",
        "0db4cdfc67fe8a3b45f3690cc766abdaf2a10f67122c6ce94b46d6c62abd6015",
    ),
    (
        "protoc-gen-connect-rust-v0.9.1-darwin-aarch64",
        "8efc5fcce492d9e9e4bc540bfefc9b7651c7b6780ba9b833d22dbd2358c7286c",
    ),
    (
        "protoc-gen-connect-rust-v0.9.1-linux-x86_64",
        "580c2a0690b9ad48364bd455cf0b7f8f0153bcbffe85140dd2ce07bfc0b1da24",
    ),
    (
        "protoc-gen-connect-rust-v0.9.1-linux-aarch64",
        "c945e5c2755d24d5fb81cc3bc9f00a2ca4778d4d687d598d9c84e6256067d685",
    ),
    (
        "protoc-gen-connect-rust-v0.9.1-windows-x86_64.exe",
        "5887b8f240a48d8009e868dbe08096b5a73e343d9140979e1bc844d7b49e3e0e",
    ),
];
const BUF_VERSION: &str = "1.73.0";

const GEN_DIR: &str = "cli/cucina-api/src/gen";
const INPUTS_FILE: &str = "inputs.sha256";

const DOCS_FILE: &str = "docs/cli.md";
const DOCS_BEGIN: &str = "<!-- BEGIN GENERATED: cargo xtask docs -->";
const DOCS_END: &str = "<!-- END GENERATED: cargo xtask docs -->";

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let check = args.iter().any(|a| a == "--check");
    match args.first().map(String::as_str) {
        Some("codegen") => codegen(check),
        Some("docs") => docs(check),
        _ => bail!("usage: cargo xtask codegen [--check] | cargo xtask docs [--check]"),
    }
}

fn repo_root() -> PathBuf {
    // cli/xtask -> repository root
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .ancestors()
        .nth(2)
        .expect("xtask lives two levels below the repository root")
        .to_path_buf()
}

/// Finds `buf`: `$BUF`, then mise's pinned install, then PATH.
fn buf_tool() -> Result<PathBuf> {
    if let Some(p) = std::env::var_os("BUF").filter(|v| !v.is_empty()) {
        return Ok(PathBuf::from(p));
    }
    if let Ok(out) = Command::new("mise")
        .args(["where", &format!("buf@{BUF_VERSION}")])
        .output()
        && out.status.success()
    {
        let dir = PathBuf::from(String::from_utf8_lossy(&out.stdout).trim());
        for candidate in [
            dir.join("bin/buf"),
            dir.join("buf/bin/buf"),
            dir.join("buf"),
        ] {
            if candidate.is_file() {
                return Ok(candidate);
            }
        }
    }
    Ok(PathBuf::from("buf"))
}

fn release_platform() -> Result<&'static str> {
    Ok(match (std::env::consts::OS, std::env::consts::ARCH) {
        ("macos", "aarch64") => "darwin-aarch64",
        ("linux", "x86_64") => "linux-x86_64",
        ("linux", "aarch64") => "linux-aarch64",
        ("windows", "x86_64") => "windows-x86_64.exe",
        (os, arch) => bail!(
            "no pinned codegen plugin binaries for {os}/{arch}; set the PROTOC_GEN_* variables"
        ),
    })
}

/// Finds a plugin: `$<ENV>`, else the pinned release binary (downloaded once with
/// `curl` into the target directory and verified against its SHA-256).
fn plugin(name: &str, version: &str, env: &str, url_prefix: &str) -> Result<PathBuf> {
    if let Some(p) = std::env::var_os(env).filter(|v| !v.is_empty()) {
        return Ok(PathBuf::from(p));
    }
    let file = format!("{name}-v{version}-{}", release_platform()?);
    let want = PLUGIN_SHA256
        .iter()
        .find(|(f, _)| *f == file)
        .map(|(_, h)| *h)
        .with_context(|| format!("no pinned checksum for {file}"))?;
    let target_dir = std::env::var_os("CARGO_TARGET_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| repo_root().join("target"));
    let cache = target_dir.join("xtask-tools");
    let path = cache.join(&file);
    let verified = |p: &Path| -> bool {
        fs::read(p)
            .map(|b| hex(&Sha256::digest(&b)) == want)
            .unwrap_or(false)
    };
    if !verified(&path) {
        fs::create_dir_all(&cache)?;
        let status = Command::new("curl")
            .args(["-fsSL", "-o"])
            .arg(&path)
            .arg(format!("{url_prefix}/{file}"))
            .status()
            .context("running curl to fetch the pinned plugin")?;
        ensure!(status.success(), "downloading {file} failed");
        ensure!(
            verified(&path),
            "{file}: SHA-256 mismatch (expected {want})"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            fs::set_permissions(&path, fs::Permissions::from_mode(0o755))?;
        }
    }
    Ok(path)
}

fn copy_protos(src: &Path, dst: &Path) -> Result<()> {
    for entry in fs::read_dir(src).with_context(|| format!("reading {}", src.display()))? {
        let path = entry?.path();
        let target = dst.join(path.file_name().unwrap_or_default());
        if path.is_dir() {
            copy_protos(&path, &target)?;
        } else if path.extension().is_some_and(|e| e == "proto") {
            fs::create_dir_all(dst)?;
            fs::copy(&path, &target)?;
        }
    }
    Ok(())
}

fn codegen(check: bool) -> Result<()> {
    let root = repo_root();
    let gen_dir = root.join(GEN_DIR);
    let buf = buf_tool()?;
    let plugin_paths: Vec<PathBuf> = PLUGINS
        .iter()
        .map(|(name, version, env, url)| plugin(name, version, env, url))
        .collect::<Result<_>>()?;
    let [buffa, packaging, connect] = [&plugin_paths[0], &plugin_paths[1], &plugin_paths[2]];

    // A self-contained buf workspace: the repository's buf.yaml only covers api/proto.
    let work = std::env::temp_dir().join(format!("cucina-codegen-{}", std::process::id()));
    if work.exists() {
        fs::remove_dir_all(&work)?;
    }
    let mut modules = String::new();
    for (src, module) in INPUT_ROOTS {
        copy_protos(&root.join(src), &work.join(module))?;
        writeln!(modules, "  - path: {module}")?;
    }
    fs::write(
        work.join("buf.yaml"),
        format!("version: v2\nmodules:\n{modules}"),
    )?;
    let out = work.join("out");
    let template = format!(
        r#"version: v2
plugins:
  - local: {buffa}
    out: {out}/buffa
    strategy: all
    opt: [views=true, json=true, exclude_package=.google.api]
  - local: {packaging}
    out: {out}/buffa
    strategy: all
    opt: [exclude_package=.google.api]
  - local: {connect}
    out: {out}/connect
    strategy: all
    opt: [buffa_module=crate::proto]
  - local: {packaging}
    out: {out}/connect
    strategy: all
    opt: [filter=services, exclude_package=.google.api]
"#,
        buffa = buffa.display(),
        packaging = packaging.display(),
        connect = connect.display(),
        out = out.display(),
    );
    fs::write(work.join("buf.gen.yaml"), template)?;
    let status = Command::new(&buf)
        .arg("generate")
        .current_dir(&work)
        .status()
        .with_context(|| format!("running {}", buf.display()))?;
    ensure!(status.success(), "buf generate failed");
    fs::write(out.join(INPUTS_FILE), inputs_manifest(&root)?)?;

    let want = read_tree(&out)?;
    let have = read_tree(&gen_dir).unwrap_or_default();
    let result = if check {
        let stale: Vec<&String> = want
            .keys()
            .chain(have.keys())
            .filter(|k| want.get(*k) != have.get(*k))
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect();
        if stale.is_empty() {
            println!("{GEN_DIR} is up to date");
            Ok(())
        } else {
            Err(anyhow::anyhow!(
                "{GEN_DIR} is stale ({} files differ, e.g. {}); run `cargo xtask codegen`",
                stale.len(),
                stale[0]
            ))
        }
    } else {
        if gen_dir.exists() {
            fs::remove_dir_all(&gen_dir)?;
        }
        for (rel, data) in &want {
            let target = gen_dir.join(rel);
            fs::create_dir_all(target.parent().expect("parent"))?;
            fs::write(&target, data)?;
        }
        println!("regenerated {GEN_DIR} ({} files)", want.len());
        Ok(())
    };
    let _ = fs::remove_dir_all(&work);
    result
}

/// Lists every input .proto (all files under the import roots) with its SHA-256.
fn inputs_manifest(root: &Path) -> Result<String> {
    let mut files = Vec::new();
    for (dir, _) in INPUT_ROOTS {
        collect_protos(&root.join(dir), &mut files)?;
    }
    files.sort();
    let mut out = String::new();
    writeln!(
        out,
        "# Inputs of the generated code in this directory (written by `cargo xtask codegen`; do not edit)."
    )?;
    let versions: Vec<String> = PLUGINS
        .iter()
        .map(|(n, v, _, _)| format!("{n} {v}"))
        .collect();
    writeln!(
        out,
        "# generator: buf {BUF_VERSION}, {}",
        versions.join(", ")
    )?;
    for file in files {
        let rel = file
            .strip_prefix(root)?
            .to_string_lossy()
            .replace('\\', "/");
        let digest = Sha256::digest(fs::read(&file)?);
        writeln!(out, "{}  {rel}", hex(&digest))?;
    }
    Ok(out)
}

fn collect_protos(dir: &Path, out: &mut Vec<PathBuf>) -> Result<()> {
    for entry in fs::read_dir(dir).with_context(|| format!("reading {}", dir.display()))? {
        let path = entry?.path();
        if path.is_dir() {
            collect_protos(&path, out)?;
        } else if path.extension().is_some_and(|e| e == "proto") {
            out.push(path);
        }
    }
    Ok(())
}

/// All files below `dir`, keyed by `/`-separated relative path.
fn read_tree(dir: &Path) -> Result<BTreeMap<String, Vec<u8>>> {
    fn walk(base: &Path, dir: &Path, out: &mut BTreeMap<String, Vec<u8>>) -> Result<()> {
        for entry in fs::read_dir(dir)? {
            let path = entry?.path();
            if path.is_dir() {
                walk(base, &path, out)?;
            } else {
                let rel = path
                    .strip_prefix(base)?
                    .to_string_lossy()
                    .replace('\\', "/");
                out.insert(rel, fs::read(&path)?);
            }
        }
        Ok(())
    }
    let mut out = BTreeMap::new();
    walk(dir, dir, &mut out)?;
    Ok(out)
}

fn hex(bytes: &[u8]) -> String {
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut s, b| {
            let _ = write!(s, "{b:02x}");
            s
        })
}

fn docs(check: bool) -> Result<()> {
    let path = repo_root().join(DOCS_FILE);
    let current = fs::read_to_string(&path).with_context(|| format!("reading {DOCS_FILE}"))?;
    let (Some(begin), Some(end)) = (current.find(DOCS_BEGIN), current.find(DOCS_END)) else {
        bail!("{DOCS_FILE} lacks the {DOCS_BEGIN} / {DOCS_END} markers");
    };
    // The renderer lives in cucinactl (examples/markdown_reference.rs) so that xtask
    // does not depend on cucinactl: `codegen` must build even when the generated
    // code cucinactl needs is missing or stale.
    let cargo = std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into());
    let out = Command::new(cargo)
        .args([
            "run",
            "--quiet",
            "--package",
            "cucinactl",
            "--example",
            "markdown_reference",
        ])
        .current_dir(repo_root())
        .output()
        .context("running the markdown_reference example")?;
    ensure!(
        out.status.success(),
        "markdown_reference failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    let generated = String::from_utf8(out.stdout)?;
    let updated = format!(
        "{}{DOCS_BEGIN}\n{generated}{}",
        &current[..begin],
        &current[end..]
    );
    if check {
        ensure!(
            updated == current,
            "{DOCS_FILE} is stale; run `cargo xtask docs`"
        );
        println!("{DOCS_FILE} is up to date");
    } else if updated != current {
        fs::write(&path, updated)?;
        println!("updated {DOCS_FILE}");
    }
    Ok(())
}
