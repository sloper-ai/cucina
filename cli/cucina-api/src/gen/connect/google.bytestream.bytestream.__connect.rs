///Shorthand for `OwnedView<ReadRequestView<'static>>`.
pub type OwnedReadRequestView = ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::ReadRequestView<'static>,
>;
///Shorthand for `OwnedView<ReadResponseView<'static>>`.
pub type OwnedReadResponseView = ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::ReadResponseView<'static>,
>;
///Shorthand for `OwnedView<WriteRequestView<'static>>`.
pub type OwnedWriteRequestView = ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::WriteRequestView<'static>,
>;
///Shorthand for `OwnedView<WriteResponseView<'static>>`.
pub type OwnedWriteResponseView = ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::WriteResponseView<'static>,
>;
///Shorthand for `OwnedView<QueryWriteStatusRequestView<'static>>`.
pub type OwnedQueryWriteStatusRequestView = ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::QueryWriteStatusRequestView<'static>,
>;
///Shorthand for `OwnedView<QueryWriteStatusResponseView<'static>>`.
pub type OwnedQueryWriteStatusResponseView = ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::QueryWriteStatusResponseView<
        'static,
    >,
>;
impl ::connectrpc::Encodable<crate::proto::google::bytestream::ReadResponse>
for crate::proto::google::bytestream::__buffa::view::ReadResponseView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::google::bytestream::ReadResponse>
for ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::ReadResponseView<'static>,
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
impl ::connectrpc::Encodable<crate::proto::google::bytestream::WriteResponse>
for crate::proto::google::bytestream::__buffa::view::WriteResponseView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::google::bytestream::WriteResponse>
for ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::WriteResponseView<'static>,
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
impl ::connectrpc::Encodable<crate::proto::google::bytestream::QueryWriteStatusResponse>
for crate::proto::google::bytestream::__buffa::view::QueryWriteStatusResponseView<'_> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<crate::proto::google::bytestream::QueryWriteStatusResponse>
for ::buffa::view::OwnedView<
    crate::proto::google::bytestream::__buffa::view::QueryWriteStatusResponseView<
        'static,
    >,
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
pub const BYTE_STREAM_SERVICE_NAME: &str = "google.bytestream.ByteStream";
/// Static [`Spec`](::connectrpc::Spec) for the `Read` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const BYTE_STREAM_READ_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/google.bytestream.ByteStream/Read",
        ::connectrpc::StreamType::ServerStream,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `Write` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const BYTE_STREAM_WRITE_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/google.bytestream.ByteStream/Write",
        ::connectrpc::StreamType::ClientStream,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `QueryWriteStatus` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const BYTE_STREAM_QUERY_WRITE_STATUS_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/google.bytestream.ByteStream/QueryWriteStatus",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// #### Introduction
///
/// The Byte Stream API enables a client to read and write a stream of bytes to
/// and from a resource. Resources have names, and these names are supplied in
/// the API calls below to identify the resource that is being read from or
/// written to.
///
/// All implementations of the Byte Stream API export the interface defined here:
///
/// * `Read()`: Reads the contents of a resource.
///
/// * `Write()`: Writes the contents of a resource. The client can call `Write()`
///   multiple times with the same resource and can check the status of the write
///   by calling `QueryWriteStatus()`.
///
/// #### Service parameters and metadata
///
/// The ByteStream API provides no direct way to access/modify any metadata
/// associated with the resource.
///
/// #### Errors
///
/// The errors returned by the service are in the Google canonical error space.
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
pub trait ByteStream: Send + Sync + 'static {
    /// `Read()` is used to retrieve the contents of a resource as a sequence
    /// of bytes. The bytes are returned in a sequence of responses, and the
    /// responses are delivered as the results of a server-side streaming RPC.
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call (until the response stream is returned);
    /// message fields are read directly on it (zero-copy). Data the
    /// returned stream needs must be copied out or converted via
    /// `.to_owned_message()`.
    fn read(
        &self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::google::bytestream::ReadRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            ::connectrpc::ServiceStream<
                impl ::connectrpc::Encodable<
                    crate::proto::google::bytestream::ReadResponse,
                > + Send + use<Self>,
            >,
        >,
    > + Send;
    /// `Write()` is used to send the contents of a resource as a sequence of
    /// bytes. The bytes are sent in a sequence of request protos of a client-side
    /// streaming RPC.
    ///
    /// A `Write()` action is resumable. If there is an error or the connection is
    /// broken during the `Write()`, the client should check the status of the
    /// `Write()` by calling `QueryWriteStatus()` and continue writing from the
    /// returned `committed_size`. This may be less than the amount of data the
    /// client previously sent.
    ///
    /// Calling `Write()` on a resource name that was previously written and
    /// finalized could cause an error, depending on whether the underlying service
    /// allows over-writing of previously written resources.
    ///
    /// When the client closes the request channel, the service will respond with
    /// a `WriteResponse`. The service will not view the resource as `complete`
    /// until the client has sent a `WriteRequest` with `finish_write` set to
    /// `true`. Sending any requests on a stream after sending a request with
    /// `finish_write` set to `true` will cause an error. The client **should**
    /// check the `WriteResponse` it receives to determine how much data the
    /// service was able to commit and whether the service views the resource as
    /// `complete` or not.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// Each `requests` item is a [`StreamMessage`](::connectrpc::StreamMessage):
    /// it owns its buffer, is `Send + 'static`, and exposes zero-copy
    /// accessor methods (`item.name()`), `.view()`, and
    /// `.to_owned_message()`.
    fn write<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        requests: ::connectrpc::InboundStream<
            crate::proto::google::bytestream::WriteRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::google::bytestream::WriteResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// `QueryWriteStatus()` is used to find the `committed_size` for a resource
    /// that is being written, which can then be used as the `write_offset` for
    /// the next `Write()` call.
    ///
    /// If the resource does not exist (i.e., the resource has been deleted, or the
    /// first `Write()` has not yet reached the service), this method returns the
    /// error `NOT_FOUND`.
    ///
    /// The client **may** call `QueryWriteStatus()` at any time to determine how
    /// much data has been processed for this resource. This is useful if the
    /// client is buffering data and needs to know which data can be safely
    /// evicted. For any sequence of `QueryWriteStatus()` calls for a given
    /// resource name, the sequence of returned `committed_size` values will be
    /// non-decreasing.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn query_write_status<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::google::bytestream::QueryWriteStatusRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::google::bytestream::QueryWriteStatusResponse,
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
pub trait ByteStreamExt: ByteStream {
    /// Register this service implementation with a Router.
    ///
    /// Takes ownership of the `Arc<Self>` and returns a new Router with
    /// this service's methods registered.
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router;
}
impl<S: ByteStream> ByteStreamExt for S {
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router {
        router
            .route_view_server_stream::<
                _,
                _,
                crate::proto::google::bytestream::ReadResponse,
            >(
                BYTE_STREAM_SERVICE_NAME,
                "Read",
                ::connectrpc::view_streaming_handler_fn({
                    let svc = ::std::sync::Arc::clone(&self);
                    move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::google::bytestream::__buffa::view::ReadRequestView<
                                'static,
                            >,
                        >|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::google::bytestream::ReadRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.read(ctx, sreq).await
                        }
                    }
                }),
            )
            .with_spec(BYTE_STREAM_READ_SPEC)
            .route_view_client_stream(
                BYTE_STREAM_SERVICE_NAME,
                "Write",
                ::connectrpc::view_client_streaming_handler_fn({
                    let svc = ::std::sync::Arc::clone(&self);
                    move |ctx, req, format| {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let req = ::connectrpc::dispatcher::codegen::into_stream_messages::<
                                crate::proto::google::bytestream::WriteRequest,
                            >(req);
                            svc.write(ctx, req)
                                .await?
                                .encode::<
                                    crate::proto::google::bytestream::WriteResponse,
                                >(format)
                        }
                    }
                }),
            )
            .with_spec(BYTE_STREAM_WRITE_SPEC)
            .route_view(
                BYTE_STREAM_SERVICE_NAME,
                "QueryWriteStatus",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::google::bytestream::__buffa::view::QueryWriteStatusRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::google::bytestream::QueryWriteStatusRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.query_write_status(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::google::bytestream::QueryWriteStatusResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(BYTE_STREAM_QUERY_WRITE_STATUS_SPEC)
    }
}
/// Type-inference marker used by [`Router::add_service`](::connectrpc::Router::add_service).
#[doc(hidden)]
pub struct ByteStreamRegisterMarker;
impl<S: ByteStream> ::connectrpc::ServiceRegister<ByteStreamRegisterMarker>
for ::std::sync::Arc<S> {
    fn register_service(self, router: ::connectrpc::Router) -> ::connectrpc::Router {
        <S as ByteStreamExt>::register(self, router)
    }
}
/// Monomorphic dispatcher for `ByteStream`.
///
/// Unlike `.register(Router)` which type-erases each method into an `Arc<dyn ErasedHandler>` stored in a `HashMap`, this struct dispatches via a compile-time `match` on method name: no vtable, no hash lookup.
///
/// # Example
///
/// ```rust,ignore
/// use connectrpc::ConnectRpcService;
///
/// let server = ByteStreamServer::new(MyImpl);
/// let service = ConnectRpcService::new(server);
/// // hand `service` to axum/hyper as a fallback_service
/// ```
pub struct ByteStreamServer<T> {
    inner: ::std::sync::Arc<T>,
}
impl<T: ByteStream> ByteStreamServer<T> {
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
impl<T> Clone for ByteStreamServer<T> {
    fn clone(&self) -> Self {
        Self {
            inner: ::std::sync::Arc::clone(&self.inner),
        }
    }
}
impl<T: ByteStream> ::connectrpc::Dispatcher for ByteStreamServer<T> {
    #[inline]
    fn lookup(
        &self,
        path: &str,
    ) -> Option<::connectrpc::dispatcher::codegen::MethodDescriptor> {
        let method = path.strip_prefix("google.bytestream.ByteStream/")?;
        match method {
            "Read" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::server_streaming()
                        .with_spec(BYTE_STREAM_READ_SPEC),
                )
            }
            "Write" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::client_streaming()
                        .with_spec(BYTE_STREAM_WRITE_SPEC),
                )
            }
            "QueryWriteStatus" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(BYTE_STREAM_QUERY_WRITE_STATUS_SPEC),
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
        let Some(method) = path.strip_prefix("google.bytestream.ByteStream/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "QueryWriteStatus" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::google::bytestream::QueryWriteStatusRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::google::bytestream::__buffa::view::QueryWriteStatusRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::google::bytestream::QueryWriteStatusRequest,
                    >::from_parts(&req, &body);
                    svc.query_write_status(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::google::bytestream::QueryWriteStatusResponse,
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
        let Some(method) = path.strip_prefix("google.bytestream.ByteStream/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_streaming(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "Read" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::google::bytestream::ReadRequest,
                    >(request, format)?;
                    let req: crate::proto::google::bytestream::__buffa::view::ReadRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::google::bytestream::ReadRequest,
                    >::from_parts(&req, &body);
                    let resp = svc.read(ctx, req).await?;
                    Ok(
                        resp
                            .map_body(|s| ::connectrpc::dispatcher::codegen::encode_response_stream::<
                                crate::proto::google::bytestream::ReadResponse,
                                _,
                                _,
                            >(s, format)),
                    )
                })
            }
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
        let Some(method) = path.strip_prefix("google.bytestream.ByteStream/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &requests, &format);
        match method {
            "Write" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let req_stream = ::connectrpc::dispatcher::codegen::decode_message_request_stream::<
                        crate::proto::google::bytestream::WriteRequest,
                    >(requests, format, ctx.decode_options().clone());
                    svc.write(ctx, req_stream)
                        .await?
                        .encode::<
                            crate::proto::google::bytestream::WriteResponse,
                        >(format)
                })
            }
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
        let Some(method) = path.strip_prefix("google.bytestream.ByteStream/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_streaming(path);
        };
        let _ = (&ctx, &requests, &format);
        match method {
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
/// let client = ByteStreamClient::new(conn, config);
/// let response = client.read(request).await?;
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
/// let client = ByteStreamClient::new(http, config);
/// let response = client.read(request).await?;
/// ```
///
/// # Working with the response
///
/// Unary calls return [`UnaryResponse<OwnedView<FooView>>`](::connectrpc::client::UnaryResponse).
/// [`view()`](::connectrpc::client::UnaryResponse::view) borrows the response
/// message, so field access is zero-copy:
///
/// ```rust,ignore
/// let resp = client.read(request).await?;
/// let name: &str = resp.view().name;  // borrow into the response buffer
/// ```
///
/// If you need the owned struct (e.g. to store or pass by value), use
/// [`into_owned()`](::connectrpc::client::UnaryResponse::into_owned):
///
/// ```rust,ignore
/// let owned = client.read(request).await?.into_owned();
/// ```
///
/// [`into_view()`](::connectrpc::client::UnaryResponse::into_view) keeps the
/// zero-copy decoded body (an `OwnedView`) without copying; field access on it
/// goes through `.reborrow()`. Streaming responses yield one
/// [`StreamMessage`](::connectrpc::StreamMessage) per received message from
/// `.message().await` — read fields zero-copy through the generated accessor
/// methods (`msg.name()`) or `.view()`, or convert with `.to_owned_message()`.
#[derive(Clone)]
pub struct ByteStreamClient<T> {
    transport: T,
    config: ::connectrpc::client::ClientConfig,
}
impl<T> ByteStreamClient<T>
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
    /// Call the Read RPC. Sends a request to /google.bytestream.ByteStream/Read.
    pub async fn read(
        &self,
        request: crate::proto::google::bytestream::ReadRequest,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::google::bytestream::__buffa::view::ReadResponseView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        self.read_with_options(request, ::connectrpc::client::CallOptions::default())
            .await
    }
    /// Call the Read RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn read_with_options(
        &self,
        request: crate::proto::google::bytestream::ReadRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::google::bytestream::__buffa::view::ReadResponseView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_server_stream(
                &self.transport,
                &self.config,
                BYTE_STREAM_READ_SPEC.with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the Write RPC. Sends a request to /google.bytestream.ByteStream/Write.
    ///
    /// `requests` is any `Stream<Item = ...> + Send + 'static` of
    /// request messages (the `ClientRequestStream` bound); messages
    /// are sent as the stream yields them. It backs the request
    /// body, so yield owned messages or feed the call from a
    /// channel-backed stream. For a collection that is already in
    /// hand, wrap it with `::connectrpc::stream_iter(...)`.
    ///
    /// Dropping the returned future cancels the call: the request
    /// body is dropped along with it, so messages the stream had
    /// not yet yielded are never delivered. A caller that needs the
    /// request delivered must drive the call to completion rather
    /// than, say, wrapping it in a `timeout`.
    pub async fn write(
        &self,
        requests: impl ::connectrpc::client::ClientRequestStream<
            crate::proto::google::bytestream::WriteRequest,
        >,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::google::bytestream::__buffa::view::WriteResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.write_with_options(requests, ::connectrpc::client::CallOptions::default())
            .await
    }
    /// Call the Write RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    ///
    /// `requests` is any `Stream<Item = ...> + Send + 'static` of
    /// request messages (the `ClientRequestStream` bound); messages
    /// are sent as the stream yields them. It backs the request
    /// body, so yield owned messages or feed the call from a
    /// channel-backed stream. For a collection that is already in
    /// hand, wrap it with `::connectrpc::stream_iter(...)`.
    ///
    /// Dropping the returned future cancels the call: the request
    /// body is dropped along with it, so messages the stream had
    /// not yet yielded are never delivered. A caller that needs the
    /// request delivered must drive the call to completion rather
    /// than, say, wrapping it in a `timeout`.
    pub async fn write_with_options(
        &self,
        requests: impl ::connectrpc::client::ClientRequestStream<
            crate::proto::google::bytestream::WriteRequest,
        >,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::google::bytestream::__buffa::view::WriteResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_client_stream(
                &self.transport,
                &self.config,
                BYTE_STREAM_WRITE_SPEC.with_origin(::connectrpc::SpecOrigin::Client),
                requests,
                options,
            )
            .await
    }
    /// Call the QueryWriteStatus RPC. Sends a request to /google.bytestream.ByteStream/QueryWriteStatus.
    pub async fn query_write_status(
        &self,
        request: crate::proto::google::bytestream::QueryWriteStatusRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::google::bytestream::__buffa::view::QueryWriteStatusResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.query_write_status_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the QueryWriteStatus RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn query_write_status_with_options(
        &self,
        request: crate::proto::google::bytestream::QueryWriteStatusRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::google::bytestream::__buffa::view::QueryWriteStatusResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                BYTE_STREAM_QUERY_WRITE_STATUS_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
}
