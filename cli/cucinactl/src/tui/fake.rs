// SPDX-License-Identifier: FSL-1.1-ALv2

//! A deterministic, in-memory [`Management`] (a fake, not a mock: R-TEST-5). It
//! serves fixed fixtures around a fixed clock ([`T0_MS`]), applies mutations to its
//! own state (a drain marks the worker drained, a kill removes the operation, a
//! revoke marks the key revoked), records every mutating request
//! ([`FakeManagement::mutations`]) and can fail the next call of a kind
//! ([`FakeManagement::fail_next`]).
//!
//! Users: the TUI tests (snapshots, reducer tables) and the demo mode
//! (`CUCINA_TUI_DEMO=1 cucinactl tui`), where the streams replay a scripted,
//! cycling load every 2 s so that the screens move (VHS tapes, MT-006 checks on
//! terminals without a cluster). All data is invented.

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use cucina_api::proto::cucina::v1 as pb;
use futures::future::BoxFuture;
use futures::stream::BoxStream;
use futures::{FutureExt as _, StreamExt as _};

use super::backend::{ApiError, Management, Reply, Request, StreamEvent};
use super::model::State;
use super::update::{Effect, Event, update};
use crate::client::reapi::{TreeEntry, TreeListing};
use crate::inspect::{
    ActionView, CommandInfo, OperationInfo, OutputFile, ResultInfo, StreamText, Timing,
};
use crate::util::{PbDuration, PbTimestamp};

/// The fixtures' "now": 2026-09-21T13:46:40Z.
pub const T0_MS: i64 = 1_790_000_000_000;
const T0: i64 = T0_MS / 1000;

type Field<T> = buffa::MessageField<T, buffa::Inline<T>>;

fn ts(seconds: i64) -> Field<PbTimestamp> {
    PbTimestamp {
        seconds,
        ..Default::default()
    }
    .into()
}

fn dur_ms(ms: i64) -> Field<PbDuration> {
    PbDuration {
        seconds: ms / 1000,
        nanos: i32::try_from((ms % 1000) * 1_000_000).unwrap_or(0),
        ..Default::default()
    }
    .into()
}

fn money(micros: i64) -> Field<pb::Money> {
    pb::Money {
        micros,
        ..Default::default()
    }
    .into()
}

fn queue(props: &[(&str, &str)]) -> pb::QueueRef {
    pb::QueueRef {
        instance_name_prefix: "main".into(),
        platform: props
            .iter()
            .map(|(n, v)| pb::PlatformProperty {
                name: (*n).into(),
                value: (*v).into(),
                ..Default::default()
            })
            .collect(),
        size_class: 1,
        ..Default::default()
    }
}

const LINUX: &[(&str, &str)] = &[("OSFamily", "linux"), ("ISA", "x86-64")];
const RISCV: &[(&str, &str)] = &[
    ("OSFamily", "linux"),
    ("ISA", "rv64g"),
    ("cucina-emulation", "qemu"),
];
const ARM: &[(&str, &str)] = &[("OSFamily", "linux"), ("ISA", "arm-a64")];
const WINDOWS: &[(&str, &str)] = &[("OSFamily", "windows"), ("ISA", "x86-64")];
const MACOS: &[(&str, &str)] = &[
    ("OSFamily", "macos"),
    ("ISA", "arm-a64"),
    ("xcode-version", "27.0"),
];

/// A cycling load profile (fraction of the peak, in %), one entry per 2 s step.
const WAVE: [u32; 16] = [10, 25, 50, 80, 100, 95, 85, 70, 55, 40, 30, 20, 12, 6, 3, 5];

fn wave(step: u64, peak: u32) -> u32 {
    WAVE[(step % WAVE.len() as u64) as usize] * peak / 100
}

#[allow(clippy::too_many_arguments)]
fn pool(
    name: &str,
    provider: &str,
    max: u32,
    desired: u32,
    launching: u32,
    busy: u32,
    idle: u32,
    generation: &str,
) -> pb::PoolSummary {
    pb::PoolSummary {
        name: name.into(),
        platform: name.into(),
        provider: provider.into(),
        min_running: 0,
        max,
        desired,
        launching,
        // Match PoolSummary's aggregate: busy/idle are subsets of registered.
        registered: busy + idle,
        busy,
        idle,
        draining: 0,
        stopped: if provider == "tart" {
            max - busy - idle
        } else {
            0
        },
        image_generation: generation.into(),
        paused: false,
        condition: "Ready".into(),
        ..Default::default()
    }
}

#[allow(clippy::too_many_arguments)]
fn worker(
    t0: i64,
    node: &str,
    pool: &str,
    state: &str,
    busy: u32,
    threads: u32,
    kind: &str,
    up: i64,
) -> pb::WorkerSummary {
    pb::WorkerSummary {
        node: node.into(),
        pool: pool.into(),
        state: state.into(),
        generation: if pool.starts_with("macos") {
            "g2"
        } else {
            "g7"
        }
        .into(),
        instance_type: kind.into(),
        host: node
            .split_once('/')
            .map(|(h, _)| h.to_string())
            .unwrap_or_default(),
        threads,
        busy_threads: busy,
        drained: false,
        launched: ts(t0 - up),
        idle_for: dur_ms(if state == "idle" { 95_000 } else { 0 }),
        private_ip: String::new(),
        ..Default::default()
    }
}

#[allow(clippy::too_many_arguments)]
fn op(
    t0: i64,
    n: u32,
    stage: &str,
    q: &[(&str, &str)],
    target: &str,
    inv: u32,
    worker: &str,
    age: i64,
) -> pb::OperationSummary {
    pb::OperationSummary {
        name: format!("4c1f0a2e-7d3b-4e5a-9b6c-{n:012x}"),
        queue: queue(q).into(),
        action_digest: format!("{}/{}", digest(n), 140 + n),
        digest_function: "SHA256".into(),
        stage: stage.into(),
        queued_at: ts(t0 - age),
        target_id: target.into(),
        invocation_id: format!("9e8d7c6b-0000-4000-8000-{inv:012x}"),
        tool_invocation_id: String::new(),
        priority: 0,
        worker_node: worker.into(),
        worker_thread: if worker.is_empty() { 0 } else { n % 32 },
        expected_duration: dur_ms(12_000),
        timeout: ts(t0 + 900),
        ..Default::default()
    }
}

/// A made-up but well-formed SHA-256 hex digest.
fn digest(n: u32) -> String {
    format!("{:016x}", u64::from(n).wrapping_mul(0x9e37_79b9_7f4a_7c15)).repeat(4)
}

#[allow(clippy::too_many_arguments)]
fn host(
    t0: i64,
    serial: &str,
    name: &str,
    phase: &str,
    running: u32,
    l2: &str,
    wan: i64,
    beat_age: i64,
    cordoned: bool,
) -> pb::HostDetail {
    let vm = |i: u32, state: &str| pb::HostVM {
        name: format!("vm-{}", (b'a' + i as u8) as char),
        pool: "macos-arm64-xcode27.0".into(),
        state: state.into(),
        image: "ghcr.io/example/cucina-macos:27.0-xcode27.0".into(),
        generation: "g2".into(),
        registered: state == "running",
        ..Default::default()
    };
    pb::HostDetail {
        summary: pb::HostSummary {
            serial: serial.into(),
            name: name.into(),
            site: "office".into(),
            phase: phase.into(),
            running_vms: running,
            slots: 2,
            l2_hit_ratio: l2.into(),
            wan_bytes_received: wan,
            last_heartbeat: ts(t0 - beat_age),
            agent_version: "0.1.0".into(),
            ..Default::default()
        }
        .into(),
        macos_version: "27.0".into(),
        tart_version: "2.40.1".into(),
        chip: "Apple M4 Pro".into(),
        cores: 14,
        memory_gib: 64,
        disk_free_gib: 412,
        vms: (0..2)
            .map(|i| vm(i, if i < running { "running" } else { "stopped" }))
            .collect(),
        images: vec!["ghcr.io/example/cucina-macos:27.0-xcode27.0".into()],
        cordoned,
        approved: true,
        labels: [("rack".to_string(), "a".to_string())]
            .into_iter()
            .collect(),
        cert_expiry: ts(t0 + 20 * 3600),
        filevault: "off".into(),
        ..Default::default()
    }
}

struct Data {
    workers: Vec<pb::WorkerSummary>,
    hosts: Vec<pb::HostDetail>,
    ops: Vec<pb::OperationSummary>,
    keys: Vec<pb::ServiceKeyInfo>,
    revocations: Vec<pb::Revocation>,
    mutations: Vec<Request>,
    fail: HashMap<String, ApiError>,
}

fn fixtures(t0: i64) -> Data {
    let l = "linux-x86-64";
    let m = "macos-arm64-xcode27.0";
    Data {
        workers: vec![
            worker(
                t0,
                "i-0a1b2c3d4e5f60718",
                l,
                "busy",
                32,
                32,
                "c8i.8xlarge",
                14 * 60,
            ),
            worker(
                t0,
                "i-0a1b2c3d4e5f60719",
                l,
                "busy",
                16,
                32,
                "c8i.8xlarge",
                9 * 60,
            ),
            worker(
                t0,
                "i-0a1b2c3d4e5f6071a",
                l,
                "launching",
                0,
                0,
                "c8i.8xlarge",
                20,
            ),
            worker(
                t0,
                "i-0f9e8d7c6b5a40312",
                "windows-x86-64",
                "launching",
                0,
                0,
                "c7a.4xlarge",
                50,
            ),
            worker(t0, "mini-1/vm-a", m, "busy", 4, 4, "", 3 * 3600),
            worker(t0, "mini-1/vm-b", m, "idle", 0, 4, "", 3 * 3600),
        ],
        hosts: vec![
            host(
                t0,
                "C07DEMO00001",
                "mini-1",
                "Online",
                2,
                "0.93",
                1_717_986_918,
                3,
                false,
            ),
            host(
                t0,
                "C07DEMO00002",
                "mini-2",
                "Online",
                0,
                "0.88",
                905_969_664,
                4,
                true,
            ),
            host(
                t0,
                "C07DEMO00003",
                "mini-3",
                "Offline",
                0,
                "",
                52_428_800,
                6 * 60,
                false,
            ),
        ],
        ops: vec![
            op(
                t0,
                1,
                "executing",
                LINUX,
                "//absl/strings:str_cat_test",
                1,
                "i-0a1b2c3d4e5f60718",
                41,
            ),
            op(
                t0,
                2,
                "executing",
                LINUX,
                "//absl/container:flat_hash_map_test",
                1,
                "i-0a1b2c3d4e5f60719",
                33,
            ),
            op(
                t0,
                3,
                "executing",
                RISCV,
                "//absl/numeric:int128_test",
                2,
                "i-0a1b2c3d4e5f60718",
                12,
            ),
            op(
                t0,
                4,
                "executing",
                MACOS,
                "//absl/base:raw_logging_internal",
                3,
                "mini-1/vm-a",
                8,
            ),
            op(t0, 5, "queued", WINDOWS, "//absl/time:time_test", 4, "", 47),
            op(
                t0,
                6,
                "queued",
                WINDOWS,
                "//absl/time:clock_test",
                4,
                "",
                46,
            ),
            op(
                t0,
                7,
                "queued",
                LINUX,
                "//absl/synchronization:mutex_test",
                1,
                "",
                9,
            ),
            op(
                t0,
                8,
                "queued",
                LINUX,
                "//absl/random:distributions_test",
                1,
                "",
                5,
            ),
        ],
        keys: vec![
            pb::ServiceKeyInfo {
                key_id: "sk-7d2f91".into(),
                account: "ci-bot".into(),
                description: "GitHub Actions fallback".into(),
                created: ts(t0 - 30 * 86_400),
                expires_at: ts(t0 + 60 * 86_400),
                last_used: ts(t0 - 2 * 3600),
                revoked: false,
                ..Default::default()
            },
            pb::ServiceKeyInfo {
                key_id: "sk-0b44e2".into(),
                account: "nightly".into(),
                description: "nightly canary".into(),
                created: ts(t0 - 9 * 86_400),
                expires_at: ts(t0 + 21 * 86_400),
                last_used: ts(t0 - 600),
                revoked: false,
                ..Default::default()
            },
            pb::ServiceKeyInfo {
                key_id: "sk-51aa03".into(),
                account: "ci-bot".into(),
                description: "rotated".into(),
                created: ts(t0 - 120 * 86_400),
                expires_at: ts(t0 - 30 * 86_400),
                last_used: ts(t0 - 31 * 86_400),
                revoked: true,
                ..Default::default()
            },
        ],
        revocations: vec![pb::Revocation {
            sub: "google:ex-employee@example.com".into(),
            sid: String::new(),
            reason: "offboarded".into(),
            created: ts(t0 - 2 * 86_400),
            created_by: "admin@example.com".into(),
            ..Default::default()
        }],
        mutations: Vec::new(),
        fail: HashMap::new(),
    }
}

/// The fake management API.
#[derive(Clone)]
pub struct FakeManagement {
    data: Arc<Mutex<Data>>,
    /// Streams tick every 2 s (demo mode) instead of sending one snapshot.
    live: bool,
    /// The fixtures' clock (Unix seconds): [`T0_MS`] in tests, start time in the demo.
    t0: i64,
}

impl Default for FakeManagement {
    fn default() -> Self {
        Self::new()
    }
}

/// Short name of a request kind (for [`FakeManagement::fail_next`]).
pub fn kind(r: &Request) -> &'static str {
    match r {
        Request::GetPool { .. } => "get_pool",
        Request::ListHosts => "list_hosts",
        Request::GetCost => "get_cost",
        Request::ListServiceKeys => "list_service_keys",
        Request::ListRevocations => "list_revocations",
        Request::ListOperations => "list_operations",
        Request::Inspect { .. } => "inspect",
        Request::DrainWorker { .. } => "drain_worker",
        Request::UndrainWorker { .. } => "undrain_worker",
        Request::DrainHost { .. } => "drain_host",
        Request::UncordonHost { .. } => "uncordon_host",
        Request::ReimageHost { .. } => "reimage_host",
        Request::KillOperation { .. } => "kill_operation",
        Request::RevokeServiceKey { .. } => "revoke_service_key",
    }
}

impl FakeManagement {
    /// Static fixtures (tests).
    pub fn new() -> FakeManagement {
        Self::at(T0, false)
    }

    /// Fixtures dated now plus a scripted load that changes every 2 s (demo mode).
    pub fn demo() -> FakeManagement {
        Self::at(crate::util::now_unix(), true)
    }

    /// Fixtures at [`T0_MS`] plus the scripted load (deterministic demo with a
    /// frozen clock: each 2 s step always looks the same).
    pub fn scripted() -> FakeManagement {
        Self::at(T0, true)
    }

    fn at(t0: i64, live: bool) -> FakeManagement {
        FakeManagement {
            data: Arc::new(Mutex::new(fixtures(t0))),
            live,
            t0,
        }
    }

    fn data(&self) -> std::sync::MutexGuard<'_, Data> {
        self.data.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Every mutating request served so far, in order.
    pub fn mutations(&self) -> Vec<Request> {
        self.data().mutations.clone()
    }

    /// Makes the next request of `kind` (see [`kind`]) fail with `error`.
    pub fn fail_next(&self, kind: &str, error: ApiError) {
        self.data().fail.insert(kind.to_string(), error);
    }

    /// The current operations.
    pub fn operations(&self) -> Vec<pb::OperationSummary> {
        self.data().ops.clone()
    }

    /// The overview at demo step `step` (step 0: the fixture state at [`T0_MS`]).
    pub fn overview(&self, step: u64) -> pb::Overview {
        let d = self.data();
        let t0 = self.t0;
        let t = t0 + 2 * step as i64;
        let peak = |p: u32| if step == 0 { p } else { wave(step, p) };
        let linux_q = peak(14);
        let linux_x = if step == 0 {
            48
        } else {
            8 + wave(step + 2, 56)
        };
        let win_q = if step == 0 {
            3
        } else {
            [3, 3, 2, 0, 0, 0, 0, 1][(step % 8) as usize]
        };
        let mac_x = if step == 0 { 6 } else { 2 + wave(step + 5, 6) };
        let qsum = |q: &[(&str, &str)],
                    pool: &str,
                    queued: u32,
                    executing: u32,
                    total: u32,
                    oldest_ms: i64,
                    p95_ms: i64| {
            pb::QueueSummary {
                queue: queue(q).into(),
                pool: pool.into(),
                queued,
                executing,
                idle_workers: total.saturating_sub(executing),
                total_workers: total,
                drains: 0,
                oldest_queued_age: dur_ms(if queued > 0 { oldest_ms } else { 0 }),
                queue_time_p95: dur_ms(p95_ms),
                ..Default::default()
            }
        };
        let linux_vms = if step == 0 {
            2
        } else {
            2 + u32::from(linux_q > 10)
        };
        let pools = vec![
            pool("linux-aarch64", "ec2", 4, 0, 0, 0, 0, "g3"),
            pool(
                "linux-x86-64",
                "ec2",
                8,
                linux_vms + 1,
                1,
                linux_vms,
                0,
                "g7",
            ),
            pool("macos-arm64-xcode27.0", "tart", 4, 2, 0, 1, 1, "g2"),
            pool(
                "windows-x86-64",
                "ec2",
                4,
                u32::from(win_q > 0),
                u32::from(win_q > 0),
                0,
                0,
                "g5",
            ),
        ];
        let mut workers_by_state: HashMap<String, u32> = HashMap::new();
        for w in &d.workers {
            *workers_by_state.entry(w.state.clone()).or_default() += 1;
        }
        let mut hosts: Vec<pb::HostSummary> = d
            .hosts
            .iter()
            .filter_map(|h| h.summary.as_option().cloned())
            .collect();
        for h in &mut hosts {
            if h.phase == "Online" {
                h.wan_bytes_received +=
                    i64::try_from(step).unwrap_or(0) * 7 * 1_048_576 * i64::from(h.running_vms);
                h.last_heartbeat = ts(t - 3);
            }
        }
        let mut alerts = vec![pb::Alert {
            name: "HostOffline".into(),
            severity: "critical".into(),
            summary: "mini-3 has not sent a heartbeat for 6 minutes".into(),
            since: ts(t0 - 6 * 60),
            labels: [("host".to_string(), "mini-3".to_string())]
                .into_iter()
                .collect(),
            ..Default::default()
        }];
        if win_q > 0 {
            alerts.push(pb::Alert {
                name: "QueueTimeHigh".into(),
                severity: "warning".into(),
                summary: "windows queue time above 30s (cold start)".into(),
                since: ts(t0 - 20),
                labels: [("pool".to_string(), "windows-x86-64".to_string())]
                    .into_iter()
                    .collect(),
                ..Default::default()
            });
        }
        pb::Overview {
            time: ts(t),
            pools,
            queues: vec![
                qsum(LINUX, "linux-x86-64", linux_q, linux_x, 64, 9_000, 2_100),
                qsum(RISCV, "linux-x86-64", peak(2), 4, 8, 3_000, 900),
                qsum(ARM, "linux-aarch64", 0, 0, 0, 0, 0),
                qsum(WINDOWS, "windows-x86-64", win_q, 0, 0, 47_000, 0),
                qsum(MACOS, "macos-arm64-xcode27.0", 0, mac_x, 8, 0, 400),
            ],
            hosts,
            cost: pb::CostSummary {
                today: money(12_400_000 + i64::try_from(step).unwrap_or(0) * 20_000),
                month_to_date: money(186_200_000 + i64::try_from(step).unwrap_or(0) * 20_000),
                standing_per_month: money(9_500_000),
                pools: cost_pools(),
                ..Default::default()
            }
            .into(),
            alerts,
            workers_by_state: workers_by_state.into_iter().collect(),
            recent_starts: vec![
                start(
                    t0,
                    "linux-x86-64",
                    "i-0a1b2c3d4e5f60719",
                    9 * 60,
                    18_400,
                    36_900,
                    39_200,
                    "slow",
                ),
                start(
                    t0,
                    "linux-x86-64",
                    "i-0a1b2c3d4e5f60718",
                    14 * 60,
                    17_100,
                    34_000,
                    37_800,
                    "slow",
                ),
                start(
                    t0,
                    "macos-arm64-xcode27.0",
                    "mini-1/vm-a",
                    3 * 3600,
                    9_800,
                    21_500,
                    24_100,
                    "tart",
                ),
            ],
            ..Default::default()
        }
    }

    /// Answers `request` immediately (the [`Management::call`] future is this, ready).
    pub fn respond(&self, request: &Request) -> Result<Reply, ApiError> {
        let mut d = self.data();
        if let Some(e) = d.fail.remove(kind(request)) {
            return Err(e);
        }
        if request.is_mutating() {
            d.mutations.push(request.clone());
        }
        let not_found =
            |what: &str| ApiError::new(Some("not_found"), format!("not_found: {what} not found"));
        Ok(match request {
            Request::GetPool { name } => {
                drop(d);
                let o = self.overview(0);
                let d = self.data();
                let summary = o
                    .pools
                    .into_iter()
                    .find(|p| &p.name == name)
                    .ok_or_else(|| not_found(&format!("pool {name}")))?;
                Reply::Pool(Box::new(pb::GetPoolResponse {
                    summary: summary.into(),
                    spec_json: format!(r#"{{"platform":"{name}"}}"#),
                    conditions: vec!["Ready=True Reconciled: ok".into()],
                    workers: d
                        .workers
                        .iter()
                        .filter(|w| &w.pool == name)
                        .cloned()
                        .collect(),
                    events: pool_events(self.t0, name),
                    starts: o
                        .recent_starts
                        .into_iter()
                        .filter(|s| &s.pool == name)
                        .collect(),
                    ..Default::default()
                }))
            }
            Request::ListHosts => Reply::Hosts(d.hosts.clone()),
            Request::GetCost => Reply::Cost(Box::new(pb::GetCostResponse {
                cost: pb::CostSummary {
                    today: money(12_400_000),
                    month_to_date: money(186_200_000),
                    standing_per_month: money(9_500_000),
                    pools: cost_pools(),
                    ..Default::default()
                }
                .into(),
                lines: cost_lines(),
                assumptions: "us-west-1 on-demand prices of 2026-10-01; 60 s minimum".into(),
                ..Default::default()
            })),
            Request::ListServiceKeys => Reply::ServiceKeys(d.keys.clone()),
            Request::ListRevocations => Reply::Revocations(d.revocations.clone()),
            Request::ListOperations => Reply::Operations(d.ops.clone()),
            Request::Inspect { subject } => {
                let op = d
                    .ops
                    .iter()
                    .find(|o| &o.name == subject)
                    .cloned()
                    .ok_or_else(|| not_found(&format!("operation {subject}")))?;
                Reply::Action(Box::new(action(&op)))
            }
            Request::DrainWorker { node } | Request::UndrainWorker { node } => {
                let drain = matches!(request, Request::DrainWorker { .. });
                let w = d
                    .workers
                    .iter_mut()
                    .find(|w| &w.node == node)
                    .ok_or_else(|| not_found(&format!("worker {node}")))?;
                w.drained = drain;
                Reply::Done(if drain {
                    format!("worker {node} is draining: no new actions; running actions finish")
                } else {
                    format!("worker {node} accepts actions again")
                })
            }
            Request::DrainHost { serial } | Request::UncordonHost { serial } => {
                let cordon = matches!(request, Request::DrainHost { .. });
                let h = d
                    .hosts
                    .iter_mut()
                    .find(|h| h.summary.as_option().is_some_and(|s| &s.serial == serial))
                    .ok_or_else(|| not_found(&format!("host {serial}")))?;
                h.cordoned = cordon;
                Reply::Done(if cordon {
                    format!("host {serial} cordoned: its VMs finish their actions and shut down")
                } else {
                    format!("host {serial} uncordoned")
                })
            }
            Request::ReimageHost { serial } => Reply::Done(format!(
                "every VM of host {serial} is re-cloned from its golden image at its next start"
            )),
            Request::KillOperation { name } => {
                let before = d.ops.len();
                d.ops.retain(|o| &o.name != name);
                if d.ops.len() == before {
                    return Err(not_found(&format!("operation {name}")));
                }
                Reply::Done(format!("operation {name} killed"))
            }
            Request::RevokeServiceKey { key_id } => {
                let k = d
                    .keys
                    .iter_mut()
                    .find(|k| &k.key_id == key_id)
                    .ok_or_else(|| not_found(&format!("key {key_id}")))?;
                k.revoked = true;
                Reply::Done(format!("service key {key_id} revoked"))
            }
        })
    }

    /// Feeds `event` to the reducer and serves every resulting call synchronously
    /// until nothing is left to do (tests).
    pub fn drive(&self, state: &mut State, event: Event) {
        let mut queue = VecDeque::from([event]);
        while let Some(ev) = queue.pop_front() {
            for fx in update(state, ev) {
                if let Effect::Call { id, request } = fx {
                    let result = self.respond(&request);
                    queue.push_back(Event::Reply {
                        id,
                        request,
                        result,
                    });
                }
            }
        }
    }

    /// Brings `state` to the fixture moment: the clock at the fixtures' time, a
    /// short load history (sparkline), the step-0 overview and every operation.
    pub fn prime(&self, state: &mut State) {
        self.drive(
            state,
            Event::Tick {
                now_ms: self.t0 * 1000,
            },
        );
        for step in (0..=12).rev() {
            let o = self.overview(step);
            self.drive(state, Event::Overview(Box::new(StreamEvent::Data(o))));
        }
        for op in self.operations() {
            let ev = pb::OperationEvent {
                kind: pb::operation_event::Kind::KIND_ADDED.into(),
                operation: op.into(),
                ..Default::default()
            };
            self.drive(state, Event::Operations(Box::new(StreamEvent::Data(ev))));
        }
    }

    /// Feeds every event of `events` through [`FakeManagement::drive`].
    pub fn drive_all(&self, state: &mut State, events: impl IntoIterator<Item = Event>) {
        for e in events {
            self.drive(state, e);
        }
    }

    /// Operation events at demo step `step` (> 0): one operation finishes, one
    /// starts executing and one is queued.
    fn op_events(&self, step: u64) -> Vec<pb::OperationEvent> {
        let mut d = self.data();
        let ev = |kind: pb::operation_event::Kind, op: pb::OperationSummary| pb::OperationEvent {
            kind: kind.into(),
            operation: op.into(),
            ..Default::default()
        };
        let mut out = Vec::new();
        if let Some(i) = d.ops.iter().position(|o| o.stage == "executing") {
            let done = d.ops.remove(i);
            out.push(ev(pb::operation_event::Kind::KIND_REMOVED, done));
        }
        if let Some(o) = d.ops.iter_mut().find(|o| o.stage == "queued") {
            o.stage = "executing".into();
            o.worker_node = "i-0a1b2c3d4e5f60719".into();
            out.push(ev(pb::operation_event::Kind::KIND_CHANGED, o.clone()));
        }
        let t0 = self.t0;
        let n = 100 + u32::try_from(step).unwrap_or(0);
        let targets = [
            ("//absl/strings:cord_test", LINUX),
            ("//absl/hash:hash_test", LINUX),
            ("//absl/time:time_zone_test", WINDOWS),
            ("//absl/log:log_basic_test", MACOS),
            ("//absl/numeric:bits_test", RISCV),
        ];
        let (target, q) = targets[(step % targets.len() as u64) as usize];
        let mut new = op(t0, n, "queued", q, target, 5 + (n / 7), "", 0);
        new.queued_at = ts(t0 + 2 * step as i64);
        d.ops.push(new.clone());
        out.push(ev(pb::operation_event::Kind::KIND_ADDED, new));
        out
    }
}

/// Parses a key script for tests: whitespace-separated tokens, each a key name
/// (`Enter`, `Esc`, `Tab`, `BackTab`, `Up`, `Down`, `Left`, `Right`, `PgUp`, `PgDn`,
/// `Home`, `End`, `Backspace`, `Space`, `C-c`) or text typed character by character
/// (`2`, `d`, `/platform:linux`).
pub fn keys(script: &str) -> Vec<Event> {
    use ratatui::crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
    let key = |code| Event::Key(KeyEvent::new(code, KeyModifiers::NONE));
    let mut out = Vec::new();
    for tok in script.split_whitespace() {
        let code = match tok {
            "Enter" => KeyCode::Enter,
            "Esc" => KeyCode::Esc,
            "Tab" => KeyCode::Tab,
            "BackTab" => KeyCode::BackTab,
            "Up" => KeyCode::Up,
            "Down" => KeyCode::Down,
            "Left" => KeyCode::Left,
            "Right" => KeyCode::Right,
            "PgUp" => KeyCode::PageUp,
            "PgDn" => KeyCode::PageDown,
            "Home" => KeyCode::Home,
            "End" => KeyCode::End,
            "Backspace" => KeyCode::Backspace,
            "Space" => KeyCode::Char(' '),
            "C-c" => {
                out.push(Event::Key(KeyEvent::new(
                    KeyCode::Char('c'),
                    KeyModifiers::CONTROL,
                )));
                continue;
            }
            text => {
                out.extend(text.chars().map(|c| key(KeyCode::Char(c))));
                continue;
            }
        };
        out.push(key(code));
    }
    out
}

#[allow(clippy::too_many_arguments)]
fn start(
    t0: i64,
    pool: &str,
    vm: &str,
    ago: i64,
    run: i64,
    reg: i64,
    first: i64,
    path: &str,
) -> pb::StartLatency {
    pb::StartLatency {
        pool: pool.into(),
        vm: vm.into(),
        launched: ts(t0 - ago),
        to_running: dur_ms(run),
        to_registered: dur_ms(reg),
        to_first_action: dur_ms(first),
        path: path.into(),
        ..Default::default()
    }
}

fn pool_events(t0: i64, pool: &str) -> Vec<pb::PoolEvent> {
    let ev = |ago: i64, kind: &str, subject: &str, message: &str| pb::PoolEvent {
        time: ts(t0 - ago),
        r#type: kind.into(),
        subject: subject.into(),
        message: message.into(),
        ..Default::default()
    };
    match pool {
        "linux-x86-64" => vec![
            ev(
                20,
                "launch",
                "i-0a1b2c3d4e5f6071a",
                "queue 14 > capacity: desired 3",
            ),
            ev(
                8 * 60 + 23,
                "register",
                "i-0a1b2c3d4e5f60719",
                "first action after 39s",
            ),
            ev(
                9 * 60,
                "launch",
                "i-0a1b2c3d4e5f60719",
                "c8i.8xlarge in us-west-1b",
            ),
            ev(
                13 * 60 + 22,
                "register",
                "i-0a1b2c3d4e5f60718",
                "first action after 38s",
            ),
            ev(14 * 60, "launch", "i-0a1b2c3d4e5f60718", "scale from zero"),
            ev(52 * 60, "terminate", "i-0a1b2c3d4e5f60601", "idle 10m"),
        ],
        "windows-x86-64" => vec![ev(
            50,
            "launch",
            "i-0f9e8d7c6b5a40312",
            "fast-launch snapshot",
        )],
        "macos-arm64-xcode27.0" => vec![ev(3 * 3600, "register", "mini-1/vm-a", "VM started")],
        _ => vec![ev(2 * 3600, "terminate", "i-0c0ffee000000a64", "idle 10m")],
    }
}

fn pool_cost(
    pool: &str,
    hours: f64,
    compute: i64,
    ebs: i64,
    ipv4: i64,
    standing: i64,
) -> pb::PoolCost {
    pb::PoolCost {
        pool: pool.into(),
        instance_seconds: (hours * 3600.0) as i64,
        compute: money(compute),
        ebs: money(ebs),
        data_transfer: money(0),
        standing: money(standing),
        public_ipv4: money(ipv4),
        ..Default::default()
    }
}

fn cost_pools() -> Vec<pb::PoolCost> {
    vec![
        pool_cost("linux-x86-64", 11.5, 8_970_000, 310_000, 58_000, 1_100_000),
        pool_cost("windows-x86-64", 2.1, 2_620_000, 140_000, 11_000, 7_300_000),
        pool_cost("linux-aarch64", 0.4, 290_000, 12_000, 2_000, 1_100_000),
        pool_cost("macos-arm64-xcode27.0", 0.0, 0, 0, 0, 0),
    ]
}

fn cost_lines() -> Vec<pb::CostLine> {
    let line =
        |pool: &str, category: &str, detail: &str, quantity: f64, unit: &str, micros: i64| {
            pb::CostLine {
                pool: pool.into(),
                category: category.into(),
                detail: detail.into(),
                quantity,
                unit: unit.into(),
                amount: money(micros),
                ..Default::default()
            }
        };
    vec![
        line(
            "linux-x86-64",
            "compute",
            "c8i.8xlarge",
            11.5,
            "instance-hours",
            8_970_000,
        ),
        line(
            "linux-x86-64",
            "ebs",
            "gp3 root 30 GiB",
            345.0,
            "GiB-hours",
            310_000,
        ),
        line(
            "linux-x86-64",
            "public-ipv4",
            "IPv4 address",
            11.5,
            "hours",
            58_000,
        ),
        line(
            "linux-x86-64",
            "ami-storage",
            "2 AMIs (current + previous)",
            2.0,
            "snapshots/month",
            1_100_000,
        ),
        line(
            "windows-x86-64",
            "compute",
            "c7a.4xlarge",
            2.1,
            "instance-hours",
            2_620_000,
        ),
        line(
            "windows-x86-64",
            "ebs",
            "gp3 root 60 GiB",
            126.0,
            "GiB-hours",
            140_000,
        ),
        line(
            "windows-x86-64",
            "fast-launch",
            "4 pre-provisioned snapshots",
            4.0,
            "snapshots/month",
            6_200_000,
        ),
        line(
            "windows-x86-64",
            "ami-storage",
            "2 AMIs",
            2.0,
            "snapshots/month",
            1_100_000,
        ),
        line(
            "linux-aarch64",
            "compute",
            "c8g.8xlarge",
            0.4,
            "instance-hours",
            290_000,
        ),
        line(
            "linux-aarch64",
            "ami-storage",
            "2 AMIs",
            2.0,
            "snapshots/month",
            1_100_000,
        ),
    ]
}

fn action(op: &pb::OperationSummary) -> ActionView {
    let q = op.queue.as_option().cloned().unwrap_or_default();
    let platform: std::collections::BTreeMap<String, String> = q
        .platform
        .iter()
        .map(|p| (p.name.clone(), p.value.clone()))
        .collect();
    let src = op.target_id.trim_start_matches("//").replace(':', "/");
    let obj = format!("bazel-out/k8-fastbuild/bin/{src}.o");
    let entry = |path: &str, kind: &'static str, size: u64| TreeEntry {
        path: path.into(),
        kind,
        digest: None,
        size_bytes: size,
        executable: false,
        target: None,
    };
    ActionView {
        instance_name: q.instance_name_prefix.clone(),
        action_digest: op.action_digest.clone(),
        operation: Some(OperationInfo {
            name: op.name.clone(),
            stage: op.stage.clone(),
            queued_at: crate::util::proto_time(op.queued_at.as_option()),
            instance_name: q.instance_name_prefix.clone(),
            platform: platform.clone(),
            size_class: q.size_class,
            invocation_id: op.invocation_id.clone(),
            target_id: op.target_id.clone(),
            worker_node: op.worker_node.clone(),
            worker_thread: op.worker_thread,
        }),
        command: CommandInfo {
            arguments: vec![
                "external/llvm/bin/clang++".into(),
                "-std=c++17".into(),
                "-O0".into(),
                "-g".into(),
                "-iquote".into(),
                ".".into(),
                "-MD".into(),
                "-MF".into(),
                obj.replace(".o", ".d"),
                "-c".into(),
                format!("{src}.cc"),
                "-o".into(),
                obj.clone(),
            ],
            environment: [
                ("PATH".to_string(), "/bin:/usr/bin".to_string()),
                ("PWD".to_string(), "/proc/self/cwd".to_string()),
            ]
            .into_iter()
            .collect(),
            working_directory: String::new(),
            output_paths: vec![obj.clone(), obj.replace(".o", ".d")],
        },
        platform,
        timeout_seconds: Some(900.0),
        do_not_cache: false,
        input_root: TreeListing {
            root_digest: format!("{}/{}", digest(900), 4_288),
            files: 412,
            directories: 37,
            symlinks: 0,
            total_file_bytes: 19_084_431,
            truncated: true,
            entries: vec![
                entry("absl", "directory", 0),
                entry("absl/base", "directory", 0),
                entry("absl/base/config.h", "file", 41_210),
                entry("absl/base/macros.h", "file", 6_015),
                entry(&format!("{src}.cc"), "file", 8_744),
                entry("external/llvm/bin/clang++", "file", 98_304_221),
            ],
        },
        result: Some(ResultInfo {
            source: "action-cache".into(),
            exit_code: 0,
            worker: op.worker_node.clone(),
            output_files: vec![
                OutputFile {
                    path: obj.clone(),
                    digest: Some(format!("{}/186412", digest(901))),
                    size_bytes: 186_412,
                    executable: false,
                },
                OutputFile {
                    path: obj.replace(".o", ".d"),
                    digest: Some(format!("{}/2048", digest(902))),
                    size_bytes: 2_048,
                    executable: false,
                },
            ],
            output_directories: vec![],
            output_symlinks: vec![],
            stdout: None,
            stderr: Some(StreamText {
                digest: None,
                size_bytes: 61,
                text: format!("{src}.cc:42:7: warning: unused variable 'n' [-Wunused-variable]\n"),
                truncated: false,
            }),
            timing: Timing {
                queued_at: crate::util::proto_time(op.queued_at.as_option()),
                worker_start: None,
                worker_completed: None,
                queue_seconds: Some(0.4),
                input_fetch_seconds: Some(0.2),
                execution_seconds: Some(3.1),
                output_upload_seconds: Some(0.1),
                total_seconds: Some(3.9),
            },
        }),
        message: None,
    }
}

impl Management for FakeManagement {
    fn watch_overview(&self) -> BoxStream<'static, StreamEvent<pb::Overview>> {
        let me = self.clone();
        if !self.live {
            return futures::stream::once(async move { StreamEvent::Data(me.overview(0)) })
                .chain(futures::stream::pending())
                .boxed();
        }
        futures::stream::unfold(0u64, move |step| {
            let me = me.clone();
            async move {
                if step > 0 {
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
                Some((StreamEvent::Data(me.overview(step)), step + 1))
            }
        })
        .boxed()
    }

    fn watch_operations(&self) -> BoxStream<'static, StreamEvent<pb::OperationEvent>> {
        let me = self.clone();
        let added = move |me: &FakeManagement| {
            me.operations()
                .into_iter()
                .map(|o| {
                    StreamEvent::Data(pb::OperationEvent {
                        kind: pb::operation_event::Kind::KIND_ADDED.into(),
                        operation: o.into(),
                        ..Default::default()
                    })
                })
                .collect::<Vec<_>>()
        };
        let first = futures::stream::iter(added(&me));
        if !self.live {
            return first.chain(futures::stream::pending()).boxed();
        }
        let ticks = futures::stream::unfold(1u64, move |step| {
            let me = me.clone();
            async move {
                tokio::time::sleep(Duration::from_secs(2)).await;
                let events: Vec<_> = me
                    .op_events(step)
                    .into_iter()
                    .map(StreamEvent::Data)
                    .collect();
                Some((futures::stream::iter(events), step + 1))
            }
        })
        .flatten();
        first.chain(ticks).boxed()
    }

    fn call(&self, request: Request) -> BoxFuture<'static, Result<Reply, ApiError>> {
        let me = self.clone();
        async move {
            if me.live {
                // A little latency so loading states are visible in the demo.
                tokio::time::sleep(Duration::from_millis(150)).await;
            }
            me.respond(&request)
        }
        .boxed()
    }
}
