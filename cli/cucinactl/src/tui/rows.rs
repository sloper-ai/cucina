// SPDX-License-Identifier: FSL-1.1-ALv2

//! The rows of every table, derived from the state: filtered by the tab's quick
//! filter and sorted. The reducer (selection, actions) and the renderer use the same
//! functions, so what is selected is always what is shown.

use cucina_api::proto::cucina::v1 as pb;

use super::filter::{FieldSpec, Filter};
use super::format;
use super::model::{State, Tab};

pub const QUEUE_FIELDS: &[FieldSpec] = &[
    ("platform", &["p", "os"]),
    ("instance", &["i", "inst"]),
    ("pool", &[]),
];
pub const POOL_FIELDS: &[FieldSpec] = &[
    ("pool", &["name"]),
    ("platform", &["p"]),
    ("provider", &[]),
    ("condition", &["cond", "status"]),
];
pub const HOST_FIELDS: &[FieldSpec] = &[
    ("host", &["name"]),
    ("serial", &[]),
    ("site", &[]),
    ("phase", &["state", "status"]),
];
pub const OP_FIELDS: &[FieldSpec] = &[
    ("platform", &["p", "os"]),
    ("instance", &["i", "inst"]),
    ("invocation", &["inv"]),
    ("stage", &["s", "state"]),
    ("target", &["t"]),
    ("worker", &["w", "node"]),
    ("name", &["op", "operation"]),
    ("digest", &["action"]),
];
pub const COST_FIELDS: &[FieldSpec] = &[("pool", &[])];
pub const KEY_FIELDS: &[FieldSpec] = &[
    ("account", &["sa"]),
    ("key", &["id"]),
    ("status", &["state"]),
    ("description", &["desc"]),
];

/// Filterable fields of a tab's main table.
pub fn fields(tab: Tab) -> &'static [FieldSpec] {
    match tab {
        Tab::Overview => QUEUE_FIELDS,
        Tab::Pools => POOL_FIELDS,
        Tab::Hosts => HOST_FIELDS,
        Tab::Operations => OP_FIELDS,
        Tab::Cost => COST_FIELDS,
        Tab::Keys => KEY_FIELDS,
    }
}

fn filter(s: &State, tab: Tab) -> Filter {
    Filter::parse(s.filter_text(tab), fields(tab))
}

// ------------------------------------------------------------------- queues

pub fn queue_key(q: &pb::QueueSummary) -> String {
    let r = q.queue.as_option();
    format!(
        "{}|{}|{}",
        r.map(|r| r.instance_name_prefix.as_str())
            .unwrap_or_default(),
        format::platform(r),
        r.map_or(0, |r| r.size_class)
    )
}

fn queue_fields(q: &pb::QueueSummary) -> Vec<(&'static str, String)> {
    let r = q.queue.as_option();
    vec![
        ("platform", format::platform(r)),
        (
            "instance",
            r.map(|r| r.instance_name_prefix.clone())
                .unwrap_or_default(),
        ),
        ("pool", q.pool.clone()),
    ]
}

/// Overview queues, busiest first.
pub fn queues(s: &State) -> Vec<&pb::QueueSummary> {
    let f = filter(s, Tab::Overview);
    let mut out: Vec<&pb::QueueSummary> = s
        .overview
        .latest
        .iter()
        .flat_map(|o| o.queues.iter())
        .filter(|q| f.is_empty() || f.matches(&queue_fields(q)))
        .collect();
    out.sort_by(|a, b| {
        (b.queued + b.executing)
            .cmp(&(a.queued + a.executing))
            .then_with(|| queue_key(a).cmp(&queue_key(b)))
    });
    out
}

// -------------------------------------------------------------------- pools

fn pool_fields(p: &pb::PoolSummary) -> Vec<(&'static str, String)> {
    vec![
        ("pool", p.name.clone()),
        ("platform", p.platform.clone()),
        ("provider", p.provider.clone()),
        ("condition", p.condition.clone()),
    ]
}

/// Pools (from the live overview), by name.
pub fn pools(s: &State) -> Vec<&pb::PoolSummary> {
    let f = filter(s, Tab::Pools);
    let mut out: Vec<&pb::PoolSummary> = s
        .overview
        .latest
        .iter()
        .flat_map(|o| o.pools.iter())
        .filter(|p| f.is_empty() || f.matches(&pool_fields(p)))
        .collect();
    out.sort_by(|a, b| a.name.cmp(&b.name));
    out
}

pub fn selected_pool(s: &State) -> Option<&pb::PoolSummary> {
    pools(s).get(s.pools.nav.index).copied()
}

/// Workers of the selected pool (from its `GetPool`), by node.
pub fn workers(s: &State) -> Vec<&pb::WorkerSummary> {
    let Some(pool) = selected_pool(s) else {
        return Vec::new();
    };
    let mut out: Vec<&pb::WorkerSummary> = s
        .pools
        .detail
        .data_for(&pool.name)
        .map(|d| d.workers.iter().collect())
        .unwrap_or_default();
    out.sort_by(|a, b| a.node.cmp(&b.node));
    out
}

pub fn selected_worker(s: &State) -> Option<&pb::WorkerSummary> {
    workers(s).get(s.pools.workers.index).copied()
}

// -------------------------------------------------------------------- hosts

fn host_name(h: &pb::HostDetail) -> String {
    h.summary
        .as_option()
        .map(|s| {
            if s.name.is_empty() {
                s.serial.clone()
            } else {
                s.name.clone()
            }
        })
        .unwrap_or_default()
}

pub fn host_serial(h: &pb::HostDetail) -> String {
    h.summary
        .as_option()
        .map(|s| s.serial.clone())
        .unwrap_or_default()
}

fn host_fields(h: &pb::HostDetail) -> Vec<(&'static str, String)> {
    let s = h.summary.as_option();
    vec![
        ("host", host_name(h)),
        ("serial", host_serial(h)),
        ("site", s.map(|s| s.site.clone()).unwrap_or_default()),
        ("phase", s.map(|s| s.phase.clone()).unwrap_or_default()),
    ]
}

/// Mac hosts (from `ListHosts`), by name.
pub fn hosts(s: &State) -> Vec<&pb::HostDetail> {
    let f = filter(s, Tab::Hosts);
    let mut out: Vec<&pb::HostDetail> = s
        .hosts
        .list
        .data
        .iter()
        .flatten()
        .filter(|h| f.is_empty() || f.matches(&host_fields(h)))
        .collect();
    out.sort_by_key(|h| host_name(h));
    out
}

pub fn selected_host(s: &State) -> Option<&pb::HostDetail> {
    hosts(s).get(s.hosts.nav.index).copied()
}

/// Display name of a host (name, else serial).
pub fn host_label(h: &pb::HostDetail) -> String {
    host_name(h)
}

// --------------------------------------------------------------- operations

fn stage_rank(stage: &str) -> u8 {
    match stage {
        "executing" => 0,
        "queued" => 1,
        "completed" => 2,
        _ => 3,
    }
}

pub fn op_fields(o: &pb::OperationSummary) -> Vec<(&'static str, String)> {
    let q = o.queue.as_option();
    vec![
        ("platform", format::platform(q)),
        (
            "instance",
            q.map(|q| q.instance_name_prefix.clone())
                .unwrap_or_default(),
        ),
        ("invocation", o.invocation_id.clone()),
        ("stage", o.stage.clone()),
        ("target", o.target_id.clone()),
        ("worker", o.worker_node.clone()),
        ("name", o.name.clone()),
        ("digest", o.action_digest.clone()),
    ]
}

/// Operations: executing, then queued (oldest first), then the rest.
pub fn ops(s: &State) -> Vec<&pb::OperationSummary> {
    let f = filter(s, Tab::Operations);
    let mut out: Vec<&pb::OperationSummary> = s
        .ops
        .ops
        .values()
        .filter(|o| f.is_empty() || f.matches(&op_fields(o)))
        .collect();
    out.sort_by(|a, b| {
        stage_rank(&a.stage)
            .cmp(&stage_rank(&b.stage))
            .then_with(|| {
                format::ts_ms(a.queued_at.as_option()).cmp(&format::ts_ms(b.queued_at.as_option()))
            })
            .then_with(|| a.name.cmp(&b.name))
    });
    out
}

pub fn selected_op(s: &State) -> Option<&pb::OperationSummary> {
    ops(s).get(s.ops.nav.index).copied()
}

// --------------------------------------------------------------------- cost

/// The cost summary: `GetCost` when loaded, else the live overview's.
pub fn cost_summary(s: &State) -> Option<&pb::CostSummary> {
    s.cost
        .detail
        .data
        .as_ref()
        .and_then(|c| c.cost.as_option())
        .or_else(|| s.overview.latest.as_ref().and_then(|o| o.cost.as_option()))
}

/// Per-pool costs, most expensive first.
pub fn cost_pools(s: &State) -> Vec<&pb::PoolCost> {
    let f = filter(s, Tab::Cost);
    let mut out: Vec<&pb::PoolCost> = cost_summary(s)
        .map(|c| c.pools.iter().collect())
        .unwrap_or_default();
    out.retain(|p| f.matches(&[("pool", p.pool.clone())]));
    out.sort_by(|a, b| total(b).cmp(&total(a)).then_with(|| a.pool.cmp(&b.pool)));
    out
}

/// Spend of a pool in the period (standing cost excluded: it is a monthly rate).
pub fn total(p: &pb::PoolCost) -> i64 {
    [&p.compute, &p.ebs, &p.data_transfer, &p.public_ipv4]
        .iter()
        .filter_map(|m| m.as_option())
        .map(|m| m.micros)
        .sum()
}

pub fn selected_cost_pool(s: &State) -> Option<&pb::PoolCost> {
    cost_pools(s).get(s.cost.nav.index).copied()
}

/// Itemised cost lines of the selected pool (all lines when nothing is selected).
pub fn cost_lines(s: &State) -> Vec<&pb::CostLine> {
    let pool = selected_cost_pool(s).map(|p| p.pool.as_str());
    s.cost
        .detail
        .data
        .iter()
        .flat_map(|c| c.lines.iter())
        .filter(|l| pool.is_none_or(|p| l.pool == p))
        .collect()
}

// --------------------------------------------------------------------- keys

fn key_fields(k: &pb::ServiceKeyInfo) -> Vec<(&'static str, String)> {
    vec![
        ("account", k.account.clone()),
        ("key", k.key_id.clone()),
        (
            "status",
            if k.revoked { "revoked" } else { "active" }.to_string(),
        ),
        ("description", k.description.clone()),
    ]
}

/// Service keys: active first, then by account and id.
pub fn keys(s: &State) -> Vec<&pb::ServiceKeyInfo> {
    let f = filter(s, Tab::Keys);
    let mut out: Vec<&pb::ServiceKeyInfo> = s
        .keys
        .keys
        .data
        .iter()
        .flatten()
        .filter(|k| f.is_empty() || f.matches(&key_fields(k)))
        .collect();
    out.sort_by(|a, b| {
        a.revoked
            .cmp(&b.revoked)
            .then_with(|| a.account.cmp(&b.account))
            .then_with(|| a.key_id.cmp(&b.key_id))
    });
    out
}

pub fn selected_key(s: &State) -> Option<&pb::ServiceKeyInfo> {
    keys(s).get(s.keys.nav.index).copied()
}
