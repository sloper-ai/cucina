// SPDX-License-Identifier: FSL-1.1-ALv2

//! In-process fake of the controller's `ManagementService` (connect-rust server,
//! gRPC protocol like the real grpc-go server). State-based: mutations change what
//! later reads return. Every call must carry `authorization: Bearer <token>`;
//! `fail_next` injects one error per method (FailNext-style knob).

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use connectrpc::{
    ConnectError, ErrorCode, RequestContext, Response, ServiceRequest, ServiceResult, ServiceStream,
};
use cucina_api::connect::cucina::v1::ManagementService;
use cucina_api::proto::cucina::v1 as pb;
use cucinactl::util::{PbDuration, PbTimestamp};

pub fn ts(seconds: i64) -> buffa::MessageField<PbTimestamp, buffa::Inline<PbTimestamp>> {
    PbTimestamp {
        seconds,
        ..Default::default()
    }
    .into()
}

pub fn dur(seconds: i64) -> buffa::MessageField<PbDuration, buffa::Inline<PbDuration>> {
    PbDuration {
        seconds,
        ..Default::default()
    }
    .into()
}

fn money(micros: i64) -> buffa::MessageField<pb::Money, buffa::Inline<pb::Money>> {
    pb::Money {
        micros,
        ..Default::default()
    }
    .into()
}

fn prop(name: &str, value: &str) -> pb::PlatformProperty {
    pb::PlatformProperty {
        name: name.into(),
        value: value.into(),
        ..Default::default()
    }
}

pub fn queue_ref() -> pb::QueueRef {
    pb::QueueRef {
        instance_name_prefix: "main".into(),
        platform: vec![prop("ISA", "x86-64"), prop("OSFamily", "linux")],
        size_class: 1,
        ..Default::default()
    }
}

const T0: i64 = 1_790_000_000;

pub fn pool() -> pb::PoolSummary {
    pb::PoolSummary {
        name: "linux-x86-64".into(),
        platform: "linux-x86-64".into(),
        provider: "ec2".into(),
        min_running: 0,
        max: 4,
        desired: 1,
        launching: 1,
        registered: 0,
        busy: 0,
        idle: 0,
        draining: 0,
        stopped: 0,
        image_generation: "g7".into(),
        paused: false,
        condition: "Ready".into(),
        message: "".into(),
        ..Default::default()
    }
}

pub fn worker() -> pb::WorkerSummary {
    pb::WorkerSummary {
        node: "i-0123456789abcdef0".into(),
        pool: "linux-x86-64".into(),
        state: "busy".into(),
        generation: "g7".into(),
        instance_type: "c8i.8xlarge".into(),
        host: "".into(),
        threads: 32,
        busy_threads: 12,
        drained: false,
        launched: ts(T0),
        idle_for: dur(0),
        private_ip: "10.0.1.23".into(),
        ..Default::default()
    }
}

pub fn operation() -> pb::OperationSummary {
    pb::OperationSummary {
        name: "op-1".into(),
        queue: queue_ref().into(),
        action_digest: format!("{}/142", "a".repeat(64)),
        digest_function: "SHA256".into(),
        stage: "executing".into(),
        queued_at: ts(T0 + 5),
        target_id: "//absl/strings:str_cat_test".into(),
        invocation_id: "inv-1".into(),
        tool_invocation_id: "tool-1".into(),
        priority: 0,
        worker_node: "i-0123456789abcdef0".into(),
        worker_thread: 3,
        expected_duration: dur(12),
        timeout: ts(T0 + 3600),
        ..Default::default()
    }
}

fn host() -> pb::HostDetail {
    pb::HostDetail {
        summary: pb::HostSummary {
            serial: "C02XK0AAJGH6".into(),
            name: "mini-1".into(),
            site: "office".into(),
            phase: "Online".into(),
            running_vms: 1,
            slots: 2,
            l2_hit_ratio: "0.93".into(),
            wan_bytes_received: 123_456,
            last_heartbeat: ts(T0 + 30),
            agent_version: "0.1.0".into(),
            ..Default::default()
        }
        .into(),
        macos_version: "27.0".into(),
        tart_version: "2.40.1".into(),
        chip: "Apple M4 Pro".into(),
        cores: 14,
        memory_gib: 64,
        disk_free_gib: 512,
        vms: vec![pb::HostVM {
            name: "vm-a".into(),
            pool: "macos-arm64-xcode27.0".into(),
            state: "running".into(),
            image: "ghcr.io/example/macos:27".into(),
            generation: "g2".into(),
            registered: true,
            ..Default::default()
        }],
        images: vec!["ghcr.io/example/macos:27".into()],
        cordoned: false,
        approved: true,
        labels: [("rack".to_string(), "a".to_string())]
            .into_iter()
            .collect(),
        cert_expiry: ts(T0 + 86_400),
        filevault: "off".into(),
        ..Default::default()
    }
}

/// Mutable state of the fake.
pub struct MgmtState {
    pub token: String,
    pub pools: Mutex<Vec<pb::PoolSummary>>,
    pub workers: Mutex<Vec<pb::WorkerSummary>>,
    pub operations: Mutex<Vec<pb::OperationSummary>>,
    /// Serialized ExecuteResponse per completed operation name (GetOperation).
    pub execute_responses: Mutex<HashMap<String, Vec<u8>>>,
    pub keys: Mutex<Vec<pb::ServiceKeyInfo>>,
    pub revocations: Mutex<Vec<pb::Revocation>>,
    pub fail: Mutex<HashMap<String, (ErrorCode, String)>>,
    /// The first N `WatchOperations` streams fail with UNAVAILABLE after one event.
    pub watch_breaks: AtomicUsize,
    pub watch_calls: AtomicUsize,
    /// Methods called (only for asserting that a request reached the server).
    pub calls: Mutex<Vec<String>>,
}

#[derive(Clone)]
pub struct FakeMgmt {
    pub state: Arc<MgmtState>,
}

impl FakeMgmt {
    pub fn new(token: &str) -> FakeMgmt {
        FakeMgmt {
            state: Arc::new(MgmtState {
                token: token.into(),
                pools: Mutex::new(vec![pool()]),
                workers: Mutex::new(vec![worker()]),
                operations: Mutex::new(vec![operation()]),
                execute_responses: Mutex::new(HashMap::new()),
                keys: Mutex::new(vec![pb::ServiceKeyInfo {
                    key_id: "k1".into(),
                    account: "ci-bot".into(),
                    description: "CI".into(),
                    created: ts(T0),
                    expires_at: ts(T0 + 86_400 * 30),
                    last_used: ts(T0 + 60),
                    revoked: false,
                    ..Default::default()
                }]),
                revocations: Mutex::new(Vec::new()),
                fail: Mutex::new(HashMap::new()),
                watch_breaks: AtomicUsize::new(0),
                watch_calls: AtomicUsize::new(0),
                calls: Mutex::new(Vec::new()),
            }),
        }
    }

    /// Makes the next call of `method` (snake_case RPC name) fail.
    pub fn fail_next(&self, method: &str, code: ErrorCode, message: &str) {
        self.state
            .fail
            .lock()
            .unwrap()
            .insert(method.into(), (code, message.into()));
    }

    /// Serves on 127.0.0.1:<ephemeral> and returns `http://127.0.0.1:<port>`.
    pub async fn serve(&self) -> String {
        let bound = connectrpc::server::Server::bind("127.0.0.1:0")
            .await
            .expect("bind");
        let addr: SocketAddr = bound.local_addr().unwrap();
        let router = connectrpc::Router::new().add_service(Arc::new(self.clone()));
        tokio::spawn(async move {
            let _ = bound.serve(router).await;
        });
        format!("http://{addr}")
    }

    fn check(&self, ctx: &RequestContext, method: &str) -> Result<(), ConnectError> {
        self.state.calls.lock().unwrap().push(method.to_string());
        let auth = ctx.header("authorization").and_then(|v| v.to_str().ok());
        if auth != Some(format!("Bearer {}", self.state.token).as_str()) {
            return Err(ConnectError::new(
                ErrorCode::Unauthenticated,
                "missing or invalid bearer token",
            ));
        }
        if let Some((code, msg)) = self.state.fail.lock().unwrap().remove(method) {
            return Err(ConnectError::new(code, msg));
        }
        Ok(())
    }
}

macro_rules! unary {
    ($name:ident, $req:ty, $resp:ty, |$this:ident, $r:ident| $body:expr) => {
        async fn $name(
            &self,
            ctx: RequestContext,
            req: ServiceRequest<'_, $req>,
        ) -> ServiceResult<$resp> {
            self.check(&ctx, stringify!($name))?;
            #[allow(unused_variables)]
            let $r: $req = req.to_owned_message();
            #[allow(unused_variables)]
            let $this = self;
            Response::ok($body)
        }
    };
}

fn chunks(parts: Vec<&'static [u8]>) -> ServiceStream<pb::LogChunk> {
    let n = parts.len();
    Box::pin(futures::stream::iter(parts.into_iter().enumerate().map(
        move |(i, p)| {
            Ok(pb::LogChunk {
                data: p.to_vec(),
                last: i + 1 == n,
                ..Default::default()
            })
        },
    )))
}

#[allow(refining_impl_trait)]
impl ManagementService for FakeMgmt {
    unary!(
        get_status,
        pb::GetStatusRequest,
        pb::GetStatusResponse,
        |me, _r| pb::GetStatusResponse {
            cluster_id: "test-cluster".into(),
            version: "0.1.0".into(),
            protocol: pb::ProtocolVersion {
                major: 1,
                minor: 0,
                ..Default::default()
            }
            .into(),
            components: vec![pb::ComponentStatus {
                name: "frontend".into(),
                state: "ready".into(),
                message: "".into(),
                ready_replicas: 1,
                desired_replicas: 1,
                ..Default::default()
            }],
            pools: me.state.pools.lock().unwrap().clone(),
            hosts_online: 1,
            hosts_total: 1,
            alerts: vec![pb::Alert {
                name: "QueueTimeHigh".into(),
                severity: "warning".into(),
                summary: "queue time above 30s".into(),
                since: ts(T0),
                labels: [("pool".to_string(), "linux-x86-64".to_string())]
                    .into_iter()
                    .collect(),
                ..Default::default()
            }],
            worker_instances: 1,
            ..Default::default()
        }
    );

    async fn watch_overview(
        &self,
        ctx: RequestContext,
        _req: ServiceRequest<'_, pb::WatchOverviewRequest>,
    ) -> ServiceResult<ServiceStream<pb::Overview>> {
        self.check(&ctx, "watch_overview")?;
        let item = pb::Overview {
            time: ts(T0),
            pools: self.state.pools.lock().unwrap().clone(),
            ..Default::default()
        };
        Response::stream_ok(futures::stream::iter(vec![Ok(item)]))
    }

    unary!(
        list_pools,
        pb::ListPoolsRequest,
        pb::ListPoolsResponse,
        |me, _r| pb::ListPoolsResponse {
            pools: me.state.pools.lock().unwrap().clone(),
            ..Default::default()
        }
    );

    async fn get_pool(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, pb::GetPoolRequest>,
    ) -> ServiceResult<pb::GetPoolResponse> {
        self.check(&ctx, "get_pool")?;
        let name = req.name.to_string();
        let pools = self.state.pools.lock().unwrap().clone();
        let Some(summary) = pools.into_iter().find(|p| p.name == name) else {
            return Err(ConnectError::new(
                ErrorCode::NotFound,
                format!("pool {name} not found"),
            ));
        };
        Response::ok(pb::GetPoolResponse {
            summary: summary.into(),
            spec_json: r#"{"platform":"linux-x86-64","max":4}"#.into(),
            conditions: vec!["Ready=True Reconciled: ok".into()],
            workers: self.state.workers.lock().unwrap().clone(),
            events: vec![pb::PoolEvent {
                time: ts(T0),
                r#type: "launch".into(),
                subject: "i-0123456789abcdef0".into(),
                message: "launched c8i.8xlarge".into(),
                ..Default::default()
            }],
            starts: vec![pb::StartLatency {
                pool: "linux-x86-64".into(),
                vm: "i-0123456789abcdef0".into(),
                launched: ts(T0),
                to_running: dur(20),
                to_registered: dur(38),
                to_first_action: dur(41),
                path: "slow".into(),
                ..Default::default()
            }],
            ..Default::default()
        })
    }

    unary!(
        set_pool_floor,
        pb::SetPoolFloorRequest,
        pb::SetPoolFloorResponse,
        |_me, r| {
            pb::SetPoolFloorResponse {
                expires_at: ts(T0 + r.expires_in.as_option().map_or(0, |d| d.seconds)),
                ..Default::default()
            }
        }
    );

    unary!(
        cordon_pool,
        pb::CordonPoolRequest,
        pb::CordonPoolResponse,
        |me, r| {
            for p in me
                .state
                .pools
                .lock()
                .unwrap()
                .iter_mut()
                .filter(|p| p.name == r.name)
            {
                p.paused = r.cordon;
            }
            pb::CordonPoolResponse::default()
        }
    );

    unary!(
        garbage_collect_pool,
        pb::GarbageCollectPoolRequest,
        pb::GarbageCollectPoolResponse,
        |_me, _r| {
            pb::GarbageCollectPoolResponse {
                deleted: vec!["volume vol-0abc".into()],
                errors: vec![],
                ..Default::default()
            }
        }
    );

    unary!(
        list_workers,
        pb::ListWorkersRequest,
        pb::ListWorkersResponse,
        |me, r| pb::ListWorkersResponse {
            workers: me
                .state
                .workers
                .lock()
                .unwrap()
                .iter()
                .filter(|w| r.pool.is_empty() || w.pool == r.pool)
                .cloned()
                .collect(),
            ..Default::default()
        }
    );

    async fn drain_worker(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, pb::DrainWorkerRequest>,
    ) -> ServiceResult<pb::DrainWorkerResponse> {
        self.check(&ctx, "drain_worker")?;
        let node = req.node.to_string();
        let mut workers = self.state.workers.lock().unwrap();
        let Some(w) = workers.iter_mut().find(|w| w.node == node) else {
            return Err(ConnectError::new(
                ErrorCode::NotFound,
                format!("worker {node} not found"),
            ));
        };
        w.drained = true;
        Response::ok(pb::DrainWorkerResponse::default())
    }

    unary!(
        undrain_worker,
        pb::DrainWorkerRequest,
        pb::DrainWorkerResponse,
        |me, r| {
            for w in me
                .state
                .workers
                .lock()
                .unwrap()
                .iter_mut()
                .filter(|w| w.node == r.node)
            {
                w.drained = false;
            }
            pb::DrainWorkerResponse::default()
        }
    );

    async fn stream_worker_logs(
        &self,
        ctx: RequestContext,
        _req: ServiceRequest<'_, pb::StreamWorkerLogsRequest>,
    ) -> ServiceResult<ServiceStream<pb::LogChunk>> {
        self.check(&ctx, "stream_worker_logs")?;
        Response::stream_ok(chunks(vec![b"line 1\n", b"line 2\n"]))
    }

    unary!(
        list_hosts,
        pb::ListHostsRequest,
        pb::ListHostsResponse,
        |_me, _r| pb::ListHostsResponse {
            hosts: vec![host()],
            ..Default::default()
        }
    );

    unary!(drain_host, pb::HostRef, pb::HostActionResponse, |_me, r| {
        pb::HostActionResponse {
            message: format!("draining {}", r.serial),
            ..Default::default()
        }
    });

    unary!(
        uncordon_host,
        pb::HostRef,
        pb::HostActionResponse,
        |_me, r| pb::HostActionResponse {
            message: format!("uncordoned {}", r.serial),
            ..Default::default()
        }
    );

    unary!(
        reimage_host,
        pb::ReimageHostRequest,
        pb::HostActionResponse,
        |_me, _r| {
            pb::HostActionResponse {
                message: "re-image scheduled".into(),
                ..Default::default()
            }
        }
    );

    async fn host_diagnostics(
        &self,
        ctx: RequestContext,
        _req: ServiceRequest<'_, pb::HostRef>,
    ) -> ServiceResult<ServiceStream<pb::LogChunk>> {
        self.check(&ctx, "host_diagnostics")?;
        Response::stream_ok(chunks(vec![b"diag-part-1", b"diag-part-2"]))
    }

    unary!(
        register_host_serials,
        pb::RegisterHostSerialsRequest,
        pb::RegisterHostSerialsResponse,
        |_me, r| {
            pb::RegisterHostSerialsResponse {
                registered: r.serials.clone(),
                already_present: vec![],
                ..Default::default()
            }
        }
    );

    unary!(
        approve_host,
        pb::ApproveHostRequest,
        pb::HostActionResponse,
        |_me, r| pb::HostActionResponse {
            message: format!("approved {}", r.serial),
            ..Default::default()
        }
    );

    unary!(
        remove_host,
        pb::HostRef,
        pb::HostActionResponse,
        |_me, r| pb::HostActionResponse {
            message: format!("removed {}", r.serial),
            ..Default::default()
        }
    );

    unary!(
        create_enroll_token,
        pb::CreateEnrollTokenRequest,
        pb::CreateEnrollTokenResponse,
        |_me, r| {
            pb::CreateEnrollTokenResponse {
                id: "et1".into(),
                token: format!("cuc_et_fake_{}", r.site),
                expires_at: ts(T0 + r.ttl.as_option().map_or(0, |d| d.seconds)),
                ..Default::default()
            }
        }
    );

    unary!(
        list_enroll_tokens,
        pb::ListEnrollTokensRequest,
        pb::ListEnrollTokensResponse,
        |_me, _r| {
            pb::ListEnrollTokensResponse {
                tokens: vec![pb::EnrollTokenInfo {
                    id: "et1".into(),
                    site: "office".into(),
                    description: "first batch".into(),
                    created: ts(T0),
                    expires_at: ts(T0 + 7 * 86_400),
                    max_hosts: 10,
                    used_hosts: 1,
                    revoked: false,
                    ..Default::default()
                }],
                ..Default::default()
            }
        }
    );

    unary!(
        revoke_enroll_token,
        pb::RevokeEnrollTokenRequest,
        pb::RevokeEnrollTokenResponse,
        |_me, _r| pb::RevokeEnrollTokenResponse::default()
    );

    unary!(
        list_queues,
        pb::ListQueuesRequest,
        pb::ListQueuesResponse,
        |_me, _r| pb::ListQueuesResponse {
            queues: vec![pb::QueueSummary {
                queue: queue_ref().into(),
                pool: "linux-x86-64".into(),
                queued: 3,
                executing: 12,
                idle_workers: 0,
                total_workers: 32,
                drains: 0,
                oldest_queued_age: dur(4),
                queue_time_p95: dur(1),
                ..Default::default()
            }],
            ..Default::default()
        }
    );

    unary!(
        list_operations,
        pb::ListOperationsRequest,
        pb::ListOperationsResponse,
        |me, _r| {
            pb::ListOperationsResponse {
                operations: me.state.operations.lock().unwrap().clone(),
                next_page_token: "".into(),
                ..Default::default()
            }
        }
    );

    async fn watch_operations(
        &self,
        ctx: RequestContext,
        _req: ServiceRequest<'_, pb::ListOperationsRequest>,
    ) -> ServiceResult<ServiceStream<pb::OperationEvent>> {
        self.check(&ctx, "watch_operations")?;
        let call = self.state.watch_calls.fetch_add(1, Ordering::SeqCst);
        let breaks = self.state.watch_breaks.load(Ordering::SeqCst);
        let event = |kind: pb::operation_event::Kind, name: &str| pb::OperationEvent {
            kind: kind.into(),
            operation: pb::OperationSummary {
                name: name.into(),
                ..operation()
            }
            .into(),
            ..Default::default()
        };
        if call < breaks {
            // One event, then the stream breaks (server restart).
            let items = vec![
                Ok(event(
                    pb::operation_event::Kind::KIND_ADDED,
                    &format!("op-before-break-{call}"),
                )),
                Err(ConnectError::new(
                    ErrorCode::Unavailable,
                    "server restarting",
                )),
            ];
            return Response::stream_ok(futures::stream::iter(items));
        }
        let items = vec![
            Ok(event(pb::operation_event::Kind::KIND_ADDED, "op-2")),
            Ok(event(pb::operation_event::Kind::KIND_CHANGED, "op-2")),
            Ok(event(pb::operation_event::Kind::KIND_REMOVED, "op-2")),
        ];
        Response::stream_ok(futures::stream::iter(items))
    }

    async fn get_operation(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, pb::GetOperationRequest>,
    ) -> ServiceResult<pb::GetOperationResponse> {
        self.check(&ctx, "get_operation")?;
        let name = req.name.to_string();
        let ops = self.state.operations.lock().unwrap().clone();
        match ops.into_iter().find(|o| o.name == name) {
            Some(op) => Response::ok(pb::GetOperationResponse {
                operation: op.into(),
                execute_response: self
                    .state
                    .execute_responses
                    .lock()
                    .unwrap()
                    .get(&name)
                    .cloned()
                    .unwrap_or_default(),
                ..Default::default()
            }),
            None => Err(ConnectError::new(
                ErrorCode::NotFound,
                format!("operation {name} not found"),
            )),
        }
    }

    unary!(
        kill_operations,
        pb::KillOperationsRequest,
        pb::KillOperationsResponse,
        |_me, _r| pb::KillOperationsResponse::default()
    );

    unary!(
        create_service_key,
        pb::CreateServiceKeyRequest,
        pb::CreateServiceKeyResponse,
        |_me, _r| {
            pb::CreateServiceKeyResponse {
                key_id: "k2".into(),
                key: format!("cuc_sk_{}_{}", "b".repeat(16), "f".repeat(52)),
                ..Default::default()
            }
        }
    );

    unary!(
        list_service_keys,
        pb::ListServiceKeysRequest,
        pb::ListServiceKeysResponse,
        |me, _r| {
            pb::ListServiceKeysResponse {
                keys: me.state.keys.lock().unwrap().clone(),
                ..Default::default()
            }
        }
    );

    unary!(
        revoke_service_key,
        pb::RevokeServiceKeyRequest,
        pb::RevokeServiceKeyResponse,
        |me, r| {
            for k in me
                .state
                .keys
                .lock()
                .unwrap()
                .iter_mut()
                .filter(|k| k.key_id == r.key_id)
            {
                k.revoked = true;
            }
            pb::RevokeServiceKeyResponse::default()
        }
    );

    unary!(
        revoke_principal,
        pb::RevokePrincipalRequest,
        pb::RevokePrincipalResponse,
        |me, r| {
            me.state.revocations.lock().unwrap().push(pb::Revocation {
                sub: r.sub.clone(),
                sid: r.sid.clone(),
                reason: r.reason.clone(),
                created: ts(T0),
                created_by: "admin@example.com".into(),
                ..Default::default()
            });
            pb::RevokePrincipalResponse {
                effective_by: ts(T0 + 180),
                ..Default::default()
            }
        }
    );

    unary!(
        list_revocations,
        pb::ListRevocationsRequest,
        pb::ListRevocationsResponse,
        |me, _r| {
            pb::ListRevocationsResponse {
                revocations: me.state.revocations.lock().unwrap().clone(),
                ..Default::default()
            }
        }
    );

    unary!(
        get_cost,
        pb::GetCostRequest,
        pb::GetCostResponse,
        |_me, _r| pb::GetCostResponse {
            cost: pb::CostSummary {
                today: money(1_250_000),
                month_to_date: money(42_000_000),
                standing_per_month: money(9_500_000),
                pools: vec![pb::PoolCost {
                    pool: "linux-x86-64".into(),
                    instance_seconds: 7_200,
                    compute: money(2_900_000),
                    ebs: money(40_000),
                    data_transfer: money(0),
                    standing: money(120_000),
                    ..Default::default()
                }],
                ..Default::default()
            }
            .into(),
            lines: vec![pb::CostLine {
                pool: "linux-x86-64".into(),
                category: "compute".into(),
                detail: "c8i.8xlarge".into(),
                quantity: 2.0,
                unit: "instance-hours".into(),
                amount: money(2_900_000),
                ..Default::default()
            }],
            assumptions: "on-demand prices of 2026-10-01; 60 s minimum".into(),
            ..Default::default()
        }
    );

    unary!(
        list_images,
        pb::ListImagesRequest,
        pb::ListImagesResponse,
        |_me, _r| pb::ListImagesResponse {
            images: vec![pb::ImageInfo {
                pool: "linux-x86-64".into(),
                reference: "ami-0123456789abcdef0".into(),
                version: "2026.10.01".into(),
                generation: "g7".into(),
                current: true,
                previous: false,
                workers_running: 1,
                created: ts(T0 - 86_400),
                fast_launch: "n-a".into(),
                ..Default::default()
            }],
            ..Default::default()
        }
    );

    async fn collect_support_bundle(
        &self,
        ctx: RequestContext,
        _req: ServiceRequest<'_, pb::CollectSupportBundleRequest>,
    ) -> ServiceResult<ServiceStream<pb::LogChunk>> {
        self.check(&ctx, "collect_support_bundle")?;
        Response::stream_ok(chunks(vec![b"bundle-1", b"bundle-2"]))
    }
}
