// SPDX-License-Identifier: FSL-1.1-ALv2

//! The TUI's port to the management API: a narrow trait ([`Management`]) with the
//! real adapter ([`Remote`], over [`crate::client::Session`]) and a fake
//! ([`super::fake::FakeManagement`]) for tests and the demo mode. The reducer never
//! calls it; it emits [`Request`]s as effects and receives [`Reply`]s as events.

use std::sync::Arc;
use std::time::Duration;

use connectrpc::ConnectError;
use cucina_api::proto::cucina::v1 as pb;
use cucina_api::proto::cucina::v1::__buffa::oneof::kill_operations_request::Target as KillTarget;
use futures::future::BoxFuture;
use futures::stream::BoxStream;
use futures::{FutureExt as _, StreamExt as _};

use crate::client::{Session, WatchItem, WatchOptions};
use crate::inspect::{ActionView, InspectOptions};

/// One element of a supervised server stream.
#[derive(Debug, Clone, PartialEq)]
pub enum StreamEvent<T> {
    Data(T),
    /// The stream broke; the client reconnects after `retry_in`.
    Reconnecting {
        attempt: u32,
        retry_in: Duration,
        reason: String,
    },
    /// The stream ended for good (a permanent error such as `permission_denied`).
    Failed {
        code: Option<String>,
        message: String,
    },
}

/// A unary call the TUI makes. Mutating calls are only ever emitted by the reducer
/// after the user confirmed them when they are destructive ([`Request::is_destructive`]).
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum Request {
    GetPool {
        name: String,
    },
    ListHosts,
    GetCost,
    ListServiceKeys,
    ListRevocations,
    /// A full listing (every page), used to resynchronise after a stream reconnect.
    ListOperations,
    /// Action inspection (an operation name, digest or Buildbarn link).
    Inspect {
        subject: String,
    },
    DrainWorker {
        node: String,
    },
    UndrainWorker {
        node: String,
    },
    DrainHost {
        serial: String,
    },
    UncordonHost {
        serial: String,
    },
    ReimageHost {
        serial: String,
    },
    KillOperation {
        name: String,
    },
    RevokeServiceKey {
        key_id: String,
    },
}

impl Request {
    /// Changes server state (audited by the controller).
    pub fn is_mutating(&self) -> bool {
        matches!(
            self,
            Request::DrainWorker { .. }
                | Request::UndrainWorker { .. }
                | Request::DrainHost { .. }
                | Request::UncordonHost { .. }
                | Request::ReimageHost { .. }
                | Request::KillOperation { .. }
                | Request::RevokeServiceKey { .. }
        )
    }

    /// Needs an explicit confirmation (R-CLI-4: drain, kill, revoke, re-image).
    pub fn is_destructive(&self) -> bool {
        matches!(
            self,
            Request::DrainWorker { .. }
                | Request::DrainHost { .. }
                | Request::ReimageHost { .. }
                | Request::KillOperation { .. }
                | Request::RevokeServiceKey { .. }
        )
    }

    /// Short human description (`drain worker i-0123…`).
    pub fn describe(&self) -> String {
        match self {
            Request::GetPool { name } => format!("load pool {name}"),
            Request::ListHosts => "list hosts".into(),
            Request::GetCost => "load cost".into(),
            Request::ListServiceKeys => "list service keys".into(),
            Request::ListRevocations => "list revocations".into(),
            Request::ListOperations => "list operations".into(),
            Request::Inspect { subject } => format!("inspect {subject}"),
            Request::DrainWorker { node } => format!("drain worker {node}"),
            Request::UndrainWorker { node } => format!("undrain worker {node}"),
            Request::DrainHost { serial } => format!("drain host {serial}"),
            Request::UncordonHost { serial } => format!("uncordon host {serial}"),
            Request::ReimageHost { serial } => format!("re-image host {serial}"),
            Request::KillOperation { name } => format!("kill operation {name}"),
            Request::RevokeServiceKey { key_id } => format!("revoke service key {key_id}"),
        }
    }
}

/// The answer to a [`Request`].
#[derive(Debug, Clone)]
pub enum Reply {
    Pool(Box<pb::GetPoolResponse>),
    Hosts(Vec<pb::HostDetail>),
    Cost(Box<pb::GetCostResponse>),
    ServiceKeys(Vec<pb::ServiceKeyInfo>),
    Revocations(Vec<pb::Revocation>),
    Operations(Vec<pb::OperationSummary>),
    Action(Box<ActionView>),
    /// A mutating call succeeded; the text is shown in the status line.
    Done(String),
}

/// A failed call, ready for the status line (`permission_denied: not an admin`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApiError {
    /// gRPC status name (`permission_denied`), when the server answered.
    pub code: Option<String>,
    pub message: String,
}

impl ApiError {
    pub fn new(code: Option<&str>, message: impl Into<String>) -> ApiError {
        ApiError {
            code: code.map(str::to_string),
            message: message.into(),
        }
    }
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl From<ConnectError> for ApiError {
    fn from(e: ConnectError) -> Self {
        ApiError {
            code: Some(e.code.as_str().to_string()),
            message: e.to_string(),
        }
    }
}

impl From<anyhow::Error> for ApiError {
    fn from(e: anyhow::Error) -> Self {
        let code = e
            .chain()
            .find_map(|c| c.downcast_ref::<ConnectError>())
            .map(|c| c.code.as_str().to_string());
        ApiError {
            code,
            message: format!("{e:#}"),
        }
    }
}

/// What the TUI needs from the management API.
pub trait Management: Send + Sync + 'static {
    /// `WatchOverview` (about every 2 s), reconnecting by itself.
    fn watch_overview(&self) -> BoxStream<'static, StreamEvent<pb::Overview>>;
    /// `WatchOperations` (all operations the caller may see), reconnecting by itself.
    fn watch_operations(&self) -> BoxStream<'static, StreamEvent<pb::OperationEvent>>;
    /// A unary call.
    fn call(&self, request: Request) -> BoxFuture<'static, Result<Reply, ApiError>>;
}

/// How often the overview is streamed (R-CLI-4: ≤ 2 s).
pub const OVERVIEW_INTERVAL: Duration = Duration::from_secs(2);

fn stream_event<T>(item: Result<WatchItem<T>, ConnectError>) -> StreamEvent<T> {
    match item {
        Ok(WatchItem::Data(t)) => StreamEvent::Data(t),
        Ok(WatchItem::Reconnecting {
            attempt,
            retry_in,
            reason,
        }) => StreamEvent::Reconnecting {
            attempt,
            retry_in,
            reason,
        },
        Err(e) => StreamEvent::Failed {
            code: Some(e.code.as_str().to_string()),
            message: e.to_string(),
        },
    }
}

/// The real adapter: the management client of an authenticated session, plus the
/// action inspector (`cucinactl action inspect`) for drill-down.
pub struct Remote {
    session: Arc<Session>,
    inspect: InspectOptions,
}

impl Remote {
    pub fn new(session: Session) -> Remote {
        let inspect = InspectOptions {
            instance: session.ctx.profile.instance_name.clone(),
            max_tree_entries: 200,
            max_output_bytes: 64 << 10,
        };
        Remote {
            session: Arc::new(session),
            inspect,
        }
    }
}

/// Upper bound on pages fetched for one operations snapshot.
const MAX_OPERATION_PAGES: usize = 50;

async fn list_all_operations(session: &Session) -> Result<Vec<pb::OperationSummary>, ApiError> {
    let mut out = Vec::new();
    let mut token = String::new();
    for _ in 0..MAX_OPERATION_PAGES {
        let resp = session
            .management
            .list_operations(pb::ListOperationsRequest {
                page: pb::Page {
                    size: 1000,
                    token: token.clone(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })
            .await?;
        out.extend(resp.operations);
        if resp.next_page_token.is_empty() {
            break;
        }
        token = resp.next_page_token;
    }
    Ok(out)
}

impl Management for Remote {
    fn watch_overview(&self) -> BoxStream<'static, StreamEvent<pb::Overview>> {
        let request = pb::WatchOverviewRequest {
            interval: crate::util::to_proto_duration(OVERVIEW_INTERVAL).into(),
            ..Default::default()
        };
        self.session
            .management
            .watch_overview(request, WatchOptions::default())
            .map(stream_event)
            .boxed()
    }

    fn watch_operations(&self) -> BoxStream<'static, StreamEvent<pb::OperationEvent>> {
        self.session
            .management
            .watch_operations(
                pb::ListOperationsRequest::default(),
                WatchOptions::default(),
            )
            .map(stream_event)
            .boxed()
    }

    fn call(&self, request: Request) -> BoxFuture<'static, Result<Reply, ApiError>> {
        let session = self.session.clone();
        let opts = self.inspect.clone();
        async move {
            let m = &session.management;
            let host = |serial: &str| pb::HostRef {
                serial: serial.to_string(),
                ..Default::default()
            };
            let reply = match request {
                Request::GetPool { name } => Reply::Pool(Box::new(
                    m.get_pool(pb::GetPoolRequest {
                        name,
                        ..Default::default()
                    })
                    .await?,
                )),
                Request::ListHosts => Reply::Hosts(m.list_hosts(Default::default()).await?.hosts),
                Request::GetCost => Reply::Cost(Box::new(m.get_cost(Default::default()).await?)),
                Request::ListServiceKeys => {
                    Reply::ServiceKeys(m.list_service_keys(Default::default()).await?.keys)
                }
                Request::ListRevocations => {
                    Reply::Revocations(m.list_revocations(Default::default()).await?.revocations)
                }
                Request::ListOperations => Reply::Operations(list_all_operations(&session).await?),
                Request::Inspect { subject } => Reply::Action(Box::new(
                    crate::inspect::inspect(&session, &subject, &opts).await?,
                )),
                Request::DrainWorker { node } => {
                    m.drain_worker(pb::DrainWorkerRequest {
                        node: node.clone(),
                        ..Default::default()
                    })
                    .await?;
                    Reply::Done(format!(
                        "worker {node} is draining: no new actions; running actions finish"
                    ))
                }
                Request::UndrainWorker { node } => {
                    m.undrain_worker(pb::DrainWorkerRequest {
                        node: node.clone(),
                        ..Default::default()
                    })
                    .await?;
                    Reply::Done(format!("worker {node} accepts actions again"))
                }
                Request::DrainHost { serial } => {
                    let r = m.drain_host(host(&serial)).await?;
                    Reply::Done(non_empty(r.message, || {
                        format!("host {serial} is draining")
                    }))
                }
                Request::UncordonHost { serial } => {
                    let r = m.uncordon_host(host(&serial)).await?;
                    Reply::Done(non_empty(r.message, || format!("host {serial} uncordoned")))
                }
                Request::ReimageHost { serial } => {
                    let r = m
                        .reimage_host(pb::ReimageHostRequest {
                            host: host(&serial).into(),
                            ..Default::default()
                        })
                        .await?;
                    Reply::Done(non_empty(r.message, || {
                        format!("host {serial}: re-image scheduled")
                    }))
                }
                Request::KillOperation { name } => {
                    m.kill_operations(pb::KillOperationsRequest {
                        target: Some(KillTarget::OperationName(name.clone())),
                        message: "killed from cucinactl tui".into(),
                        ..Default::default()
                    })
                    .await?;
                    Reply::Done(format!("operation {name} killed"))
                }
                Request::RevokeServiceKey { key_id } => {
                    m.revoke_service_key(pb::RevokeServiceKeyRequest {
                        key_id: key_id.clone(),
                        ..Default::default()
                    })
                    .await?;
                    Reply::Done(format!("service key {key_id} revoked"))
                }
            };
            Ok(reply)
        }
        .boxed()
    }
}

fn non_empty(s: String, default: impl FnOnce() -> String) -> String {
    if s.trim().is_empty() { default() } else { s }
}
