// SPDX-License-Identifier: FSL-1.1-ALv2

//! In-process fake of the REAPI client endpoint (connect-rust server, gRPC
//! protocol): Capabilities (advertises ZSTD), ActionCache.GetActionResult, and
//! ByteStream.Read for `blobs/` and `compressed-blobs/zstd/` resource names, over
//! an in-memory CAS. Requires `authorization: Bearer <token>`.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use connectrpc::{
    ConnectError, ErrorCode, InboundStream, RequestContext, Response, ServiceRequest,
    ServiceResult, ServiceStream,
};
use cucina_api::connect::build::bazel::remote::execution::v2::{
    ActionCache, ActionCacheRegisterMarker, Capabilities, CapabilitiesRegisterMarker,
};
use cucina_api::connect::google::bytestream::{ByteStream, ByteStreamRegisterMarker};
use cucina_api::proto::build::bazel::remote::execution::v2 as re;
use cucina_api::proto::google::bytestream as bs;
use sha2::Digest as _;

/// Content-addressed storage + action cache.
#[derive(Default)]
pub struct Store {
    pub blobs: Mutex<HashMap<String, Vec<u8>>>,
    pub action_results: Mutex<HashMap<String, re::ActionResult>>,
    /// Resource names read through ByteStream (asserts compressed reads).
    pub reads: Mutex<Vec<String>>,
}

#[derive(Clone)]
pub struct FakeReapi {
    pub store: Arc<Store>,
    pub token: String,
    pub zstd: bool,
}

/// The digest of `data`.
pub fn digest(data: &[u8]) -> re::Digest {
    re::Digest {
        hash: cucinactl::util::hex(&sha2::Sha256::digest(data)),
        size_bytes: data.len() as i64,
        ..Default::default()
    }
}

impl FakeReapi {
    pub fn new(token: &str) -> FakeReapi {
        FakeReapi {
            store: Arc::new(Store::default()),
            token: token.into(),
            zstd: true,
        }
    }

    /// Stores a blob and returns its digest.
    pub fn put(&self, data: &[u8]) -> re::Digest {
        let d = digest(data);
        self.store
            .blobs
            .lock()
            .unwrap()
            .insert(d.hash.clone(), data.to_vec());
        d
    }

    /// Stores a protobuf message and returns its digest.
    pub fn put_proto<M: buffa::Message>(&self, m: &M) -> re::Digest {
        self.put(&m.encode_to_vec())
    }

    pub async fn serve(&self) -> String {
        let bound = connectrpc::server::Server::bind("127.0.0.1:0")
            .await
            .expect("bind");
        let addr: SocketAddr = bound.local_addr().unwrap();
        let router = connectrpc::Router::new()
            .add_service::<FakeReapi, CapabilitiesRegisterMarker>(Arc::new(self.clone()))
            .add_service::<FakeReapi, ActionCacheRegisterMarker>(Arc::new(self.clone()))
            .add_service::<FakeReapi, ByteStreamRegisterMarker>(Arc::new(self.clone()));
        tokio::spawn(async move {
            let _ = bound.serve(router).await;
        });
        format!("grpc://{addr}")
    }

    fn check(&self, ctx: &RequestContext) -> Result<(), ConnectError> {
        let auth = ctx.header("authorization").and_then(|v| v.to_str().ok());
        if auth == Some(format!("Bearer {}", self.token).as_str()) {
            Ok(())
        } else {
            Err(ConnectError::new(
                ErrorCode::Unauthenticated,
                "missing or invalid bearer token",
            ))
        }
    }
}

fn unimplemented<T>() -> ServiceResult<T> {
    Err(ConnectError::new(
        ErrorCode::Unimplemented,
        "not implemented by the fake",
    ))
}

#[allow(refining_impl_trait)]
impl Capabilities for FakeReapi {
    async fn get_capabilities(
        &self,
        ctx: RequestContext,
        _req: ServiceRequest<'_, re::GetCapabilitiesRequest>,
    ) -> ServiceResult<re::ServerCapabilities> {
        self.check(&ctx)?;
        let compressors = if self.zstd {
            vec![re::compressor::Value::ZSTD.into()]
        } else {
            vec![]
        };
        Response::ok(re::ServerCapabilities {
            cache_capabilities: re::CacheCapabilities {
                supported_compressors: compressors,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })
    }
}

#[allow(refining_impl_trait)]
impl ActionCache for FakeReapi {
    async fn get_action_result(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, re::GetActionResultRequest>,
    ) -> ServiceResult<re::ActionResult> {
        self.check(&ctx)?;
        let req = req.to_owned_message();
        let hash = req
            .action_digest
            .as_option()
            .map(|d| d.hash.clone())
            .unwrap_or_default();
        match self.store.action_results.lock().unwrap().get(&hash) {
            Some(r) => Response::ok(r.clone()),
            None => Err(ConnectError::new(ErrorCode::NotFound, "no action result")),
        }
    }

    async fn update_action_result(
        &self,
        _ctx: RequestContext,
        _req: ServiceRequest<'_, re::UpdateActionResultRequest>,
    ) -> ServiceResult<re::ActionResult> {
        unimplemented()
    }
}

#[allow(refining_impl_trait)]
impl ByteStream for FakeReapi {
    async fn read(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, bs::ReadRequest>,
    ) -> ServiceResult<ServiceStream<bs::ReadResponse>> {
        self.check(&ctx)?;
        let name = req.resource_name.to_string();
        self.store.reads.lock().unwrap().push(name.clone());
        // [{instance}/]{compressed-blobs/zstd|blobs}/{hash}/{size}
        let parts: Vec<&str> = name.split('/').collect();
        let (compressed, hash) = match parts
            .iter()
            .position(|p| *p == "compressed-blobs" || *p == "blobs")
        {
            Some(i) if parts[i] == "compressed-blobs" && parts.get(i + 1) == Some(&"zstd") => {
                (true, parts.get(i + 2).copied())
            }
            Some(i) if parts[i] == "blobs" => (false, parts.get(i + 1).copied()),
            _ => {
                return Err(ConnectError::new(
                    ErrorCode::InvalidArgument,
                    "bad resource name",
                ));
            }
        };
        let Some(data) = hash.and_then(|h| self.store.blobs.lock().unwrap().get(h).cloned()) else {
            return Err(ConnectError::new(ErrorCode::NotFound, "blob not found"));
        };
        let payload = if compressed {
            zstd::encode_all(data.as_slice(), 3).expect("zstd")
        } else {
            data
        };
        // Split into two chunks to exercise reassembly.
        let mid = payload.len() / 2;
        let items = vec![
            Ok(bs::ReadResponse {
                data: payload[..mid].to_vec(),
                ..Default::default()
            }),
            Ok(bs::ReadResponse {
                data: payload[mid..].to_vec(),
                ..Default::default()
            }),
        ];
        Response::stream_ok(futures::stream::iter(items))
    }

    async fn write(
        &self,
        _ctx: RequestContext,
        _requests: InboundStream<bs::WriteRequest>,
    ) -> ServiceResult<bs::WriteResponse> {
        unimplemented()
    }

    async fn query_write_status(
        &self,
        _ctx: RequestContext,
        _req: ServiceRequest<'_, bs::QueryWriteStatusRequest>,
    ) -> ServiceResult<bs::QueryWriteStatusResponse> {
        unimplemented()
    }
}
