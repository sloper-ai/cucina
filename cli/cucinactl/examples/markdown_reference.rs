// SPDX-License-Identifier: FSL-1.1-ALv2

//! Prints the Markdown command reference embedded in docs/cli.md
//! (`cargo xtask docs` runs this).

fn main() {
    print!("{}", cucinactl::cli::markdown_reference());
}
