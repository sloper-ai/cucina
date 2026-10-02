// SPDX-License-Identifier: FSL-1.1-ALv2

//! Fleet commands over the management API: status, pools, workers, hosts, queues,
//! operations, action inspection, cost, images and diagnostics.

use std::io::Write as _;
use std::path::PathBuf;

use anyhow::{Context, Result};
use cucina_api::proto::cucina::v1 as pb;
use futures::StreamExt as _;

use super::Ctx;
use crate::cli::{
    ActionCmd, CostArgs, DiagArgs, EnrollTokenCmd, HostsCmd, LogUnit, OpsCmd, OpsFilter, PoolsCmd,
    Stage, WorkersCmd,
};
use crate::client::{ChunkStream, WatchItem, WatchOptions};
use crate::output::{OutputFormat, emit, emit_line};
use crate::util::{proto_time, to_proto_duration};
use crate::views::*;
use cucina_api::proto::cucina::v1::__buffa::oneof::kill_operations_request::Target as KillTarget;

pub async fn status(ctx: &Ctx) -> Result<()> {
    let s = ctx.session().await?;
    let r = s
        .management
        .get_status(pb::GetStatusRequest::default())
        .await?;
    emit(ctx.global.output, &StatusView::from(&r))
}

pub async fn pools(ctx: &Ctx, cmd: PoolsCmd) -> Result<()> {
    let s = ctx.session().await?;
    let m = &s.management;
    match cmd {
        PoolsCmd::List => {
            let r = m.list_pools(pb::ListPoolsRequest::default()).await?;
            emit(
                ctx.global.output,
                &PoolListView {
                    pools: r.pools.iter().map(PoolView::from).collect(),
                },
            )
        }
        PoolsCmd::Describe { name } => {
            let r = m
                .get_pool(pb::GetPoolRequest {
                    name,
                    ..Default::default()
                })
                .await?;
            emit(ctx.global.output, &PoolDescribeView::from(&r))
        }
        PoolsCmd::ScaleFloor {
            name,
            min,
            duration,
        } => {
            if min > 0 && duration.is_none() {
                return Err(crate::exit::CliError::usage(
                    "--for is required: a floor is standing cost and must expire (ADR 0582)",
                )
                .into());
            }
            let r = m
                .set_pool_floor(pb::SetPoolFloorRequest {
                    name: name.clone(),
                    min_running: min,
                    expires_in: duration.map(to_proto_duration).into(),
                    ..Default::default()
                })
                .await?;
            emit(
                ctx.global.output,
                &PoolFloorView {
                    pool: name,
                    min_running: min,
                    expires_at: proto_time(r.expires_at.as_option()),
                },
            )
        }
        PoolsCmd::Cordon { name, undo } => {
            if !undo {
                ctx.confirm(&format!("cordon pool {name} (no new workers launch)"))?;
            }
            m.cordon_pool(pb::CordonPoolRequest {
                name: name.clone(),
                cordon: !undo,
                ..Default::default()
            })
            .await?;
            let action = if undo { "uncordon pool" } else { "cordon pool" };
            emit(ctx.global.output, &ResultView::new(action, &name, ""))
        }
        PoolsCmd::Gc { name, dry_run } => {
            let target = name.clone().unwrap_or_default();
            if !dry_run {
                let what = if target.is_empty() {
                    "all pools".to_string()
                } else {
                    format!("pool {target}")
                };
                ctx.confirm(&format!("delete orphaned volumes and ENIs of {what}"))?;
            }
            let r = m
                .garbage_collect_pool(pb::GarbageCollectPoolRequest {
                    name: target.clone(),
                    dry_run,
                    ..Default::default()
                })
                .await?;
            emit(
                ctx.global.output,
                &PoolGcView {
                    pool: target,
                    dry_run,
                    deleted: r.deleted,
                    errors: r.errors,
                },
            )
        }
    }
}

pub async fn workers(ctx: &Ctx, cmd: WorkersCmd) -> Result<()> {
    let s = ctx.session().await?;
    let m = &s.management;
    match cmd {
        WorkersCmd::List { pool } => {
            let r = m
                .list_workers(pb::ListWorkersRequest {
                    pool: pool.unwrap_or_default(),
                    ..Default::default()
                })
                .await?;
            emit(
                ctx.global.output,
                &WorkerListView {
                    workers: r.workers.iter().map(WorkerView::from).collect(),
                },
            )
        }
        WorkersCmd::Drain { node } => {
            ctx.confirm(&format!("drain worker {node}"))?;
            m.drain_worker(pb::DrainWorkerRequest {
                node: node.clone(),
                ..Default::default()
            })
            .await?;
            emit(
                ctx.global.output,
                &ResultView::new(
                    "drain worker",
                    &node,
                    "running actions finish; no new ones start",
                ),
            )
        }
        WorkersCmd::Undrain { node } => {
            m.undrain_worker(pb::DrainWorkerRequest {
                node: node.clone(),
                ..Default::default()
            })
            .await?;
            emit(
                ctx.global.output,
                &ResultView::new("undrain worker", &node, ""),
            )
        }
        WorkersCmd::Logs {
            node,
            unit,
            tail,
            follow,
        } => {
            let unit = match unit {
                LogUnit::BbWorker => "bb-worker",
                LogUnit::BbRunner => "bb-runner",
                LogUnit::Agent => "agent",
            };
            let request = pb::StreamWorkerLogsRequest {
                node,
                unit: unit.into(),
                tail_lines: tail,
                follow,
                ..Default::default()
            };
            let out = ctx.global.output;
            m.stream_chunks(ChunkStream::WorkerLogs(request), |data| {
                emit_line(
                    out,
                    &LogView {
                        text: String::from_utf8_lossy(data).into_owned(),
                    },
                )
            })
            .await?;
            Ok(())
        }
    }
}

async fn stream_to_file(
    m: &crate::client::ManagementClient,
    kind: ChunkStream,
    path: &PathBuf,
    format: OutputFormat,
) -> Result<()> {
    let tmp = path.with_extension("partial");
    let mut file =
        std::fs::File::create(&tmp).with_context(|| format!("creating {}", tmp.display()))?;
    let progress = (format == OutputFormat::Table && crate::util::stderr_is_tty()).then(|| {
        let pb = indicatif::ProgressBar::new_spinner();
        pb.set_message("downloading");
        pb
    });
    let mut bytes = 0u64;
    m.stream_chunks(kind, |data| {
        file.write_all(data)?;
        bytes += data.len() as u64;
        if let Some(pb) = &progress {
            pb.set_message(format!("downloading ({bytes} bytes)"));
            pb.tick();
        }
        Ok(())
    })
    .await?;
    file.sync_all()?;
    drop(file);
    std::fs::rename(&tmp, path)?;
    if let Some(pb) = progress {
        pb.finish_and_clear();
    }
    emit(
        format,
        &FileView {
            path: path.display().to_string(),
            bytes,
        },
    )
}

fn timestamped(prefix: &str) -> PathBuf {
    let now = jiff::Timestamp::now()
        .strftime("%Y%m%dT%H%M%SZ")
        .to_string();
    PathBuf::from(format!("{prefix}-{now}.tar.gz"))
}

pub async fn hosts(ctx: &Ctx, cmd: HostsCmd) -> Result<()> {
    let s = ctx.session().await?;
    let m = &s.management;
    let out = ctx.global.output;
    match cmd {
        HostsCmd::List => {
            let r = m.list_hosts(pb::ListHostsRequest::default()).await?;
            emit(
                out,
                &HostListView {
                    hosts: r.hosts.iter().map(HostView::from).collect(),
                },
            )
        }
        HostsCmd::Drain { host } => {
            ctx.confirm(&format!(
                "drain host {host} (its VMs shut down after their actions)"
            ))?;
            let r = m.drain_host(host_ref(&host)).await?;
            emit(out, &ResultView::new("drain host", &host, r.message))
        }
        HostsCmd::Uncordon { host } => {
            let r = m.uncordon_host(host_ref(&host)).await?;
            emit(out, &ResultView::new("uncordon host", &host, r.message))
        }
        HostsCmd::ReImage { host, vm } => {
            let scope = vm
                .as_deref()
                .map_or("all VMs".to_string(), |v| format!("VM {v}"));
            ctx.confirm(&format!("re-image {scope} on host {host}"))?;
            let r = m
                .reimage_host(pb::ReimageHostRequest {
                    host: host_ref(&host).into(),
                    vm: vm.unwrap_or_default(),
                    ..Default::default()
                })
                .await?;
            emit(out, &ResultView::new("re-image host", &host, r.message))
        }
        HostsCmd::Diag { host, file } => {
            let path = file.unwrap_or_else(|| timestamped(&format!("cucina-host-{host}")));
            stream_to_file(m, ChunkStream::HostDiagnostics(host_ref(&host)), &path, out).await
        }
        HostsCmd::Register {
            serials,
            site,
            labels,
        } => {
            let r = m
                .register_host_serials(pb::RegisterHostSerialsRequest {
                    serials,
                    site,
                    labels: labels.into_iter().collect(),
                    ..Default::default()
                })
                .await?;
            emit(
                out,
                &HostRegisterView {
                    registered: r.registered,
                    already_present: r.already_present,
                },
            )
        }
        HostsCmd::Approve { serial } => {
            let r = m
                .approve_host(pb::ApproveHostRequest {
                    serial: serial.clone(),
                    ..Default::default()
                })
                .await?;
            emit(out, &ResultView::new("approve host", &serial, r.message))
        }
        HostsCmd::Remove { host } => {
            ctx.confirm(&format!("remove host {host}"))?;
            let r = m.remove_host(host_ref(&host)).await?;
            emit(out, &ResultView::new("remove host", &host, r.message))
        }
        HostsCmd::EnrollToken(EnrollTokenCmd::Create {
            site,
            ttl,
            max_hosts,
            description,
        }) => {
            let r = m
                .create_enroll_token(pb::CreateEnrollTokenRequest {
                    site,
                    ttl: to_proto_duration(ttl).into(),
                    max_hosts,
                    description,
                    ..Default::default()
                })
                .await?;
            emit(
                out,
                &EnrollTokenView {
                    id: r.id,
                    token: r.token,
                    expires_at: proto_time(r.expires_at.as_option()),
                },
            )
        }
        HostsCmd::EnrollToken(EnrollTokenCmd::List) => {
            let r = m
                .list_enroll_tokens(pb::ListEnrollTokensRequest::default())
                .await?;
            emit(out, &EnrollTokenListView::from(&r))
        }
        HostsCmd::EnrollToken(EnrollTokenCmd::Revoke { id }) => {
            ctx.confirm(&format!("revoke enrollment token {id}"))?;
            m.revoke_enroll_token(pb::RevokeEnrollTokenRequest {
                id: id.clone(),
                ..Default::default()
            })
            .await?;
            emit(
                out,
                &ResultView::new(
                    "revoke enrollment token",
                    &id,
                    "new enrollments with it are refused",
                ),
            )
        }
    }
}

pub async fn queues(ctx: &Ctx) -> Result<()> {
    let s = ctx.session().await?;
    let r = s
        .management
        .list_queues(pb::ListQueuesRequest::default())
        .await?;
    emit(ctx.global.output, &QueueListView::from(&r))
}

fn platform_props(p: &[(String, String)]) -> Vec<pb::PlatformProperty> {
    p.iter()
        .map(|(k, v)| pb::PlatformProperty {
            name: k.clone(),
            value: v.clone(),
            ..Default::default()
        })
        .collect()
}

fn ops_request(f: &OpsFilter) -> pb::ListOperationsRequest {
    let queue = (!f.platform.is_empty()).then(|| pb::QueueRef {
        instance_name_prefix: f.instance.clone(),
        platform: platform_props(&f.platform),
        ..Default::default()
    });
    pb::ListOperationsRequest {
        queue: queue.into(),
        stage: f
            .stage
            .map(|s| match s {
                Stage::Queued => "queued",
                Stage::Executing => "executing",
                Stage::Completed => "completed",
            })
            .unwrap_or_default()
            .to_string(),
        instance_name: f.instance.clone(),
        invocation_id: f.invocation.clone(),
        page: pb::Page {
            size: f.limit,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}

fn host_ref(host: &str) -> pb::HostRef {
    pb::HostRef {
        serial: host.to_string(),
        ..Default::default()
    }
}

pub async fn ops(ctx: &Ctx, cmd: OpsCmd) -> Result<()> {
    let s = ctx.session().await?;
    let m = &s.management;
    let out = ctx.global.output;
    match cmd {
        OpsCmd::List(filter) => {
            let r = m.list_operations(ops_request(&filter)).await?;
            emit(
                out,
                &OperationListView {
                    operations: r.operations.iter().map(OperationView::from).collect(),
                    next_page_token: r.next_page_token,
                },
            )
        }
        OpsCmd::Watch(filter) => {
            let refresher = crate::client::spawn_refresher(s.ctx.clone(), s.token.clone());
            let stream = m.watch_operations(ops_request(&filter), WatchOptions::default());
            futures::pin_mut!(stream);
            let mut seen = 0u64;
            let result = async {
                loop {
                    if filter.count.is_some_and(|c| seen >= c) {
                        return Ok(());
                    }
                    tokio::select! {
                        item = stream.next() => match item {
                            None => return Ok(()),
                            Some(Err(status)) => return Err(anyhow::Error::from(status)),
                            Some(Ok(WatchItem::Data(ev))) => {
                                let kind = match ev.kind.as_known() {
                                    Some(pb::operation_event::Kind::KIND_ADDED) => "added",
                                    Some(pb::operation_event::Kind::KIND_CHANGED) => "changed",
                                    Some(pb::operation_event::Kind::KIND_REMOVED) => "removed",
                                    _ => "unknown",
                                };
                                emit_line(out, &OperationEventView {
                                    kind: kind.into(),
                                    operation: ev.operation.as_option().map(OperationView::from),
                                    detail: None,
                                })?;
                                seen += 1;
                            }
                            Some(Ok(WatchItem::Reconnecting { attempt, retry_in, reason })) => {
                                emit_line(out, &OperationEventView {
                                    kind: "reconnecting".into(),
                                    operation: None,
                                    detail: Some(format!(
                                        "attempt {attempt} in {}: {reason}",
                                        crate::util::format_duration(retry_in)
                                    )),
                                })?;
                            }
                        },
                        _ = tokio::signal::ctrl_c() => return Ok(()),
                    }
                }
            }
            .await;
            refresher.abort();
            result
        }
        OpsCmd::Kill {
            operation,
            queue_without_workers,
            platform,
            instance,
            size_class,
            message,
        } => {
            let (target, label) = if queue_without_workers {
                anyhow::ensure!(
                    !platform.is_empty(),
                    crate::exit::CliError::usage(
                        "--queue-without-workers needs --platform name=value"
                    )
                );
                let q = pb::QueueRef {
                    instance_name_prefix: instance,
                    platform: platform_props(&platform),
                    size_class,
                    ..Default::default()
                };
                let label = format!(
                    "queue {}",
                    platform
                        .iter()
                        .map(|(k, v)| format!("{k}={v}"))
                        .collect::<Vec<_>>()
                        .join(",")
                );
                (KillTarget::QueueWithoutWorkers(Box::new(q)), label)
            } else {
                let name = operation.unwrap_or_default();
                (
                    KillTarget::OperationName(name.clone()),
                    format!("operation {name}"),
                )
            };
            ctx.confirm(&format!("kill {label}"))?;
            m.kill_operations(pb::KillOperationsRequest {
                target: Some(target),
                message,
                ..Default::default()
            })
            .await?;
            emit(out, &ResultView::new("kill", &label, ""))
        }
    }
}

pub async fn action(ctx: &Ctx, cmd: ActionCmd) -> Result<()> {
    let ActionCmd::Inspect(args) = cmd;
    let s = ctx.session().await?;
    let opts = crate::inspect::InspectOptions {
        instance: args
            .instance
            .clone()
            .unwrap_or_else(|| s.ctx.profile.instance_name.clone()),
        max_tree_entries: args.max_tree_entries,
        max_output_bytes: args.max_output_bytes,
    };
    let view = crate::inspect::inspect(&s, &args.subject, &opts).await?;
    emit(ctx.global.output, &view)
}

fn parse_since(s: &str) -> Result<crate::util::PbTimestamp> {
    let ts: jiff::Timestamp = match s.parse() {
        Ok(ts) => ts,
        Err(_) => {
            let date: jiff::civil::Date = s.parse().map_err(|_| {
                crate::exit::CliError::usage(format!(
                    "invalid --since {s:?} (use RFC 3339 or YYYY-MM-DD)"
                ))
            })?;
            date.to_zoned(jiff::tz::TimeZone::UTC)?.timestamp()
        }
    };
    Ok(crate::util::PbTimestamp {
        seconds: ts.as_second(),
        ..Default::default()
    })
}

pub async fn cost(ctx: &Ctx, args: &CostArgs) -> Result<()> {
    let since = args.since.as_deref().map(parse_since).transpose()?;
    let s = ctx.session().await?;
    let r = s
        .management
        .get_cost(pb::GetCostRequest {
            pool: args.pool.clone().unwrap_or_default(),
            since: since.into(),
            ..Default::default()
        })
        .await?;
    emit(ctx.global.output, &CostView::from(&r))
}

pub async fn images(ctx: &Ctx) -> Result<()> {
    let s = ctx.session().await?;
    let r = s
        .management
        .list_images(pb::ListImagesRequest::default())
        .await?;
    emit(ctx.global.output, &ImageListView::from(&r))
}

pub async fn diag(ctx: &Ctx, args: &DiagArgs) -> Result<()> {
    let s = ctx.session().await?;
    let path = args
        .file
        .clone()
        .unwrap_or_else(|| timestamped("cucina-support"));
    let request = pb::CollectSupportBundleRequest {
        include_logs: args.include_logs,
        ..Default::default()
    };
    stream_to_file(
        &s.management,
        ChunkStream::SupportBundle(request),
        &path,
        ctx.global.output,
    )
    .await
}
