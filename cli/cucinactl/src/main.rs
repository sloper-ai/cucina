// SPDX-License-Identifier: FSL-1.1-ALv2

//! `cucinactl` (and `cucina-credential-helper` via argv[0] dispatch).

fn main() -> std::process::ExitCode {
    cucinactl::run_main()
}
