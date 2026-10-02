///Shorthand for `OwnedView<HostMessageView<'static>>`.
pub type OwnedHostMessageView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::HostMessageView<'static>,
>;
///Shorthand for `OwnedView<ControllerMessageView<'static>>`.
pub type OwnedControllerMessageView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::ControllerMessageView<'static>,
>;
///Shorthand for `OwnedView<RenewCertificateRequestView<'static>>`.
pub type OwnedRenewCertificateRequestView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::RenewCertificateRequestView<'static>,
>;
///Shorthand for `OwnedView<RenewCertificateResponseView<'static>>`.
pub type OwnedRenewCertificateResponseView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::RenewCertificateResponseView<'static>,
>;
///Shorthand for `OwnedView<IssueVmIdentityRequestView<'static>>`.
pub type OwnedIssueVmIdentityRequestView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::IssueVMIdentityRequestView<'static>,
>;
///Shorthand for `OwnedView<IssueVmIdentityResponseView<'static>>`.
pub type OwnedIssueVmIdentityResponseView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::IssueVMIdentityResponseView<'static>,
>;
///Shorthand for `OwnedView<GetRegistryCredentialsRequestView<'static>>`.
pub type OwnedGetRegistryCredentialsRequestView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsRequestView<'static>,
>;
///Shorthand for `OwnedView<GetRegistryCredentialsResponseView<'static>>`.
pub type OwnedGetRegistryCredentialsResponseView = ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsResponseView<'static>,
>;
impl ::connectrpc::Encodable<crate::proto::cucina::v1::ControllerMessage>
for crate::proto::cucina::v1::__buffa::view::ControllerMessageView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::ControllerMessage>
for ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::ControllerMessageView<'static>,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self.reborrow(), codec)
    }
    /// An `OwnedView` still holds the buffer it was decoded from, so
    /// its large fields can be handed to the response body by
    /// reference count instead of copied. The bare view impl above
    /// cannot do this: it has borrows but no buffer to name.
    fn encode_segments(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::connectrpc::EncodedBody, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body_segments(
            self.reborrow(),
            self.bytes(),
            codec,
        )
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::RenewCertificateResponse>
for crate::proto::cucina::v1::__buffa::view::RenewCertificateResponseView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::RenewCertificateResponse>
for ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::RenewCertificateResponseView<'static>,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self.reborrow(), codec)
    }
    /// An `OwnedView` still holds the buffer it was decoded from, so
    /// its large fields can be handed to the response body by
    /// reference count instead of copied. The bare view impl above
    /// cannot do this: it has borrows but no buffer to name.
    fn encode_segments(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::connectrpc::EncodedBody, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body_segments(
            self.reborrow(),
            self.bytes(),
            codec,
        )
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::IssueVMIdentityResponse>
for crate::proto::cucina::v1::__buffa::view::IssueVMIdentityResponseView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::IssueVMIdentityResponse>
for ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::IssueVMIdentityResponseView<'static>,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self.reborrow(), codec)
    }
    /// An `OwnedView` still holds the buffer it was decoded from, so
    /// its large fields can be handed to the response body by
    /// reference count instead of copied. The bare view impl above
    /// cannot do this: it has borrows but no buffer to name.
    fn encode_segments(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::connectrpc::EncodedBody, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body_segments(
            self.reborrow(),
            self.bytes(),
            codec,
        )
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::GetRegistryCredentialsResponse>
for crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsResponseView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::cucina::v1::GetRegistryCredentialsResponse>
for ::buffa::view::OwnedView<
    crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsResponseView<'static>,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self.reborrow(), codec)
    }
    /// An `OwnedView` still holds the buffer it was decoded from, so
    /// its large fields can be handed to the response body by
    /// reference count instead of copied. The bare view impl above
    /// cannot do this: it has borrows but no buffer to name.
    fn encode_segments(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::connectrpc::EncodedBody, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body_segments(
            self.reborrow(),
            self.bytes(),
            codec,
        )
    }
}
/// Full service name for this service.
pub const HOST_SERVICE_SERVICE_NAME: &str = "cucina.v1.HostService";
/// Static [`Spec`](::connectrpc::Spec) for the `Connect` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const HOST_SERVICE_CONNECT_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/cucina.v1.HostService/Connect",
        ::connectrpc::StreamType::BidiStream,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `RenewCertificate` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const HOST_SERVICE_RENEW_CERTIFICATE_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/cucina.v1.HostService/RenewCertificate",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `IssueVMIdentity` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const HOST_SERVICE_ISSUE_VM_IDENTITY_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/cucina.v1.HostService/IssueVMIdentity",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `GetRegistryCredentials` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const HOST_SERVICE_GET_REGISTRY_CREDENTIALS_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/cucina.v1.HostService/GetRegistryCredentials",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// HostService is served by the controller over mTLS (client certificate = host
/// identity, URI SAN spiffe://cucina/host/\<serial\>). cucina-hostd always dials OUT
/// (works behind NAT) and reconnects with backoff. Hostd enforces the same safety
/// rules itself (at most 2 running macOS VMs, dead-man switch) even if the controller
/// is gone (R-POOL-7).
///
/// # Implementing handlers
///
/// Implement methods with plain `async fn`; the returned future satisfies
/// the `Send` bound automatically.
///
/// **Unary and server-streaming requests** arrive as
/// [`ServiceRequest<'_, Req>`](::connectrpc::ServiceRequest): a zero-copy
/// view of the request plus its body, valid for the duration of the call.
/// Fields are read directly (`request.name` is a `&str` into the decoded
/// buffer) and the borrow may be held across `.await` points. Anything
/// that must outlive the call — `tokio::spawn`, channels, server state,
/// or data captured by a returned response stream — takes owned data:
/// call `request.to_owned_message()` (or copy the specific fields)
/// first.
///
/// **Client-streaming and bidi requests** arrive as
/// [`InboundStream<Req>`](::connectrpc::InboundStream) — a
/// `ServiceStream` of [`StreamMessage`](::connectrpc::StreamMessage)s.
/// Each item owns its decoded buffer and is `Send + 'static`, so items
/// can be buffered or moved into spawned tasks; read fields zero-copy
/// through the generated accessor methods (`item.name()`) or `.view()`,
/// convert with `.to_owned_message()`, or yield an item back unchanged —
/// `StreamMessage<M>` implements `Encodable<M>`.
///
/// Request types resolved through `extern_path` (e.g. well-known types
/// from another crate) use the same wrappers; the crate that owns the
/// type must be generated with buffa ≥ 0.9.0 and views enabled so the
/// backing `HasMessageView` impl exists.
///
/// The `impl Encodable<Out>` return bound accepts the owned `Out`, the
/// generated `OutView<'_>` / `OwnedOutView`,
/// [`MaybeBorrowed`](::connectrpc::MaybeBorrowed), or
/// [`PreEncoded`](::connectrpc::PreEncoded) for handlers that encode a
/// non-`'static` view internally and pass the bytes across the handler
/// boundary. View bodies are not emitted for output types mapped via
/// `extern_path` (the impl would be an orphan); return owned for
/// WKT/extern outputs.
///
/// Server-streaming and bidi-streaming methods return
/// `ServiceStream<impl Encodable<Out> + Send + use<Self>>`. The
/// `use<Self>` precise-capturing clause excludes `&self`'s lifetime and
/// the request's lifetime (unary methods use `use<'a, Self>` and may
/// borrow from `&self`), so stream items must be `'static` and cannot
/// borrow from the request. To stream view-encoded data, encode each
/// item inside the stream body and yield
/// [`PreEncoded`](::connectrpc::PreEncoded) — see its `# Streaming
/// example` doc.
#[allow(clippy::type_complexity)]
pub trait HostService: Send + Sync + 'static {
    /// Connect is the long-lived bidirectional stream: hostd reports facts, VM state,
    /// heartbeats and metrics; the controller sends commands (R-MAC-6).
    ///
    /// Each `requests` item is a [`StreamMessage`](::connectrpc::StreamMessage):
    /// it owns its buffer, is `Send + 'static`, and exposes zero-copy
    /// accessor methods (`item.name()`), `.view()`, and
    /// `.to_owned_message()`.
    fn connect(
        &self,
        ctx: ::connectrpc::RequestContext,
        requests: ::connectrpc::InboundStream<crate::proto::cucina::v1::HostMessage>,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            ::connectrpc::ServiceStream<
                impl ::connectrpc::Encodable<
                    crate::proto::cucina::v1::ControllerMessage,
                > + Send + use<Self>,
            >,
        >,
    > + Send;
    /// RenewCertificate issues a fresh host certificate before expiry (key unchanged or rotated by CSR).
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn renew_certificate<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::cucina::v1::RenewCertificateRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::cucina::v1::RenewCertificateResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// IssueVMIdentity issues a short-lived worker identity and settings for one VM. VMs only
    /// ever receive credentials through their host (R-MAC-4).
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn issue_vm_identity<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::cucina::v1::IssueVMIdentityRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::cucina::v1::IssueVMIdentityResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// GetRegistryCredentials returns short-lived (or read-only package) registry credentials
    /// for pre-pulling Tart images; hostd passes them via TART_REGISTRY_* env vars, never the keychain.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn get_registry_credentials<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::cucina::v1::GetRegistryCredentialsRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::cucina::v1::GetRegistryCredentialsResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
}
/// Extension trait for registering a service implementation with a Router.
///
/// This trait is automatically implemented for all types that implement the service trait.
/// Prefer [`Router::add_service`](::connectrpc::Router::add_service) for
/// top-down registration; `register` remains available for compatibility
/// and cases where the service-first call shape is more convenient.
///
/// # Example
///
/// ```rust,ignore
/// use std::sync::Arc;
///
/// let service = Arc::new(MyServiceImpl);
/// let router = service.register(Router::new());
/// ```
pub trait HostServiceExt: HostService {
    /// Register this service implementation with a Router.
    ///
    /// Takes ownership of the `Arc<Self>` and returns a new Router with
    /// this service's methods registered.
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router;
}
impl<S: HostService> HostServiceExt for S {
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router {
        router
            .route_view_bidi_stream::<
                _,
                _,
                crate::proto::cucina::v1::ControllerMessage,
            >(
                HOST_SERVICE_SERVICE_NAME,
                "Connect",
                ::connectrpc::view_bidi_streaming_handler_fn({
                    let svc = ::std::sync::Arc::clone(&self);
                    move |ctx, req| {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let req = ::connectrpc::dispatcher::codegen::into_stream_messages::<
                                crate::proto::cucina::v1::HostMessage,
                            >(req);
                            svc.connect(ctx, req).await
                        }
                    }
                }),
            )
            .with_spec(HOST_SERVICE_CONNECT_SPEC)
            .route_view(
                HOST_SERVICE_SERVICE_NAME,
                "RenewCertificate",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::cucina::v1::__buffa::view::RenewCertificateRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::cucina::v1::RenewCertificateRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.renew_certificate(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::cucina::v1::RenewCertificateResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(HOST_SERVICE_RENEW_CERTIFICATE_SPEC)
            .route_view(
                HOST_SERVICE_SERVICE_NAME,
                "IssueVMIdentity",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::cucina::v1::__buffa::view::IssueVMIdentityRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::cucina::v1::IssueVMIdentityRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.issue_vm_identity(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::cucina::v1::IssueVMIdentityResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(HOST_SERVICE_ISSUE_VM_IDENTITY_SPEC)
            .route_view(
                HOST_SERVICE_SERVICE_NAME,
                "GetRegistryCredentials",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::cucina::v1::GetRegistryCredentialsRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.get_registry_credentials(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::cucina::v1::GetRegistryCredentialsResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(HOST_SERVICE_GET_REGISTRY_CREDENTIALS_SPEC)
    }
}
/// Type-inference marker used by [`Router::add_service`](::connectrpc::Router::add_service).
#[doc(hidden)]
pub struct HostServiceRegisterMarker;
impl<S: HostService> ::connectrpc::ServiceRegister<HostServiceRegisterMarker>
for ::std::sync::Arc<S> {
    fn register_service(self, router: ::connectrpc::Router) -> ::connectrpc::Router {
        <S as HostServiceExt>::register(self, router)
    }
}
/// Monomorphic dispatcher for `HostService`.
///
/// Unlike `.register(Router)` which type-erases each method into an `Arc<dyn ErasedHandler>` stored in a `HashMap`, this struct dispatches via a compile-time `match` on method name: no vtable, no hash lookup.
///
/// # Example
///
/// ```rust,ignore
/// use connectrpc::ConnectRpcService;
///
/// let server = HostServiceServer::new(MyImpl);
/// let service = ConnectRpcService::new(server);
/// // hand `service` to axum/hyper as a fallback_service
/// ```
pub struct HostServiceServer<T> {
    inner: ::std::sync::Arc<T>,
}
impl<T: HostService> HostServiceServer<T> {
    /// Wrap a service implementation in a monomorphic dispatcher.
    pub fn new(service: T) -> Self {
        Self {
            inner: ::std::sync::Arc::new(service),
        }
    }
    /// Wrap an already-`Arc`'d service implementation.
    pub fn from_arc(inner: ::std::sync::Arc<T>) -> Self {
        Self { inner }
    }
}
impl<T> Clone for HostServiceServer<T> {
    fn clone(&self) -> Self {
        Self {
            inner: ::std::sync::Arc::clone(&self.inner),
        }
    }
}
impl<T: HostService> ::connectrpc::Dispatcher for HostServiceServer<T> {
    #[inline]
    fn lookup(
        &self,
        path: &str,
    ) -> Option<::connectrpc::dispatcher::codegen::MethodDescriptor> {
        let method = path.strip_prefix("cucina.v1.HostService/")?;
        match method {
            "Connect" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::bidi_streaming()
                        .with_spec(HOST_SERVICE_CONNECT_SPEC),
                )
            }
            "RenewCertificate" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(HOST_SERVICE_RENEW_CERTIFICATE_SPEC),
                )
            }
            "IssueVMIdentity" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(HOST_SERVICE_ISSUE_VM_IDENTITY_SPEC),
                )
            }
            "GetRegistryCredentials" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(HOST_SERVICE_GET_REGISTRY_CREDENTIALS_SPEC),
                )
            }
            _ => None,
        }
    }
    fn call_unary(
        &self,
        path: &str,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::Payload,
        format: ::connectrpc::CodecFormat,
    ) -> ::connectrpc::dispatcher::codegen::UnaryResult {
        let Some(method) = path.strip_prefix("cucina.v1.HostService/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "RenewCertificate" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::cucina::v1::RenewCertificateRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::cucina::v1::__buffa::view::RenewCertificateRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::cucina::v1::RenewCertificateRequest,
                    >::from_parts(&req, &body);
                    svc.renew_certificate(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::cucina::v1::RenewCertificateResponse,
                        >(format)
                })
            }
            "IssueVMIdentity" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::cucina::v1::IssueVMIdentityRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::cucina::v1::__buffa::view::IssueVMIdentityRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::cucina::v1::IssueVMIdentityRequest,
                    >::from_parts(&req, &body);
                    svc.issue_vm_identity(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::cucina::v1::IssueVMIdentityResponse,
                        >(format)
                })
            }
            "GetRegistryCredentials" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::cucina::v1::GetRegistryCredentialsRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::cucina::v1::GetRegistryCredentialsRequest,
                    >::from_parts(&req, &body);
                    svc.get_registry_credentials(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::cucina::v1::GetRegistryCredentialsResponse,
                        >(format)
                })
            }
            _ => ::connectrpc::dispatcher::codegen::unimplemented_unary(path),
        }
    }
    fn call_server_streaming(
        &self,
        path: &str,
        ctx: ::connectrpc::RequestContext,
        request: ::buffa::bytes::Bytes,
        format: ::connectrpc::CodecFormat,
    ) -> ::connectrpc::dispatcher::codegen::StreamingResult {
        let Some(method) = path.strip_prefix("cucina.v1.HostService/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_streaming(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            _ => ::connectrpc::dispatcher::codegen::unimplemented_streaming(path),
        }
    }
    fn call_client_streaming(
        &self,
        path: &str,
        ctx: ::connectrpc::RequestContext,
        requests: ::connectrpc::dispatcher::codegen::RequestStream,
        format: ::connectrpc::CodecFormat,
    ) -> ::connectrpc::dispatcher::codegen::UnaryResult {
        let Some(method) = path.strip_prefix("cucina.v1.HostService/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &requests, &format);
        match method {
            _ => ::connectrpc::dispatcher::codegen::unimplemented_unary(path),
        }
    }
    fn call_bidi_streaming(
        &self,
        path: &str,
        ctx: ::connectrpc::RequestContext,
        requests: ::connectrpc::dispatcher::codegen::RequestStream,
        format: ::connectrpc::CodecFormat,
    ) -> ::connectrpc::dispatcher::codegen::StreamingResult {
        let Some(method) = path.strip_prefix("cucina.v1.HostService/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_streaming(path);
        };
        let _ = (&ctx, &requests, &format);
        match method {
            "Connect" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let req_stream = ::connectrpc::dispatcher::codegen::decode_message_request_stream::<
                        crate::proto::cucina::v1::HostMessage,
                    >(requests, format, ctx.decode_options().clone());
                    let resp = svc.connect(ctx, req_stream).await?;
                    Ok(
                        resp
                            .map_body(|s| ::connectrpc::dispatcher::codegen::encode_response_stream::<
                                crate::proto::cucina::v1::ControllerMessage,
                                _,
                                _,
                            >(s, format)),
                    )
                })
            }
            _ => ::connectrpc::dispatcher::codegen::unimplemented_streaming(path),
        }
    }
}
/// Client for this service.
///
/// Generic over `T: ClientTransport`. For **gRPC** (HTTP/2), use
/// `Http2Connection` — it has honest `poll_ready` and composes with
/// `tower::balance` for multi-connection load balancing. For **Connect
/// over HTTP/1.1** (or unknown protocol), use `HttpClient`.
///
/// # Example (gRPC / HTTP/2)
///
/// ```rust,ignore
/// use connectrpc::client::{Http2Connection, ClientConfig};
/// use connectrpc::Protocol;
///
/// let uri: http::Uri = "http://localhost:8080".parse()?;
/// let conn = Http2Connection::connect_plaintext(uri.clone()).await?.shared(1024);
/// let config = ClientConfig::new(uri).with_protocol(Protocol::Grpc);
///
/// let client = HostServiceClient::new(conn, config);
/// let response = client.connect(request).await?;
/// ```
///
/// # Example (Connect / HTTP/1.1 or ALPN)
///
/// ```rust,ignore
/// use connectrpc::client::{HttpClient, ClientConfig};
///
/// let http = HttpClient::plaintext();  // cleartext http:// only
/// let config = ClientConfig::new("http://localhost:8080".parse()?);
///
/// let client = HostServiceClient::new(http, config);
/// let response = client.connect(request).await?;
/// ```
///
/// # Working with the response
///
/// Unary calls return [`UnaryResponse<OwnedView<FooView>>`](::connectrpc::client::UnaryResponse).
/// [`view()`](::connectrpc::client::UnaryResponse::view) borrows the response
/// message, so field access is zero-copy:
///
/// ```rust,ignore
/// let resp = client.connect(request).await?;
/// let name: &str = resp.view().name;  // borrow into the response buffer
/// ```
///
/// If you need the owned struct (e.g. to store or pass by value), use
/// [`into_owned()`](::connectrpc::client::UnaryResponse::into_owned):
///
/// ```rust,ignore
/// let owned = client.connect(request).await?.into_owned();
/// ```
///
/// [`into_view()`](::connectrpc::client::UnaryResponse::into_view) keeps the
/// zero-copy decoded body (an `OwnedView`) without copying; field access on it
/// goes through `.reborrow()`. Streaming responses yield one
/// [`StreamMessage`](::connectrpc::StreamMessage) per received message from
/// `.message().await` — read fields zero-copy through the generated accessor
/// methods (`msg.name()`) or `.view()`, or convert with `.to_owned_message()`.
#[derive(Clone)]
pub struct HostServiceClient<T> {
    transport: T,
    config: ::connectrpc::client::ClientConfig,
}
impl<T> HostServiceClient<T>
where
    T: ::connectrpc::client::ClientTransport,
    <T::ResponseBody as ::connectrpc::http_body::Body>::Error: ::std::fmt::Display,
{
    /// Create a new client with the given transport and configuration.
    pub fn new(transport: T, config: ::connectrpc::client::ClientConfig) -> Self {
        Self { transport, config }
    }
    /// Get the client configuration.
    pub fn config(&self) -> &::connectrpc::client::ClientConfig {
        &self.config
    }
    /// Get a mutable reference to the client configuration.
    pub fn config_mut(&mut self) -> &mut ::connectrpc::client::ClientConfig {
        &mut self.config
    }
    /// Call the Connect RPC. Sends a request to /cucina.v1.HostService/Connect.
    pub async fn connect(
        &self,
    ) -> Result<
        ::connectrpc::client::BidiStream<
            T::ResponseBody,
            crate::proto::cucina::v1::HostMessage,
            crate::proto::cucina::v1::__buffa::view::ControllerMessageView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        self.connect_with_options(::connectrpc::client::CallOptions::default()).await
    }
    /// Call the Connect RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn connect_with_options(
        &self,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::BidiStream<
            T::ResponseBody,
            crate::proto::cucina::v1::HostMessage,
            crate::proto::cucina::v1::__buffa::view::ControllerMessageView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_bidi_stream(
                &self.transport,
                &self.config,
                HOST_SERVICE_CONNECT_SPEC.with_origin(::connectrpc::SpecOrigin::Client),
                options,
            )
            .await
    }
    /// Call the RenewCertificate RPC. Sends a request to /cucina.v1.HostService/RenewCertificate.
    pub async fn renew_certificate(
        &self,
        request: crate::proto::cucina::v1::RenewCertificateRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::cucina::v1::__buffa::view::RenewCertificateResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.renew_certificate_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the RenewCertificate RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn renew_certificate_with_options(
        &self,
        request: crate::proto::cucina::v1::RenewCertificateRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::cucina::v1::__buffa::view::RenewCertificateResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                HOST_SERVICE_RENEW_CERTIFICATE_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the IssueVMIdentity RPC. Sends a request to /cucina.v1.HostService/IssueVMIdentity.
    pub async fn issue_vm_identity(
        &self,
        request: crate::proto::cucina::v1::IssueVMIdentityRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::cucina::v1::__buffa::view::IssueVMIdentityResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.issue_vm_identity_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the IssueVMIdentity RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn issue_vm_identity_with_options(
        &self,
        request: crate::proto::cucina::v1::IssueVMIdentityRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::cucina::v1::__buffa::view::IssueVMIdentityResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                HOST_SERVICE_ISSUE_VM_IDENTITY_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the GetRegistryCredentials RPC. Sends a request to /cucina.v1.HostService/GetRegistryCredentials.
    pub async fn get_registry_credentials(
        &self,
        request: crate::proto::cucina::v1::GetRegistryCredentialsRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.get_registry_credentials_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the GetRegistryCredentials RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn get_registry_credentials_with_options(
        &self,
        request: crate::proto::cucina::v1::GetRegistryCredentialsRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::cucina::v1::__buffa::view::GetRegistryCredentialsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                HOST_SERVICE_GET_REGISTRY_CREDENTIALS_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
}
