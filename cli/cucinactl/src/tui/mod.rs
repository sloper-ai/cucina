// SPDX-License-Identifier: FSL-1.1-ALv2

//! Interactive terminal UI (`cucinactl tui`, or no arguments on a TTY), R-CLI-4.
//!
//! Boundary for the TUI implementation (ratatui 0.30 with `ratatui::init/restore`):
//! [`run`] receives the command context; it opens an authenticated
//! [`crate::client::Session`] (management client + token handle), starts
//! [`crate::client::spawn_refresher`] so the 15-minute JWT never lapses, and drives
//! its views from `ManagementClient::watch_overview` / `watch_operations` (which
//! reconnect by themselves) plus the unary list/describe calls. Destructive actions
//! must ask for confirmation (R-CLI-4). Action drill-down uses
//! [`crate::inspect::inspect`].

use anyhow::Result;

use crate::commands::Ctx;
use crate::exit::CliError;

/// Runs the TUI until the user quits.
pub async fn run(_ctx: &Ctx) -> Result<()> {
    Err(CliError::new(
        crate::exit::ExitCode::Error,
        "the TUI is not available in this build yet; use the CLI commands (see `cucinactl --help`)",
    )
    .into())
}
