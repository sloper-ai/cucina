// SPDX-License-Identifier: FSL-1.1-ALv2

//! Drift check for the checked-in generated code (R-LIB-3 "Protos: generated code
//! checked in + a CI drift check"). `cargo xtask codegen` records the SHA-256 of
//! every input `.proto` in `src/gen/inputs.sha256`; this test fails when a proto
//! was changed, added or removed without regenerating. It needs no `protoc`;
//! `cargo xtask codegen --check` (CI) additionally regenerates and diffs the code.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

const INCLUDE_DIRS: &[&str] = &["api/proto", "cli/cucina-api/proto"];

fn repo_root() -> PathBuf {
    // Bazel: data files live in the runfiles tree of the main repository.
    if let Some(srcdir) = std::env::var_os("TEST_SRCDIR") {
        let workspace = std::env::var("TEST_WORKSPACE").unwrap_or_else(|_| "_main".into());
        return Path::new(&srcdir).join(workspace);
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

fn collect(root: &Path, dir: &Path, out: &mut BTreeMap<String, String>) {
    for entry in fs::read_dir(dir).unwrap_or_else(|e| panic!("reading {}: {e}", dir.display())) {
        let path = entry.expect("dir entry").path();
        if path.is_dir() {
            collect(root, &path, out);
        } else if path.extension().is_some_and(|e| e == "proto") {
            let rel = path
                .strip_prefix(root)
                .expect("under root")
                .to_string_lossy()
                .replace('\\', "/");
            let digest = Sha256::digest(fs::read(&path).expect("read proto"));
            let hex = digest.iter().fold(String::new(), |mut s, b| {
                let _ = write!(s, "{b:02x}");
                s
            });
            out.insert(rel, hex);
        }
    }
}

#[test]
fn generated_code_matches_its_proto_inputs() {
    let root = repo_root();
    let manifest = fs::read_to_string(root.join("cli/cucina-api/src/gen/inputs.sha256"))
        .expect("src/gen/inputs.sha256 exists (run `cargo xtask codegen`)");
    let recorded: BTreeMap<String, String> = manifest
        .lines()
        .filter(|l| !l.starts_with('#') && !l.trim().is_empty())
        .map(|l| {
            let (hash, path) = l.split_once("  ").expect("`<sha256>  <path>` lines");
            (path.to_string(), hash.to_string())
        })
        .collect();

    let mut actual = BTreeMap::new();
    for dir in INCLUDE_DIRS {
        collect(&root, &root.join(dir), &mut actual);
    }
    assert_eq!(
        recorded, actual,
        "the .proto inputs changed since cli/cucina-api/src/gen was generated; run `cargo xtask codegen`"
    );
}
