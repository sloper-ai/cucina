// SPDX-License-Identifier: FSL-1.1-ALv2

//! `cucinactl`: the Cucina management CLI (R-CLI), its Bazel credential helper
//! (R-AUTH-8), `.bazelrc` generator (UC9) and the reusable client library
//! ([`client`]) shared with the TUI ([`tui`]).
//!
//! The binary is also installed as `cucina-credential-helper` (hardlink or copy);
//! [`run_main`] dispatches on argv[0].

pub mod auth;
pub mod bazelrc;
pub mod catalog;
pub mod cli;
pub mod client;
pub mod commands;
pub mod config;
pub mod credential_helper;
pub mod exit;
pub mod http;
pub mod inspect;
pub mod output;
pub mod tls;
pub mod tui;
pub mod util;
pub mod views;

use std::ffi::OsString;
use std::path::Path;

/// Release version stamped by Bazel (`CUCINA_VERSION`, ADR 0150), or the Cargo
/// package version for a native Cargo build.
pub const VERSION: &str = match option_env!("CUCINA_VERSION") {
    Some(version) => version,
    None => env!("CARGO_PKG_VERSION"),
};

/// File stem that selects the credential-helper personality.
pub const CREDENTIAL_HELPER_NAME: &str = "cucina-credential-helper";

/// Whether argv[0] names the credential helper (`cucina-credential-helper[.exe]`).
pub fn invoked_as_credential_helper(argv0: &OsString) -> bool {
    Path::new(argv0)
        .file_stem()
        .and_then(|s| s.to_str())
        .is_some_and(|s| s.eq_ignore_ascii_case(CREDENTIAL_HELPER_NAME))
}

/// Process entry point for both personalities.
pub fn run_main() -> std::process::ExitCode {
    tls::install_crypto_provider();
    let args: Vec<OsString> = std::env::args_os().collect();
    if args.first().is_some_and(invoked_as_credential_helper) {
        return credential_helper::main_argv0(&args[1..]);
    }
    cli::main(args)
}
