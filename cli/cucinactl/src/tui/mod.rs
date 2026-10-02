// SPDX-License-Identifier: FSL-1.1-ALv2

//! Interactive terminal UI (`cucinactl tui`, or `cucinactl` with no arguments on a
//! terminal), R-CLI-4 / UC13. See docs/tui.md for the key map and the views.
//!
//! Architecture (ADR 0850):
//! * [`model::State`] holds everything on screen; [`update::update`] is the pure
//!   reducer (`(state, event) -> effects`); [`view::render`] draws a state and nothing
//!   else. Snapshot tests drive the reducer with [`fake::FakeManagement`] and render
//!   into ratatui's `TestBackend`.
//! * [`backend::Management`] is the port to the management API: [`backend::Remote`]
//!   (the `cucinactl::client` session: `WatchOverview`/`WatchOperations` streams that
//!   reconnect with backoff, unary calls, the action inspector) or the fake.
//! * [`runtime`] owns the terminal (`ratatui::try_init`/`restore`, whose panic hook
//!   restores the terminal), input, the clock, streams, signals and effects.
//! * Destructive actions (drain, kill, revoke, re-image) are only sent after a
//!   confirmation dialog answered "yes" (default "No").

pub mod backend;
pub mod fake;
pub mod filter;
pub mod format;
pub mod inspector;
pub mod layout;
pub mod model;
pub mod rows;
mod runtime;
pub mod theme;
pub mod update;
pub mod view;

use std::sync::Arc;

use anyhow::Result;

use crate::cli::ColorMode;
use crate::commands::Ctx;
use crate::exit::CliError;

pub use backend::{ApiError, Management, Remote, Reply, Request, StreamEvent};
pub use model::{State, Tab};
pub use theme::Theme;
pub use update::{Effect, Event, StreamKind, update};
pub use view::render;

/// `CUCINA_TUI_DEMO`: run against the built-in fake (no cluster, no login) for
/// terminal checks (MT-006), recordings and VHS goldens:
/// * `1` / `live`: invented data dated now, with a load that changes every 2 s;
/// * `scripted`: the same load at a fixed clock (every 2 s step looks the same);
/// * `frozen`: static data at a fixed clock (every screen is reproducible).
pub const DEMO_ENV: &str = "CUCINA_TUI_DEMO";
/// `CUCINA_TUI_MOUSE=0`: start with mouse capture off (toggle with `m`).
pub const MOUSE_ENV: &str = "CUCINA_TUI_MOUSE";

fn env_flag(name: &str) -> Option<bool> {
    let v = std::env::var(name).ok()?;
    Some(!matches!(
        v.trim().to_ascii_lowercase().as_str(),
        "" | "0" | "false" | "no" | "off"
    ))
}

/// Whether the locale promises UTF-8 (POSIX precedence: `LC_ALL`, `LC_CTYPE`,
/// `LANG`). No locale at all counts as UTF-8 (GUI terminals on macOS, Windows).
pub fn utf8_locale() -> bool {
    if cfg!(windows) {
        return true;
    }
    for var in ["LC_ALL", "LC_CTYPE", "LANG"] {
        if let Ok(v) = std::env::var(var)
            && !v.is_empty()
        {
            let v = v.to_ascii_lowercase();
            return v.contains("utf-8") || v.contains("utf8");
        }
    }
    true
}

/// The palette for `--color` (`auto` honours `NO_COLOR` and `TERM=dumb`), ASCII
/// drawing outside UTF-8 locales.
pub fn theme_for(mode: ColorMode) -> Theme {
    let color = match mode {
        ColorMode::Always => true,
        ColorMode::Never => false,
        ColorMode::Auto => {
            let no_color = std::env::var_os("NO_COLOR").is_some_and(|v| !v.is_empty());
            let dumb = std::env::var("TERM").is_ok_and(|t| t == "dumb");
            !(no_color || dumb)
        }
    };
    Theme {
        color,
        ascii: !utf8_locale(),
    }
}

/// Runs the TUI until the user quits.
pub async fn run(ctx: &Ctx) -> Result<()> {
    if !crate::util::interactive() {
        return Err(CliError::usage(
            "the TUI needs a terminal (stdin and stdout must be a TTY); use the CLI commands for scripts",
        )
        .into());
    }
    let theme = theme_for(ctx.global.color);
    let mouse = env_flag(MOUSE_ENV).unwrap_or(true);
    if env_flag(DEMO_ENV).unwrap_or(false) {
        let fixed = runtime::Clock::Fixed(fake::T0_MS);
        let (backend, clock) = match std::env::var(DEMO_ENV).unwrap_or_default().trim() {
            "frozen" => (fake::FakeManagement::new(), fixed),
            "scripted" => (fake::FakeManagement::scripted(), fixed),
            _ => (fake::FakeManagement::demo(), runtime::Clock::Wall),
        };
        let state = State::new("demo (fake data)", theme, mouse, (0, 0));
        return runtime::run(Arc::new(backend), state, None, clock).await;
    }
    // Authenticate before touching the terminal, so "run `cucinactl login`" and
    // other setup errors print normally (with their exit codes).
    let session = ctx.session().await?;
    let refresher = (session.ctx.clone(), session.token.clone());
    let state = State::new(session.ctx.name.clone(), theme, mouse, (0, 0));
    let backend = Arc::new(Remote::new(session));
    runtime::run(backend, state, Some(refresher), runtime::Clock::Wall).await
}
