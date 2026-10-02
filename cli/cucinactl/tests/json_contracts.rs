// SPDX-License-Identifier: FSL-1.1-ALv2

//! Contract test for every `--output json` document (R-CLI-3, R-TEST-6 "`cucinactl`:
//! contract tests on `--output json` schemas"): each command runs as a real process
//! against fake management and REAPI servers and its stdout must validate against
//! `schemas/<schema>.schema.json`. Also guards that every schema file is exercised
//! (no orphan or untested schema) — `login.v1` is validated by tests/login.rs.

mod support;

use std::collections::BTreeSet;
use std::path::Path;

use cucina_api::proto::build::bazel::remote::execution::v2 as re;
use cucinactl::config::AuthMethod;
use serde_json::Value;
use support::fake_mgmt::{FakeMgmt, ts};
use support::fake_reapi::FakeReapi;
use support::{TempDir, cmd, fake_jwt, now, schema, write_profile, write_token};

/// Seeds the fake CAS/AC with one failed action and returns its digest string.
fn seed_action(reapi: &FakeReapi) -> String {
    let file_a = reapi.put(b"int main() { return 1; }\n");
    let header = reapi.put(b"#pragma once\n");
    let sub = reapi.put_proto(&re::Directory {
        files: vec![re::FileNode {
            name: "a.h".into(),
            digest: header.clone().into(),
            ..Default::default()
        }],
        symlinks: vec![re::SymlinkNode {
            name: "link.h".into(),
            target: "a.h".into(),
            ..Default::default()
        }],
        ..Default::default()
    });
    let root = reapi.put_proto(&re::Directory {
        files: vec![re::FileNode {
            name: "main.cc".into(),
            digest: file_a.into(),
            is_executable: false,
            ..Default::default()
        }],
        directories: vec![re::DirectoryNode {
            name: "lib".into(),
            digest: sub.into(),
            ..Default::default()
        }],
        ..Default::default()
    });
    let command = reapi.put_proto(&re::Command {
        arguments: vec!["/bin/bash".into(), "-c".into(), "cc main.cc -o out".into()],
        environment_variables: vec![re::command::EnvironmentVariable {
            name: "PATH".into(),
            value: "/bin:/usr/bin".into(),
            ..Default::default()
        }],
        output_paths: vec!["out".into()],
        ..Default::default()
    });
    let action = reapi.put_proto(&re::Action {
        command_digest: command.into(),
        input_root_digest: root.into(),
        timeout: support::fake_mgmt::dur(60),
        platform: re::Platform {
            properties: vec![
                re::platform::Property {
                    name: "ISA".into(),
                    value: "x86-64".into(),
                    ..Default::default()
                },
                re::platform::Property {
                    name: "OSFamily".into(),
                    value: "linux".into(),
                    ..Default::default()
                },
            ],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    });
    let stderr = reapi.put(b"main.cc:1: error: something went wrong\n");
    let out = reapi.put(b"\x7fELF");
    reapi.store.action_results.lock().unwrap().insert(
        action.hash.clone(),
        re::ActionResult {
            exit_code: 1,
            stdout_raw: b"compiling\n".to_vec(),
            stderr_digest: stderr.into(),
            output_files: vec![re::OutputFile {
                path: "out".into(),
                digest: out.into(),
                is_executable: true,
                ..Default::default()
            }],
            execution_metadata: re::ExecutedActionMetadata {
                worker: "{\"node\":\"i-0123456789abcdef0\",\"pool\":\"linux-x86-64\"}".into(),
                queued_timestamp: ts(1_790_000_000),
                worker_start_timestamp: ts(1_790_000_002),
                input_fetch_start_timestamp: ts(1_790_000_002),
                input_fetch_completed_timestamp: ts(1_790_000_003),
                execution_start_timestamp: ts(1_790_000_003),
                execution_completed_timestamp: ts(1_790_000_009),
                output_upload_start_timestamp: ts(1_790_000_009),
                output_upload_completed_timestamp: ts(1_790_000_010),
                worker_completed_timestamp: ts(1_790_000_010),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        },
    );
    format!("{}/{}", action.hash, action.size_bytes)
}

fn run_json(dir: &Path, args: &[&str]) -> Vec<Value> {
    let out = cmd(dir)
        .arg("--output")
        .arg("json")
        .args(args)
        .output()
        .expect("run cucinactl");
    assert!(
        out.status.success(),
        "`cucinactl {}` failed ({}): {}",
        args.join(" "),
        out.status,
        String::from_utf8_lossy(&out.stderr)
    );
    let text = String::from_utf8(out.stdout).expect("utf-8 stdout");
    // One pretty document, or JSON lines for streams.
    match serde_json::from_str::<Value>(&text) {
        Ok(v) => vec![v],
        Err(_) => text
            .lines()
            .filter(|l| !l.trim().is_empty())
            .map(|l| serde_json::from_str(l).unwrap_or_else(|e| panic!("bad JSON line {l:?}: {e}")))
            .collect(),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn every_json_output_matches_its_schema() {
    let jwt = fake_jwt(&serde_json::json!({
        "iss": "http://127.0.0.1:9", "aud": "buildbarn", "sub": "test:admin", "sid": "s1",
        "exp": now() + 900, "cucina": {"admin": ["main"], "execute": ["main"]}
    }));
    let mgmt = FakeMgmt::new(&jwt);
    let mgmt_url = mgmt.serve().await;
    let reapi = FakeReapi::new(&jwt);
    let reapi_url = reapi.serve().await;
    let digest = seed_action(&reapi);

    let dir = TempDir::new();
    let mut p = support::profile(
        "http://127.0.0.1:9",
        &mgmt_url,
        &reapi_url,
        AuthMethod::ServiceKey,
    );
    // bazelrc requires a grpcs:// client endpoint; inspect uses the plaintext fake.
    p.jwks_uri = String::new();
    write_profile(dir.path(), "test", &p);
    write_token(
        dir.path(),
        "test",
        &jwt,
        now() + 900,
        AuthMethod::ServiceKey,
    );
    let files = TempDir::new();
    let diag = files.path().join("bundle.tar.gz");
    let host_diag = files.path().join("host.tar.gz");

    let rows: Vec<(Vec<String>, &str)> = vec![
        (vec!["status".into()], "status.v1"),
        (vec!["pools".into(), "list".into()], "pool-list.v1"),
        (
            vec!["pools".into(), "describe".into(), "linux-x86-64".into()],
            "pool-describe.v1",
        ),
        (
            [
                "pools",
                "scale-floor",
                "linux-x86-64",
                "--min",
                "1",
                "--for",
                "2h",
            ]
            .map(String::from)
            .to_vec(),
            "pool-floor.v1",
        ),
        (
            ["pools", "cordon", "linux-x86-64", "--yes"]
                .map(String::from)
                .to_vec(),
            "result.v1",
        ),
        (
            ["pools", "gc", "--dry-run"].map(String::from).to_vec(),
            "pool-gc.v1",
        ),
        (
            ["workers", "list"].map(String::from).to_vec(),
            "worker-list.v1",
        ),
        (
            ["workers", "drain", "i-0123456789abcdef0", "--yes"]
                .map(String::from)
                .to_vec(),
            "result.v1",
        ),
        (
            ["workers", "logs", "i-0123456789abcdef0"]
                .map(String::from)
                .to_vec(),
            "log.v1",
        ),
        (["hosts", "list"].map(String::from).to_vec(), "host-list.v1"),
        (
            ["hosts", "register", "C02AAA", "C02BBB", "--site", "office"]
                .map(String::from)
                .to_vec(),
            "host-register.v1",
        ),
        (
            ["hosts", "enroll-token", "create", "--site", "office"]
                .map(String::from)
                .to_vec(),
            "enroll-token.v1",
        ),
        (
            ["hosts", "enroll-token", "list"].map(String::from).to_vec(),
            "enroll-token-list.v1",
        ),
        (
            vec![
                "hosts".into(),
                "diag".into(),
                "C02XK0AAJGH6".into(),
                "-f".into(),
                host_diag.display().to_string(),
            ],
            "file.v1",
        ),
        (vec!["queues".into()], "queue-list.v1"),
        (
            ["ops", "list"].map(String::from).to_vec(),
            "operation-list.v1",
        ),
        (
            ["ops", "watch", "--count", "3"].map(String::from).to_vec(),
            "operation-event.v1",
        ),
        (
            vec!["action".into(), "inspect".into(), digest.clone()],
            "action.v1",
        ),
        (
            ["keys", "create", "--account", "ci-bot"]
                .map(String::from)
                .to_vec(),
            "service-key.v1",
        ),
        (
            ["keys", "list"].map(String::from).to_vec(),
            "service-key-list.v1",
        ),
        (
            ["keys", "revoke", "--sub", "google:1", "--yes"]
                .map(String::from)
                .to_vec(),
            "revocation.v1",
        ),
        (
            ["keys", "revocations"].map(String::from).to_vec(),
            "revocation-list.v1",
        ),
        (vec!["cost".into()], "cost.v1"),
        (vec!["images".into()], "image-list.v1"),
        (
            vec!["diag".into(), "-f".into(), diag.display().to_string()],
            "file.v1",
        ),
        (vec!["whoami".into()], "whoami.v1"),
        (["config", "view"].map(String::from).to_vec(), "config.v1"),
        (["config", "path"].map(String::from).to_vec(), "path.v1"),
        (
            ["bazelrc", "--list-targets"].map(String::from).to_vec(),
            "target-list.v1",
        ),
    ];

    let mut covered = BTreeSet::new();
    for (args, name) in &rows {
        let dir = dir.path().to_path_buf();
        let args = args.clone();
        let docs = tokio::task::spawn_blocking(move || {
            let refs: Vec<&str> = args.iter().map(String::as_str).collect();
            run_json(&dir, &refs)
        })
        .await
        .unwrap();
        assert!(!docs.is_empty(), "{name}: no output");
        for doc in &docs {
            schema::assert_valid(name, doc);
        }
        covered.insert(name.to_string());
    }

    // The action document carries the failed result read through ByteStream (zstd).
    assert!(
        reapi
            .store
            .reads
            .lock()
            .unwrap()
            .iter()
            .all(|r| r.contains("compressed-blobs/zstd/")),
        "blobs are read as compressed-blobs/zstd when the server advertises ZSTD"
    );

    // `action inspect <operation>`: a just-failed operation's ExecuteResponse (which the
    // action cache never stores) is used; without one the action cache answers.
    {
        let mut failed = support::fake_mgmt::operation();
        failed.name = "op-failed".into();
        failed.action_digest = digest.clone();
        failed.stage = "completed".into();
        let mut cached = failed.clone();
        cached.name = "op-cached".into();
        let mut ops = mgmt.state.operations.lock().unwrap();
        ops.push(failed);
        ops.push(cached);
    }
    let stderr = reapi.put(b"error: the build broke\n");
    mgmt.state.execute_responses.lock().unwrap().insert(
        "op-failed".into(),
        buffa::Message::encode_to_vec(&re::ExecuteResponse {
            result: re::ActionResult {
                exit_code: 2,
                stderr_digest: stderr.into(),
                ..Default::default()
            }
            .into(),
            message: "exit status 2".into(),
            ..Default::default()
        }),
    );
    for (op, source, exit_code) in [
        ("op-failed", "execute-response", 2),
        ("op-cached", "action-cache", 1),
    ] {
        let dir_path = dir.path().to_path_buf();
        let docs = tokio::task::spawn_blocking(move || {
            run_json(&dir_path, &["action", "inspect", op])
        })
        .await
        .unwrap();
        schema::assert_valid("action.v1", &docs[0]);
        assert_eq!(docs[0]["operation"]["name"], op);
        assert_eq!(docs[0]["result"]["source"], source, "{op}");
        assert_eq!(docs[0]["result"]["exit_code"], exit_code, "{op}");
    }

    // bazelrc needs a TLS client endpoint (grpcs://).
    let mut tls_profile = p.clone();
    tls_profile.remote_executor = "grpcs://cucina.test.invalid:443".into();
    write_profile(dir.path(), "test", &tls_profile);
    let dir_path = dir.path().to_path_buf();
    let docs = tokio::task::spawn_blocking(move || {
        run_json(
            &dir_path,
            &[
                "bazelrc",
                "--platform",
                "linux",
                "--helper-path",
                "/opt/cucina-credential-helper",
            ],
        )
    })
    .await
    .unwrap();
    schema::assert_valid("bazelrc.v1", &docs[0]);
    covered.insert("bazelrc.v1".into());

    // Errors in JSON mode go to stderr as error.v1.
    let dir_path = dir.path().to_path_buf();
    let out = tokio::task::spawn_blocking(move || {
        cmd(&dir_path)
            .args(["-o", "json", "pools", "describe", "no-such-pool"])
            .output()
            .unwrap()
    })
    .await
    .unwrap();
    assert_eq!(out.status.code(), Some(5));
    let err: Value = serde_json::from_slice(&out.stderr).expect("error.v1 on stderr");
    schema::assert_valid("error.v1", &err);
    covered.insert("error.v1".into());
    covered.insert("login.v1".into()); // tests/login.rs

    let all: BTreeSet<String> =
        std::fs::read_dir(support::repo_root().join("cli/cucinactl/schemas"))
            .unwrap()
            .map(|e| {
                e.unwrap()
                    .file_name()
                    .to_string_lossy()
                    .trim_end_matches(".schema.json")
                    .to_string()
            })
            .collect();
    assert_eq!(
        covered, all,
        "every schema file is exercised by a contract row"
    );
}
