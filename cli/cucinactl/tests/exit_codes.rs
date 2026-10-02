// SPDX-License-Identifier: FSL-1.1-ALv2

//! Stable, documented exit codes (R-CLI-3; docs/cli.md §Exit codes): every failure
//! class maps to its code — 0 ok, 1 error, 2 usage, 3 auth required, 4 permission
//! denied, 5 not found, 6 unavailable, 7 conflict/precondition.

mod support;

use connectrpc::ErrorCode;
use cucinactl::config::AuthMethod;
use support::fake_mgmt::FakeMgmt;
use support::{TempDir, cmd, fake_jwt, now, write_profile, write_token};

/// (case, config directory, injected server error, arguments, expected exit code)
type Row<'a> = (
    &'a str,
    &'a std::path::Path,
    Option<ErrorCode>,
    Vec<&'a str>,
    i32,
);

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn failures_map_to_documented_exit_codes() {
    let jwt = fake_jwt(&serde_json::json!({"sub": "test:admin", "exp": now() + 900}));
    let mgmt = FakeMgmt::new(&jwt);
    let url = mgmt.serve().await;

    let live = TempDir::new();
    write_profile(
        live.path(),
        "t",
        &support::profile(
            "https://cucina.test.invalid",
            &url,
            "grpcs://cucina.test.invalid",
            AuthMethod::ServiceKey,
        ),
    );
    write_token(live.path(), "t", &jwt, now() + 900, AuthMethod::ServiceKey);

    // A management endpoint nobody listens on.
    let closed = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let closed_url = format!("http://{}", closed.local_addr().unwrap());
    drop(closed);
    let down = TempDir::new();
    write_profile(
        down.path(),
        "t",
        &support::profile(
            "https://cucina.test.invalid",
            &closed_url,
            "grpcs://cucina.test.invalid",
            AuthMethod::ServiceKey,
        ),
    );
    write_token(down.path(), "t", &jwt, now() + 900, AuthMethod::ServiceKey);

    let none = TempDir::new();

    let rows: Vec<Row<'_>> = vec![
        ("ok", live.path(), None, vec!["status"], 0),
        (
            "usage: missing argument",
            live.path(),
            None,
            vec!["pools", "describe"],
            2,
        ),
        (
            "usage: invalid flag value",
            live.path(),
            None,
            vec!["--output", "yaml", "status"],
            2,
        ),
        (
            "usage: destructive without --yes off a TTY",
            live.path(),
            None,
            vec!["workers", "drain", "i-1"],
            2,
        ),
        (
            "auth required: no profile",
            none.path(),
            None,
            vec!["status"],
            3,
        ),
        (
            "auth required: UNAUTHENTICATED",
            live.path(),
            Some(ErrorCode::Unauthenticated),
            vec!["status"],
            3,
        ),
        (
            "permission denied",
            live.path(),
            Some(ErrorCode::PermissionDenied),
            vec!["status"],
            4,
        ),
        (
            "not found",
            live.path(),
            None,
            vec!["pools", "describe", "no-such-pool"],
            5,
        ),
        (
            "unavailable: nothing listening",
            down.path(),
            None,
            vec!["status"],
            6,
        ),
        (
            "unavailable: UNAVAILABLE",
            live.path(),
            Some(ErrorCode::Unavailable),
            vec!["status"],
            6,
        ),
        (
            "conflict: FAILED_PRECONDITION",
            live.path(),
            Some(ErrorCode::FailedPrecondition),
            vec!["status"],
            7,
        ),
        (
            "error: INTERNAL",
            live.path(),
            Some(ErrorCode::Internal),
            vec!["status"],
            1,
        ),
    ];
    for (name, dir, fail, args, want) in rows {
        if let Some(code) = fail {
            mgmt.fail_next("get_status", code, name);
        }
        let dir = dir.to_path_buf();
        let args: Vec<String> = args.iter().map(|s| s.to_string()).collect();
        let out = tokio::task::spawn_blocking(move || cmd(&dir).args(&args).output().unwrap())
            .await
            .unwrap();
        assert_eq!(
            out.status.code(),
            Some(want),
            "{name}: stderr={}",
            String::from_utf8_lossy(&out.stderr)
        );
    }
}
