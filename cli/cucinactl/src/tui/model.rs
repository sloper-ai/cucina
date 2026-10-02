// SPDX-License-Identifier: FSL-1.1-ALv2

//! The TUI state. Everything the screen shows is derived from [`State`] alone
//! (including the clock, `now_ms`), so [`super::view::render`] is a pure function and
//! snapshot tests are deterministic. [`super::update::update`] is the only mutator.

use std::collections::{BTreeMap, HashMap, VecDeque};

use cucina_api::proto::cucina::v1 as pb;

use super::backend::Request;
use super::theme::{Theme, Tone};
use crate::inspect::ActionView;

/// Unary data on screen is refreshed at least this often (R-CLI-4: ≤ 2 s).
pub const REFRESH_MS: i64 = 2_000;
/// Slow-moving data (cost lines, keys).
pub const SLOW_REFRESH_MS: i64 = 10_000;
/// Without new overview data for this long the header says STALE.
pub const STALE_AFTER_MS: i64 = 5_000;
/// Samples kept for the queued/executing sparkline (4 min at 2 s).
pub const HISTORY_LEN: usize = 120;
pub const STATUS_MS: i64 = 6_000;
pub const ERROR_STATUS_MS: i64 = 15_000;
/// A stream that failed with `unauthenticated` is retried after this long (the token
/// refresher has usually renewed the session by then).
pub const RESTART_AFTER_MS: i64 = 10_000;
/// Below this size only a "terminal too small" message is drawn.
pub const MIN_WIDTH: u16 = 60;
pub const MIN_HEIGHT: u16 = 16;

/// The views (R-CLI-4), in tab order.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Tab {
    Overview,
    Pools,
    Hosts,
    Operations,
    Cost,
    Keys,
}

impl Tab {
    pub const ALL: [Tab; 6] = [
        Tab::Overview,
        Tab::Pools,
        Tab::Hosts,
        Tab::Operations,
        Tab::Cost,
        Tab::Keys,
    ];

    pub fn index(self) -> usize {
        self as usize
    }

    /// Tab-bar label.
    pub fn label(self) -> &'static str {
        match self {
            Tab::Overview => "Overview",
            Tab::Pools => "Pools",
            Tab::Hosts => "Hosts",
            Tab::Operations => "Ops",
            Tab::Cost => "Cost",
            Tab::Keys => "Keys",
        }
    }
}

/// Selection and scroll position of one table. The selection follows the row's key
/// across refreshes, so a row stays selected while others come and go.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Nav {
    pub index: usize,
    pub key: Option<String>,
    pub offset: usize,
}

impl Nav {
    fn scroll(&mut self, len: usize, visible: usize) {
        let visible = visible.max(1);
        if self.index < self.offset {
            self.offset = self.index;
        } else if self.index >= self.offset + visible {
            self.offset = self.index + 1 - visible;
        }
        self.offset = self.offset.min(len.saturating_sub(visible));
    }

    /// Re-resolves the selection against the current rows (`keys`) and keeps it
    /// within the `visible` rows.
    pub fn sync(&mut self, keys: &[String], visible: usize) {
        if keys.is_empty() {
            self.index = 0;
            self.offset = 0;
            return;
        }
        if let Some(i) = self
            .key
            .as_ref()
            .and_then(|k| keys.iter().position(|x| x == k))
        {
            self.index = i;
        }
        self.index = self.index.min(keys.len() - 1);
        self.key = Some(keys[self.index].clone());
        self.scroll(keys.len(), visible);
    }

    /// Selects row `index` (clamped).
    pub fn select(&mut self, keys: &[String], index: usize, visible: usize) {
        if keys.is_empty() {
            return;
        }
        self.index = index.min(keys.len() - 1);
        self.key = Some(keys[self.index].clone());
        self.scroll(keys.len(), visible);
    }

    /// Moves the selection by `delta` rows (clamped).
    pub fn step(&mut self, keys: &[String], delta: isize, visible: usize) {
        let target = self.index.saturating_add_signed(delta);
        self.select(keys, target, visible);
    }
}

/// Unary data fetched on demand and refreshed while visible.
#[derive(Debug, Clone)]
pub struct Fetch<T> {
    /// What the data belongs to (a pool name, an operation; empty for global lists).
    pub key: String,
    pub data: Option<T>,
    pub error: Option<String>,
    pub fetched_ms: Option<i64>,
    pub in_flight: bool,
}

impl<T> Default for Fetch<T> {
    fn default() -> Self {
        Fetch {
            key: String::new(),
            data: None,
            error: None,
            fetched_ms: None,
            in_flight: false,
        }
    }
}

impl<T> Fetch<T> {
    /// Whether `key`'s data should be (re)fetched now.
    pub fn due(&self, key: &str, now: i64, every: i64) -> bool {
        !self.in_flight && (self.key != key || self.fetched_ms.is_none_or(|t| now - t >= every))
    }

    /// Marks a request for `key` in flight (switching keys drops the old data).
    pub fn begin(&mut self, key: &str) {
        if self.key != key {
            self.key = key.to_string();
            self.data = None;
            self.error = None;
            self.fetched_ms = None;
        }
        self.in_flight = true;
    }

    /// Stores a reply for `key`; failures keep the previous data.
    pub fn finish(&mut self, key: &str, now: i64, result: Result<T, String>) {
        if self.key != key {
            return;
        }
        self.in_flight = false;
        self.fetched_ms = Some(now);
        match result {
            Ok(d) => {
                self.data = Some(d);
                self.error = None;
            }
            Err(e) => self.error = Some(e),
        }
    }

    /// Data for `key`, if that is what is loaded.
    pub fn data_for(&self, key: &str) -> Option<&T> {
        (self.key == key).then_some(self.data.as_ref()).flatten()
    }

    /// Forces a refresh at the next opportunity.
    pub fn invalidate(&mut self) {
        self.fetched_ms = None;
    }
}

/// Health of one server stream.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct StreamStatus {
    pub last_data_ms: Option<i64>,
    pub reconnect: Option<Reconnect>,
    /// Ended for good (permanent error).
    pub failed: Option<String>,
    /// When to start the stream again after a failure.
    pub restart_at_ms: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reconnect {
    pub attempt: u32,
    pub retry_at_ms: i64,
    pub reason: String,
}

/// What the header's connection badge says.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Liveness {
    Connecting,
    Live,
    /// No fresh data for longer than [`STALE_AFTER_MS`] (age unknown before the first data).
    Stale {
        age_ms: Option<i64>,
    },
    Failed(String),
}

impl StreamStatus {
    pub fn liveness(&self, now: i64) -> Liveness {
        if let Some(f) = &self.failed {
            return Liveness::Failed(f.clone());
        }
        match self.last_data_ms {
            None if self.reconnect.is_some() => Liveness::Stale { age_ms: None },
            None => Liveness::Connecting,
            Some(t) if now - t <= STALE_AFTER_MS => Liveness::Live,
            Some(t) => Liveness::Stale {
                age_ms: Some(now - t),
            },
        }
    }
}

/// One point of the queued/executing sparkline.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Sample {
    pub queued: u64,
    pub executing: u64,
}

#[derive(Debug, Clone, Default)]
pub struct OverviewState {
    pub latest: Option<pb::Overview>,
    pub history: VecDeque<Sample>,
    /// The queues table.
    pub nav: Nav,
    /// Last two `(time ms, bytes)` WAN samples per host serial (for rates).
    pub wan: HashMap<String, [(i64, i64); 2]>,
}

/// Which table of the Pools view has the keyboard.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum PoolFocus {
    #[default]
    Pools,
    Workers,
}

#[derive(Debug, Clone, Default)]
pub struct PoolsState {
    pub nav: Nav,
    pub workers: Nav,
    pub focus: PoolFocus,
    /// `GetPool` of the selected pool: workers, scale timeline, cold starts.
    pub detail: Fetch<pb::GetPoolResponse>,
}

#[derive(Debug, Clone, Default)]
pub struct HostsState {
    pub nav: Nav,
    pub list: Fetch<Vec<pb::HostDetail>>,
}

/// The action inspector (drill-down from an operation).
#[derive(Debug, Clone, Default)]
pub struct Inspector {
    pub subject: String,
    pub fetch: Fetch<ActionView>,
    pub scroll: usize,
}

#[derive(Debug, Clone, Default)]
pub struct OpsState {
    /// Live operations by name, maintained from `WatchOperations` events.
    pub ops: BTreeMap<String, pb::OperationSummary>,
    pub nav: Nav,
    pub stream: StreamStatus,
    /// After a reconnect: when to fetch a full listing (drops operations that ended
    /// while the stream was down).
    pub resync_at_ms: Option<i64>,
    pub resync_in_flight: bool,
    pub inspector: Option<Inspector>,
}

#[derive(Debug, Clone, Default)]
pub struct CostState {
    pub nav: Nav,
    pub detail: Fetch<pb::GetCostResponse>,
}

#[derive(Debug, Clone, Default)]
pub struct KeysState {
    pub nav: Nav,
    pub keys: Fetch<Vec<pb::ServiceKeyInfo>>,
    pub revocations: Fetch<Vec<pb::Revocation>>,
}

/// A destructive action waiting for an explicit "yes" (default: No).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Confirm {
    pub request: Request,
    pub title: String,
    pub body: Vec<String>,
    /// The highlighted button; `false` (No) when the dialog opens.
    pub yes: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Modal {
    Help,
    Confirm(Confirm),
}

/// The filter prompt (`/` or `:`); the filter applies as you type.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Prompt {
    pub input: String,
    /// Cursor position in characters.
    pub cursor: usize,
    /// The filter to restore on Esc.
    pub previous: String,
}

/// A transient message in the status line (results of mutating calls, hints).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Status {
    pub text: String,
    pub tone: Tone,
    pub until_ms: i64,
}

/// The whole TUI state.
#[derive(Debug, Clone)]
pub struct State {
    /// Clock (Unix ms), advanced by `Tick` events.
    pub now_ms: i64,
    /// Terminal size (columns, rows).
    pub size: (u16, u16),
    pub theme: Theme,
    /// Profile (or "demo") shown in the header.
    pub profile: String,
    pub tab: Tab,
    pub overview_stream: StreamStatus,
    pub overview: OverviewState,
    pub pools: PoolsState,
    pub hosts: HostsState,
    pub ops: OpsState,
    pub cost: CostState,
    pub keys: KeysState,
    /// Quick filter per tab.
    pub filters: [String; 6],
    pub prompt: Option<Prompt>,
    pub modal: Option<Modal>,
    pub status: Option<Status>,
    /// Last token-renewal failure (cleared by the next success).
    pub auth_error: Option<String>,
    /// Mouse capture on (clicks and wheel) or off (terminal text selection).
    pub mouse: bool,
    pub quit: bool,
    /// Requests issued so far (effect ids).
    pub next_id: u64,
}

impl State {
    pub fn new(profile: impl Into<String>, theme: Theme, mouse: bool, size: (u16, u16)) -> State {
        State {
            now_ms: 0,
            size,
            theme,
            profile: profile.into(),
            tab: Tab::Overview,
            overview_stream: StreamStatus::default(),
            overview: OverviewState::default(),
            pools: PoolsState::default(),
            hosts: HostsState::default(),
            ops: OpsState::default(),
            cost: CostState::default(),
            keys: KeysState::default(),
            filters: Default::default(),
            prompt: None,
            modal: None,
            status: None,
            auth_error: None,
            mouse,
            quit: false,
            next_id: 0,
        }
    }

    /// The filter text of `tab`.
    pub fn filter_text(&self, tab: Tab) -> &str {
        &self.filters[tab.index()]
    }
}
