// SPDX-License-Identifier: FSL-1.1-ALv2

//! Reusable client library for the Cucina management API (R-CLI-2) and the REAPI
//! client endpoint, shared by the CLI commands and the TUI (`cucinactl tui`).
//!
//! * connect-rust clients speaking the **gRPC protocol** over HTTP/2 (the servers
//!   are grpc-go; ADR 0003), on one shared HTTP/2 connection per endpoint that
//!   reconnects by itself.
//! * TLS: rustls with the aws-lc-rs provider, OS trust via
//!   `rustls-platform-verifier`, optional extra CA bundle; plaintext `http://` only
//!   to loopback (tests).
//! * Every call carries `Authorization: Bearer <Cucina JWT>` from a shared
//!   [`TokenHandle`] that [`spawn_refresher`] keeps fresh for long sessions.
//! * Unary calls have a deadline; `Watch*` server streams reconnect with jittered
//!   exponential backoff ([`watch`]) and stop when the consumer drops the stream.
//! * The CLI never needs kubeconfig: the controller proxies cluster state.

pub mod reapi;
pub mod watch;

use std::sync::{Arc, RwLock};
use std::time::Duration;

use anyhow::{Context, Result};
use connectrpc::client::{CallOptions, ClientConfig, Http2Connection, SharedHttp2Connection};
use connectrpc::{ConnectError, Protocol};
use cucina_api::connect::cucina::v1::ManagementServiceClient;
use cucina_api::proto::cucina::v1 as pb;
use futures::StreamExt as _;

use crate::auth::{self, ProfileCtx};
use crate::exit::CliError;

pub use watch::{WatchItem, WatchOptions};

/// Largest response message accepted (support bundles stream in chunks).
pub const MAX_MESSAGE_SIZE: usize = 64 << 20;

/// The current Cucina JWT, shared by every client of a session.
#[derive(Debug, Clone, Default)]
pub struct TokenHandle(Arc<RwLock<Option<String>>>);

impl TokenHandle {
    pub fn new(token: impl Into<String>) -> TokenHandle {
        TokenHandle(Arc::new(RwLock::new(Some(token.into()))))
    }
    pub fn set(&self, token: impl Into<String>) {
        if let Ok(mut g) = self.0.write() {
            *g = Some(token.into());
        }
    }
    pub fn get(&self) -> Option<String> {
        self.0.read().ok().and_then(|g| g.clone())
    }

    /// Call options carrying the bearer token (and a deadline, for unary calls).
    pub fn options(&self, timeout: Option<Duration>) -> Result<CallOptions, ConnectError> {
        let mut o = CallOptions::default().with_max_message_size(MAX_MESSAGE_SIZE);
        if let Some(t) = timeout {
            o = o.with_timeout(t);
        }
        match self.get() {
            Some(token) => o.try_with_header("authorization", format!("Bearer {token}")),
            None => Ok(o),
        }
    }
}

/// Connection settings.
#[derive(Debug, Clone)]
pub struct ConnectOptions {
    /// `host:port`, `https://host:port`, `grpcs://host:port` (or `http://127.0.0.1:port`).
    pub endpoint: String,
    pub ca_file: Option<std::path::PathBuf>,
    pub connect_timeout: Duration,
}

/// A lazily connecting HTTP/2 transport plus the client configuration for it.
#[derive(Clone)]
pub struct Channel {
    pub transport: SharedHttp2Connection,
    pub config: ClientConfig,
}

/// Builds a channel (gRPC protocol). Connection errors surface as `UNAVAILABLE`
/// on the first call. Must run inside a tokio runtime.
pub fn channel(opts: &ConnectOptions) -> Result<Channel> {
    let raw = opts.endpoint.trim();
    let (uri, tls) = if let Some(rest) = raw.strip_prefix("grpcs://") {
        (format!("https://{rest}"), true)
    } else if let Some(rest) = raw.strip_prefix("grpc://") {
        (format!("http://{rest}"), false)
    } else if raw.starts_with("https://") {
        (raw.to_string(), true)
    } else if raw.starts_with("http://") {
        (raw.to_string(), false)
    } else {
        (format!("https://{raw}"), true)
    };
    let host =
        crate::config::host_of(&uri).with_context(|| format!("no host in endpoint {raw:?}"))?;
    if !tls
        && !(host == "127.0.0.1" || host == "::1" || host == "localhost")
        && std::env::var(crate::http::ALLOW_INSECURE_HTTP_ENV).as_deref() != Ok("1")
    {
        return Err(CliError::usage(format!(
            "refusing plaintext gRPC to {host}: use TLS (grpcs:// or https://)"
        ))
        .into());
    }
    let uri: http::Uri = uri
        .trim_end_matches('/')
        .parse()
        .with_context(|| format!("invalid endpoint {raw:?}"))?;
    let builder = Http2Connection::builder()
        .establishment_timeout(opts.connect_timeout)
        .keep_alive_interval(Duration::from_secs(30))
        .keep_alive_while_idle(true);
    let connection = if tls {
        let config = crate::tls::client_config(opts.ca_file.as_deref())?;
        builder.lazy_tls(uri.clone(), Arc::new(config))
    } else {
        builder.lazy_plaintext(uri.clone())
    };
    Ok(Channel {
        transport: connection.shared(1024),
        config: ClientConfig::new(uri)
            .with_protocol(Protocol::Grpc)
            .with_default_max_message_size(MAX_MESSAGE_SIZE),
    })
}

/// Keeps `handle` fresh for a long-running session (TUI, `ops watch`): renews the
/// cached token whenever fewer than 5 minutes remain.
pub fn spawn_refresher(ctx: ProfileCtx, handle: TokenHandle) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        loop {
            let wait = match auth::ensure_token(&ctx, auth::token::RENEW_BEFORE_SECS, 60).await {
                Ok(t) => {
                    handle.set(t.access_token.clone());
                    let left =
                        t.remaining(crate::util::now_unix()) - auth::token::RENEW_BEFORE_SECS;
                    Duration::from_secs(u64::try_from(left.max(5)).unwrap_or(5))
                }
                Err(e) => {
                    tracing::warn!("token renewal failed: {e:#}");
                    Duration::from_secs(15)
                }
            };
            tokio::time::sleep(wait).await;
        }
    })
}

/// Management API client.
#[derive(Clone)]
pub struct ManagementClient {
    inner: ManagementServiceClient<SharedHttp2Connection>,
    token: TokenHandle,
    timeout: Duration,
}

macro_rules! unary {
    ($name:ident, $with_options:ident, $req:ty, $resp:ty) => {
        pub async fn $name(&self, request: $req) -> Result<$resp, ConnectError> {
            let options = self.token.options(Some(self.timeout))?;
            self.inner
                .$with_options(request, options)
                .await
                .map(|r| r.into_owned())
        }
    };
}

impl ManagementClient {
    /// Wraps a channel (tests and the TUI may build their own).
    pub fn new(channel: Channel, token: TokenHandle, timeout: Duration) -> ManagementClient {
        ManagementClient {
            inner: ManagementServiceClient::new(channel.transport, channel.config),
            token,
            timeout,
        }
    }

    /// Per-call deadline for unary RPCs.
    pub fn timeout(&self) -> Duration {
        self.timeout
    }

    unary!(
        get_status,
        get_status_with_options,
        pb::GetStatusRequest,
        pb::GetStatusResponse
    );
    unary!(
        list_pools,
        list_pools_with_options,
        pb::ListPoolsRequest,
        pb::ListPoolsResponse
    );
    unary!(
        get_pool,
        get_pool_with_options,
        pb::GetPoolRequest,
        pb::GetPoolResponse
    );
    unary!(
        set_pool_floor,
        set_pool_floor_with_options,
        pb::SetPoolFloorRequest,
        pb::SetPoolFloorResponse
    );
    unary!(
        cordon_pool,
        cordon_pool_with_options,
        pb::CordonPoolRequest,
        pb::CordonPoolResponse
    );
    unary!(
        garbage_collect_pool,
        garbage_collect_pool_with_options,
        pb::GarbageCollectPoolRequest,
        pb::GarbageCollectPoolResponse
    );
    unary!(
        list_workers,
        list_workers_with_options,
        pb::ListWorkersRequest,
        pb::ListWorkersResponse
    );
    unary!(
        drain_worker,
        drain_worker_with_options,
        pb::DrainWorkerRequest,
        pb::DrainWorkerResponse
    );
    unary!(
        undrain_worker,
        undrain_worker_with_options,
        pb::DrainWorkerRequest,
        pb::DrainWorkerResponse
    );
    unary!(
        list_hosts,
        list_hosts_with_options,
        pb::ListHostsRequest,
        pb::ListHostsResponse
    );
    unary!(
        drain_host,
        drain_host_with_options,
        pb::HostRef,
        pb::HostActionResponse
    );
    unary!(
        uncordon_host,
        uncordon_host_with_options,
        pb::HostRef,
        pb::HostActionResponse
    );
    unary!(
        reimage_host,
        reimage_host_with_options,
        pb::ReimageHostRequest,
        pb::HostActionResponse
    );
    unary!(
        register_host_serials,
        register_host_serials_with_options,
        pb::RegisterHostSerialsRequest,
        pb::RegisterHostSerialsResponse
    );
    unary!(
        approve_host,
        approve_host_with_options,
        pb::ApproveHostRequest,
        pb::HostActionResponse
    );
    unary!(
        remove_host,
        remove_host_with_options,
        pb::HostRef,
        pb::HostActionResponse
    );
    unary!(
        create_enroll_token,
        create_enroll_token_with_options,
        pb::CreateEnrollTokenRequest,
        pb::CreateEnrollTokenResponse
    );
    unary!(
        list_enroll_tokens,
        list_enroll_tokens_with_options,
        pb::ListEnrollTokensRequest,
        pb::ListEnrollTokensResponse
    );
    unary!(
        revoke_enroll_token,
        revoke_enroll_token_with_options,
        pb::RevokeEnrollTokenRequest,
        pb::RevokeEnrollTokenResponse
    );
    unary!(
        list_queues,
        list_queues_with_options,
        pb::ListQueuesRequest,
        pb::ListQueuesResponse
    );
    unary!(
        list_operations,
        list_operations_with_options,
        pb::ListOperationsRequest,
        pb::ListOperationsResponse
    );
    unary!(
        get_operation,
        get_operation_with_options,
        pb::GetOperationRequest,
        pb::GetOperationResponse
    );
    unary!(
        kill_operations,
        kill_operations_with_options,
        pb::KillOperationsRequest,
        pb::KillOperationsResponse
    );
    unary!(
        create_service_key,
        create_service_key_with_options,
        pb::CreateServiceKeyRequest,
        pb::CreateServiceKeyResponse
    );
    unary!(
        list_service_keys,
        list_service_keys_with_options,
        pb::ListServiceKeysRequest,
        pb::ListServiceKeysResponse
    );
    unary!(
        revoke_service_key,
        revoke_service_key_with_options,
        pb::RevokeServiceKeyRequest,
        pb::RevokeServiceKeyResponse
    );
    unary!(
        revoke_principal,
        revoke_principal_with_options,
        pb::RevokePrincipalRequest,
        pb::RevokePrincipalResponse
    );
    unary!(
        list_revocations,
        list_revocations_with_options,
        pb::ListRevocationsRequest,
        pb::ListRevocationsResponse
    );
    unary!(
        get_cost,
        get_cost_with_options,
        pb::GetCostRequest,
        pb::GetCostResponse
    );
    unary!(
        list_images,
        list_images_with_options,
        pb::ListImagesRequest,
        pb::ListImagesResponse
    );

    /// `WatchOverview` with reconnect/backoff (TUI overview, ≤ 2 s refresh).
    pub fn watch_overview(
        &self,
        request: pb::WatchOverviewRequest,
        opts: WatchOptions,
    ) -> impl futures::Stream<Item = Result<WatchItem<pb::Overview>, ConnectError>> + use<> {
        let client = self.inner.clone();
        let token = self.token.clone();
        watch::reconnecting(opts, move || {
            let client = client.clone();
            let request = request.clone();
            let options = token.options(None);
            async move {
                let stream = client
                    .watch_overview_with_options(request, options?)
                    .await?;
                Ok(futures::stream::unfold(Some(stream), |state| async move {
                    let mut s = state?;
                    match s.message().await {
                        Ok(Some(m)) => Some((Ok(m.to_owned_message()), Some(s))),
                        Ok(None) => None,
                        Err(e) => Some((Err(e), None)),
                    }
                })
                .boxed())
            }
        })
    }

    /// `WatchOperations` with reconnect/backoff (`ops watch`, TUI operations view).
    pub fn watch_operations(
        &self,
        request: pb::ListOperationsRequest,
        opts: WatchOptions,
    ) -> impl futures::Stream<Item = Result<WatchItem<pb::OperationEvent>, ConnectError>> + use<>
    {
        let client = self.inner.clone();
        let token = self.token.clone();
        watch::reconnecting(opts, move || {
            let client = client.clone();
            let request = request.clone();
            let options = token.options(None);
            async move {
                let stream = client
                    .watch_operations_with_options(request, options?)
                    .await?;
                Ok(futures::stream::unfold(Some(stream), |state| async move {
                    let mut s = state?;
                    match s.message().await {
                        Ok(Some(m)) => Some((Ok(m.to_owned_message()), Some(s))),
                        Ok(None) => None,
                        Err(e) => Some((Err(e), None)),
                    }
                })
                .boxed())
            }
        })
    }

    /// Collects a `LogChunk` server stream (worker logs, host diagnostics, support
    /// bundle), calling `sink` for each chunk; stops at the chunk marked `last`.
    pub async fn stream_chunks(
        &self,
        kind: ChunkStream,
        mut sink: impl FnMut(&[u8]) -> Result<()>,
    ) -> Result<()> {
        let options = self.token.options(None)?;
        let mut stream = match kind {
            ChunkStream::WorkerLogs(r) => {
                self.inner
                    .stream_worker_logs_with_options(r, options)
                    .await?
            }
            ChunkStream::HostDiagnostics(r) => {
                self.inner.host_diagnostics_with_options(r, options).await?
            }
            ChunkStream::SupportBundle(r) => {
                self.inner
                    .collect_support_bundle_with_options(r, options)
                    .await?
            }
        };
        while let Some(chunk) = stream.message().await? {
            let chunk = chunk.to_owned_message();
            sink(&chunk.data)?;
            if chunk.last {
                break;
            }
        }
        Ok(())
    }
}

/// The `LogChunk` streaming RPCs.
#[derive(Debug, Clone)]
pub enum ChunkStream {
    WorkerLogs(pb::StreamWorkerLogsRequest),
    HostDiagnostics(pb::HostRef),
    SupportBundle(pb::CollectSupportBundleRequest),
}

/// An authenticated session for one profile: management client + token handle.
pub struct Session {
    pub ctx: ProfileCtx,
    pub token: TokenHandle,
    pub management: ManagementClient,
    pub timeout: Duration,
}

impl Session {
    /// Ensures a valid token (renewing if needed) and connects lazily.
    pub async fn open(ctx: ProfileCtx, timeout: Duration) -> Result<Session> {
        let token = auth::ensure_token(&ctx, 60, 30).await?;
        let handle = TokenHandle::new(token.access_token);
        let channel = channel(&ConnectOptions {
            endpoint: ctx.profile.management.clone(),
            ca_file: ctx.profile.ca_file.clone(),
            connect_timeout: timeout.min(Duration::from_secs(10)),
        })?;
        let management = ManagementClient::new(channel, handle.clone(), timeout);
        Ok(Session {
            ctx,
            token: handle,
            management,
            timeout,
        })
    }

    /// A CAS/AC/ByteStream client on the client endpoint with the same token.
    pub fn reapi(&self, instance_name: &str) -> Result<reapi::Reapi> {
        let channel = channel(&ConnectOptions {
            endpoint: self.ctx.profile.remote_executor.clone(),
            ca_file: self.ctx.profile.ca_file.clone(),
            connect_timeout: self.timeout.min(Duration::from_secs(10)),
        })?;
        Ok(reapi::Reapi::new(
            channel,
            self.token.clone(),
            instance_name,
            self.timeout,
        ))
    }
}
