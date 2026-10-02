// SPDX-License-Identifier: FSL-1.1-ALv2

//! The reducer: [`update`] applies one [`Event`] to the [`State`] and returns the
//! [`Effect`]s to run (API calls, stream restarts, terminal tweaks). It does no I/O
//! and reads no clock (time arrives as `Tick` events), so it is deterministic.
//!
//! Safety rule (R-CLI-4): a destructive request (drain, kill, revoke, re-image) is
//! only ever emitted from [`confirm`], i.e. after the confirmation dialog was
//! answered "yes" (default "No").

use cucina_api::proto::cucina::v1 as pb;
use ratatui::crossterm::event::{
    KeyCode, KeyEvent, KeyEventKind, KeyModifiers, MouseButton, MouseEvent, MouseEventKind,
};
use ratatui::layout::{Position, Rect};

use super::backend::{ApiError, Reply, Request, StreamEvent};
use super::format;
use super::layout::{self, TableId};
use super::model::{
    Confirm, ERROR_STATUS_MS, HISTORY_LEN, Inspector, Modal, Nav, PoolFocus, Prompt, REFRESH_MS,
    RESTART_AFTER_MS, Reconnect, SLOW_REFRESH_MS, STATUS_MS, Sample, State, Status, StreamStatus,
    Tab,
};
use super::rows;
use super::theme::Tone;

/// Everything that can happen to the TUI.
#[derive(Debug, Clone)]
pub enum Event {
    /// The clock (Unix ms); sent every 250 ms.
    Tick {
        now_ms: i64,
    },
    Key(KeyEvent),
    Mouse(MouseEvent),
    Resize {
        width: u16,
        height: u16,
    },
    Overview(Box<StreamEvent<pb::Overview>>),
    Operations(Box<StreamEvent<pb::OperationEvent>>),
    /// The answer to an [`Effect::Call`].
    Reply {
        id: u64,
        request: Request,
        result: Result<Reply, ApiError>,
    },
    /// Outcome of a background token renewal.
    Token(Result<(), String>),
    /// SIGINT/SIGTERM.
    Quit,
}

/// The server streams the TUI follows.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StreamKind {
    Overview,
    Operations,
}

/// Work for the runtime.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Effect {
    Call {
        id: u64,
        request: Request,
    },
    /// (Re)start a stream that ended.
    StartStream(StreamKind),
    MouseCapture(bool),
    /// Redraw everything (Ctrl-L).
    Repaint,
}

/// Applies `event` and returns the effects to run.
pub fn update(s: &mut State, event: Event) -> Vec<Effect> {
    let mut fx = Vec::new();
    match event {
        Event::Tick { now_ms } => {
            s.now_ms = now_ms;
            if s.status.as_ref().is_some_and(|st| now_ms >= st.until_ms) {
                s.status = None;
            }
        }
        Event::Resize { width, height } => s.size = (width, height),
        Event::Key(k) => on_key(s, k, &mut fx),
        Event::Mouse(m) => on_mouse(s, m, &mut fx),
        Event::Overview(ev) => on_overview(s, *ev),
        Event::Operations(ev) => on_operation(s, *ev),
        Event::Reply {
            id: _,
            request,
            result,
        } => on_reply(s, request, result),
        Event::Token(r) => on_token(s, r),
        Event::Quit => s.quit = true,
    }
    sync(s);
    poll(s, &mut fx);
    fx
}

// ---------------------------------------------------------------- helpers

fn say(s: &mut State, tone: Tone, text: impl Into<String>) {
    let ms = if tone == Tone::Bad {
        ERROR_STATUS_MS
    } else {
        STATUS_MS
    };
    s.status = Some(Status {
        text: text.into(),
        tone,
        until_ms: s.now_ms + ms,
    });
}

fn hint(s: &mut State, text: impl Into<String>) {
    say(s, Tone::Neutral, text);
}

fn call(s: &mut State, fx: &mut Vec<Effect>, request: Request) {
    s.next_id += 1;
    fx.push(Effect::Call {
        id: s.next_id,
        request,
    });
}

/// The tables of a tab, in dependency order (the selected pool decides which
/// workers are listed).
fn tables(tab: Tab) -> &'static [TableId] {
    match tab {
        Tab::Overview => &[TableId::Queues],
        Tab::Pools => &[TableId::Pools, TableId::Workers],
        Tab::Hosts => &[TableId::Hosts],
        Tab::Operations => &[TableId::Ops],
        Tab::Cost => &[TableId::CostPools],
        Tab::Keys => &[TableId::Keys],
    }
}

fn keys_of(s: &State, id: TableId) -> Vec<String> {
    match id {
        TableId::Queues => rows::queues(s).into_iter().map(rows::queue_key).collect(),
        TableId::Pools => rows::pools(s).iter().map(|p| p.name.clone()).collect(),
        TableId::Workers => rows::workers(s).iter().map(|w| w.node.clone()).collect(),
        TableId::Hosts => rows::hosts(s).into_iter().map(rows::host_serial).collect(),
        TableId::Ops => rows::ops(s).iter().map(|o| o.name.clone()).collect(),
        TableId::CostPools => rows::cost_pools(s).iter().map(|p| p.pool.clone()).collect(),
        TableId::Keys => rows::keys(s).iter().map(|k| k.key_id.clone()).collect(),
    }
}

fn nav_mut(s: &mut State, id: TableId) -> &mut Nav {
    match id {
        TableId::Queues => &mut s.overview.nav,
        TableId::Pools => &mut s.pools.nav,
        TableId::Workers => &mut s.pools.workers,
        TableId::Hosts => &mut s.hosts.nav,
        TableId::Ops => &mut s.ops.nav,
        TableId::CostPools => &mut s.cost.nav,
        TableId::Keys => &mut s.keys.nav,
    }
}

/// The table the arrow keys move in.
pub fn focused_table(s: &State) -> TableId {
    match s.tab {
        Tab::Overview => TableId::Queues,
        Tab::Pools if s.pools.focus == PoolFocus::Workers => TableId::Workers,
        Tab::Pools => TableId::Pools,
        Tab::Hosts => TableId::Hosts,
        Tab::Operations => TableId::Ops,
        Tab::Cost => TableId::CostPools,
        Tab::Keys => TableId::Keys,
    }
}

fn visible(s: &State, id: TableId) -> usize {
    layout::visible_rows(layout::table_area(s, id))
}

/// Keeps the visible tables' selections on their rows and within view (hidden tabs
/// are synced when they are shown: switching tabs is an event too).
fn sync(s: &mut State) {
    for &id in tables(s.tab) {
        let keys = keys_of(s, id);
        let v = visible(s, id);
        nav_mut(s, id).sync(&keys, v);
    }
    let max = inspector_max_scroll(s);
    if let Some(i) = s.ops.inspector.as_mut() {
        i.scroll = i.scroll.min(max);
    }
}

fn select(s: &mut State, id: TableId, index: usize) {
    let keys = keys_of(s, id);
    let v = visible(s, id);
    nav_mut(s, id).select(&keys, index, v);
}

fn step(s: &mut State, id: TableId, delta: isize) {
    let keys = keys_of(s, id);
    let v = visible(s, id);
    nav_mut(s, id).step(&keys, delta, v);
}

fn inspector_max_scroll(s: &State) -> usize {
    let Some(i) = &s.ops.inspector else {
        return 0;
    };
    let total = i
        .fetch
        .data
        .as_ref()
        .map_or(0, |v| super::inspector::lines(v, &s.theme).len());
    total.saturating_sub(layout::inspector_height(s))
}

fn stream_mut(s: &mut State, kind: StreamKind) -> &mut StreamStatus {
    match kind {
        StreamKind::Overview => &mut s.overview_stream,
        StreamKind::Operations => &mut s.ops.stream,
    }
}

fn switch_tab(s: &mut State, tab: Tab) {
    s.prompt = None;
    s.tab = tab;
}

// ------------------------------------------------------------------- keys

fn on_key(s: &mut State, k: KeyEvent, fx: &mut Vec<Effect>) {
    if k.kind == KeyEventKind::Release {
        return;
    }
    let ctrl = k.modifiers.contains(KeyModifiers::CONTROL);
    match k.code {
        KeyCode::Char('c') if ctrl => {
            s.quit = true;
            return;
        }
        KeyCode::Char('l') if ctrl => {
            fx.push(Effect::Repaint);
            return;
        }
        _ => {}
    }
    match s.modal.take() {
        Some(Modal::Confirm(c)) => return on_confirm_key(s, c, k, fx),
        Some(Modal::Help) => {
            if !matches!(
                k.code,
                KeyCode::Esc | KeyCode::Enter | KeyCode::Char('?') | KeyCode::Char('q')
            ) {
                s.modal = Some(Modal::Help);
            }
            return;
        }
        None => {}
    }
    if s.prompt.is_some() {
        return on_prompt_key(s, k);
    }
    if ctrl || k.modifiers.contains(KeyModifiers::ALT) {
        return;
    }
    if s.tab == Tab::Operations && s.ops.inspector.is_some() && on_inspector_key(s, k) {
        return;
    }
    let page = visible(s, focused_table(s)).max(1) as isize;
    let table = focused_table(s);
    match k.code {
        KeyCode::Char('q') => s.quit = true,
        KeyCode::Char('?') => s.modal = Some(Modal::Help),
        KeyCode::Tab => switch_tab(s, Tab::ALL[(s.tab.index() + 1) % Tab::ALL.len()]),
        KeyCode::BackTab => switch_tab(
            s,
            Tab::ALL[(s.tab.index() + Tab::ALL.len() - 1) % Tab::ALL.len()],
        ),
        KeyCode::Char(c @ '1'..='6') => switch_tab(s, Tab::ALL[(c as usize) - ('1' as usize)]),
        KeyCode::Char('/') | KeyCode::Char(':') => open_prompt(s),
        KeyCode::Char('m') => {
            s.mouse = !s.mouse;
            fx.push(Effect::MouseCapture(s.mouse));
            hint(
                s,
                if s.mouse {
                    "mouse on: click tabs and rows, wheel scrolls"
                } else {
                    "mouse off: the terminal selects text again"
                },
            );
        }
        KeyCode::Char('r') => refresh(s),
        KeyCode::Esc => back(s, true),
        KeyCode::Left | KeyCode::Char('h') => back(s, false),
        KeyCode::Enter | KeyCode::Right | KeyCode::Char('l') => drill(s, fx),
        KeyCode::Up | KeyCode::Char('k') => step(s, table, -1),
        KeyCode::Down | KeyCode::Char('j') => step(s, table, 1),
        KeyCode::PageUp => step(s, table, -page),
        KeyCode::PageDown => step(s, table, page),
        KeyCode::Home | KeyCode::Char('g') => select(s, table, 0),
        KeyCode::End | KeyCode::Char('G') => select(s, table, usize::MAX),
        KeyCode::Char('d') => action_drain(s),
        KeyCode::Char('u') => action_undrain(s, fx),
        KeyCode::Char('R') => action_reimage(s),
        KeyCode::Char('x') => action_remove(s),
        _ => {}
    }
}

fn on_confirm_key(s: &mut State, mut c: Confirm, k: KeyEvent, fx: &mut Vec<Effect>) {
    match k.code {
        KeyCode::Char('y') | KeyCode::Char('Y') => confirm(s, c, fx),
        KeyCode::Char('n') | KeyCode::Char('N') | KeyCode::Esc | KeyCode::Char('q') => cancel(s, c),
        KeyCode::Left
        | KeyCode::Right
        | KeyCode::Tab
        | KeyCode::BackTab
        | KeyCode::Char('h')
        | KeyCode::Char('l') => {
            c.yes = !c.yes;
            s.modal = Some(Modal::Confirm(c));
        }
        KeyCode::Enter | KeyCode::Char(' ') => {
            if c.yes {
                confirm(s, c, fx)
            } else {
                cancel(s, c)
            }
        }
        // Anything else leaves the question open.
        _ => s.modal = Some(Modal::Confirm(c)),
    }
}

/// The only place a destructive request is emitted.
fn confirm(s: &mut State, c: Confirm, fx: &mut Vec<Effect>) {
    hint(s, format!("{}…", c.request.describe()));
    call(s, fx, c.request);
}

fn cancel(s: &mut State, c: Confirm) {
    hint(s, format!("cancelled: {}", c.request.describe()));
}

enum PromptOutcome {
    Edited,
    Accept,
    Cancel,
    Ignored,
}

fn on_prompt_key(s: &mut State, k: KeyEvent) {
    let tab = s.tab.index();
    let ctrl = k.modifiers.contains(KeyModifiers::CONTROL);
    let outcome = match s.prompt.as_mut() {
        None => return,
        Some(p) => {
            let len = p.input.chars().count();
            let byte = |p: &Prompt, i: usize| {
                p.input
                    .char_indices()
                    .nth(i)
                    .map_or(p.input.len(), |(b, _)| b)
            };
            match k.code {
                KeyCode::Enter => PromptOutcome::Accept,
                KeyCode::Esc => PromptOutcome::Cancel,
                KeyCode::Char('u') if ctrl => {
                    p.input.clear();
                    p.cursor = 0;
                    PromptOutcome::Edited
                }
                KeyCode::Char(c) if !ctrl => {
                    let at = byte(p, p.cursor);
                    p.input.insert(at, c);
                    p.cursor += 1;
                    PromptOutcome::Edited
                }
                KeyCode::Backspace if p.cursor > 0 => {
                    let at = byte(p, p.cursor - 1);
                    p.input.remove(at);
                    p.cursor -= 1;
                    PromptOutcome::Edited
                }
                KeyCode::Delete if p.cursor < len => {
                    let at = byte(p, p.cursor);
                    p.input.remove(at);
                    PromptOutcome::Edited
                }
                KeyCode::Left => {
                    p.cursor = p.cursor.saturating_sub(1);
                    PromptOutcome::Ignored
                }
                KeyCode::Right => {
                    p.cursor = (p.cursor + 1).min(len);
                    PromptOutcome::Ignored
                }
                KeyCode::Home => {
                    p.cursor = 0;
                    PromptOutcome::Ignored
                }
                KeyCode::End => {
                    p.cursor = len;
                    PromptOutcome::Ignored
                }
                _ => PromptOutcome::Ignored,
            }
        }
    };
    match outcome {
        PromptOutcome::Edited => {
            if let Some(p) = &s.prompt {
                s.filters[tab] = p.input.clone();
            }
        }
        PromptOutcome::Accept => s.prompt = None,
        PromptOutcome::Cancel => {
            if let Some(p) = s.prompt.take() {
                s.filters[tab] = p.previous;
            }
        }
        PromptOutcome::Ignored => {}
    }
}

fn open_prompt(s: &mut State) {
    if s.tab == Tab::Operations && s.ops.inspector.is_some() {
        return;
    }
    let current = s.filters[s.tab.index()].clone();
    s.prompt = Some(Prompt {
        cursor: current.chars().count(),
        input: current.clone(),
        previous: current,
    });
}

/// Inspector keys; returns false for keys handled globally (tabs, help, mouse).
fn on_inspector_key(s: &mut State, k: KeyEvent) -> bool {
    let max = inspector_max_scroll(s);
    let page = layout::inspector_height(s).max(1);
    let Some(i) = s.ops.inspector.as_mut() else {
        return false;
    };
    match k.code {
        KeyCode::Esc
        | KeyCode::Left
        | KeyCode::Backspace
        | KeyCode::Char('h')
        | KeyCode::Char('q') => s.ops.inspector = None,
        KeyCode::Up | KeyCode::Char('k') => i.scroll = i.scroll.saturating_sub(1),
        KeyCode::Down | KeyCode::Char('j') => i.scroll = (i.scroll + 1).min(max),
        KeyCode::PageUp => i.scroll = i.scroll.saturating_sub(page),
        KeyCode::PageDown | KeyCode::Char(' ') => i.scroll = (i.scroll + page).min(max),
        KeyCode::Home | KeyCode::Char('g') => i.scroll = 0,
        KeyCode::End | KeyCode::Char('G') => i.scroll = max,
        KeyCode::Tab
        | KeyCode::BackTab
        | KeyCode::Char('1'..='6')
        | KeyCode::Char('?')
        | KeyCode::Char('m')
        | KeyCode::Char('r') => return false,
        _ => {}
    }
    true
}

fn back(s: &mut State, esc: bool) {
    if s.tab == Tab::Pools && s.pools.focus == PoolFocus::Workers {
        s.pools.focus = PoolFocus::Pools;
        return;
    }
    let tab = s.tab.index();
    if esc && !s.filters[tab].is_empty() {
        s.filters[tab].clear();
        hint(s, "filter cleared");
    }
}

fn drill(s: &mut State, fx: &mut Vec<Effect>) {
    match s.tab {
        Tab::Overview => {
            // A queue's operations.
            let Some(q) = rows::queues(s).get(s.overview.nav.index).copied() else {
                return;
            };
            let r = q.queue.as_option();
            let mut filter = format!("platform:\"{}\"", format::platform(r));
            if let Some(inst) = r
                .map(|r| r.instance_name_prefix.as_str())
                .filter(|i| !i.is_empty())
            {
                filter.push_str(&format!(" instance:{inst}"));
            }
            s.filters[Tab::Operations.index()] = filter;
            s.ops.inspector = None;
            switch_tab(s, Tab::Operations);
        }
        Tab::Pools => {
            if s.pools.focus == PoolFocus::Pools && !rows::workers(s).is_empty() {
                s.pools.focus = PoolFocus::Workers;
            }
        }
        Tab::Operations => {
            if let Some(name) = rows::selected_op(s).map(|o| o.name.clone()) {
                open_inspector(s, name, fx);
            }
        }
        Tab::Hosts | Tab::Cost | Tab::Keys => {}
    }
}

fn open_inspector(s: &mut State, subject: String, fx: &mut Vec<Effect>) {
    let mut i = Inspector {
        subject: subject.clone(),
        ..Default::default()
    };
    i.fetch.begin(&subject);
    s.ops.inspector = Some(i);
    call(s, fx, Request::Inspect { subject });
}

fn refresh(s: &mut State) {
    let now = s.now_ms;
    for st in [&mut s.overview_stream, &mut s.ops.stream] {
        if st.failed.is_some() {
            st.restart_at_ms = Some(now);
        }
    }
    s.pools.detail.invalidate();
    s.hosts.list.invalidate();
    s.cost.detail.invalidate();
    s.keys.keys.invalidate();
    s.keys.revocations.invalidate();
    if s.tab == Tab::Operations {
        s.ops.resync_at_ms = Some(now);
    }
    hint(s, "refreshing");
}

// ----------------------------------------------------------------- actions

fn ask(s: &mut State, request: Request, title: String, body: Vec<String>) {
    s.modal = Some(Modal::Confirm(Confirm {
        request,
        title,
        body,
        yes: false,
    }));
}

fn action_drain(s: &mut State) {
    match s.tab {
        Tab::Pools => {
            if s.pools.focus != PoolFocus::Workers {
                return hint(
                    s,
                    "select a worker first (Enter moves to the workers table)",
                );
            }
            let Some(w) = rows::selected_worker(s) else {
                return hint(s, "no worker selected");
            };
            if w.drained {
                let node = w.node.clone();
                return hint(s, format!("{node} is already draining (u undrains it)"));
            }
            let (node, pool) = (w.node.clone(), w.pool.clone());
            let busy = format!("{}/{}", w.busy_threads, w.threads);
            ask(
                s,
                Request::DrainWorker { node: node.clone() },
                format!("Drain worker {node}?"),
                vec![
                    format!("Pool {pool}; {busy} runner threads busy."),
                    "It takes no new actions; running actions finish, then".into(),
                    "it is stopped once idle. u (undrain) reverts this.".into(),
                ],
            );
        }
        Tab::Hosts => {
            let Some(h) = rows::selected_host(s) else {
                return hint(s, "no host selected");
            };
            let (name, serial) = (rows::host_label(h), rows::host_serial(h));
            if h.cordoned {
                return hint(s, format!("{name} is already cordoned (u uncordons it)"));
            }
            let vms = h.summary.as_option().map_or(String::new(), |x| {
                format!("{}/{} VMs running", x.running_vms, x.slots)
            });
            ask(
                s,
                Request::DrainHost {
                    serial: serial.clone(),
                },
                format!("Drain host {name}?"),
                vec![
                    format!("Serial {serial}; {vms}."),
                    "Its VMs finish their actions and shut down; no new VMs".into(),
                    "start until you uncordon it (u).".into(),
                ],
            );
        }
        _ => hint(s, "d (drain) works in Pools (on a worker) and Hosts"),
    }
}

fn action_undrain(s: &mut State, fx: &mut Vec<Effect>) {
    match s.tab {
        Tab::Pools if s.pools.focus == PoolFocus::Workers => {
            let Some(w) = rows::selected_worker(s) else {
                return hint(s, "no worker selected");
            };
            let node = w.node.clone();
            if !w.drained {
                return hint(s, format!("{node} is not drained"));
            }
            hint(s, format!("undraining {node}…"));
            call(s, fx, Request::UndrainWorker { node });
        }
        Tab::Hosts => {
            let Some(h) = rows::selected_host(s) else {
                return hint(s, "no host selected");
            };
            let (name, serial) = (rows::host_label(h), rows::host_serial(h));
            if !h.cordoned {
                return hint(s, format!("{name} is not cordoned"));
            }
            hint(s, format!("uncordoning {name}…"));
            call(s, fx, Request::UncordonHost { serial });
        }
        _ => hint(
            s,
            "u (undrain/uncordon) works in Pools (on a worker) and Hosts",
        ),
    }
}

fn action_reimage(s: &mut State) {
    if s.tab != Tab::Hosts {
        return hint(s, "R (re-image) works in Hosts");
    }
    let Some(h) = rows::selected_host(s) else {
        return hint(s, "no host selected");
    };
    let (name, serial) = (rows::host_label(h), rows::host_serial(h));
    ask(
        s,
        Request::ReimageHost {
            serial: serial.clone(),
        },
        format!("Re-image every VM of {name}?"),
        vec![
            format!("Serial {serial}."),
            "Each VM is re-cloned from its golden image at its next".into(),
            "start; the VMs' local (L1) caches are lost.".into(),
        ],
    );
}

fn action_remove(s: &mut State) {
    match s.tab {
        Tab::Operations => {
            let Some(o) = rows::selected_op(s) else {
                return hint(s, "no operation selected");
            };
            if o.stage == "completed" {
                let name = o.name.clone();
                return hint(s, format!("{name} has already completed"));
            }
            let (name, target, stage) = (o.name.clone(), o.target_id.clone(), o.stage.clone());
            ask(
                s,
                Request::KillOperation { name: name.clone() },
                "Kill this operation?".into(),
                vec![
                    format!("{name} ({stage})"),
                    format!("Target {}", format::dash(&target)),
                    "Bazel sees the action fail (FAILED_PRECONDITION).".into(),
                ],
            );
        }
        Tab::Keys => {
            let Some(k) = rows::selected_key(s) else {
                return hint(s, "no key selected");
            };
            if k.revoked {
                let id = k.key_id.clone();
                return hint(s, format!("key {id} is already revoked"));
            }
            let (id, account, desc) = (k.key_id.clone(), k.account.clone(), k.description.clone());
            ask(
                s,
                Request::RevokeServiceKey { key_id: id.clone() },
                format!("Revoke service key {id}?"),
                vec![
                    format!("Account {account} ({}).", format::dash(&desc)),
                    "The STS refuses the key from now on; tokens it already".into(),
                    "issued expire within 15 minutes.".into(),
                ],
            );
        }
        _ => hint(s, "x (kill / revoke) works in Ops and Keys"),
    }
}

// ------------------------------------------------------------------ mouse

fn contains(r: Rect, x: u16, y: u16) -> bool {
    r.contains(Position { x, y })
}

fn tables_on_screen(s: &State) -> Vec<TableId> {
    match s.tab {
        Tab::Overview => vec![TableId::Queues],
        Tab::Pools => vec![TableId::Pools, TableId::Workers],
        Tab::Hosts => vec![TableId::Hosts],
        Tab::Operations if s.ops.inspector.is_some() => vec![],
        Tab::Operations => vec![TableId::Ops],
        Tab::Cost => vec![TableId::CostPools],
        Tab::Keys => vec![TableId::Keys],
    }
}

fn focus_table(s: &mut State, id: TableId) {
    match id {
        TableId::Pools => s.pools.focus = PoolFocus::Pools,
        TableId::Workers => s.pools.focus = PoolFocus::Workers,
        _ => {}
    }
}

fn on_mouse(s: &mut State, m: MouseEvent, fx: &mut Vec<Effect>) {
    let area = layout::full(s);
    if !s.mouse || layout::too_small(area) {
        return;
    }
    let (x, y) = (m.column, m.row);
    match m.kind {
        MouseEventKind::Down(MouseButton::Left) => click(s, area, x, y, fx),
        MouseEventKind::ScrollDown => wheel(s, x, y, 3),
        MouseEventKind::ScrollUp => wheel(s, x, y, -3),
        _ => {}
    }
}

fn click(s: &mut State, area: Rect, x: u16, y: u16, fx: &mut Vec<Effect>) {
    match s.modal.take() {
        Some(Modal::Confirm(c)) => {
            let d = layout::dialog(area, c.body.len());
            if contains(d.yes, x, y) {
                confirm(s, c, fx);
            } else if contains(d.no, x, y) {
                cancel(s, c);
            } else {
                s.modal = Some(Modal::Confirm(c));
            }
            return;
        }
        Some(Modal::Help) => return,
        None => {}
    }
    s.prompt = None;
    let sc = layout::screen(area);
    if y == sc.tabs.y {
        if let Some((tab, ..)) = layout::tab_labels()
            .into_iter()
            .find(|(_, _, x0, x1)| (*x0..*x1).contains(&x))
        {
            switch_tab(s, tab);
        }
        return;
    }
    for id in tables_on_screen(s) {
        let a = layout::table_area(s, id);
        if !contains(a, x, y) {
            continue;
        }
        focus_table(s, id);
        let first = a.y + 2;
        let end = a.y + a.height.saturating_sub(1);
        if y >= first && y < end {
            let offset = nav_mut(s, id).offset;
            select(s, id, offset + usize::from(y - first));
        }
        return;
    }
}

fn wheel(s: &mut State, x: u16, y: u16, delta: isize) {
    if s.modal.is_some() {
        return;
    }
    if s.tab == Tab::Operations && s.ops.inspector.is_some() {
        let max = inspector_max_scroll(s);
        if let Some(i) = s.ops.inspector.as_mut() {
            i.scroll = i.scroll.saturating_add_signed(delta).min(max);
        }
        return;
    }
    if let Some(id) = tables_on_screen(s)
        .into_iter()
        .find(|id| contains(layout::table_area(s, *id), x, y))
    {
        focus_table(s, id);
        step(s, id, delta);
    }
}

// ------------------------------------------------------------------- data

fn on_overview(s: &mut State, ev: StreamEvent<pb::Overview>) {
    let now = s.now_ms;
    match ev {
        StreamEvent::Data(o) => {
            let st = &mut s.overview_stream;
            st.last_data_ms = Some(now);
            st.reconnect = None;
            st.failed = None;
            st.restart_at_ms = None;
            let sample = Sample {
                queued: o.queues.iter().map(|q| u64::from(q.queued)).sum(),
                executing: o.queues.iter().map(|q| u64::from(q.executing)).sum(),
            };
            let h = &mut s.overview.history;
            h.push_back(sample);
            while h.len() > HISTORY_LEN {
                h.pop_front();
            }
            for host in &o.hosts {
                let e = s
                    .overview
                    .wan
                    .entry(host.serial.clone())
                    .or_insert([(now, host.wan_bytes_received); 2]);
                if e[1].0 != now {
                    e[0] = e[1];
                    e[1] = (now, host.wan_bytes_received);
                }
            }
            s.overview.latest = Some(o);
        }
        StreamEvent::Reconnecting {
            attempt,
            retry_in,
            reason,
        } => {
            s.overview_stream.reconnect = Some(Reconnect {
                attempt,
                retry_at_ms: now + i64::try_from(retry_in.as_millis()).unwrap_or(i64::MAX / 2),
                reason,
            });
        }
        StreamEvent::Failed { code, message } => {
            stream_failed(&mut s.overview_stream, now, code, message)
        }
    }
}

fn stream_failed(st: &mut StreamStatus, now: i64, code: Option<String>, message: String) {
    st.reconnect = None;
    st.failed = Some(message);
    if code.as_deref() == Some("unauthenticated") {
        st.restart_at_ms = Some(now + RESTART_AFTER_MS);
    }
}

fn on_operation(s: &mut State, ev: StreamEvent<pb::OperationEvent>) {
    let now = s.now_ms;
    match ev {
        StreamEvent::Data(e) => {
            let st = &mut s.ops.stream;
            st.last_data_ms = Some(now);
            st.reconnect = None;
            st.failed = None;
            st.restart_at_ms = None;
            let removed = e.kind.as_known() == Some(pb::operation_event::Kind::KIND_REMOVED);
            if let Some(op) = e.operation.into_option() {
                if removed {
                    s.ops.ops.remove(&op.name);
                } else {
                    s.ops.ops.insert(op.name.clone(), op);
                }
            }
        }
        StreamEvent::Reconnecting {
            attempt,
            retry_in,
            reason,
        } => {
            let retry_at = now + i64::try_from(retry_in.as_millis()).unwrap_or(i64::MAX / 2);
            s.ops.stream.reconnect = Some(Reconnect {
                attempt,
                retry_at_ms: retry_at,
                reason,
            });
            // The new stream replays current operations as ADDED but cannot report
            // those that ended meanwhile: take a full listing once it is back.
            s.ops.resync_at_ms = Some(retry_at + 500);
        }
        StreamEvent::Failed { code, message } => {
            stream_failed(&mut s.ops.stream, now, code, message)
        }
    }
}

fn unexpected(r: &Reply) -> String {
    format!("unexpected reply {r:?}")
}

fn on_reply(s: &mut State, request: Request, result: Result<Reply, ApiError>) {
    let now = s.now_ms;
    macro_rules! take {
        ($variant:ident) => {
            match result {
                Ok(Reply::$variant(v)) => Ok(v),
                Ok(other) => Err(unexpected(&other)),
                Err(e) => Err(e.message),
            }
        };
    }
    match &request {
        Request::GetPool { name } => {
            let r = take!(Pool).map(|b| *b);
            s.pools.detail.finish(name, now, r);
        }
        Request::ListHosts => {
            let r = take!(Hosts);
            s.hosts.list.finish("", now, r);
        }
        Request::GetCost => {
            let r = take!(Cost).map(|b| *b);
            s.cost.detail.finish("", now, r);
        }
        Request::ListServiceKeys => {
            let r = take!(ServiceKeys);
            s.keys.keys.finish("", now, r);
        }
        Request::ListRevocations => {
            let r = take!(Revocations);
            s.keys.revocations.finish("", now, r);
        }
        Request::ListOperations => {
            s.ops.resync_in_flight = false;
            match take!(Operations) {
                Ok(ops) => {
                    s.ops.ops = ops.into_iter().map(|o| (o.name.clone(), o)).collect();
                }
                Err(_) => s.ops.resync_at_ms = Some(now + REFRESH_MS),
            }
        }
        Request::Inspect { subject } => {
            let r = take!(Action).map(|b| *b);
            if let Some(i) = s.ops.inspector.as_mut() {
                i.fetch.finish(subject, now, r);
            }
        }
        mutating => match result {
            Ok(Reply::Done(text)) => {
                say(s, Tone::Good, format!("✓ {text}"));
                match mutating {
                    Request::DrainWorker { .. } | Request::UndrainWorker { .. } => {
                        s.pools.detail.invalidate()
                    }
                    Request::DrainHost { .. }
                    | Request::UncordonHost { .. }
                    | Request::ReimageHost { .. } => s.hosts.list.invalidate(),
                    Request::RevokeServiceKey { .. } => {
                        s.keys.keys.invalidate();
                        s.keys.revocations.invalidate();
                    }
                    _ => {}
                }
            }
            Ok(other) => say(s, Tone::Bad, unexpected(&other)),
            Err(e) => say(
                s,
                Tone::Bad,
                format!("✗ {} failed: {}", mutating.describe(), e.message),
            ),
        },
    }
}

fn on_token(s: &mut State, r: Result<(), String>) {
    match r {
        Ok(()) => s.auth_error = None,
        Err(e) => {
            if s.auth_error.is_none() {
                say(
                    s,
                    Tone::Bad,
                    format!("session renewal failed (retrying): {e}"),
                );
            }
            s.auth_error = Some(e);
        }
    }
}

// ------------------------------------------------------------------ polling

/// Issues the fetches the visible view needs and restarts streams that are due.
fn poll(s: &mut State, fx: &mut Vec<Effect>) {
    let now = s.now_ms;
    if now == 0 {
        return;
    }
    for kind in [StreamKind::Overview, StreamKind::Operations] {
        let st = stream_mut(s, kind);
        if st.restart_at_ms.is_some_and(|t| now >= t) {
            st.restart_at_ms = None;
            st.failed = None;
            st.reconnect = None;
            fx.push(Effect::StartStream(kind));
        }
    }
    if s.ops.resync_at_ms.is_some_and(|t| now >= t) && !s.ops.resync_in_flight {
        s.ops.resync_at_ms = None;
        s.ops.resync_in_flight = true;
        call(s, fx, Request::ListOperations);
    }
    match s.tab {
        Tab::Pools => {
            if let Some(name) = rows::selected_pool(s).map(|p| p.name.clone())
                && s.pools.detail.due(&name, now, REFRESH_MS)
            {
                s.pools.detail.begin(&name);
                call(s, fx, Request::GetPool { name });
            }
        }
        Tab::Hosts => {
            if s.hosts.list.due("", now, REFRESH_MS) {
                s.hosts.list.begin("");
                call(s, fx, Request::ListHosts);
            }
        }
        Tab::Cost => {
            if s.cost.detail.due("", now, SLOW_REFRESH_MS) {
                s.cost.detail.begin("");
                call(s, fx, Request::GetCost);
            }
        }
        Tab::Keys => {
            if s.keys.keys.due("", now, SLOW_REFRESH_MS) {
                s.keys.keys.begin("");
                call(s, fx, Request::ListServiceKeys);
            }
            if s.keys.revocations.due("", now, SLOW_REFRESH_MS) {
                s.keys.revocations.begin("");
                call(s, fx, Request::ListRevocations);
            }
        }
        Tab::Overview | Tab::Operations => {}
    }
}
