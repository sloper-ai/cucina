// SPDX-License-Identifier: FSL-1.1-ALv2

//! Colour palette. Only the 16 ANSI colours are used (no RGB or 256-colour indexes), so
//! the TUI looks the same on 16-colour terminals, Windows Terminal, tmux and over SSH.
//! With `NO_COLOR` (or `--color never`) every style degrades to modifiers that need no
//! colour at all (bold, reversed), so selection and state stay visible. In a locale
//! that is not UTF-8 (`LANG=C`), [`asciify`] replaces box drawing and symbols with
//! ASCII after rendering.

use ratatui::buffer::Buffer;
use ratatui::style::{Color, Modifier, Style};

/// The palette in effect (part of the state, so rendering stays a pure function).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Theme {
    pub color: bool,
    /// Draw ASCII only (the locale is not UTF-8).
    pub ascii: bool,
}

/// Single-column ASCII stand-ins for every non-ASCII symbol the TUI draws.
fn ascii_symbol(sym: &str) -> Option<&'static str> {
    Some(match sym {
        "─" | "━" | "—" => "-",
        "│" | "┃" => "|",
        "┌" | "┐" | "└" | "┘" | "├" | "┤" | "┬" | "┴" | "┼" => "+",
        "●" => "*",
        "◐" => "~",
        "◌" => "o",
        "✖" | "✗" | "×" => "x",
        "✓" => "+",
        "·" => "-",
        "…" => ".",
        "↑" => "^",
        "↓" => "v",
        "←" => "<",
        "→" => ">",
        "▁" => "_",
        "▂" | "▃" => ".",
        "▄" | "▅" => "-",
        "▆" | "▇" => "=",
        "█" => "#",
        _ => return None,
    })
}

/// Replaces the TUI's box drawing and symbols in `buf` with ASCII (same widths).
pub fn asciify(buf: &mut Buffer) {
    for cell in &mut buf.content {
        if let Some(a) = ascii_symbol(cell.symbol()) {
            cell.set_symbol(a);
        }
    }
}

/// How a state word reads at a glance.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Tone {
    Good,
    Busy,
    Bad,
    Neutral,
}

/// Classifies the state words the management API uses (pool conditions, worker and VM
/// states, host phases, operation stages, alert severities).
pub fn tone(word: &str) -> Tone {
    let w = word.trim().to_ascii_lowercase();
    match w.split_whitespace().next().unwrap_or_default() {
        "ready" | "online" | "idle" | "registered" | "running" | "active" | "ok" | "current"
        | "live" | "approved" | "info" | "completed" => Tone::Good,
        "launching" | "busy" | "executing" | "queued" | "draining" | "drained" | "cordoned"
        | "warning" | "stopped" | "pending" | "stale" | "previous" | "reconnecting"
        | "connecting" | "starting" => Tone::Busy,
        "failed" | "down" | "offline" | "critical" | "revoked" | "error" | "degraded"
        | "disconnected" | "noqueue" | "queuenotdeclared" | "nocapacity" => Tone::Bad,
        _ => Tone::Neutral,
    }
}

impl Theme {
    pub const COLOR: Theme = Theme {
        color: true,
        ascii: false,
    };
    pub const PLAIN: Theme = Theme {
        color: false,
        ascii: false,
    };

    fn fg(&self, c: Color) -> Style {
        if self.color {
            Style::new().fg(c)
        } else {
            Style::new()
        }
    }

    pub fn ok(&self) -> Style {
        self.fg(Color::Green)
    }
    pub fn warn(&self) -> Style {
        self.fg(Color::Yellow)
    }
    pub fn bad(&self) -> Style {
        self.fg(Color::Red).add_modifier(Modifier::BOLD)
    }
    pub fn dim(&self) -> Style {
        self.fg(Color::DarkGray)
    }
    pub fn accent(&self) -> Style {
        if self.color {
            Style::new().fg(Color::Cyan)
        } else {
            Style::new().add_modifier(Modifier::BOLD)
        }
    }
    pub fn bold(&self) -> Style {
        Style::new().add_modifier(Modifier::BOLD)
    }
    /// Table header row.
    pub fn header(&self) -> Style {
        self.fg(Color::Cyan).add_modifier(Modifier::BOLD)
    }
    /// The selected row: reversed in the focused table, bold elsewhere.
    pub fn selected(&self, focused: bool) -> Style {
        if focused {
            Style::new().add_modifier(Modifier::REVERSED)
        } else {
            Style::new().add_modifier(Modifier::BOLD | Modifier::UNDERLINED)
        }
    }
    /// Borders of the focused block.
    pub fn border(&self, focused: bool) -> Style {
        if focused {
            self.fg(Color::Cyan)
        } else {
            Style::new()
        }
    }
    /// Key names in the footer and help.
    pub fn key(&self) -> Style {
        self.fg(Color::Cyan).add_modifier(Modifier::BOLD)
    }
    pub fn tab(&self, active: bool) -> Style {
        if active {
            Style::new().add_modifier(Modifier::REVERSED | Modifier::BOLD)
        } else {
            Style::new()
        }
    }
    pub fn tone(&self, tone: Tone) -> Style {
        match tone {
            Tone::Good => self.ok(),
            Tone::Busy => self.warn(),
            Tone::Bad => self.bad(),
            Tone::Neutral => Style::new(),
        }
    }
    /// Style for a state word (see [`tone`]).
    pub fn state(&self, word: &str) -> Style {
        self.tone(tone(word))
    }
}
