// SPDX-License-Identifier: FSL-1.1-ALv2

//! The action inspector's text: the same [`ActionView`] that `cucinactl action
//! inspect` prints (command, environment, platform, input tree, outputs, exit code,
//! stdout/stderr, timing, worker), laid out one item per line so nothing needs
//! wrapping and long commands stay readable.

use ratatui::text::{Line, Span};

use super::format;
use super::theme::Theme;
use crate::inspect::ActionView;

fn field(theme: &Theme, name: &str, value: String) -> Line<'static> {
    Line::from(vec![
        Span::styled(format!("{name:<11} "), theme.bold()),
        Span::raw(value),
    ])
}

fn heading(theme: &Theme, text: String) -> Line<'static> {
    Line::from(Span::styled(text, theme.header()))
}

fn indented(text: impl Into<String>) -> Line<'static> {
    Line::from(format!("  {}", text.into()))
}

fn push_text(out: &mut Vec<Line<'static>>, text: &str) {
    for l in text.lines() {
        out.push(indented(l.replace('\t', "    ")));
    }
}

/// The inspector's lines for `v`.
pub fn lines(v: &ActionView, theme: &Theme) -> Vec<Line<'static>> {
    let mut out = vec![
        field(theme, "Action", v.action_digest.clone()),
        field(theme, "Instance", format::dash(&v.instance_name)),
    ];
    if let Some(op) = &v.operation {
        out.push(Line::from(vec![
            Span::styled(format!("{:<11} ", "Operation"), theme.bold()),
            Span::raw(format!("{} ", op.name)),
            Span::styled(format!("({})", op.stage), theme.state(&op.stage)),
        ]));
        if !op.invocation_id.is_empty() {
            out.push(field(theme, "Invocation", op.invocation_id.clone()));
        }
        if !op.target_id.is_empty() {
            out.push(field(theme, "Target", op.target_id.clone()));
        }
        if !op.worker_node.is_empty() {
            out.push(field(
                theme,
                "Worker",
                format!("{} thread {}", op.worker_node, op.worker_thread),
            ));
        }
    }
    let platform: Vec<String> = v.platform.iter().map(|(k, v)| format!("{k}={v}")).collect();
    out.push(field(theme, "Platform", format::dash(&platform.join(", "))));
    if let Some(t) = v.timeout_seconds {
        out.push(field(theme, "Timeout", format::secs(Some(t))));
    }
    if v.do_not_cache {
        out.push(field(theme, "Caching", "do_not_cache".into()));
    }

    out.push(Line::default());
    let wd = if v.command.working_directory.is_empty() {
        String::new()
    } else {
        format!(" (in {})", v.command.working_directory)
    };
    out.push(heading(
        theme,
        format!("Command: {} arguments{wd}", v.command.arguments.len()),
    ));
    for a in &v.command.arguments {
        out.push(indented(a.clone()));
    }
    out.push(heading(
        theme,
        format!("Environment: {} variables", v.command.environment.len()),
    ));
    for (k, val) in &v.command.environment {
        out.push(indented(format!("{k}={val}")));
    }
    out.push(heading(theme, "Outputs requested".into()));
    for p in &v.command.output_paths {
        out.push(indented(p.clone()));
    }

    out.push(Line::default());
    let t = &v.input_root;
    out.push(heading(
        theme,
        format!(
            "Input root: {} files, {} directories, {} symlinks, {}{}",
            t.files,
            t.directories,
            t.symlinks,
            format::bytes(t.total_file_bytes),
            if t.truncated {
                " (listing truncated)"
            } else {
                ""
            }
        ),
    ));
    out.push(indented(t.root_digest.clone()));
    for e in &t.entries {
        let detail = match e.kind {
            "file" => format!(
                "{}{}",
                format::bytes(e.size_bytes),
                if e.executable { ", executable" } else { "" }
            ),
            "symlink" => format!("-> {}", e.target.as_deref().unwrap_or_default()),
            _ => "dir".into(),
        };
        out.push(indented(format!("{}  ({detail})", e.path)));
    }

    out.push(Line::default());
    match &v.result {
        None => out.push(heading(theme, "Result: none".into())),
        Some(r) => {
            let word = if r.exit_code == 0 { "ok" } else { "failed" };
            out.push(Line::from(vec![
                Span::styled(
                    format!("Result: exit code {} ", r.exit_code),
                    theme.header(),
                ),
                Span::styled(format!("({word})"), theme.state(word)),
                Span::raw(format!(", from {}", r.source)),
            ]));
            if !r.worker.is_empty() {
                out.push(indented(format!("worker {}", r.worker)));
            }
            let tm = &r.timing;
            let parts: Vec<String> = [
                ("queued", tm.queue_seconds),
                ("input fetch", tm.input_fetch_seconds),
                ("execution", tm.execution_seconds),
                ("output upload", tm.output_upload_seconds),
                ("total", tm.total_seconds),
            ]
            .iter()
            .filter_map(|(n, s)| s.map(|s| format!("{n} {}", format::secs(Some(s)))))
            .collect();
            if !parts.is_empty() {
                out.push(indented(format!("timing: {}", parts.join(" · "))));
            }
            for f in &r.output_files {
                out.push(indented(format!(
                    "output {}  {}{}",
                    f.path,
                    format::bytes(f.size_bytes),
                    if f.executable { ", executable" } else { "" }
                )));
            }
            for d in &r.output_directories {
                out.push(indented(format!("output dir {}", d.path)));
            }
            for l in &r.output_symlinks {
                out.push(indented(format!("output link {} -> {}", l.path, l.target)));
            }
            for (name, s) in [("stdout", &r.stdout), ("stderr", &r.stderr)] {
                if let Some(s) = s {
                    out.push(heading(
                        theme,
                        format!(
                            "{name} ({}{})",
                            format::bytes(s.size_bytes),
                            if s.truncated { ", truncated" } else { "" }
                        ),
                    ));
                    push_text(&mut out, &s.text);
                }
            }
        }
    }
    if let Some(m) = &v.message {
        out.push(Line::from(vec![
            Span::styled("note: ", theme.warn()),
            Span::raw(m.clone()),
        ]));
    }
    out
}
