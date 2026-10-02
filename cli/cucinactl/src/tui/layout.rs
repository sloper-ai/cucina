// SPDX-License-Identifier: FSL-1.1-ALv2

//! Screen geometry, shared by rendering and by the reducer (mouse hit-testing,
//! scrolling), so a click lands on exactly the row that is drawn there.

use ratatui::layout::{Constraint, Layout, Rect};

use super::model::{MIN_HEIGHT, MIN_WIDTH, State, Tab};
use super::rows;

/// Top-level regions.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Screen {
    pub tabs: Rect,
    pub body: Rect,
    pub status: Rect,
    pub footer: Rect,
}

pub fn full(s: &State) -> Rect {
    Rect::new(0, 0, s.size.0, s.size.1)
}

pub fn too_small(area: Rect) -> bool {
    area.width < MIN_WIDTH || area.height < MIN_HEIGHT
}

pub fn screen(area: Rect) -> Screen {
    let [tabs, body, status, footer] = Layout::vertical([
        Constraint::Length(1),
        Constraint::Fill(1),
        Constraint::Length(1),
        Constraint::Length(1),
    ])
    .areas(area);
    Screen {
        tabs,
        body,
        status,
        footer,
    }
}

/// Text and x-range `[start, end)` of each tab in the tab bar.
pub fn tab_labels() -> Vec<(Tab, String, u16, u16)> {
    let mut x = 0u16;
    Tab::ALL
        .iter()
        .map(|&t| {
            let text = format!(" {} {} ", t.index() + 1, t.label());
            let w = text.chars().count() as u16;
            let item = (t, text, x, x + w);
            x += w + 1;
            item
        })
        .collect()
}

/// Width of the side column in the overview.
const SIDE: u16 = 27;

#[derive(Debug, Clone, Copy)]
pub struct OverviewAreas {
    pub queues: Rect,
    pub workers: Rect,
    pub pools: Rect,
    pub spend: Rect,
    pub activity: Rect,
    pub alerts: Rect,
}

pub fn overview(body: Rect, alerts: usize) -> OverviewAreas {
    let alerts_h = (alerts.max(1) as u16 + 2).min(6);
    let [top, mid, activity, alerts] = Layout::vertical([
        Constraint::Min(6),
        Constraint::Min(6),
        Constraint::Length(4),
        Constraint::Length(alerts_h),
    ])
    .areas(body);
    let [queues, workers] =
        Layout::horizontal([Constraint::Fill(1), Constraint::Length(SIDE)]).areas(top);
    let [pools, spend] =
        Layout::horizontal([Constraint::Fill(1), Constraint::Length(SIDE)]).areas(mid);
    OverviewAreas {
        queues,
        workers,
        pools,
        spend,
        activity,
        alerts,
    }
}

#[derive(Debug, Clone, Copy)]
pub struct PoolsAreas {
    pub pools: Rect,
    pub workers: Rect,
    pub timeline: Rect,
    pub starts: Rect,
}

pub fn pools(body: Rect, n_pools: usize) -> PoolsAreas {
    let max = (body.height / 3).max(4);
    let pools_h = (n_pools.max(1) as u16 + 3).clamp(4, max);
    let [pools, workers, bottom] = Layout::vertical([
        Constraint::Length(pools_h),
        Constraint::Min(4),
        Constraint::Length(7),
    ])
    .areas(body);
    let [timeline, starts] =
        Layout::horizontal([Constraint::Fill(1), Constraint::Fill(1)]).areas(bottom);
    PoolsAreas {
        pools,
        workers,
        timeline,
        starts,
    }
}

#[derive(Debug, Clone, Copy)]
pub struct HostsAreas {
    pub hosts: Rect,
    pub detail: Rect,
}

pub fn hosts(body: Rect, n_hosts: usize) -> HostsAreas {
    let max = (body.height / 2).max(4);
    let h = (n_hosts.max(1) as u16 + 3).clamp(4, max);
    let [hosts, detail] =
        Layout::vertical([Constraint::Length(h), Constraint::Fill(1)]).areas(body);
    HostsAreas { hosts, detail }
}

#[derive(Debug, Clone, Copy)]
pub struct CostAreas {
    pub summary: Rect,
    pub projection: Rect,
    pub pools: Rect,
    pub lines: Rect,
}

pub fn cost(body: Rect) -> CostAreas {
    let [top, pools, lines] = Layout::vertical([
        Constraint::Length(5),
        Constraint::Fill(1),
        Constraint::Fill(1),
    ])
    .areas(body);
    let [summary, projection] =
        Layout::horizontal([Constraint::Fill(1), Constraint::Fill(1)]).areas(top);
    CostAreas {
        summary,
        projection,
        pools,
        lines,
    }
}

#[derive(Debug, Clone, Copy)]
pub struct KeysAreas {
    pub keys: Rect,
    pub revocations: Rect,
}

pub fn keys(body: Rect) -> KeysAreas {
    let [keys, revocations] =
        Layout::vertical([Constraint::Fill(3), Constraint::Fill(2)]).areas(body);
    KeysAreas { keys, revocations }
}

/// Tables with a selection.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TableId {
    Queues,
    Pools,
    Workers,
    Hosts,
    Ops,
    CostPools,
    Keys,
}

/// Where `id` is drawn (a zero rect when not on screen).
pub fn table_area(s: &State, id: TableId) -> Rect {
    let area = full(s);
    if too_small(area) {
        return Rect::default();
    }
    let body = screen(area).body;
    match id {
        TableId::Queues => {
            let alerts = s.overview.latest.as_ref().map_or(0, |o| o.alerts.len());
            overview(body, alerts).queues
        }
        TableId::Pools => pools(body, rows::pools(s).len()).pools,
        TableId::Workers => pools(body, rows::pools(s).len()).workers,
        TableId::Hosts => hosts(body, rows::hosts(s).len()).hosts,
        TableId::Ops => body,
        TableId::CostPools => cost(body).pools,
        TableId::Keys => keys(body).keys,
    }
}

/// Rows that fit in a bordered table with a header line.
pub fn visible_rows(area: Rect) -> usize {
    usize::from(area.height.saturating_sub(3))
}

/// Lines that fit in the inspector body (bordered).
pub fn inspector_height(s: &State) -> usize {
    usize::from(screen(full(s)).body.height.saturating_sub(2))
}

/// The confirmation dialog and its two buttons.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Dialog {
    pub area: Rect,
    pub no: Rect,
    pub yes: Rect,
}

pub const NO_LABEL: &str = "[ No ]";
pub const YES_LABEL: &str = "[ Yes ]";

pub fn dialog(area: Rect, body_lines: usize) -> Dialog {
    let w = area.width.saturating_sub(4).min(70);
    let h = (body_lines as u16 + 5).min(area.height.saturating_sub(2));
    let d = area.centered(Constraint::Length(w), Constraint::Length(h));
    let y = d.y + d.height.saturating_sub(2);
    let (nw, yw) = (NO_LABEL.len() as u16, YES_LABEL.len() as u16);
    let total = nw + 3 + yw;
    let x = d.x + d.width.saturating_sub(total) / 2;
    Dialog {
        area: d,
        no: Rect::new(x, y, nw, 1),
        yes: Rect::new(x + nw + 3, y, yw, 1),
    }
}

/// The help overlay.
pub fn help(area: Rect, lines: usize) -> Rect {
    let w = area.width.saturating_sub(4).min(74);
    let h = (lines as u16 + 2).min(area.height.saturating_sub(2));
    area.centered(Constraint::Length(w), Constraint::Length(h))
}
