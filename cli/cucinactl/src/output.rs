// SPDX-License-Identifier: FSL-1.1-ALv2

//! Output: human tables by default, `--output json` for scripts (no YAML anywhere).
//!
//! Every JSON document carries a `schema` field naming its JSON Schema in
//! `cli/cucinactl/schemas/<schema>.schema.json` (contract-tested). Colors follow
//! `--color`, `NO_COLOR`/`CLICOLOR_FORCE` and TTY detection via `anstream`.

use std::io::{self, Write};

use anyhow::Result;
use comfy_table::{Cell, ContentArrangement, Table, presets};
use serde::Serialize;

/// Output format selected with `--output`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, clap::ValueEnum)]
pub enum OutputFormat {
    /// Human-readable tables and text.
    #[default]
    Table,
    /// One JSON document (or JSON lines for streams) per the documented schemas.
    Json,
}

/// A command result that can be rendered as a table or as JSON.
pub trait Render: Serialize {
    /// Schema name, e.g. `pool-list.v1` (file `schemas/pool-list.v1.schema.json`).
    const SCHEMA: &'static str;
    /// Writes the human representation.
    fn human(&self, out: &mut dyn Write) -> io::Result<()>;
}

#[derive(Serialize)]
struct Envelope<'a, T: Serialize> {
    schema: &'static str,
    #[serde(flatten)]
    data: &'a T,
}

/// Serializes `value` with its `schema` field (compact, for JSON-lines streams).
pub fn to_json_line<R: Render>(value: &R) -> Result<String> {
    Ok(serde_json::to_string(&Envelope {
        schema: R::SCHEMA,
        data: value,
    })?)
}

/// Prints a result to stdout in the selected format.
pub fn emit<R: Render>(format: OutputFormat, value: &R) -> Result<()> {
    let mut out = anstream::stdout().lock();
    match format {
        OutputFormat::Json => {
            serde_json::to_writer_pretty(
                &mut out,
                &Envelope {
                    schema: R::SCHEMA,
                    data: value,
                },
            )?;
            writeln!(out)?;
        }
        OutputFormat::Table => value.human(&mut out)?,
    }
    out.flush()?;
    Ok(())
}

/// Prints one element of a stream (JSON lines in JSON mode).
pub fn emit_line<R: Render>(format: OutputFormat, value: &R) -> Result<()> {
    let mut out = anstream::stdout().lock();
    match format {
        OutputFormat::Json => writeln!(out, "{}", to_json_line(value)?)?,
        OutputFormat::Table => value.human(&mut out)?,
    }
    out.flush()?;
    Ok(())
}

/// Whether stdout gets colors (`--color`, `NO_COLOR`, `CLICOLOR_FORCE`, TTY).
pub fn colors_enabled() -> bool {
    anstream::AutoStream::choice(&std::io::stdout()) != anstream::ColorChoice::Never
}

/// A borderless, kubectl-style table. Cell colors use comfy-table's own styling
/// (applied after layout, so columns stay aligned) and follow [`colors_enabled`].
pub fn table(header: &[&str]) -> Table {
    let mut t = Table::new();
    t.load_style(presets::NOTHING)
        .set_content_arrangement(ContentArrangement::Disabled)
        .set_header(header.iter().map(|h| h.to_ascii_uppercase()));
    if colors_enabled() {
        t.enforce_styling();
    } else {
        t.force_no_tty();
    }
    for col in t.column_iter_mut() {
        col.set_padding((0, 2));
    }
    t
}

/// Builds a table row from strings and [`Cell`]s.
#[macro_export]
macro_rules! cells {
    ($($e:expr),* $(,)?) => { vec![$(comfy_table::Cell::from($e)),*] };
}

/// A colored state cell for tables (see [`state`]).
pub fn state_cell(word: &str) -> Cell {
    match state_color(word) {
        Some(c) => Cell::new(word).fg(match c {
            anstyle::AnsiColor::Green => comfy_table::Color::Green,
            anstyle::AnsiColor::Yellow => comfy_table::Color::Yellow,
            _ => comfy_table::Color::Red,
        }),
        None => Cell::new(word),
    }
}

/// Writes a table followed by a newline; prints `none` hints for empty tables.
pub fn write_table(out: &mut dyn Write, t: &Table, empty_hint: &str) -> io::Result<()> {
    if t.row_count() == 0 {
        writeln!(out, "{empty_hint}")
    } else {
        let text = t.to_string();
        for line in text.lines() {
            writeln!(out, "{}", line.trim_end())?;
        }
        Ok(())
    }
}

/// `key: value` lines with aligned keys.
pub fn write_fields(out: &mut dyn Write, fields: &[(&str, String)]) -> io::Result<()> {
    let width = fields.iter().map(|(k, _)| k.len()).max().unwrap_or(0);
    for (k, v) in fields {
        writeln!(out, "{k:<width$}  {v}")?;
    }
    Ok(())
}

/// Bold section heading (colors stripped automatically when disabled).
pub fn heading(out: &mut dyn Write, title: &str) -> io::Result<()> {
    let style = anstyle::Style::new().bold();
    writeln!(out, "{style}{title}{style:#}")
}

fn state_color(word: &str) -> Option<anstyle::AnsiColor> {
    match word
        .to_ascii_lowercase()
        .split_whitespace()
        .next()
        .unwrap_or_default()
    {
        "ready" | "online" | "idle" | "registered" | "approved" | "ok" | "running" | "current"
        | "active" | "added" => Some(anstyle::AnsiColor::Green),
        "degraded" | "draining" | "launching" | "pending" | "queued" | "executing" | "busy"
        | "cordoned" | "warning" | "stopped" | "previous" | "changed" => {
            Some(anstyle::AnsiColor::Yellow)
        }
        "down" | "failed" | "offline" | "critical" | "revoked" | "denied" | "error" | "removed" => {
            Some(anstyle::AnsiColor::Red)
        }
        _ => None,
    }
}

/// Colors a state word for free text (not table cells; use [`state_cell`]): green
/// for healthy, yellow for transitional, red for bad. Stripped when colors are off.
pub fn state(word: &str) -> String {
    match state_color(word) {
        Some(c) => {
            let style = anstyle::Style::new().fg_color(Some(c.into()));
            format!("{style}{word}{style:#}")
        }
        None => word.to_string(),
    }
}

/// Formats micro-dollars as `$1,234.56`.
pub fn usd(micros: i64) -> String {
    let negative = micros < 0;
    let cents = (micros.unsigned_abs() + 5_000) / 10_000;
    let (dollars, cents) = (cents / 100, cents % 100);
    let digits = dollars.to_string();
    let mut grouped = String::new();
    for (i, c) in digits.chars().enumerate() {
        if i > 0 && (digits.len() - i) % 3 == 0 {
            grouped.push(',');
        }
        grouped.push(c);
    }
    format!("{}${grouped}.{cents:02}", if negative { "-" } else { "" })
}

/// `-` for empty strings in tables.
pub fn dash(s: &str) -> String {
    if s.is_empty() {
        "-".into()
    } else {
        s.to_string()
    }
}

/// Renders an error to stderr (JSON object in JSON mode).
pub fn error(format: OutputFormat, code: crate::exit::ExitCode, err: &anyhow::Error) {
    let mut stderr = anstream::stderr().lock();
    match format {
        OutputFormat::Json => {
            let doc = serde_json::json!({
                "schema": "error.v1",
                "error": {
                    "code": code.name(),
                    "exit_code": code.code(),
                    "message": format!("{err:#}"),
                }
            });
            let _ = writeln!(stderr, "{doc}");
        }
        OutputFormat::Table => {
            let style = anstyle::Style::new()
                .bold()
                .fg_color(Some(anstyle::AnsiColor::Red.into()));
            let _ = writeln!(stderr, "{style}error:{style:#} {err:#}");
        }
    }
}
