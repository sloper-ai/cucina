// SPDX-License-Identifier: FSL-1.1-ALv2

//! `cucinactl tui` end to end in a pseudo-terminal against the in-process fake
//! management server (R-CLI-4, UC13; the scripted TUI session of T20): the live
//! overview appears, views switch, a drain is opened and cancelled without reaching
//! the API, and `q` restores the terminal and exits 0. Unix: rexpect; Windows:
//! expectrl (ConPTY).
//!
//! Expectations are single words: the TUI redraws only changed cells, so a phrase
//! may reach the PTY as pieces separated by cursor movements.

mod support;

use cucinactl::config::AuthMethod;
use support::fake_mgmt::FakeMgmt;
use support::{TempDir, fake_jwt, now, write_profile, write_token};

/// Starts the fake server and a logged-in profile; returns (runtime, server, config dir).
fn fixture() -> (tokio::runtime::Runtime, FakeMgmt, TempDir) {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let jwt = fake_jwt(&serde_json::json!({"sub": "test:admin", "exp": now() + 900}));
    let mgmt = FakeMgmt::new(&jwt);
    let url = rt.block_on(mgmt.serve());
    let dir = TempDir::new();
    write_profile(
        dir.path(),
        "t",
        &support::profile(
            "https://cucina.test.invalid",
            &url,
            "grpcs://cucina.test.invalid",
            AuthMethod::ServiceKey,
        ),
    );
    write_token(dir.path(), "t", &jwt, now() + 900, AuthMethod::ServiceKey);
    (rt, mgmt, dir)
}

/// Guards: R-CLI-4 — a real terminal remains mandatory. The ConPTY launch below
/// must not be made to pass by treating ordinary redirected streams as a TTY.
fn assert_pipes_are_not_a_terminal(dir: &std::path::Path) {
    let out = support::cmd(dir)
        .args(["--output", "json", "tui"])
        .stdin(std::process::Stdio::null())
        .output()
        .expect("run TUI with redirected streams");
    assert_eq!(out.status.code(), Some(2));
    assert!(out.stdout.is_empty());
    let error: serde_json::Value = serde_json::from_slice(&out.stderr).expect("error.v1");
    assert_eq!(error["error"]["code"], "usage");
}

/// The fake saw no drain and the worker is not drained.
fn assert_not_drained(mgmt: &FakeMgmt) {
    let calls = mgmt.state.calls.lock().unwrap().clone();
    assert!(calls.iter().any(|c| c == "watch_overview"), "{calls:?}");
    assert!(calls.iter().any(|c| c == "get_pool"), "{calls:?}");
    assert!(!calls.iter().any(|c| c == "drain_worker"), "{calls:?}");
    assert!(
        mgmt.state
            .workers
            .lock()
            .unwrap()
            .iter()
            .all(|w| !w.drained)
    );
}

#[cfg(unix)]
#[test]
fn tui_session_in_a_pty() {
    use rexpect::session::{Options, PtySession, spawn_with_options};

    // Failures show the last 2 KiB of the screen stream, not all of it.
    fn expect(p: &mut PtySession, what: &str) {
        if let Err(e) = p.exp_string(what) {
            let text = format!("{e:?}");
            let tail: String = text
                .chars()
                .rev()
                .take(2048)
                .collect::<Vec<_>>()
                .into_iter()
                .rev()
                .collect();
            panic!("expected {what:?} on screen; output ended with: {tail}");
        }
    }
    // `send` goes through a LineWriter: flush so single keys arrive.
    fn key(p: &mut PtySession, k: &str) {
        p.send(k).expect("send");
        p.flush().expect("flush");
    }

    let (_rt, mgmt, dir) = fixture();
    assert_pipes_are_not_a_terminal(dir.path());
    // A new PTY has no size: set one before the TUI starts.
    let mut cmd = support::command_at(std::path::Path::new("/bin/sh"), dir.path());
    cmd.arg("-c")
        .arg("stty cols 100 rows 30 && exec \"$0\" tui")
        .arg(support::bin())
        .env("TERM", "xterm-256color");
    let opts = Options::new().timeout_ms(Some(30_000));
    let mut p = spawn_with_options(cmd, opts).expect("spawn cucinactl tui");

    expect(&mut p, "Overview");
    expect(&mut p, "linux-x86-64");
    key(&mut p, "2");
    expect(&mut p, "i-0123456789abcdef0");
    key(&mut p, "\r");
    key(&mut p, "d");
    expect(&mut p, "Confirm");
    key(&mut p, "n");
    expect(&mut p, "cancelled:");
    key(&mut p, "q");
    p.exp_eof().expect("exits");
    // nix's WaitStatus is not re-exported by rexpect: `Exited(Pid(n), 0)`.
    let status = format!("{:?}", p.process().wait().expect("wait"));
    assert!(
        status.starts_with("Exited(") && status.ends_with(", 0)"),
        "{status}"
    );
    assert_not_drained(&mgmt);
}

#[cfg(windows)]
#[test]
fn tui_session_in_a_pty() {
    use expectrl::{Expect, Session};

    // CI must report the cause of a failed PTY session, not only ExpectTimeout.
    // Read at most 64 KiB without blocking after the original deadline, and print
    // only escaped 2 KiB head/tail samples (no raw terminal-control sequences).
    fn expect(p: &mut expectrl::session::OsSession, mgmt: &FakeMgmt, what: &str) {
        if let Err(error) = p.expect(what) {
            let alive = p.get_process().is_alive();
            let exit = p.get_process().wait(Some(0));
            let mut bytes = Vec::new();
            let mut read_error = None;
            for _ in 0..16 {
                let mut chunk = [0u8; 4096];
                match p.try_read(&mut chunk) {
                    Ok(0) => break,
                    Ok(n) => bytes.extend_from_slice(&chunk[..n]),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                    Err(e) => {
                        read_error = Some(e.to_string());
                        break;
                    }
                }
            }
            // The fixture is isolated, but do not print its bearer token even if
            // an unexpected child error happens to include it.
            let safe_output = String::from_utf8_lossy(&bytes)
                .replace(mgmt.state.token.as_str(), "[redacted-test-token]");
            let safe_bytes = safe_output.as_bytes();
            let head = String::from_utf8_lossy(&safe_bytes[..safe_bytes.len().min(2048)]);
            let tail =
                String::from_utf8_lossy(&safe_bytes[safe_bytes.len().saturating_sub(2048)..]);
            let (call_count, recent_calls) = match mgmt.state.calls.try_lock() {
                Ok(calls) => (
                    Some(calls.len()),
                    calls.iter().rev().take(8).cloned().collect::<Vec<_>>(),
                ),
                Err(std::sync::TryLockError::WouldBlock) => {
                    (None, vec!["<unavailable: call log locked>".into()])
                }
                Err(std::sync::TryLockError::Poisoned(_)) => {
                    (None, vec!["<unavailable: poisoned call log>".into()])
                }
            };
            panic!(
                "expected {what:?} in ConPTY: {error:?}; child_alive={alive:?}; \
                 child_exit_zero_wait={exit:?}; captured_bytes={} (limit 65536); \
                 read_error={read_error:?}; head={head:?}; tail={tail:?}; \
                 fake_api_calls={call_count:?} recent={recent_calls:?}",
                bytes.len()
            );
        }
    }

    let (_rt, mgmt, dir) = fixture();
    assert_pipes_are_not_a_terminal(dir.path());
    // conpty 0.5 constructs CreateProcessW itself: Command's stdio settings are
    // not applied and only explicitly set environment entries are forwarded.
    // Bootstrap inside the real pseudoconsole, then open its console devices for
    // the TUI rather than inheriting Bazel's redirected standard handles.
    let mut cmd = support::command_at(std::path::Path::new("cmd.exe"), dir.path());
    for name in [
        "SystemRoot",
        "SystemDrive",
        "WINDIR",
        "ComSpec",
        "PATH",
        "PATHEXT",
        "TEMP",
        "TMP",
    ] {
        if let Some(value) = std::env::var_os(name) {
            cmd.env(name, value);
        }
    }
    cmd.env(
        "CUCINA_PTY_EXE",
        std::path::absolute(support::bin()).expect("absolute cucinactl path"),
    );
    // conpty concatenates Command arguments verbatim, so use cmd's documented
    // /S /C outer quotes. The executable path is expanded once inside quotes.
    cmd.args([
        "/d",
        "/s",
        "/c",
        r#"""%CUCINA_PTY_EXE%" tui <CONIN$ >CONOUT$ 2>&1""#,
    ]);
    let mut p = Session::spawn(cmd).expect("spawn cucinactl tui in ConPTY");
    // ConPTY without a parent console starts tiny: give the TUI room.
    p.get_process_mut().resize(100, 30).expect("resize");
    p.set_expect_timeout(Some(std::time::Duration::from_secs(30)));

    expect(&mut p, &mgmt, "Overview");
    expect(&mut p, &mgmt, "linux-x86-64");
    p.send("2").unwrap();
    expect(&mut p, &mgmt, "i-0123456789abcdef0");
    p.send("\r").unwrap();
    p.send("d").unwrap();
    expect(&mut p, &mgmt, "Confirm");
    p.send("n").unwrap();
    expect(&mut p, &mgmt, "cancelled:");
    p.send("q").unwrap();
    // ConPTY keeps its output pipe open after the child exits: wait for the process.
    assert_eq!(p.get_process().wait(Some(30_000)).expect("exits"), 0);
    assert_not_drained(&mgmt);
}
