// SPDX-License-Identifier: FSL-1.1-ALv2

//! Rendering: [`render`] draws the [`State`] and nothing else (no clock, no I/O), so
//! the same state always produces the same screen (`insta` + `TestBackend` snapshots).

use cucina_api::proto::cucina::v1 as pb;
use ratatui::Frame;
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::Style;
use ratatui::text::{Line, Span, Text};
use ratatui::widgets::{Block, Borders, Cell, Clear, Paragraph, Row, Sparkline, Table};

use super::format;
use super::layout::{self, NO_LABEL, YES_LABEL};
use super::model::{Confirm, Liveness, Modal, Nav, PoolFocus, State, Tab};
use super::rows;
use super::theme::{Theme, Tone};

/// Draws the whole screen.
pub fn render(s: &State, f: &mut Frame) {
    let area = f.area();
    if layout::too_small(area) {
        too_small(s, f, area);
        if s.theme.ascii {
            super::theme::asciify(f.buffer_mut());
        }
        return;
    }
    let sc = layout::screen(area);
    tabs(s, f, sc.tabs);
    match s.tab {
        Tab::Overview => overview(s, f, sc.body),
        Tab::Pools => pools(s, f, sc.body),
        Tab::Hosts => hosts(s, f, sc.body),
        Tab::Operations => match &s.ops.inspector {
            Some(_) => inspector(s, f, sc.body),
            None => operations(s, f, sc.body),
        },
        Tab::Cost => cost(s, f, sc.body),
        Tab::Keys => keys(s, f, sc.body),
    }
    status_line(s, f, sc.status);
    footer(s, f, sc.footer);
    match &s.modal {
        Some(Modal::Help) => help(s, f, area),
        Some(Modal::Confirm(c)) => confirm(s, f, area, c),
        None => {}
    }
    if s.theme.ascii {
        super::theme::asciify(f.buffer_mut());
    }
}

// ------------------------------------------------------------------ chrome

fn too_small(s: &State, f: &mut Frame, area: Rect) {
    let t = &s.theme;
    let text = Text::from(vec![
        Line::styled("Terminal too small", t.bold()),
        Line::from(format!(
            "{}×{} — cucinactl tui needs at least {}×{}.",
            area.width,
            area.height,
            super::model::MIN_WIDTH,
            super::model::MIN_HEIGHT
        )),
        Line::from("Resize the window, or press q to quit."),
    ]);
    let h = 3.min(area.height);
    let y = area.y + area.height.saturating_sub(h) / 2;
    f.render_widget(
        Paragraph::new(text)
            .centered()
            .wrap(ratatui::widgets::Wrap { trim: true }),
        Rect::new(area.x, y, area.width, area.height - (y - area.y)),
    );
}

fn badge(s: &State, room: u16) -> Line<'static> {
    let t = &s.theme;
    let (word, style, detail) = match s.overview_stream.liveness(s.now_ms) {
        Liveness::Connecting => ("◌ CONNECTING", t.warn(), String::new()),
        Liveness::Live => ("● LIVE", t.ok(), String::new()),
        Liveness::Stale { age_ms } => {
            let mut d = age_ms
                .map(|a| format!(" {}", format::compact_secs(a / 1000)))
                .unwrap_or_default();
            if let Some(r) = &s.overview_stream.reconnect {
                let secs = ((r.retry_at_ms - s.now_ms).max(0) + 999) / 1000;
                d.push_str(&format!(" · retry #{} in {secs}s", r.attempt));
            }
            ("◐ STALE", t.warn(), d)
        }
        Liveness::Failed(_) => ("✖ DISCONNECTED", t.bad(), " (r retries)".into()),
    };
    let profile = format!(" │ {}", s.profile);
    let full = word.chars().count() + detail.chars().count() + profile.chars().count();
    let mut spans = vec![Span::styled(word, style)];
    if full <= usize::from(room) {
        spans.push(Span::styled(detail, style));
        spans.push(Span::styled(profile, t.dim()));
    } else if word.chars().count() + detail.chars().count() <= usize::from(room) {
        spans.push(Span::styled(detail, style));
    }
    Line::from(spans)
}

fn tabs(s: &State, f: &mut Frame, area: Rect) {
    let t = &s.theme;
    let labels = layout::tab_labels();
    let mut spans = Vec::new();
    for (tab, text, ..) in &labels {
        spans.push(Span::styled(text.clone(), t.tab(*tab == s.tab)));
        spans.push(Span::raw(" "));
    }
    let used = labels.last().map_or(0, |l| l.3) + 1;
    f.render_widget(Paragraph::new(Line::from(spans)), area);
    let room = area.width.saturating_sub(used + 1);
    let b = badge(s, room);
    let w = (b.width() as u16).min(room);
    if w > 0 {
        let r = Rect::new(area.x + area.width - w - 1, area.y, w, 1);
        f.render_widget(Paragraph::new(b), r);
    }
}

fn status_line(s: &State, f: &mut Frame, area: Rect) {
    let t = &s.theme;
    if let Some(p) = &s.prompt {
        let label = "filter> ";
        let line = Line::from(vec![
            Span::styled(label, t.key()),
            Span::raw(p.input.clone()),
        ]);
        f.render_widget(Paragraph::new(line), area);
        let x = area.x + (label.len() + p.cursor) as u16;
        f.set_cursor_position((x.min(area.right().saturating_sub(1)), area.y));
        return;
    }
    let line = if let Some(st) = &s.status {
        Line::styled(st.text.clone(), t.tone(st.tone))
    } else if let Some(e) = &s.auth_error {
        Line::styled(format!("session renewal failing: {e}"), t.bad())
    } else {
        let filter = s.filter_text(s.tab);
        if filter.is_empty() {
            Line::default()
        } else {
            Line::from(vec![
                Span::styled("filter: ", t.dim()),
                Span::raw(filter.to_string()),
                Span::styled("  (/ edits, Esc clears)", t.dim()),
            ])
        }
    };
    f.render_widget(Paragraph::new(line), area);
}

fn hints(s: &State) -> Vec<(&'static str, &'static str)> {
    if let Some(Modal::Confirm(_)) = &s.modal {
        return vec![
            ("y", "yes"),
            ("n/Esc", "no"),
            ("←→", "choose"),
            ("Enter", "answer"),
        ];
    }
    if s.prompt.is_some() {
        return vec![
            ("Enter", "apply"),
            ("Esc", "cancel"),
            ("field:value", "match a column"),
            ("Ctrl-U", "clear"),
        ];
    }
    let mut h = match s.tab {
        Tab::Overview => vec![("Enter", "queue's ops"), ("↑↓", "select"), ("/", "filter")],
        Tab::Pools if s.pools.focus == PoolFocus::Workers => vec![
            ("d", "drain"),
            ("u", "undrain"),
            ("↑↓", "worker"),
            ("Esc", "pools"),
        ],
        Tab::Pools => vec![("Enter", "workers"), ("↑↓", "pool"), ("/", "filter")],
        Tab::Hosts => vec![
            ("d", "drain"),
            ("u", "uncordon"),
            ("R", "re-image"),
            ("↑↓", "select"),
            ("/", "filter"),
        ],
        Tab::Operations if s.ops.inspector.is_some() => {
            vec![("↑↓ PgUp PgDn", "scroll"), ("Esc", "back")]
        }
        Tab::Operations => vec![
            ("Enter", "inspect"),
            ("x", "kill"),
            ("↑↓", "select"),
            ("/", "filter"),
        ],
        Tab::Cost => vec![("↑↓", "pool"), ("/", "filter")],
        Tab::Keys => vec![("x", "revoke"), ("↑↓", "select"), ("/", "filter")],
    };
    h.extend([("Tab", "view"), ("?", "help"), ("q", "quit")]);
    h
}

fn footer(s: &State, f: &mut Frame, area: Rect) {
    let t = &s.theme;
    let mut spans = Vec::new();
    let mut used = 0usize;
    for (k, d) in hints(s) {
        let w = k.chars().count() + d.chars().count() + 3;
        if used + w > usize::from(area.width) {
            break;
        }
        spans.push(Span::styled(format!(" {k}"), t.key()));
        spans.push(Span::raw(format!(" {d} ")));
        used += w;
    }
    f.render_widget(Paragraph::new(Line::from(spans)), area);
}

// ----------------------------------------------------------------- tables

fn block(t: &Theme, title: Line<'static>, focused: bool) -> Block<'static> {
    Block::new()
        .borders(Borders::ALL)
        .border_style(t.border(focused))
        .title(title)
}

fn title(t: &Theme, text: impl Into<String>) -> Line<'static> {
    Line::from(Span::styled(format!(" {} ", text.into()), t.bold()))
}

/// The first visible row for `nav` in `visible` rows (the view's own copy of the
/// reducer's scrolling, so a size change never hides the selection).
fn window(nav: &Nav, visible: usize, len: usize) -> usize {
    let visible = visible.max(1);
    let mut offset = nav.offset;
    if nav.index < offset {
        offset = nav.index;
    } else if nav.index >= offset + visible {
        offset = nav.index + 1 - visible;
    }
    offset.min(len.saturating_sub(visible))
}

struct TableSpec<'a> {
    title: Line<'static>,
    header: &'a [&'a str],
    widths: &'a [Constraint],
    rows: Vec<Row<'static>>,
    /// Selection (and whether this table has the keyboard).
    nav: Option<(&'a Nav, bool)>,
    empty: String,
}

fn table(s: &State, f: &mut Frame, area: Rect, spec: TableSpec<'_>) {
    let t = &s.theme;
    let visible = layout::visible_rows(area);
    let len = spec.rows.len();
    let focused = spec.nav.is_some_and(|(_, focus)| focus);
    let mut b = block(t, spec.title, focused);
    let (offset, selected) = match spec.nav {
        Some((nav, focus)) if len > 0 => (window(nav, visible, len), Some((nav.index, focus))),
        _ => (0, None),
    };
    if len > visible {
        b = b.title_top(
            Line::from(format!(
                " {}-{} of {len} ",
                offset + 1,
                (offset + visible).min(len)
            ))
            .right_aligned(),
        );
    }
    let rows: Vec<Row<'static>> = spec
        .rows
        .into_iter()
        .enumerate()
        .skip(offset)
        .take(visible)
        .map(|(i, r)| match selected {
            Some((sel, focus)) if sel == i => r.style(t.selected(focus)),
            _ => r,
        })
        .collect();
    let header = Row::new(spec.header.iter().map(|h| Cell::from(*h))).style(t.header());
    let widget = Table::new(rows, spec.widths.to_vec())
        .header(header)
        .column_spacing(1)
        .block(b);
    f.render_widget(widget, area);
    if len == 0 && area.height > 3 && area.width > 2 {
        let r = Rect::new(area.x + 1, area.y + 2, area.width - 2, 1);
        f.render_widget(Paragraph::new(Span::styled(spec.empty, t.dim())), r);
    }
}

fn state_cell(t: &Theme, word: &str) -> Cell<'static> {
    Cell::from(Span::styled(format::dash(word), t.state(word)))
}

fn right(text: impl Into<String>) -> Cell<'static> {
    Cell::from(Line::from(text.into()).right_aligned())
}

/// A fetch error or "loading…" placeholder for an empty table.
fn empty_text<T>(fetch: &super::model::Fetch<T>, none: &str) -> String {
    match (&fetch.error, fetch.data.is_some(), fetch.in_flight) {
        (Some(e), _, _) => format!("error: {e}"),
        (None, false, _) => "loading…".into(),
        _ => none.into(),
    }
}

// ---------------------------------------------------------------- overview

fn overview(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let o = s.overview.latest.as_ref();
    let alerts: Vec<&pb::Alert> = o.map(|o| o.alerts.iter().collect()).unwrap_or_default();
    let a = layout::overview(body, alerts.len());

    // Queues.
    let queues = rows::queues(s);
    let multi_instance = {
        let mut names: Vec<&str> = queues
            .iter()
            .filter_map(|q| q.queue.as_option())
            .map(|r| r.instance_name_prefix.as_str())
            .collect();
        names.sort_unstable();
        names.dedup();
        names.len() > 1
    };
    let qrows = queues
        .iter()
        .map(|q| {
            let r = q.queue.as_option();
            let mut platform = format::platform(r);
            if multi_instance && let Some(r) = r {
                platform = format!("{}:{platform}", r.instance_name_prefix);
            }
            let queued_style = if q.queued > 0 && q.idle_workers == 0 && q.total_workers == 0 {
                t.warn()
            } else {
                Style::new()
            };
            Row::new(vec![
                Cell::from(platform),
                right(r.map_or(0, |r| r.size_class).to_string()),
                Cell::from(Line::styled(q.queued.to_string(), queued_style).right_aligned()),
                right(q.executing.to_string()),
                right(format::dur(q.oldest_queued_age.as_option())),
                right(format::dur(q.queue_time_p95.as_option())),
                Cell::from(format::dash(&q.pool)),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.queues,
        TableSpec {
            title: title(t, "Queues"),
            header: &["PLATFORM", "SC", "QUEUED", "EXEC", "OLDEST", "P95", "POOL"],
            widths: &[
                Constraint::Fill(2),
                Constraint::Length(2),
                Constraint::Length(6),
                Constraint::Length(5),
                Constraint::Length(6),
                Constraint::Length(5),
                Constraint::Fill(1),
            ],
            rows: qrows,
            nav: Some((&s.overview.nav, true)),
            empty: if o.is_some() {
                "no queues".into()
            } else {
                "waiting for the controller…".into()
            },
        },
    );

    // Workers by state.
    let by = |k: &str| {
        o.and_then(|o| o.workers_by_state.get(k).copied())
            .unwrap_or(0)
    };
    let total: u32 = o.map_or(0, |o| o.workers_by_state.values().sum());
    let grid = [
        ("launching", "idle"),
        ("registered", "draining"),
        ("busy", "stopped"),
        ("failed", ""),
    ];
    let lines: Vec<Line> = grid
        .iter()
        .map(|(l, r)| {
            let right_cell = if r.is_empty() {
                Span::styled(format!("{:<8}{:>3}", "total", total), t.bold())
            } else {
                Span::styled(format!("{:<8}{:>3}", r, by(r)), count_style(t, r, by(r)))
            };
            Line::from(vec![
                Span::styled(format!("{:<10}{:>3}", l, by(l)), count_style(t, l, by(l))),
                Span::raw(" "),
                right_cell,
            ])
        })
        .collect();
    f.render_widget(
        Paragraph::new(lines).block(block(t, title(t, "Workers"), false)),
        a.workers,
    );

    // Pools: desired vs actual.
    let prows = rows::pools(s)
        .into_iter()
        .map(|p| {
            let running = p.registered + p.busy + p.idle + p.draining;
            let actual = if p.launching > 0 {
                format!("{running}+{}", p.launching)
            } else {
                running.to_string()
            };
            let cond = if p.paused {
                "cordoned"
            } else {
                p.condition.as_str()
            };
            let actual_style = if running + p.launching < p.desired {
                t.warn()
            } else {
                Style::new()
            };
            Row::new(vec![
                Cell::from(p.name.clone()),
                right(p.desired.to_string()),
                Cell::from(Line::styled(actual, actual_style).right_aligned()),
                right(p.max.to_string()),
                state_cell(t, cond),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.pools,
        TableSpec {
            title: title(t, "Pools: desired / actual"),
            header: &["POOL", "DESIRED", "ACTUAL", "MAX", "CONDITION"],
            widths: &[
                Constraint::Fill(2),
                Constraint::Length(7),
                Constraint::Length(6),
                Constraint::Length(4),
                Constraint::Fill(1),
            ],
            rows: prows,
            nav: None,
            empty: "no pools".into(),
        },
    );

    // Spend and hosts.
    let cost = o.and_then(|o| o.cost.as_option());
    let hosts = o.map(|o| o.hosts.as_slice()).unwrap_or_default();
    let online = hosts
        .iter()
        .filter(|h| h.phase.eq_ignore_ascii_case("online"))
        .count();
    let kv = |k: &str, v: String| Line::from(format!("{k:<13}{v:>12}"));
    let spend = vec![
        kv(
            "today",
            format::money(cost.and_then(|c| c.today.as_option())),
        ),
        kv(
            "month",
            format::money(cost.and_then(|c| c.month_to_date.as_option())),
        ),
        kv(
            "idle/month",
            format::money(cost.and_then(|c| c.standing_per_month.as_option())),
        ),
        Line::from(vec![
            Span::raw(format!("{:<13}", "hosts online")),
            Span::styled(
                format!("{:>12}", format!("{online}/{}", hosts.len())),
                if online < hosts.len() {
                    t.warn()
                } else {
                    Style::new()
                },
            ),
        ]),
    ];
    f.render_widget(
        Paragraph::new(spend).block(block(t, title(t, "Spend"), false)),
        a.spend,
    );

    // Activity sparklines.
    let b = block(t, title(t, "Activity: queued / executing"), false);
    let inner = b.inner(a.activity);
    f.render_widget(b, a.activity);
    let [q_row, e_row] =
        Layout::vertical([Constraint::Length(1), Constraint::Length(1)]).areas(inner);
    let last = s.overview.history.back().copied().unwrap_or_default();
    for (row, label, value, data, style) in [
        (
            q_row,
            "queued",
            last.queued,
            s.overview
                .history
                .iter()
                .map(|x| x.queued)
                .collect::<Vec<_>>(),
            t.warn(),
        ),
        (
            e_row,
            "executing",
            last.executing,
            s.overview
                .history
                .iter()
                .map(|x| x.executing)
                .collect::<Vec<_>>(),
            t.ok(),
        ),
    ] {
        let [l, r] = Layout::horizontal([Constraint::Length(16), Constraint::Fill(1)]).areas(row);
        f.render_widget(Paragraph::new(format!("{label:<10}{value:>5} ")), l);
        let skip = data.len().saturating_sub(usize::from(r.width));
        f.render_widget(Sparkline::default().data(&data[skip..]).style(style), r);
    }

    // Alerts.
    let mut alerts = alerts;
    alerts.sort_by_key(|a| {
        (
            match a.severity.as_str() {
                "critical" => 0,
                "warning" => 1,
                _ => 2,
            },
            format::ts_ms(a.since.as_option()).unwrap_or(i64::MAX),
        )
    });
    let n = alerts.len();
    let lines: Vec<Line> = if alerts.is_empty() {
        vec![Line::styled("no alerts", t.ok())]
    } else {
        alerts
            .iter()
            .map(|a| {
                let mut labels: Vec<String> =
                    a.labels.iter().map(|(k, v)| format!("{k}={v}")).collect();
                labels.sort();
                Line::from(vec![
                    Span::styled(format!("{:<9}", a.severity), t.state(&a.severity)),
                    Span::styled(format!("{} ", a.name), t.bold()),
                    Span::raw(a.summary.clone()),
                    Span::styled(
                        format!(
                            "  {} · {}",
                            labels.join(" "),
                            format::age(s.now_ms, a.since.as_option())
                        ),
                        t.dim(),
                    ),
                ])
            })
            .collect()
    };
    f.render_widget(
        Paragraph::new(lines).block(block(t, title(t, format!("Alerts ({n})")), false)),
        a.alerts,
    );
}

fn count_style(t: &Theme, state: &str, n: u32) -> Style {
    if n == 0 {
        return t.dim();
    }
    match state {
        "failed" => t.bad(),
        "launching" | "draining" => t.warn(),
        "busy" | "idle" | "registered" => t.ok(),
        _ => Style::new(),
    }
}

// ------------------------------------------------------------------- pools

fn pools(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let list = rows::pools(s);
    let a = layout::pools(body, list.len());
    let focus_pools = s.pools.focus == PoolFocus::Pools;
    let prows = list
        .iter()
        .map(|p| {
            let cond = if p.paused {
                "cordoned"
            } else {
                p.condition.as_str()
            };
            Row::new(vec![
                Cell::from(p.name.clone()),
                Cell::from(p.provider.clone()),
                right(format!("{}/{}", p.desired, p.max)),
                right(p.launching.to_string()),
                right((p.registered + p.idle).to_string()),
                right(p.busy.to_string()),
                right(p.draining.to_string()),
                Cell::from(format::dash(&p.image_generation)),
                state_cell(t, cond),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.pools,
        TableSpec {
            title: title(t, format!("Pools ({})", list.len())),
            header: &[
                "POOL",
                "PROVIDER",
                "DES/MAX",
                "BOOT",
                "READY",
                "BUSY",
                "DRAIN",
                "IMAGE",
                "CONDITION",
            ],
            widths: &[
                Constraint::Fill(2),
                Constraint::Length(8),
                Constraint::Length(7),
                Constraint::Length(4),
                Constraint::Length(5),
                Constraint::Length(4),
                Constraint::Length(5),
                Constraint::Length(5),
                Constraint::Fill(1),
            ],
            rows: prows,
            nav: Some((&s.pools.nav, focus_pools)),
            empty: if s.overview.latest.is_some() {
                "no pools".into()
            } else {
                "waiting for the controller…".into()
            },
        },
    );

    let pool = rows::selected_pool(s);
    let name = pool.map(|p| p.name.clone()).unwrap_or_default();
    let detail = s.pools.detail.data_for(&name);
    let workers = rows::workers(s);
    let wrows = workers
        .iter()
        .map(|w| {
            let state = if w.drained {
                "draining"
            } else {
                w.state.as_str()
            };
            Row::new(vec![
                Cell::from(w.node.clone()),
                state_cell(t, state),
                right(format!("{}/{}", w.busy_threads, w.threads)),
                Cell::from(format::dash(&w.instance_type)),
                Cell::from(format::dash(&w.generation)),
                right(match format::dur_secs(w.idle_for.as_option()) {
                    Some(x) if x > 0.0 => format::secs(Some(x)),
                    _ => "-".into(),
                }),
                right(format::age(s.now_ms, w.launched.as_option())),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.workers,
        TableSpec {
            title: title(
                t,
                format!("Workers · {} ({})", format::dash(&name), workers.len()),
            ),
            header: &["NODE", "STATE", "BUSY", "TYPE", "GEN", "IDLE", "UP"],
            widths: &[
                Constraint::Fill(2),
                Constraint::Length(9),
                Constraint::Length(7),
                Constraint::Fill(1),
                Constraint::Length(4),
                Constraint::Length(6),
                Constraint::Length(6),
            ],
            rows: wrows,
            nav: Some((&s.pools.workers, !focus_pools)),
            empty: if pool.is_none() {
                "select a pool".into()
            } else {
                empty_text(&s.pools.detail, "no workers (scaled to zero)")
            },
        },
    );

    // Scale timeline (most recent first).
    let events: Vec<Line> = detail
        .map(|d| {
            d.events
                .iter()
                .map(|e| {
                    let tone = match e.r#type.as_str() {
                        "fail" | "ice" => Tone::Bad,
                        "drain" | "terminate" | "rollout" => Tone::Busy,
                        "launch" | "register" => Tone::Good,
                        _ => Tone::Neutral,
                    };
                    Line::from(vec![
                        Span::styled(
                            format!("{:>6} ", format::age(s.now_ms, e.time.as_option())),
                            t.dim(),
                        ),
                        Span::styled(format!("{:<9} ", e.r#type), t.tone(tone)),
                        Span::raw(format!("{} {}", e.subject, e.message)),
                    ])
                })
                .collect()
        })
        .unwrap_or_default();
    let events = if events.is_empty() {
        vec![Line::styled("no scale events", t.dim())]
    } else {
        events
    };
    f.render_widget(
        Paragraph::new(events).block(block(t, title(t, "Scale timeline"), false)),
        a.timeline,
    );

    // Cold starts: API call -> running -> registered -> first action.
    let starts: Vec<&pb::StartLatency> = match detail {
        Some(d) if !d.starts.is_empty() => d.starts.iter().collect(),
        _ => s
            .overview
            .latest
            .iter()
            .flat_map(|o| o.recent_starts.iter())
            .filter(|x| x.pool == name)
            .collect(),
    };
    let mut firsts: Vec<f64> = starts
        .iter()
        .filter_map(|x| format::dur_secs(x.to_first_action.as_option()))
        .collect();
    firsts.sort_by(f64::total_cmp);
    let summary = match (firsts.get(firsts.len() / 2), firsts.last()) {
        (Some(p50), Some(max)) => format!(
            "Cold starts · p50 {} · max {}",
            format::secs(Some(*p50)),
            format::secs(Some(*max))
        ),
        _ => "Cold starts".into(),
    };
    let srows = starts
        .iter()
        .map(|x| {
            Row::new(vec![
                Cell::from(x.vm.clone()),
                Cell::from(format::dash(&x.path)),
                right(format::dur(x.to_running.as_option())),
                right(format::dur(x.to_registered.as_option())),
                right(format::dur(x.to_first_action.as_option())),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.starts,
        TableSpec {
            title: title(t, summary),
            header: &["VM", "PATH", "RUN", "REG", "1ST"],
            widths: &[
                Constraint::Fill(1),
                Constraint::Length(10),
                Constraint::Length(5),
                Constraint::Length(5),
                Constraint::Length(5),
            ],
            rows: srows,
            nav: None,
            empty: "no recent starts".into(),
        },
    );
}

// ------------------------------------------------------------------- hosts

/// A host's summary from the live overview (2 s fresh) when it has one, else from
/// the last `ListHosts`.
fn live_summary<'a>(s: &'a State, h: &'a pb::HostDetail) -> Option<&'a pb::HostSummary> {
    let own = h.summary.as_option()?;
    s.overview
        .latest
        .iter()
        .flat_map(|o| o.hosts.iter())
        .find(|x| x.serial == own.serial)
        .or(Some(own))
}

fn wan_rate(s: &State, serial: &str) -> Option<f64> {
    let [(t0, b0), (t1, b1)] = *s.overview.wan.get(serial)?;
    (t1 > t0).then(|| (b1 - b0).max(0) as f64 * 1000.0 / (t1 - t0) as f64)
}

fn hosts(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let list = rows::hosts(s);
    let a = layout::hosts(body, list.len());
    let hrows = list
        .iter()
        .map(|h| {
            let sum = live_summary(s, h);
            let get = |pick: fn(&pb::HostSummary) -> String| sum.map(pick).unwrap_or_default();
            let serial = get(|x| x.serial.clone());
            let phase = get(|x| x.phase.clone());
            let wan = sum.map_or(0, |x| x.wan_bytes_received.max(0) as u64);
            let wan_text = match wan_rate(s, &serial) {
                Some(r) => format!("{} · {}", format::bytes(wan), format::rate(r)),
                None => format::bytes(wan),
            };
            Row::new(vec![
                Cell::from(rows::host_label(h)),
                Cell::from(format::dash(&get(|x| x.site.clone()))),
                state_cell(t, &phase),
                if h.cordoned {
                    state_cell(t, "cordoned")
                } else {
                    Cell::from("-")
                },
                right(sum.map_or("-".into(), |x| format!("{}/{}", x.running_vms, x.slots))),
                right(format::ratio(&get(|x| x.l2_hit_ratio.clone()))),
                Cell::from(wan_text),
                right(format::age(
                    s.now_ms,
                    sum.and_then(|x| x.last_heartbeat.as_option()),
                )),
                Cell::from(format::dash(&get(|x| x.agent_version.clone()))),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.hosts,
        TableSpec {
            title: title(t, format!("Mac hosts ({})", list.len())),
            header: &[
                "HOST", "SITE", "PHASE", "CORDON", "VMS", "L2 HIT", "WAN RX", "BEAT", "AGENT",
            ],
            widths: &[
                Constraint::Fill(1),
                Constraint::Length(8),
                Constraint::Length(8),
                Constraint::Length(8),
                Constraint::Length(5),
                Constraint::Length(6),
                Constraint::Fill(1),
                Constraint::Length(6),
                Constraint::Length(7),
            ],
            rows: hrows,
            nav: Some((&s.hosts.nav, true)),
            empty: empty_text(&s.hosts.list, "no Mac hosts enrolled"),
        },
    );

    let Some(h) = rows::selected_host(s) else {
        f.render_widget(block(t, title(t, "Host"), false), a.detail);
        return;
    };
    let serial = rows::host_serial(h);
    let b = block(
        t,
        title(t, format!("{} · {serial}", rows::host_label(h))),
        false,
    );
    let inner = b.inner(a.detail);
    f.render_widget(b, a.detail);
    let mut facts = vec![
        format!("macOS {}", format::dash(&h.macos_version)),
        format!("Tart {}", format::dash(&h.tart_version)),
        format!(
            "{} · {} cores · {} GiB",
            format::dash(&h.chip),
            h.cores,
            h.memory_gib
        ),
        format!("{} GiB free", h.disk_free_gib),
        format!(
            "cert {}",
            format::until(s.now_ms, h.cert_expiry.as_option())
        ),
    ];
    if !h.approved {
        facts.push("not approved".into());
    }
    let mut labels: Vec<String> = h.labels.iter().map(|(k, v)| format!("{k}={v}")).collect();
    labels.sort();
    let mut images = h.images.clone();
    images.sort();
    let [facts_area, vms_area, images_area] = Layout::vertical([
        Constraint::Length(1),
        Constraint::Fill(1),
        Constraint::Length(1),
    ])
    .areas(inner);
    f.render_widget(Paragraph::new(facts.join(" · ")), facts_area);
    let vrows = h
        .vms
        .iter()
        .map(|v| {
            Row::new(vec![
                Cell::from(v.name.clone()),
                Cell::from(format::dash(&v.pool)),
                state_cell(t, &v.state),
                Cell::from(format::dash(&v.image)),
                Cell::from(format::dash(&v.generation)),
                Cell::from(if v.registered { "yes" } else { "no" }),
            ])
        })
        .collect::<Vec<_>>();
    let widths = [
        Constraint::Length(8),
        Constraint::Fill(1),
        Constraint::Length(9),
        Constraint::Fill(2),
        Constraint::Length(4),
        Constraint::Length(4),
    ];
    let header = Row::new(["VM", "POOL", "STATE", "IMAGE", "GEN", "REG"]).style(t.header());
    if vrows.is_empty() {
        f.render_widget(Paragraph::new(Line::styled("no VMs", t.dim())), vms_area);
    } else {
        f.render_widget(
            Table::new(vrows, widths).header(header).column_spacing(1),
            vms_area,
        );
    }
    let mut tail = vec![
        Span::styled("images ", t.dim()),
        Span::raw(images.join(", ")),
    ];
    if !labels.is_empty() {
        tail.push(Span::styled("  labels ", t.dim()));
        tail.push(Span::raw(labels.join(" ")));
    }
    f.render_widget(Paragraph::new(Line::from(tail)), images_area);
}

// -------------------------------------------------------------- operations

fn operations(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let list = rows::ops(s);
    let total = s.ops.ops.len();
    let orows = list
        .iter()
        .map(|o| {
            let q = o.queue.as_option();
            Row::new(vec![
                state_cell(t, &o.stage),
                right(format::age(s.now_ms, o.queued_at.as_option())),
                Cell::from(format::platform(q)),
                Cell::from(format::dash(
                    q.map(|q| q.instance_name_prefix.as_str())
                        .unwrap_or_default(),
                )),
                Cell::from(format::dash(&o.target_id)),
                Cell::from(format::dash(&o.invocation_id)),
                Cell::from(if o.worker_node.is_empty() {
                    "-".to_string()
                } else {
                    format!("{}#{}", o.worker_node, o.worker_thread)
                }),
            ])
        })
        .collect();
    let mut head = if list.len() == total {
        format!("Operations ({total})")
    } else {
        format!("Operations ({} of {total})", list.len())
    };
    match s.ops.stream.liveness(s.now_ms) {
        Liveness::Live | Liveness::Connecting => {}
        Liveness::Stale { .. } => head.push_str(" · stream reconnecting"),
        Liveness::Failed(e) => head.push_str(&format!(" · stream stopped: {e}")),
    }
    table(
        s,
        f,
        body,
        TableSpec {
            title: title(t, head),
            header: &[
                "STAGE", "AGE", "PLATFORM", "INST", "TARGET", "INV", "WORKER",
            ],
            // The invocation id is a UUID: its first 8 characters identify it on
            // screen (the filter matches the whole id).
            widths: &[
                Constraint::Length(9),
                Constraint::Length(5),
                Constraint::Fill(3),
                Constraint::Length(6),
                Constraint::Fill(5),
                Constraint::Length(8),
                Constraint::Fill(3),
            ],
            rows: orows,
            nav: Some((&s.ops.nav, true)),
            empty: if total == 0 {
                "no queued or executing operations".into()
            } else {
                "no operation matches the filter".into()
            },
        },
    );
}

fn inspector(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let Some(i) = &s.ops.inspector else {
        return;
    };
    let mut b = block(t, title(t, format!("Action · {}", i.subject)), true);
    let content: Vec<Line> = match (&i.fetch.data, &i.fetch.error) {
        (Some(v), _) => super::inspector::lines(v, t),
        (None, Some(e)) => vec![Line::styled(format!("inspection failed: {e}"), t.bad())],
        (None, None) => vec![Line::styled(
            format!("reading the action from the CAS/AC ({})…", i.subject),
            t.dim(),
        )],
    };
    let height = usize::from(body.height.saturating_sub(2));
    if content.len() > height {
        b = b.title_top(
            Line::from(format!(
                " lines {}-{} of {} ",
                i.scroll + 1,
                (i.scroll + height).min(content.len()),
                content.len()
            ))
            .right_aligned(),
        );
    }
    let scroll = u16::try_from(i.scroll).unwrap_or(u16::MAX);
    f.render_widget(Paragraph::new(content).block(b).scroll((scroll, 0)), body);
}

// -------------------------------------------------------------------- cost

fn cost(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let a = layout::cost(body);
    let summary = rows::cost_summary(s);
    let m = |f: fn(&pb::CostSummary) -> Option<&pb::Money>| format::money(summary.and_then(f));
    let lines = vec![
        Line::from(format!("{:<15}{:>12}", "today", m(|c| c.today.as_option()))),
        Line::from(format!(
            "{:<15}{:>12}",
            "month to date",
            m(|c| c.month_to_date.as_option())
        )),
        Line::styled("estimated from instance-hours", t.dim()),
    ];
    f.render_widget(
        Paragraph::new(lines).block(block(t, title(t, "Spend"), false)),
        a.summary,
    );

    // Idle projection: what the deployment costs per month with every pool at zero.
    let detail = s.cost.detail.data.as_ref();
    let mut standing: Vec<(String, i64)> = Vec::new();
    for l in detail.iter().flat_map(|d| d.lines.iter()) {
        if matches!(l.category.as_str(), "ami-storage" | "fast-launch") {
            let micros = l.amount.as_option().map_or(0, |a| a.micros);
            match standing.iter_mut().find(|(c, _)| *c == l.category) {
                Some((_, v)) => *v += micros,
                None => standing.push((l.category.clone(), micros)),
            }
        }
    }
    let mut proj = vec![Line::from(vec![
        Span::raw(format!("{:<15}", "at zero scale")),
        Span::styled(
            format!("{:>12}/month", m(|c| c.standing_per_month.as_option())),
            t.bold(),
        ),
    ])];
    if standing.is_empty() {
        proj.push(Line::styled(
            "AMI storage + Windows Fast Launch snapshots",
            t.dim(),
        ));
    } else {
        proj.push(Line::styled(
            standing
                .iter()
                .map(|(c, v)| format!("{c} {}", crate::output::usd(*v)))
                .collect::<Vec<_>>()
                .join(" · "),
            t.dim(),
        ));
    }
    proj.push(Line::styled("(no EC2 instances exist when idle)", t.dim()));
    f.render_widget(
        Paragraph::new(proj).block(block(t, title(t, "Idle projection"), false)),
        a.projection,
    );

    let pools = rows::cost_pools(s);
    let crows = pools
        .iter()
        .map(|p| {
            let money = |x: &buffa::MessageField<pb::Money, buffa::Inline<pb::Money>>| {
                right(format::money(x.as_option()))
            };
            Row::new(vec![
                Cell::from(p.pool.clone()),
                right(format!("{:.1}", p.instance_seconds as f64 / 3600.0)),
                money(&p.compute),
                money(&p.ebs),
                money(&p.data_transfer),
                money(&p.public_ipv4),
                Cell::from(
                    Line::styled(crate::output::usd(rows::total(p)), t.bold()).right_aligned(),
                ),
                money(&p.standing),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.pools,
        TableSpec {
            title: title(t, "Per pool"),
            header: &[
                "POOL", "HOURS", "COMPUTE", "EBS", "XFER", "IPV4", "TOTAL", "IDLE/MO",
            ],
            widths: &[
                Constraint::Fill(1),
                Constraint::Length(6),
                Constraint::Length(9),
                Constraint::Length(8),
                Constraint::Length(8),
                Constraint::Length(7),
                Constraint::Length(9),
                Constraint::Length(8),
            ],
            rows: crows,
            nav: Some((&s.cost.nav, true)),
            empty: "no cost data yet".into(),
        },
    );

    let selected = rows::selected_cost_pool(s).map(|p| p.pool.clone());
    let lrows = rows::cost_lines(s)
        .into_iter()
        .map(|l| {
            Row::new(vec![
                Cell::from(l.category.clone()),
                Cell::from(format::dash(&l.detail)),
                right(format!("{:.2}", l.quantity)),
                Cell::from(format::dash(&l.unit)),
                right(format::money(l.amount.as_option())),
            ])
        })
        .collect();
    let mut lines_title = format!(
        "Itemised · {}",
        selected.unwrap_or_else(|| "all pools".into())
    );
    if let Some(a) = detail
        .map(|d| d.assumptions.as_str())
        .filter(|a| !a.is_empty())
    {
        lines_title.push_str(&format!(" · {a}"));
    }
    table(
        s,
        f,
        a.lines,
        TableSpec {
            title: title(t, lines_title),
            header: &["CATEGORY", "DETAIL", "QUANTITY", "UNIT", "AMOUNT"],
            widths: &[
                Constraint::Length(14),
                Constraint::Fill(2),
                Constraint::Length(9),
                Constraint::Fill(1),
                Constraint::Length(10),
            ],
            rows: lrows,
            nav: None,
            empty: empty_text(&s.cost.detail, "no itemised lines"),
        },
    );
}

// -------------------------------------------------------------------- keys

fn keys(s: &State, f: &mut Frame, body: Rect) {
    let t = &s.theme;
    let a = layout::keys(body);
    let list = rows::keys(s);
    let krows = list
        .iter()
        .map(|k| {
            Row::new(vec![
                Cell::from(k.key_id.clone()),
                Cell::from(k.account.clone()),
                Cell::from(format::dash(&k.description)),
                right(format::age(s.now_ms, k.created.as_option())),
                right(format::until(s.now_ms, k.expires_at.as_option())),
                right(format::age(s.now_ms, k.last_used.as_option())),
                state_cell(t, if k.revoked { "revoked" } else { "active" }),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.keys,
        TableSpec {
            title: title(t, format!("Service keys ({})", list.len())),
            header: &[
                "KEY",
                "ACCOUNT",
                "DESCRIPTION",
                "AGE",
                "EXPIRES",
                "USED",
                "STATUS",
            ],
            widths: &[
                Constraint::Fill(1),
                Constraint::Fill(1),
                Constraint::Fill(2),
                Constraint::Length(6),
                Constraint::Length(9),
                Constraint::Length(6),
                Constraint::Length(7),
            ],
            rows: krows,
            nav: Some((&s.keys.nav, true)),
            empty: empty_text(&s.keys.keys, "no service keys"),
        },
    );
    let revs: Vec<&pb::Revocation> = s.keys.revocations.data.iter().flatten().collect();
    let rrows = revs
        .iter()
        .map(|r| {
            let who = if r.sid.is_empty() {
                r.sub.clone()
            } else {
                format!("session {}", r.sid)
            };
            Row::new(vec![
                Cell::from(who),
                Cell::from(format::dash(&r.reason)),
                Cell::from(format::dash(&r.created_by)),
                right(format::age(s.now_ms, r.created.as_option())),
            ])
        })
        .collect();
    table(
        s,
        f,
        a.revocations,
        TableSpec {
            title: title(t, format!("Revoked principals ({})", revs.len())),
            header: &["PRINCIPAL", "REASON", "BY", "AGE"],
            widths: &[
                Constraint::Fill(2),
                Constraint::Fill(2),
                Constraint::Fill(1),
                Constraint::Length(6),
            ],
            rows: rrows,
            nav: None,
            empty: empty_text(&s.keys.revocations, "nobody is revoked"),
        },
    );
}

// ------------------------------------------------------------------ modals

/// The key map (also the help overlay).
pub const HELP: &[(&str, &str)] = &[
    ("Tab / Shift-Tab, 1-6", "switch view"),
    ("↑↓ j k, PgUp PgDn, g G", "move the selection"),
    (
        "Enter / → / l",
        "drill in: queue → ops, pool → workers, op → action",
    ),
    ("Esc / ← / h", "back; Esc also clears the filter"),
    ("/ or :", "filter as you type: words, or field:value"),
    ("d / u", "drain / undrain a worker; drain / uncordon a host"),
    ("R", "re-image a host's VMs"),
    ("x", "kill an operation; revoke a service key"),
    ("r", "refresh now (and reconnect stopped streams)"),
    ("m", "mouse capture on/off (off: select text)"),
    ("Ctrl-L", "redraw the screen"),
    ("q, Ctrl-C", "quit (q also closes the inspector)"),
];

fn help(s: &State, f: &mut Frame, area: Rect) {
    let t = &s.theme;
    let mut lines: Vec<Line> = HELP
        .iter()
        .map(|(k, d)| {
            Line::from(vec![
                Span::styled(format!(" {k:<24}"), t.key()),
                Span::raw(d.to_string()),
            ])
        })
        .collect();
    lines.push(Line::default());
    lines.push(Line::styled(
        " Drain, kill, revoke and re-image always ask first; the default is No.",
        t.dim(),
    ));
    let r = layout::help(area, lines.len());
    f.render_widget(Clear, r);
    f.render_widget(
        Paragraph::new(lines).block(block(t, title(t, "Keys (Esc closes)"), true)),
        r,
    );
}

fn confirm(s: &State, f: &mut Frame, area: Rect, c: &Confirm) {
    let t = &s.theme;
    let d = layout::dialog(area, c.body.len());
    f.render_widget(Clear, d.area);
    let b = Block::new()
        .borders(Borders::ALL)
        .border_style(t.bad())
        .title(Line::from(Span::styled(" Confirm ", t.bad())));
    let inner = b.inner(d.area);
    f.render_widget(b, d.area);
    let mut lines = vec![Line::styled(c.title.clone(), t.bold())];
    lines.extend(c.body.iter().map(|l| Line::from(l.clone())));
    f.render_widget(Paragraph::new(lines), inner);
    let button = |label: &'static str, on: bool| {
        Paragraph::new(Span::styled(
            label,
            if on { t.selected(true) } else { Style::new() },
        ))
    };
    f.render_widget(button(NO_LABEL, !c.yes), d.no);
    f.render_widget(button(YES_LABEL, c.yes), d.yes);
}
