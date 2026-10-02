// SPDX-License-Identifier: FSL-1.1-ALv2

//! Output documents of the management commands. The JSON field names are a stable,
//! documented contract (`schemas/*.schema.json`, contract-tested); they are decoupled
//! from the protobuf messages on purpose. Conventions: timestamps are RFC 3339 UTC
//! strings (or `null`), durations are seconds (numbers), money is integer
//! micro-dollars in `*_usd_micros` fields.

use std::collections::BTreeMap;
use std::io::{self, Write};

use cucina_api::proto::cucina::v1 as pb;
use serde::Serialize;

use crate::cells;
use crate::output::{self, Render, dash, state, state_cell, table, usd, write_fields, write_table};
use crate::util::{human_secs, proto_secs, proto_time};

fn money(m: Option<&pb::Money>) -> i64 {
    m.map_or(0, |m| m.micros)
}

fn platform_map(p: &[pb::PlatformProperty]) -> BTreeMap<String, String> {
    p.iter()
        .map(|kv| (kv.name.clone(), kv.value.clone()))
        .collect()
}

fn platform_text(p: &BTreeMap<String, String>) -> String {
    let v: Vec<String> = p.iter().map(|(k, v)| format!("{k}={v}")).collect();
    dash(&v.join(","))
}

// ------------------------------------------------------------------ generic result

/// `result.v1`: acknowledgement of a mutating command.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ResultView {
    pub ok: bool,
    pub action: String,
    pub target: String,
    pub message: String,
}

impl ResultView {
    pub fn new(action: &str, target: &str, message: impl Into<String>) -> ResultView {
        ResultView {
            ok: true,
            action: action.into(),
            target: target.into(),
            message: message.into(),
        }
    }
}

impl Render for ResultView {
    const SCHEMA: &'static str = "result.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        if self.message.is_empty() {
            writeln!(out, "{} {}: done", self.action, self.target)
        } else {
            writeln!(out, "{} {}: {}", self.action, self.target, self.message)
        }
    }
}

// ------------------------------------------------------------------------ status

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ComponentView {
    pub name: String,
    pub state: String,
    pub message: String,
    pub ready_replicas: u32,
    pub desired_replicas: u32,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct AlertView {
    pub name: String,
    pub severity: String,
    pub summary: String,
    pub since: Option<String>,
    pub labels: BTreeMap<String, String>,
}

impl From<&pb::Alert> for AlertView {
    fn from(a: &pb::Alert) -> Self {
        AlertView {
            name: a.name.clone(),
            severity: a.severity.clone(),
            summary: a.summary.clone(),
            since: proto_time(a.since.as_option()),
            labels: a
                .labels
                .iter()
                .map(|(k, v)| (k.clone(), v.clone()))
                .collect(),
        }
    }
}

/// `status.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct StatusView {
    pub cluster_id: String,
    pub version: String,
    pub protocol: String,
    pub components: Vec<ComponentView>,
    pub pools: Vec<PoolView>,
    pub hosts_online: u32,
    pub hosts_total: u32,
    pub worker_instances: u32,
    pub alerts: Vec<AlertView>,
}

impl From<&pb::GetStatusResponse> for StatusView {
    fn from(r: &pb::GetStatusResponse) -> Self {
        StatusView {
            cluster_id: r.cluster_id.clone(),
            version: r.version.clone(),
            protocol: r
                .protocol
                .as_option()
                .map(|p| format!("{}.{}", p.major, p.minor))
                .unwrap_or_default(),
            components: r
                .components
                .iter()
                .map(|c| ComponentView {
                    name: c.name.clone(),
                    state: c.state.clone(),
                    message: c.message.clone(),
                    ready_replicas: c.ready_replicas,
                    desired_replicas: c.desired_replicas,
                })
                .collect(),
            pools: r.pools.iter().map(PoolView::from).collect(),
            hosts_online: r.hosts_online,
            hosts_total: r.hosts_total,
            worker_instances: r.worker_instances,
            alerts: r.alerts.iter().map(AlertView::from).collect(),
        }
    }
}

impl Render for StatusView {
    const SCHEMA: &'static str = "status.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write_fields(
            out,
            &[
                ("Cluster", dash(&self.cluster_id)),
                (
                    "Version",
                    format!("{} (API {})", dash(&self.version), dash(&self.protocol)),
                ),
                (
                    "Hosts",
                    format!("{}/{} online", self.hosts_online, self.hosts_total),
                ),
                ("Worker instances", self.worker_instances.to_string()),
            ],
        )?;
        writeln!(out)?;
        let mut t = table(&["component", "state", "ready", "message"]);
        for c in &self.components {
            t.add_row(cells![
                c.name.clone(),
                state_cell(&c.state),
                format!("{}/{}", c.ready_replicas, c.desired_replicas),
                dash(&c.message),
            ]);
        }
        write_table(out, &t, "no components reported")?;
        writeln!(out)?;
        write_pools(out, &self.pools)?;
        if !self.alerts.is_empty() {
            writeln!(out)?;
            write_alerts(out, &self.alerts)?;
        }
        Ok(())
    }
}

fn write_alerts(out: &mut dyn Write, alerts: &[AlertView]) -> io::Result<()> {
    let mut t = table(&["alert", "severity", "since", "summary"]);
    for a in alerts {
        t.add_row(cells![
            a.name.clone(),
            state_cell(&a.severity),
            a.since.clone().unwrap_or_else(|| "-".into()),
            a.summary.clone(),
        ]);
    }
    write_table(out, &t, "no alerts")
}

// ------------------------------------------------------------------------- pools

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolView {
    pub name: String,
    pub platform: String,
    pub provider: String,
    pub min_running: u32,
    pub max: u32,
    pub desired: u32,
    pub launching: u32,
    pub registered: u32,
    pub busy: u32,
    pub idle: u32,
    pub draining: u32,
    pub stopped: u32,
    pub image_generation: String,
    pub paused: bool,
    pub condition: String,
    pub message: String,
}

impl From<&pb::PoolSummary> for PoolView {
    fn from(p: &pb::PoolSummary) -> Self {
        PoolView {
            name: p.name.clone(),
            platform: p.platform.clone(),
            provider: p.provider.clone(),
            min_running: p.min_running,
            max: p.max,
            desired: p.desired,
            launching: p.launching,
            registered: p.registered,
            busy: p.busy,
            idle: p.idle,
            draining: p.draining,
            stopped: p.stopped,
            image_generation: p.image_generation.clone(),
            paused: p.paused,
            condition: p.condition.clone(),
            message: p.message.clone(),
        }
    }
}

fn write_pools(out: &mut dyn Write, pools: &[PoolView]) -> io::Result<()> {
    let mut t = table(&[
        "pool",
        "provider",
        "min",
        "max",
        "desired",
        "launching",
        "ready",
        "busy",
        "draining",
        "condition",
    ]);
    for p in pools {
        t.add_row(cells![
            format!("{}{}", p.name, if p.paused { " (cordoned)" } else { "" }),
            p.provider.clone(),
            p.min_running.to_string(),
            p.max.to_string(),
            p.desired.to_string(),
            p.launching.to_string(),
            (p.registered + p.idle).to_string(),
            p.busy.to_string(),
            p.draining.to_string(),
            state_cell(&dash(&p.condition)),
        ]);
    }
    write_table(out, &t, "no pools")
}

/// `pool-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolListView {
    pub pools: Vec<PoolView>,
}

impl Render for PoolListView {
    const SCHEMA: &'static str = "pool-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write_pools(out, &self.pools)
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct WorkerView {
    pub node: String,
    pub pool: String,
    pub state: String,
    pub generation: String,
    pub instance_type: String,
    pub host: String,
    pub threads: u32,
    pub busy_threads: u32,
    pub drained: bool,
    pub launched: Option<String>,
    pub idle_seconds: Option<f64>,
    pub private_ip: String,
}

impl From<&pb::WorkerSummary> for WorkerView {
    fn from(w: &pb::WorkerSummary) -> Self {
        WorkerView {
            node: w.node.clone(),
            pool: w.pool.clone(),
            state: w.state.clone(),
            generation: w.generation.clone(),
            instance_type: w.instance_type.clone(),
            host: w.host.clone(),
            threads: w.threads,
            busy_threads: w.busy_threads,
            drained: w.drained,
            launched: proto_time(w.launched.as_option()),
            idle_seconds: proto_secs(w.idle_for.as_option()),
            private_ip: w.private_ip.clone(),
        }
    }
}

fn write_workers(out: &mut dyn Write, workers: &[WorkerView]) -> io::Result<()> {
    let mut t = table(&[
        "node",
        "pool",
        "state",
        "busy",
        "type",
        "generation",
        "idle",
        "launched",
    ]);
    for w in workers {
        t.add_row(cells![
            format!("{}{}", w.node, if w.drained { " (drained)" } else { "" }),
            w.pool.clone(),
            state_cell(&w.state),
            format!("{}/{}", w.busy_threads, w.threads),
            dash(&w.instance_type),
            dash(&w.generation),
            human_secs(w.idle_seconds),
            w.launched.clone().unwrap_or_else(|| "-".into()),
        ]);
    }
    write_table(out, &t, "no workers")
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolEventView {
    pub time: Option<String>,
    #[serde(rename = "type")]
    pub kind: String,
    pub subject: String,
    pub message: String,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct StartView {
    pub pool: String,
    pub vm: String,
    pub launched: Option<String>,
    pub to_running_seconds: Option<f64>,
    pub to_registered_seconds: Option<f64>,
    pub to_first_action_seconds: Option<f64>,
    pub path: String,
}

impl From<&pb::StartLatency> for StartView {
    fn from(s: &pb::StartLatency) -> Self {
        StartView {
            pool: s.pool.clone(),
            vm: s.vm.clone(),
            launched: proto_time(s.launched.as_option()),
            to_running_seconds: proto_secs(s.to_running.as_option()),
            to_registered_seconds: proto_secs(s.to_registered.as_option()),
            to_first_action_seconds: proto_secs(s.to_first_action.as_option()),
            path: s.path.clone(),
        }
    }
}

/// `pool-describe.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolDescribeView {
    pub pool: PoolView,
    /// The WorkerPool spec as JSON (as returned by the controller).
    pub spec: serde_json::Value,
    pub conditions: Vec<String>,
    pub workers: Vec<WorkerView>,
    pub events: Vec<PoolEventView>,
    pub starts: Vec<StartView>,
}

impl From<&pb::GetPoolResponse> for PoolDescribeView {
    fn from(r: &pb::GetPoolResponse) -> Self {
        PoolDescribeView {
            pool: r
                .summary
                .as_option()
                .map(PoolView::from)
                .unwrap_or_else(|| PoolView::from(&pb::PoolSummary::default())),
            spec: serde_json::from_str(&r.spec_json).unwrap_or(serde_json::Value::Null),
            conditions: r.conditions.clone(),
            workers: r.workers.iter().map(WorkerView::from).collect(),
            events: r
                .events
                .iter()
                .map(|e| PoolEventView {
                    time: proto_time(e.time.as_option()),
                    kind: e.r#type.clone(),
                    subject: e.subject.clone(),
                    message: e.message.clone(),
                })
                .collect(),
            starts: r.starts.iter().map(StartView::from).collect(),
        }
    }
}

impl Render for PoolDescribeView {
    const SCHEMA: &'static str = "pool-describe.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let p = &self.pool;
        write_fields(
            out,
            &[
                ("Pool", p.name.clone()),
                ("Platform", p.platform.clone()),
                ("Provider", p.provider.clone()),
                (
                    "Size",
                    format!(
                        "min {} / max {} / desired {}",
                        p.min_running, p.max, p.desired
                    ),
                ),
                (
                    "Workers",
                    format!(
                        "{} launching, {} registered, {} busy, {} idle, {} draining, {} stopped",
                        p.launching, p.registered, p.busy, p.idle, p.draining, p.stopped
                    ),
                ),
                ("Image generation", dash(&p.image_generation)),
                ("Cordoned", p.paused.to_string()),
                (
                    "Condition",
                    format!("{} {}", state(&dash(&p.condition)), p.message),
                ),
            ],
        )?;
        if !self.conditions.is_empty() {
            output::heading(out, "Conditions")?;
            for c in &self.conditions {
                writeln!(out, "  {c}")?;
            }
        }
        output::heading(out, "Workers")?;
        write_workers(out, &self.workers)?;
        output::heading(out, "Recent events")?;
        let mut t = table(&["time", "type", "subject", "message"]);
        for e in self.events.iter().take(20) {
            t.add_row(vec![
                e.time.clone().unwrap_or_else(|| "-".into()),
                e.kind.clone(),
                dash(&e.subject),
                e.message.clone(),
            ]);
        }
        write_table(out, &t, "no events")?;
        output::heading(out, "Cold starts")?;
        let mut t = table(&[
            "vm",
            "launched",
            "running",
            "registered",
            "first action",
            "path",
        ]);
        for s in self.starts.iter().take(10) {
            t.add_row(vec![
                s.vm.clone(),
                s.launched.clone().unwrap_or_else(|| "-".into()),
                human_secs(s.to_running_seconds),
                human_secs(s.to_registered_seconds),
                human_secs(s.to_first_action_seconds),
                dash(&s.path),
            ]);
        }
        write_table(out, &t, "no recent starts")?;
        if !self.spec.is_null() {
            output::heading(out, "Spec")?;
            writeln!(
                out,
                "{}",
                serde_json::to_string_pretty(&self.spec).unwrap_or_default()
            )?;
        }
        Ok(())
    }
}

/// `pool-floor.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolFloorView {
    pub pool: String,
    pub min_running: u32,
    pub expires_at: Option<String>,
}

impl Render for PoolFloorView {
    const SCHEMA: &'static str = "pool-floor.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        writeln!(
            out,
            "pool {}: at least {} running workers until {} (standing cost while it lasts)",
            self.pool,
            self.min_running,
            self.expires_at.as_deref().unwrap_or("-")
        )
    }
}

/// `pool-gc.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolGcView {
    pub pool: String,
    pub dry_run: bool,
    pub deleted: Vec<String>,
    pub errors: Vec<String>,
}

impl Render for PoolGcView {
    const SCHEMA: &'static str = "pool-gc.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let verb = if self.dry_run {
            "would delete"
        } else {
            "deleted"
        };
        if self.deleted.is_empty() {
            writeln!(out, "nothing to delete")?;
        }
        for d in &self.deleted {
            writeln!(out, "{verb} {d}")?;
        }
        for e in &self.errors {
            writeln!(out, "error: {e}")?;
        }
        Ok(())
    }
}

/// `worker-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct WorkerListView {
    pub workers: Vec<WorkerView>,
}

impl Render for WorkerListView {
    const SCHEMA: &'static str = "worker-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write_workers(out, &self.workers)
    }
}

// ------------------------------------------------------------------------- hosts

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct HostVmView {
    pub name: String,
    pub pool: String,
    pub state: String,
    pub image: String,
    pub generation: String,
    pub registered: bool,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct HostView {
    pub serial: String,
    pub name: String,
    pub site: String,
    pub phase: String,
    pub running_vms: u32,
    pub slots: u32,
    pub l2_hit_ratio: String,
    pub wan_bytes_received: i64,
    pub last_heartbeat: Option<String>,
    pub agent_version: String,
    pub macos_version: String,
    pub tart_version: String,
    pub chip: String,
    pub cores: u32,
    pub memory_gib: u32,
    pub disk_free_gib: u32,
    pub vms: Vec<HostVmView>,
    pub images: Vec<String>,
    pub cordoned: bool,
    pub approved: bool,
    pub labels: BTreeMap<String, String>,
    pub cert_expiry: Option<String>,
    pub filevault: String,
}

impl From<&pb::HostDetail> for HostView {
    fn from(h: &pb::HostDetail) -> Self {
        let s = h.summary.as_option().cloned().unwrap_or_default();
        HostView {
            serial: s.serial,
            name: s.name,
            site: s.site,
            phase: s.phase,
            running_vms: s.running_vms,
            slots: s.slots,
            l2_hit_ratio: s.l2_hit_ratio,
            wan_bytes_received: s.wan_bytes_received,
            last_heartbeat: proto_time(s.last_heartbeat.as_option()),
            agent_version: s.agent_version,
            macos_version: h.macos_version.clone(),
            tart_version: h.tart_version.clone(),
            chip: h.chip.clone(),
            cores: h.cores,
            memory_gib: h.memory_gib,
            disk_free_gib: h.disk_free_gib,
            vms: h
                .vms
                .iter()
                .map(|v| HostVmView {
                    name: v.name.clone(),
                    pool: v.pool.clone(),
                    state: v.state.clone(),
                    image: v.image.clone(),
                    generation: v.generation.clone(),
                    registered: v.registered,
                })
                .collect(),
            images: h.images.clone(),
            cordoned: h.cordoned,
            approved: h.approved,
            labels: h
                .labels
                .iter()
                .map(|(k, v)| (k.clone(), v.clone()))
                .collect(),
            cert_expiry: proto_time(h.cert_expiry.as_option()),
            filevault: h.filevault.clone(),
        }
    }
}

/// `host-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct HostListView {
    pub hosts: Vec<HostView>,
}

impl Render for HostListView {
    const SCHEMA: &'static str = "host-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&[
            "serial",
            "name",
            "site",
            "phase",
            "vms",
            "l2 hit",
            "macos",
            "agent",
            "heartbeat",
        ]);
        for h in &self.hosts {
            let mut phase = h.phase.clone();
            if h.cordoned {
                phase.push_str(" (cordoned)");
            }
            if !h.approved {
                phase.push_str(" (pending approval)");
            }
            t.add_row(cells![
                h.serial.clone(),
                dash(&h.name),
                dash(&h.site),
                state_cell(&phase),
                format!("{}/{}", h.running_vms, h.slots),
                dash(&h.l2_hit_ratio),
                dash(&h.macos_version),
                dash(&h.agent_version),
                h.last_heartbeat.clone().unwrap_or_else(|| "-".into()),
            ]);
        }
        write_table(out, &t, "no hosts")
    }
}

/// `host-register.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct HostRegisterView {
    pub registered: Vec<String>,
    pub already_present: Vec<String>,
}

impl Render for HostRegisterView {
    const SCHEMA: &'static str = "host-register.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        for s in &self.registered {
            writeln!(out, "registered {s}")?;
        }
        for s in &self.already_present {
            writeln!(out, "already registered {s}")?;
        }
        Ok(())
    }
}

/// `enroll-token.v1`: a newly created token (the value is shown only here, once).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct EnrollTokenView {
    pub id: String,
    pub token: String,
    pub expires_at: Option<String>,
}

impl Render for EnrollTokenView {
    const SCHEMA: &'static str = "enroll-token.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write_fields(
            out,
            &[
                ("ID", self.id.clone()),
                ("Token", self.token.clone()),
                (
                    "Expires",
                    self.expires_at.clone().unwrap_or_else(|| "-".into()),
                ),
            ],
        )?;
        writeln!(
            out,
            "Store the token in your MDM's managed preferences now; it is not shown again."
        )
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct EnrollTokenInfoView {
    pub id: String,
    pub site: String,
    pub description: String,
    pub created: Option<String>,
    pub expires_at: Option<String>,
    pub max_hosts: u32,
    pub used_hosts: u32,
    pub revoked: bool,
}

/// `enroll-token-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct EnrollTokenListView {
    pub tokens: Vec<EnrollTokenInfoView>,
}

impl From<&pb::ListEnrollTokensResponse> for EnrollTokenListView {
    fn from(r: &pb::ListEnrollTokensResponse) -> Self {
        EnrollTokenListView {
            tokens: r
                .tokens
                .iter()
                .map(|t| EnrollTokenInfoView {
                    id: t.id.clone(),
                    site: t.site.clone(),
                    description: t.description.clone(),
                    created: proto_time(t.created.as_option()),
                    expires_at: proto_time(t.expires_at.as_option()),
                    max_hosts: t.max_hosts,
                    used_hosts: t.used_hosts,
                    revoked: t.revoked,
                })
                .collect(),
        }
    }
}

impl Render for EnrollTokenListView {
    const SCHEMA: &'static str = "enroll-token-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&["id", "site", "hosts", "expires", "state", "description"]);
        for k in &self.tokens {
            t.add_row(cells![
                k.id.clone(),
                k.site.clone(),
                format!("{}/{}", k.used_hosts, k.max_hosts),
                k.expires_at.clone().unwrap_or_else(|| "-".into()),
                state_cell(if k.revoked { "revoked" } else { "active" }),
                dash(&k.description),
            ]);
        }
        write_table(out, &t, "no enrollment tokens")
    }
}

// -------------------------------------------------------------------- queues/ops

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct QueueRefView {
    pub instance_name: String,
    pub platform: BTreeMap<String, String>,
    pub size_class: u32,
}

impl From<Option<&pb::QueueRef>> for QueueRefView {
    fn from(q: Option<&pb::QueueRef>) -> Self {
        let q = q.cloned().unwrap_or_default();
        QueueRefView {
            instance_name: q.instance_name_prefix,
            platform: platform_map(&q.platform),
            size_class: q.size_class,
        }
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct QueueView {
    pub queue: QueueRefView,
    pub pool: String,
    pub queued: u32,
    pub executing: u32,
    pub idle_workers: u32,
    pub total_workers: u32,
    pub drains: u32,
    pub oldest_queued_seconds: Option<f64>,
    pub queue_time_p95_seconds: Option<f64>,
}

/// `queue-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct QueueListView {
    pub queues: Vec<QueueView>,
}

impl From<&pb::ListQueuesResponse> for QueueListView {
    fn from(r: &pb::ListQueuesResponse) -> Self {
        QueueListView {
            queues: r
                .queues
                .iter()
                .map(|q| QueueView {
                    queue: QueueRefView::from(q.queue.as_option()),
                    pool: q.pool.clone(),
                    queued: q.queued,
                    executing: q.executing,
                    idle_workers: q.idle_workers,
                    total_workers: q.total_workers,
                    drains: q.drains,
                    oldest_queued_seconds: proto_secs(q.oldest_queued_age.as_option()),
                    queue_time_p95_seconds: proto_secs(q.queue_time_p95.as_option()),
                })
                .collect(),
        }
    }
}

impl Render for QueueListView {
    const SCHEMA: &'static str = "queue-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&[
            "instance",
            "platform",
            "size",
            "pool",
            "queued",
            "executing",
            "workers",
            "oldest",
            "p95",
        ]);
        for q in &self.queues {
            t.add_row(vec![
                dash(&q.queue.instance_name),
                platform_text(&q.queue.platform),
                q.queue.size_class.to_string(),
                dash(&q.pool),
                q.queued.to_string(),
                q.executing.to_string(),
                format!(
                    "{}/{}",
                    q.total_workers - q.idle_workers.min(q.total_workers),
                    q.total_workers
                ),
                human_secs(q.oldest_queued_seconds),
                human_secs(q.queue_time_p95_seconds),
            ]);
        }
        write_table(out, &t, "no queues")
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct OperationView {
    pub name: String,
    pub queue: QueueRefView,
    pub action_digest: String,
    pub digest_function: String,
    pub stage: String,
    pub queued_at: Option<String>,
    pub target_id: String,
    pub invocation_id: String,
    pub tool_invocation_id: String,
    pub priority: i32,
    pub worker_node: String,
    pub worker_thread: u32,
    pub expected_duration_seconds: Option<f64>,
    pub timeout: Option<String>,
}

impl From<&pb::OperationSummary> for OperationView {
    fn from(o: &pb::OperationSummary) -> Self {
        OperationView {
            name: o.name.clone(),
            queue: QueueRefView::from(o.queue.as_option()),
            action_digest: o.action_digest.clone(),
            digest_function: o.digest_function.clone(),
            stage: o.stage.clone(),
            queued_at: proto_time(o.queued_at.as_option()),
            target_id: o.target_id.clone(),
            invocation_id: o.invocation_id.clone(),
            tool_invocation_id: o.tool_invocation_id.clone(),
            priority: o.priority,
            worker_node: o.worker_node.clone(),
            worker_thread: o.worker_thread,
            expected_duration_seconds: proto_secs(o.expected_duration.as_option()),
            timeout: proto_time(o.timeout.as_option()),
        }
    }
}

/// `operation-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct OperationListView {
    pub operations: Vec<OperationView>,
    pub next_page_token: String,
}

impl Render for OperationListView {
    const SCHEMA: &'static str = "operation-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&[
            "operation",
            "stage",
            "platform",
            "target",
            "worker",
            "queued at",
        ]);
        for o in &self.operations {
            t.add_row(cells![
                o.name.clone(),
                state_cell(&o.stage),
                platform_text(&o.queue.platform),
                dash(&o.target_id),
                dash(&o.worker_node),
                o.queued_at.clone().unwrap_or_else(|| "-".into()),
            ]);
        }
        write_table(out, &t, "no operations")?;
        if !self.next_page_token.is_empty() {
            writeln!(
                out,
                "(more operations exist; narrow the filter or raise --limit)"
            )?;
        }
        Ok(())
    }
}

/// `operation-event.v1` (one JSON line per event in `ops watch -o json`).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct OperationEventView {
    /// `added`, `changed`, `removed`, or `reconnecting` (stream interruption).
    pub kind: String,
    pub operation: Option<OperationView>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

impl Render for OperationEventView {
    const SCHEMA: &'static str = "operation-event.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        match &self.operation {
            Some(o) => writeln!(
                out,
                "{:<8} {}  {}  {}  {}",
                self.kind,
                o.name,
                state(&o.stage),
                platform_text(&o.queue.platform),
                dash(&o.worker_node)
            ),
            None => writeln!(
                out,
                "{:<8} {}",
                self.kind,
                self.detail.as_deref().unwrap_or("")
            ),
        }
    }
}

// ---------------------------------------------------------------------- identity

/// `service-key.v1`: a newly created key (shown once).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ServiceKeyView {
    pub key_id: String,
    pub account: String,
    pub key: String,
}

impl Render for ServiceKeyView {
    const SCHEMA: &'static str = "service-key.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write_fields(
            out,
            &[
                ("Key ID", self.key_id.clone()),
                ("Account", self.account.clone()),
                ("Key", self.key.clone()),
            ],
        )?;
        writeln!(
            out,
            "The key is shown once. Use it with `cucinactl login <url> --key <file>`; revoke it with `cucinactl keys revoke {}`.",
            self.key_id
        )
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ServiceKeyInfoView {
    pub key_id: String,
    pub account: String,
    pub description: String,
    pub created: Option<String>,
    pub expires_at: Option<String>,
    pub last_used: Option<String>,
    pub revoked: bool,
}

/// `service-key-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ServiceKeyListView {
    pub keys: Vec<ServiceKeyInfoView>,
}

impl From<&pb::ListServiceKeysResponse> for ServiceKeyListView {
    fn from(r: &pb::ListServiceKeysResponse) -> Self {
        ServiceKeyListView {
            keys: r
                .keys
                .iter()
                .map(|k| ServiceKeyInfoView {
                    key_id: k.key_id.clone(),
                    account: k.account.clone(),
                    description: k.description.clone(),
                    created: proto_time(k.created.as_option()),
                    expires_at: proto_time(k.expires_at.as_option()),
                    last_used: proto_time(k.last_used.as_option()),
                    revoked: k.revoked,
                })
                .collect(),
        }
    }
}

impl Render for ServiceKeyListView {
    const SCHEMA: &'static str = "service-key-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&[
            "key id",
            "account",
            "state",
            "created",
            "expires",
            "last used",
            "description",
        ]);
        for k in &self.keys {
            t.add_row(cells![
                k.key_id.clone(),
                k.account.clone(),
                state_cell(if k.revoked { "revoked" } else { "active" }),
                k.created.clone().unwrap_or_else(|| "-".into()),
                k.expires_at.clone().unwrap_or_else(|| "never".into()),
                k.last_used.clone().unwrap_or_else(|| "-".into()),
                dash(&k.description),
            ]);
        }
        write_table(out, &t, "no service keys")
    }
}

/// `revocation.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct RevocationView {
    pub sub: String,
    pub sid: String,
    pub effective_by: Option<String>,
}

impl Render for RevocationView {
    const SCHEMA: &'static str = "revocation.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let who = if self.sid.is_empty() {
            format!("principal {}", self.sub)
        } else {
            format!("session {}", self.sid)
        };
        writeln!(
            out,
            "revoked {who}; effective everywhere by {}",
            self.effective_by.as_deref().unwrap_or("-")
        )
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct RevocationEntryView {
    pub sub: String,
    pub sid: String,
    pub reason: String,
    pub created: Option<String>,
    pub created_by: String,
}

/// `revocation-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct RevocationListView {
    pub revocations: Vec<RevocationEntryView>,
}

impl Render for RevocationListView {
    const SCHEMA: &'static str = "revocation-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&["sub", "sid", "created", "by", "reason"]);
        for r in &self.revocations {
            t.add_row(vec![
                dash(&r.sub),
                dash(&r.sid),
                r.created.clone().unwrap_or_else(|| "-".into()),
                dash(&r.created_by),
                dash(&r.reason),
            ]);
        }
        write_table(out, &t, "no revocations")
    }
}

// ------------------------------------------------------------ cost/images/diag

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PoolCostView {
    pub pool: String,
    pub instance_seconds: i64,
    pub compute_usd_micros: i64,
    pub ebs_usd_micros: i64,
    pub data_transfer_usd_micros: i64,
    pub standing_usd_micros: i64,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct CostLineView {
    pub pool: String,
    pub category: String,
    pub detail: String,
    pub quantity: f64,
    pub unit: String,
    pub amount_usd_micros: i64,
}

/// `cost.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct CostView {
    pub today_usd_micros: i64,
    pub month_to_date_usd_micros: i64,
    pub standing_per_month_usd_micros: i64,
    pub pools: Vec<PoolCostView>,
    pub lines: Vec<CostLineView>,
    pub assumptions: String,
}

impl From<&pb::GetCostResponse> for CostView {
    fn from(r: &pb::GetCostResponse) -> Self {
        let c = r.cost.as_option().cloned().unwrap_or_default();
        CostView {
            today_usd_micros: money(c.today.as_option()),
            month_to_date_usd_micros: money(c.month_to_date.as_option()),
            standing_per_month_usd_micros: money(c.standing_per_month.as_option()),
            pools: c
                .pools
                .iter()
                .map(|p| PoolCostView {
                    pool: p.pool.clone(),
                    instance_seconds: p.instance_seconds,
                    compute_usd_micros: money(p.compute.as_option()),
                    ebs_usd_micros: money(p.ebs.as_option()),
                    data_transfer_usd_micros: money(p.data_transfer.as_option()),
                    standing_usd_micros: money(p.standing.as_option()),
                })
                .collect(),
            lines: r
                .lines
                .iter()
                .map(|l| CostLineView {
                    pool: l.pool.clone(),
                    category: l.category.clone(),
                    detail: l.detail.clone(),
                    quantity: l.quantity,
                    unit: l.unit.clone(),
                    amount_usd_micros: money(l.amount.as_option()),
                })
                .collect(),
            assumptions: r.assumptions.clone(),
        }
    }
}

impl Render for CostView {
    const SCHEMA: &'static str = "cost.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write_fields(
            out,
            &[
                ("Today", usd(self.today_usd_micros)),
                ("Month to date", usd(self.month_to_date_usd_micros)),
                (
                    "Standing (per month, all pools at zero)",
                    usd(self.standing_per_month_usd_micros),
                ),
            ],
        )?;
        writeln!(out)?;
        let mut t = table(&[
            "pool",
            "instance hours",
            "compute",
            "ebs",
            "transfer",
            "standing",
        ]);
        for p in &self.pools {
            t.add_row(vec![
                p.pool.clone(),
                format!("{:.1}", p.instance_seconds as f64 / 3600.0),
                usd(p.compute_usd_micros),
                usd(p.ebs_usd_micros),
                usd(p.data_transfer_usd_micros),
                usd(p.standing_usd_micros),
            ]);
        }
        write_table(out, &t, "no pool costs")?;
        if !self.assumptions.is_empty() {
            writeln!(out, "\n{}", self.assumptions)?;
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ImageView {
    pub pool: String,
    pub reference: String,
    pub version: String,
    pub generation: String,
    pub current: bool,
    pub previous: bool,
    pub workers_running: u32,
    pub created: Option<String>,
    pub fast_launch: String,
}

/// `image-list.v1`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ImageListView {
    pub images: Vec<ImageView>,
}

impl From<&pb::ListImagesResponse> for ImageListView {
    fn from(r: &pb::ListImagesResponse) -> Self {
        ImageListView {
            images: r
                .images
                .iter()
                .map(|i| ImageView {
                    pool: i.pool.clone(),
                    reference: i.reference.clone(),
                    version: i.version.clone(),
                    generation: i.generation.clone(),
                    current: i.current,
                    previous: i.previous,
                    workers_running: i.workers_running,
                    created: proto_time(i.created.as_option()),
                    fast_launch: i.fast_launch.clone(),
                })
                .collect(),
        }
    }
}

impl Render for ImageListView {
    const SCHEMA: &'static str = "image-list.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut t = table(&[
            "pool",
            "version",
            "generation",
            "role",
            "workers",
            "reference",
            "fast launch",
        ]);
        for i in &self.images {
            let role = if i.current {
                "current"
            } else if i.previous {
                "previous"
            } else {
                "-"
            };
            t.add_row(cells![
                i.pool.clone(),
                dash(&i.version),
                dash(&i.generation),
                state_cell(role),
                i.workers_running.to_string(),
                i.reference.clone(),
                dash(&i.fast_launch),
            ]);
        }
        write_table(out, &t, "no images")
    }
}

/// `file.v1`: a file written by `diag` / `hosts diag`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct FileView {
    pub path: String,
    pub bytes: u64,
}

impl Render for FileView {
    const SCHEMA: &'static str = "file.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        writeln!(out, "wrote {} ({} bytes)", self.path, self.bytes)
    }
}

/// `log.v1`: one chunk of a log stream (`workers logs -o json` prints JSON lines).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct LogView {
    pub text: String,
}

impl Render for LogView {
    const SCHEMA: &'static str = "log.v1";
    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        write!(out, "{}", self.text)
    }
}
