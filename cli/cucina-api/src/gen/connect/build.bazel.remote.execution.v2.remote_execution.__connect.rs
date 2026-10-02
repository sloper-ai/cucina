///Shorthand for `OwnedView<ExecuteRequestView<'static>>`.
pub type OwnedExecuteRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::ExecuteRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<OperationView<'static>>`.
pub type OwnedOperationView = ::buffa::view::OwnedView<
    crate::proto::google::longrunning::__buffa::view::OperationView<'static>,
>;
///Shorthand for `OwnedView<WaitExecutionRequestView<'static>>`.
pub type OwnedWaitExecutionRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::WaitExecutionRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<GetActionResultRequestView<'static>>`.
pub type OwnedGetActionResultRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetActionResultRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<ActionResultView<'static>>`.
pub type OwnedActionResultView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
        'static,
    >,
>;
///Shorthand for `OwnedView<UpdateActionResultRequestView<'static>>`.
pub type OwnedUpdateActionResultRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::UpdateActionResultRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<FindMissingBlobsRequestView<'static>>`.
pub type OwnedFindMissingBlobsRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<FindMissingBlobsResponseView<'static>>`.
pub type OwnedFindMissingBlobsResponseView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsResponseView<
        'static,
    >,
>;
///Shorthand for `OwnedView<BatchUpdateBlobsRequestView<'static>>`.
pub type OwnedBatchUpdateBlobsRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<BatchUpdateBlobsResponseView<'static>>`.
pub type OwnedBatchUpdateBlobsResponseView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsResponseView<
        'static,
    >,
>;
///Shorthand for `OwnedView<BatchReadBlobsRequestView<'static>>`.
pub type OwnedBatchReadBlobsRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<BatchReadBlobsResponseView<'static>>`.
pub type OwnedBatchReadBlobsResponseView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsResponseView<
        'static,
    >,
>;
///Shorthand for `OwnedView<GetTreeRequestView<'static>>`.
pub type OwnedGetTreeRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<GetTreeResponseView<'static>>`.
pub type OwnedGetTreeResponseView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeResponseView<
        'static,
    >,
>;
///Shorthand for `OwnedView<SplitBlobRequestView<'static>>`.
pub type OwnedSplitBlobRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<SplitBlobResponseView<'static>>`.
pub type OwnedSplitBlobResponseView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobResponseView<
        'static,
    >,
>;
///Shorthand for `OwnedView<SpliceBlobRequestView<'static>>`.
pub type OwnedSpliceBlobRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<SpliceBlobResponseView<'static>>`.
pub type OwnedSpliceBlobResponseView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobResponseView<
        'static,
    >,
>;
///Shorthand for `OwnedView<GetCapabilitiesRequestView<'static>>`.
pub type OwnedGetCapabilitiesRequestView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetCapabilitiesRequestView<
        'static,
    >,
>;
///Shorthand for `OwnedView<ServerCapabilitiesView<'static>>`.
pub type OwnedServerCapabilitiesView = ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::ServerCapabilitiesView<
        'static,
    >,
>;
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::ActionResult,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::ActionResult,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsResponse,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsResponseView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsResponse,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsResponseView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsResponse,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsResponseView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsResponse,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsResponseView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsResponse,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsResponseView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsResponse,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsResponseView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::GetTreeResponse,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeResponseView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::GetTreeResponse,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeResponseView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::SplitBlobResponse,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobResponseView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::SplitBlobResponse,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobResponseView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::SpliceBlobResponse,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobResponseView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::SpliceBlobResponse,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobResponseView<
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
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::ServerCapabilities,
>
for crate::proto::build::bazel::remote::execution::v2::__buffa::view::ServerCapabilitiesView<
    '_,
> {
    fn encode(
        &self,
        codec: ::connectrpc::CodecFormat,
    ) -> ::std::result::Result<::buffa::bytes::Bytes, ::connectrpc::ConnectError> {
        ::connectrpc::__codegen::encode_view_body(self, codec)
    }
}
impl ::connectrpc::Encodable<
    crate::proto::build::bazel::remote::execution::v2::ServerCapabilities,
>
for ::buffa::view::OwnedView<
    crate::proto::build::bazel::remote::execution::v2::__buffa::view::ServerCapabilitiesView<
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
pub const EXECUTION_SERVICE_NAME: &str = "build.bazel.remote.execution.v2.Execution";
/// Static [`Spec`](::connectrpc::Spec) for the `Execute` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const EXECUTION_EXECUTE_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.Execution/Execute",
        ::connectrpc::StreamType::ServerStream,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `WaitExecution` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const EXECUTION_WAIT_EXECUTION_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.Execution/WaitExecution",
        ::connectrpc::StreamType::ServerStream,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// The Remote Execution API is used to execute an
/// \[Action\]\[build.bazel.remote.execution.v2.Action\] on the remote
/// workers.
///
/// As with other services in the Remote Execution API, any call may return an
/// error with a \[RetryInfo\]\[google.rpc.RetryInfo\] error detail providing
/// information about when the client should retry the request; clients SHOULD
/// respect the information provided.
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
pub trait Execution: Send + Sync + 'static {
    /// Execute an action remotely.
    ///
    /// In order to execute an action, the client must first upload all of the
    /// inputs, the
    /// \[Command\]\[build.bazel.remote.execution.v2.Command\] to run, and the
    /// \[Action\]\[build.bazel.remote.execution.v2.Action\] into the
    /// \[ContentAddressableStorage\]\[build.bazel.remote.execution.v2.ContentAddressableStorage\].
    /// It then calls `Execute` with an `action_digest` referring to them. The
    /// server will run the action and eventually return the result.
    ///
    /// The input `Action`'s fields MUST meet the various canonicalization
    /// requirements specified in the documentation for their types so that it has
    /// the same digest as other logically equivalent `Action`s. The server MAY
    /// enforce the requirements and return errors if a non-canonical input is
    /// received. It MAY also proceed without verifying some or all of the
    /// requirements, such as for performance reasons. If the server does not
    /// verify the requirement, then it will treat the `Action` as distinct from
    /// another logically equivalent action if they hash differently.
    ///
    /// Returns a stream of
    /// \[google.longrunning.Operation\]\[google.longrunning.Operation\] messages
    /// describing the resulting execution, with eventual `response`
    /// \[ExecuteResponse\]\[build.bazel.remote.execution.v2.ExecuteResponse\]. The
    /// `metadata` on the operation is of type
    /// \[ExecuteOperationMetadata\]\[build.bazel.remote.execution.v2.ExecuteOperationMetadata\].
    ///
    /// If the client remains connected after the first response is returned after
    /// the server, then updates are streamed as if the client had called
    /// \[WaitExecution\]\[build.bazel.remote.execution.v2.Execution.WaitExecution\]
    /// until the execution completes or the request reaches an error. The
    /// operation can also be queried using \[Operations
    /// API\]\[google.longrunning.Operations.GetOperation\].
    ///
    /// The server NEED NOT implement other methods or functionality of the
    /// Operations API.
    ///
    /// Errors discovered during creation of the `Operation` will be reported
    /// as gRPC Status errors, while errors that occurred while running the
    /// action will be reported in the `status` field of the `ExecuteResponse`. The
    /// server MUST NOT set the `error` field of the `Operation` proto.
    /// The possible errors include:
    ///
    /// * `INVALID_ARGUMENT`: One or more arguments are invalid.
    /// * `FAILED_PRECONDITION`: One or more errors occurred in setting up the
    ///   action requested, such as a missing input or command or no worker being
    ///   available. The client may be able to fix the errors and retry.
    /// * `RESOURCE_EXHAUSTED`: There is insufficient quota of some resource to run
    ///   the action.
    /// * `UNAVAILABLE`: Due to a transient condition, such as all workers being
    ///   occupied (and the server does not support a queue), the action could not
    ///   be started. The client should retry.
    /// * `INTERNAL`: An internal error occurred in the execution engine or the
    ///   worker.
    /// * `DEADLINE_EXCEEDED`: The execution timed out.
    /// * `CANCELLED`: The operation was cancelled by the client. This status is
    ///   only possible if the server implements the Operations API CancelOperation
    ///   method, and it was called for the current execution.
    ///
    /// In the case of a missing input or command, the server SHOULD additionally
    /// send a \[PreconditionFailure\]\[google.rpc.PreconditionFailure\] error detail
    /// where, for each requested blob not present in the CAS, there is a
    /// `Violation` with a `type` of `MISSING` and a `subject` of
    /// `"blobs/{digest_function/}{hash}/{size}"` indicating the digest of the
    /// missing blob. The `subject` is formatted the same way as the
    /// `resource_name` provided to
    /// \[ByteStream.Read\]\[google.bytestream.ByteStream.Read\], with the leading
    /// instance name omitted. `digest_function` MUST thus be omitted if its value
    /// is one of MD5, MURMUR3, SHA1, SHA256, SHA384, SHA512, or VSO.
    ///
    /// The server does not need to guarantee that a call to this method leads to
    /// at most one execution of the action. The server MAY execute the action
    /// multiple times, potentially in parallel. These redundant executions MAY
    /// continue to run, even if the operation is completed.
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call (until the response stream is returned);
    /// message fields are read directly on it (zero-copy). Data the
    /// returned stream needs must be copied out or converted via
    /// `.to_owned_message()`.
    fn execute(
        &self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::ExecuteRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            ::connectrpc::ServiceStream<
                impl ::connectrpc::Encodable<
                    crate::proto::google::longrunning::Operation,
                > + Send + use<Self>,
            >,
        >,
    > + Send;
    /// Wait for an execution operation to complete. When the client initially
    /// makes the request, the server immediately responds with the current status
    /// of the execution. The server will leave the request stream open until the
    /// operation completes, and then respond with the completed operation. The
    /// server MAY choose to stream additional updates as execution progresses,
    /// such as to provide an update as to the state of the execution.
    ///
    /// In addition to the cases described for Execute, the WaitExecution method
    /// may fail as follows:
    ///
    /// * `NOT_FOUND`: The operation no longer exists due to any of a transient
    ///   condition, an unknown operation name, or if the server implements the
    ///   Operations API DeleteOperation method and it was called for the current
    ///   execution. The client should call `Execute` to retry.
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call (until the response stream is returned);
    /// message fields are read directly on it (zero-copy). Data the
    /// returned stream needs must be copied out or converted via
    /// `.to_owned_message()`.
    fn wait_execution(
        &self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::WaitExecutionRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            ::connectrpc::ServiceStream<
                impl ::connectrpc::Encodable<
                    crate::proto::google::longrunning::Operation,
                > + Send + use<Self>,
            >,
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
pub trait ExecutionExt: Execution {
    /// Register this service implementation with a Router.
    ///
    /// Takes ownership of the `Arc<Self>` and returns a new Router with
    /// this service's methods registered.
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router;
}
impl<S: Execution> ExecutionExt for S {
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router {
        router
            .route_view_server_stream::<
                _,
                _,
                crate::proto::google::longrunning::Operation,
            >(
                EXECUTION_SERVICE_NAME,
                "Execute",
                ::connectrpc::view_streaming_handler_fn({
                    let svc = ::std::sync::Arc::clone(&self);
                    move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::ExecuteRequestView<
                                'static,
                            >,
                        >|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::ExecuteRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.execute(ctx, sreq).await
                        }
                    }
                }),
            )
            .with_spec(EXECUTION_EXECUTE_SPEC)
            .route_view_server_stream::<
                _,
                _,
                crate::proto::google::longrunning::Operation,
            >(
                EXECUTION_SERVICE_NAME,
                "WaitExecution",
                ::connectrpc::view_streaming_handler_fn({
                    let svc = ::std::sync::Arc::clone(&self);
                    move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::WaitExecutionRequestView<
                                'static,
                            >,
                        >|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::WaitExecutionRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.wait_execution(ctx, sreq).await
                        }
                    }
                }),
            )
            .with_spec(EXECUTION_WAIT_EXECUTION_SPEC)
    }
}
/// Type-inference marker used by [`Router::add_service`](::connectrpc::Router::add_service).
#[doc(hidden)]
pub struct ExecutionRegisterMarker;
impl<S: Execution> ::connectrpc::ServiceRegister<ExecutionRegisterMarker>
for ::std::sync::Arc<S> {
    fn register_service(self, router: ::connectrpc::Router) -> ::connectrpc::Router {
        <S as ExecutionExt>::register(self, router)
    }
}
/// Monomorphic dispatcher for `Execution`.
///
/// Unlike `.register(Router)` which type-erases each method into an `Arc<dyn ErasedHandler>` stored in a `HashMap`, this struct dispatches via a compile-time `match` on method name: no vtable, no hash lookup.
///
/// # Example
///
/// ```rust,ignore
/// use connectrpc::ConnectRpcService;
///
/// let server = ExecutionServer::new(MyImpl);
/// let service = ConnectRpcService::new(server);
/// // hand `service` to axum/hyper as a fallback_service
/// ```
pub struct ExecutionServer<T> {
    inner: ::std::sync::Arc<T>,
}
impl<T: Execution> ExecutionServer<T> {
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
impl<T> Clone for ExecutionServer<T> {
    fn clone(&self) -> Self {
        Self {
            inner: ::std::sync::Arc::clone(&self.inner),
        }
    }
}
impl<T: Execution> ::connectrpc::Dispatcher for ExecutionServer<T> {
    #[inline]
    fn lookup(
        &self,
        path: &str,
    ) -> Option<::connectrpc::dispatcher::codegen::MethodDescriptor> {
        let method = path.strip_prefix("build.bazel.remote.execution.v2.Execution/")?;
        match method {
            "Execute" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::server_streaming()
                        .with_spec(EXECUTION_EXECUTE_SPEC),
                )
            }
            "WaitExecution" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::server_streaming()
                        .with_spec(EXECUTION_WAIT_EXECUTION_SPEC),
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Execution/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Execution/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_streaming(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "Execute" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::ExecuteRequest,
                    >(request, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::ExecuteRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::ExecuteRequest,
                    >::from_parts(&req, &body);
                    let resp = svc.execute(ctx, req).await?;
                    Ok(
                        resp
                            .map_body(|s| ::connectrpc::dispatcher::codegen::encode_response_stream::<
                                crate::proto::google::longrunning::Operation,
                                _,
                                _,
                            >(s, format)),
                    )
                })
            }
            "WaitExecution" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::WaitExecutionRequest,
                    >(request, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::WaitExecutionRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::WaitExecutionRequest,
                    >::from_parts(&req, &body);
                    let resp = svc.wait_execution(ctx, req).await?;
                    Ok(
                        resp
                            .map_body(|s| ::connectrpc::dispatcher::codegen::encode_response_stream::<
                                crate::proto::google::longrunning::Operation,
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Execution/") else {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Execution/") else {
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
/// let client = ExecutionClient::new(conn, config);
/// let response = client.execute(request).await?;
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
/// let client = ExecutionClient::new(http, config);
/// let response = client.execute(request).await?;
/// ```
///
/// # Working with the response
///
/// Unary calls return [`UnaryResponse<OwnedView<FooView>>`](::connectrpc::client::UnaryResponse).
/// [`view()`](::connectrpc::client::UnaryResponse::view) borrows the response
/// message, so field access is zero-copy:
///
/// ```rust,ignore
/// let resp = client.execute(request).await?;
/// let name: &str = resp.view().name;  // borrow into the response buffer
/// ```
///
/// If you need the owned struct (e.g. to store or pass by value), use
/// [`into_owned()`](::connectrpc::client::UnaryResponse::into_owned):
///
/// ```rust,ignore
/// let owned = client.execute(request).await?.into_owned();
/// ```
///
/// [`into_view()`](::connectrpc::client::UnaryResponse::into_view) keeps the
/// zero-copy decoded body (an `OwnedView`) without copying; field access on it
/// goes through `.reborrow()`. Streaming responses yield one
/// [`StreamMessage`](::connectrpc::StreamMessage) per received message from
/// `.message().await` — read fields zero-copy through the generated accessor
/// methods (`msg.name()`) or `.view()`, or convert with `.to_owned_message()`.
#[derive(Clone)]
pub struct ExecutionClient<T> {
    transport: T,
    config: ::connectrpc::client::ClientConfig,
}
impl<T> ExecutionClient<T>
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
    /// Call the Execute RPC. Sends a request to /build.bazel.remote.execution.v2.Execution/Execute.
    pub async fn execute(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::ExecuteRequest,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::google::longrunning::__buffa::view::OperationView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        self.execute_with_options(request, ::connectrpc::client::CallOptions::default())
            .await
    }
    /// Call the Execute RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn execute_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::ExecuteRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::google::longrunning::__buffa::view::OperationView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_server_stream(
                &self.transport,
                &self.config,
                EXECUTION_EXECUTE_SPEC.with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the WaitExecution RPC. Sends a request to /build.bazel.remote.execution.v2.Execution/WaitExecution.
    pub async fn wait_execution(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::WaitExecutionRequest,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::google::longrunning::__buffa::view::OperationView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        self.wait_execution_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the WaitExecution RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn wait_execution_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::WaitExecutionRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::google::longrunning::__buffa::view::OperationView<'static>,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_server_stream(
                &self.transport,
                &self.config,
                EXECUTION_WAIT_EXECUTION_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
}
/// Full service name for this service.
pub const ACTION_CACHE_SERVICE_NAME: &str = "build.bazel.remote.execution.v2.ActionCache";
/// Static [`Spec`](::connectrpc::Spec) for the `GetActionResult` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const ACTION_CACHE_GET_ACTION_RESULT_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ActionCache/GetActionResult",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `UpdateActionResult` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const ACTION_CACHE_UPDATE_ACTION_RESULT_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ActionCache/UpdateActionResult",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// The action cache API is used to query whether a given action has already been
/// performed and, if so, retrieve its result. Unlike the
/// \[ContentAddressableStorage\]\[build.bazel.remote.execution.v2.ContentAddressableStorage\],
/// which addresses blobs by their own content, the action cache addresses the
/// \[ActionResult\]\[build.bazel.remote.execution.v2.ActionResult\] by a
/// digest of the encoded \[Action\]\[build.bazel.remote.execution.v2.Action\]
/// which produced them.
///
/// The lifetime of entries in the action cache is implementation-specific, but
/// the server SHOULD assume that more recently used entries are more likely to
/// be used again.
///
/// As with other services in the Remote Execution API, any call may return an
/// error with a \[RetryInfo\]\[google.rpc.RetryInfo\] error detail providing
/// information about when the client should retry the request; clients SHOULD
/// respect the information provided.
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
pub trait ActionCache: Send + Sync + 'static {
    /// Retrieve a cached execution result.
    ///
    /// Implementations SHOULD ensure that any blobs referenced from the
    /// \[ContentAddressableStorage\]\[build.bazel.remote.execution.v2.ContentAddressableStorage\]
    /// are available at the time of returning the
    /// \[ActionResult\]\[build.bazel.remote.execution.v2.ActionResult\] and will be
    /// for some period of time afterwards. The lifetimes of the referenced blobs SHOULD be increased
    /// if necessary and applicable.
    ///
    /// Errors:
    ///
    /// * `NOT_FOUND`: The requested `ActionResult` is not in the cache.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn get_action_result<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::GetActionResultRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::ActionResult,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// Upload a new execution result.
    ///
    /// In order to allow the server to perform access control based on the type of
    /// action, and to assist with client debugging, the client MUST first upload
    /// the \[Action\]\[build.bazel.remote.execution.v2.Action\] that produced the
    /// result, along with its
    /// \[Command\]\[build.bazel.remote.execution.v2.Command\], into the
    /// `ContentAddressableStorage`.
    ///
    /// Server implementations MAY modify the
    /// `UpdateActionResultRequest.action_result` and return an equivalent value.
    ///
    /// Errors:
    ///
    /// * `INVALID_ARGUMENT`: One or more arguments are invalid.
    /// * `FAILED_PRECONDITION`: One or more errors occurred in updating the
    ///   action result, such as a missing command or action.
    /// * `RESOURCE_EXHAUSTED`: There is insufficient storage space to add the
    ///   entry to the cache.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn update_action_result<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::UpdateActionResultRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::ActionResult,
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
pub trait ActionCacheExt: ActionCache {
    /// Register this service implementation with a Router.
    ///
    /// Takes ownership of the `Arc<Self>` and returns a new Router with
    /// this service's methods registered.
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router;
}
impl<S: ActionCache> ActionCacheExt for S {
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router {
        router
            .route_view(
                ACTION_CACHE_SERVICE_NAME,
                "GetActionResult",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetActionResultRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::GetActionResultRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.get_action_result(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::ActionResult,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(ACTION_CACHE_GET_ACTION_RESULT_SPEC)
            .route_view(
                ACTION_CACHE_SERVICE_NAME,
                "UpdateActionResult",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::UpdateActionResultRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::UpdateActionResultRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.update_action_result(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::ActionResult,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(ACTION_CACHE_UPDATE_ACTION_RESULT_SPEC)
    }
}
/// Type-inference marker used by [`Router::add_service`](::connectrpc::Router::add_service).
#[doc(hidden)]
pub struct ActionCacheRegisterMarker;
impl<S: ActionCache> ::connectrpc::ServiceRegister<ActionCacheRegisterMarker>
for ::std::sync::Arc<S> {
    fn register_service(self, router: ::connectrpc::Router) -> ::connectrpc::Router {
        <S as ActionCacheExt>::register(self, router)
    }
}
/// Monomorphic dispatcher for `ActionCache`.
///
/// Unlike `.register(Router)` which type-erases each method into an `Arc<dyn ErasedHandler>` stored in a `HashMap`, this struct dispatches via a compile-time `match` on method name: no vtable, no hash lookup.
///
/// # Example
///
/// ```rust,ignore
/// use connectrpc::ConnectRpcService;
///
/// let server = ActionCacheServer::new(MyImpl);
/// let service = ConnectRpcService::new(server);
/// // hand `service` to axum/hyper as a fallback_service
/// ```
pub struct ActionCacheServer<T> {
    inner: ::std::sync::Arc<T>,
}
impl<T: ActionCache> ActionCacheServer<T> {
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
impl<T> Clone for ActionCacheServer<T> {
    fn clone(&self) -> Self {
        Self {
            inner: ::std::sync::Arc::clone(&self.inner),
        }
    }
}
impl<T: ActionCache> ::connectrpc::Dispatcher for ActionCacheServer<T> {
    #[inline]
    fn lookup(
        &self,
        path: &str,
    ) -> Option<::connectrpc::dispatcher::codegen::MethodDescriptor> {
        let method = path.strip_prefix("build.bazel.remote.execution.v2.ActionCache/")?;
        match method {
            "GetActionResult" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(ACTION_CACHE_GET_ACTION_RESULT_SPEC),
                )
            }
            "UpdateActionResult" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(ACTION_CACHE_UPDATE_ACTION_RESULT_SPEC),
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ActionCache/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "GetActionResult" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::GetActionResultRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetActionResultRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::GetActionResultRequest,
                    >::from_parts(&req, &body);
                    svc.get_action_result(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::ActionResult,
                        >(format)
                })
            }
            "UpdateActionResult" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::UpdateActionResultRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::UpdateActionResultRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::UpdateActionResultRequest,
                    >::from_parts(&req, &body);
                    svc.update_action_result(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::ActionResult,
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ActionCache/") else {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ActionCache/") else {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ActionCache/") else {
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
/// let client = ActionCacheClient::new(conn, config);
/// let response = client.get_action_result(request).await?;
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
/// let client = ActionCacheClient::new(http, config);
/// let response = client.get_action_result(request).await?;
/// ```
///
/// # Working with the response
///
/// Unary calls return [`UnaryResponse<OwnedView<FooView>>`](::connectrpc::client::UnaryResponse).
/// [`view()`](::connectrpc::client::UnaryResponse::view) borrows the response
/// message, so field access is zero-copy:
///
/// ```rust,ignore
/// let resp = client.get_action_result(request).await?;
/// let name: &str = resp.view().name;  // borrow into the response buffer
/// ```
///
/// If you need the owned struct (e.g. to store or pass by value), use
/// [`into_owned()`](::connectrpc::client::UnaryResponse::into_owned):
///
/// ```rust,ignore
/// let owned = client.get_action_result(request).await?.into_owned();
/// ```
///
/// [`into_view()`](::connectrpc::client::UnaryResponse::into_view) keeps the
/// zero-copy decoded body (an `OwnedView`) without copying; field access on it
/// goes through `.reborrow()`. Streaming responses yield one
/// [`StreamMessage`](::connectrpc::StreamMessage) per received message from
/// `.message().await` — read fields zero-copy through the generated accessor
/// methods (`msg.name()`) or `.view()`, or convert with `.to_owned_message()`.
#[derive(Clone)]
pub struct ActionCacheClient<T> {
    transport: T,
    config: ::connectrpc::client::ClientConfig,
}
impl<T> ActionCacheClient<T>
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
    /// Call the GetActionResult RPC. Sends a request to /build.bazel.remote.execution.v2.ActionCache/GetActionResult.
    pub async fn get_action_result(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::GetActionResultRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.get_action_result_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the GetActionResult RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn get_action_result_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::GetActionResultRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                ACTION_CACHE_GET_ACTION_RESULT_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the UpdateActionResult RPC. Sends a request to /build.bazel.remote.execution.v2.ActionCache/UpdateActionResult.
    pub async fn update_action_result(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::UpdateActionResultRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.update_action_result_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the UpdateActionResult RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn update_action_result_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::UpdateActionResultRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::ActionResultView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                ACTION_CACHE_UPDATE_ACTION_RESULT_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
}
/// Full service name for this service.
pub const CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME: &str = "build.bazel.remote.execution.v2.ContentAddressableStorage";
/// Static [`Spec`](::connectrpc::Spec) for the `FindMissingBlobs` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CONTENT_ADDRESSABLE_STORAGE_FIND_MISSING_BLOBS_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `BatchUpdateBlobs` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CONTENT_ADDRESSABLE_STORAGE_BATCH_UPDATE_BLOBS_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchUpdateBlobs",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `BatchReadBlobs` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CONTENT_ADDRESSABLE_STORAGE_BATCH_READ_BLOBS_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchReadBlobs",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `GetTree` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CONTENT_ADDRESSABLE_STORAGE_GET_TREE_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ContentAddressableStorage/GetTree",
        ::connectrpc::StreamType::ServerStream,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `SplitBlob` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CONTENT_ADDRESSABLE_STORAGE_SPLIT_BLOB_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ContentAddressableStorage/SplitBlob",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// Static [`Spec`](::connectrpc::Spec) for the `SpliceBlob` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CONTENT_ADDRESSABLE_STORAGE_SPLICE_BLOB_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.ContentAddressableStorage/SpliceBlob",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// The CAS (content-addressable storage) is used to store the inputs to and
/// outputs from the execution service. Each piece of content is addressed by the
/// digest of its binary data.
///
/// Most of the binary data stored in the CAS is opaque to the execution engine,
/// and is only used as a communication medium. In order to build an
/// \[Action\]\[build.bazel.remote.execution.v2.Action\],
/// however, the client will need to also upload the
/// \[Command\]\[build.bazel.remote.execution.v2.Command\] and input root
/// \[Directory\]\[build.bazel.remote.execution.v2.Directory\] for the Action.
/// The Command and Directory messages must be marshalled to wire format and then
/// uploaded under the hash as with any other piece of content. In practice, the
/// input root directory is likely to refer to other Directories in its
/// hierarchy, which must also each be uploaded on their own.
///
/// For small file uploads the client should group them together and call
/// \[BatchUpdateBlobs\]\[build.bazel.remote.execution.v2.ContentAddressableStorage.BatchUpdateBlobs\].
///
/// For large uploads, the client must use the
/// \[Write method\]\[google.bytestream.ByteStream.Write\] of the ByteStream API.
///
/// For uncompressed data, the `WriteRequest.resource_name` is of the following form:
/// `{instance_name}/uploads/{uuid}/blobs/{digest_function/}{hash}/{size}{/optional_metadata}`
///
/// Where:
/// * `instance_name` is an identifier used to distinguish between the various
///   instances on the server. Syntax and semantics of this field are defined
///   by the server; Clients must not make any assumptions about it (e.g.,
///   whether it spans multiple path segments or not). If it is the empty path,
///   the leading slash is omitted, so that the `resource_name` becomes
///   `uploads/{uuid}/blobs/{digest_function/}{hash}/{size}{/optional_metadata}`.
///   To simplify parsing, a path segment cannot equal any of the following
///   keywords: `blobs`, `uploads`, `actions`, `actionResults`, `operations`,
///   `capabilities` or `compressed-blobs`.
/// * `uuid` is a version 4 UUID generated by the client, used to avoid
///   collisions between concurrent uploads of the same data. Clients MAY
///   reuse the same `uuid` for uploading different blobs.
/// * `digest_function` is a lowercase string form of a `DigestFunction.Value`
///   enum, indicating which digest function was used to compute `hash`. If the
///   digest function used is one of MD5, MURMUR3, SHA1, SHA256, SHA384, SHA512,
///   or VSO, this component MUST be omitted. In that case the server SHOULD
///   infer the digest function using the length of the `hash` and the digest
///   functions announced in the server's capabilities.
/// * `hash` and `size` refer to the \[Digest\]\[build.bazel.remote.execution.v2.Digest\]
///   of the data being uploaded.
/// * `optional_metadata` is implementation specific data, which clients MAY omit.
///   Servers MAY ignore this metadata.
///
/// Data can alternatively be uploaded in compressed form, with the following
/// `WriteRequest.resource_name` form:
/// `{instance_name}/uploads/{uuid}/compressed-blobs/{compressor}/{digest_function/}{uncompressed_hash}/{uncompressed_size}{/optional_metadata}`
///
/// Where:
/// * `instance_name`, `uuid`, `digest_function` and `optional_metadata` are
///   defined as above.
/// * `compressor` is a lowercase string form of a `Compressor.Value` enum
///   other than `identity`, which is supported by the server and advertised in
///   \[CacheCapabilities.supported_compressors\]\[build.bazel.remote.execution.v2.CacheCapabilities.supported_compressors\].
/// * `uncompressed_hash` and `uncompressed_size` refer to the
///   \[Digest\]\[build.bazel.remote.execution.v2.Digest\] of the data being
///   uploaded, once uncompressed. Servers MUST verify that these match
///   the uploaded data once uncompressed, and MUST return an
///   `INVALID_ARGUMENT` error in the case of mismatch.
///
/// Note that when writing compressed blobs, the `WriteRequest.write_offset` in
/// the initial request in a stream refers to the offset in the uncompressed form
/// of the blob. In subsequent requests, `WriteRequest.write_offset` MUST be the
/// sum of the first request's 'WriteRequest.write_offset' and the total size of
/// all the compressed data bundles in the previous requests.
/// Note that this mixes an uncompressed offset with a compressed byte length,
/// which is nonsensical, but it is done to fit the semantics of the existing
/// ByteStream protocol.
///
/// Uploads of the same data MAY occur concurrently in any form, compressed or
/// uncompressed.
///
/// Clients SHOULD NOT use gRPC-level compression for ByteStream API `Write`
/// calls of compressed blobs, since this would compress already-compressed data.
///
/// When attempting an upload, if another client has already completed the upload
/// (which may occur in the middle of a single upload if another client uploads
/// the same blob concurrently), the request will terminate immediately without
/// error, and with a response whose `committed_size` is the value `-1` if this
/// is a compressed upload, or with the full size of the uploaded file if this is
/// an uncompressed upload (regardless of how much data was transmitted by the
/// client). If the client completes the upload but the
/// \[Digest\]\[build.bazel.remote.execution.v2.Digest\] does not match, an
/// `INVALID_ARGUMENT` error will be returned. In either case, the client should
/// not attempt to retry the upload.
///
/// Small downloads can be grouped and requested in a batch via
/// \[BatchReadBlobs\]\[build.bazel.remote.execution.v2.ContentAddressableStorage.BatchReadBlobs\].
///
/// For large downloads, the client must use the
/// \[Read method\]\[google.bytestream.ByteStream.Read\] of the ByteStream API.
///
/// For uncompressed data, the `ReadRequest.resource_name` is of the following form:
/// `{instance_name}/blobs/{digest_function/}{hash}/{size}`
/// Where `instance_name`, `digest_function`, `hash` and `size` are defined as
/// for uploads.
///
/// Data can alternatively be downloaded in compressed form, with the following
/// `ReadRequest.resource_name` form:
/// `{instance_name}/compressed-blobs/{compressor}/{digest_function/}{uncompressed_hash}/{uncompressed_size}`
///
/// Where:
/// * `instance_name`, `compressor` and `digest_function` are defined as for
///   uploads.
/// * `uncompressed_hash` and `uncompressed_size` refer to the
///   \[Digest\]\[build.bazel.remote.execution.v2.Digest\] of the data being
///   downloaded, once uncompressed. Clients MUST verify that these match
///   the downloaded data once uncompressed, and take appropriate steps in
///   the case of failure such as retrying a limited number of times or
///   surfacing an error to the user.
///
/// When downloading compressed blobs:
/// * `ReadRequest.read_offset` refers to the offset in the uncompressed form
///   of the blob.
/// * Servers MUST return `INVALID_ARGUMENT` if `ReadRequest.read_limit` is
///   non-zero.
/// * Servers MAY use any compression level they choose, including different
///   levels for different blobs (e.g. choosing a level designed for maximum
///   speed for data known to be incompressible).
/// * Clients SHOULD NOT use gRPC-level compression, since this would compress
///   already-compressed data.
///
/// Servers MUST be able to provide data for all recently advertised blobs in
/// each of the compression formats that the server supports, as well as in
/// uncompressed form.
///
/// Additionally, ByteStream requests MAY come with an additional plain text header
/// that indicates the `resource_name` of the blob being sent.  The header, if
/// present, MUST follow the following convention:
/// * name: `build.bazel.remote.execution.v2.resource-name`.
/// * contents: the plain text resource_name of the request message.
/// If set, the contents of the header MUST match the `resource_name` of the request
/// message.  Servers MAY use this header to assist in routing requests to the
/// appropriate backend.
///
/// The lifetime of entries in the CAS is implementation specific, but it SHOULD
/// be long enough to allow for newly-added and recently looked-up entries to be
/// used in subsequent calls (e.g. to
/// \[Execute\]\[build.bazel.remote.execution.v2.Execution.Execute\]).
///
/// Servers MUST behave as though empty blobs are always available, even if they
/// have not been uploaded. Clients MAY optimize away the uploading or
/// downloading of empty blobs.
///
/// As with other services in the Remote Execution API, any call may return an
/// error with a \[RetryInfo\]\[google.rpc.RetryInfo\] error detail providing
/// information about when the client should retry the request; clients SHOULD
/// respect the information provided.
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
pub trait ContentAddressableStorage: Send + Sync + 'static {
    /// Determine if blobs are present in the CAS.
    ///
    /// Clients can use this API before uploading blobs to determine which ones are
    /// already present in the CAS and do not need to be uploaded again.
    ///
    /// Servers SHOULD increase the lifetimes of the referenced blobs if necessary and
    /// applicable.
    ///
    /// There are no method-specific errors.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn find_missing_blobs<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// Upload many blobs at once.
    ///
    /// The server may enforce a limit of the combined total size of blobs
    /// to be uploaded using this API. This limit may be obtained using the
    /// \[Capabilities\]\[build.bazel.remote.execution.v2.Capabilities\] API.
    /// Requests exceeding the limit should either be split into smaller
    /// chunks or uploaded using the
    /// \[ByteStream API\]\[google.bytestream.ByteStream\], as appropriate.
    ///
    /// This request is equivalent to calling a Bytestream `Write` request
    /// on each individual blob, in parallel. The requests may succeed or fail
    /// independently.
    ///
    /// Errors:
    ///
    /// * `INVALID_ARGUMENT`: The client attempted to upload more than the
    ///   server supported limit.
    ///
    /// Individual requests may return the following errors, additionally:
    ///
    /// * `RESOURCE_EXHAUSTED`: There is insufficient disk quota to store the blob.
    /// * `INVALID_ARGUMENT`: The
    /// \[Digest\]\[build.bazel.remote.execution.v2.Digest\] does not match the
    /// provided data.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn batch_update_blobs<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// Download many blobs at once.
    ///
    /// The server may enforce a limit of the combined total size of blobs
    /// to be downloaded using this API. This limit may be obtained using the
    /// \[Capabilities\]\[build.bazel.remote.execution.v2.Capabilities\] API.
    /// Requests exceeding the limit should either be split into smaller
    /// chunks or downloaded using the
    /// \[ByteStream API\]\[google.bytestream.ByteStream\], as appropriate.
    ///
    /// This request is equivalent to calling a Bytestream `Read` request
    /// on each individual blob, in parallel. The requests may succeed or fail
    /// independently.
    ///
    /// Errors:
    ///
    /// * `INVALID_ARGUMENT`: The client attempted to read more than the
    ///   server supported limit.
    ///
    /// Every error on individual read will be returned in the corresponding digest
    /// status.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn batch_read_blobs<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// Fetch the entire directory tree rooted at a node.
    ///
    /// This request must be targeted at a
    /// \[Directory\]\[build.bazel.remote.execution.v2.Directory\] stored in the
    /// \[ContentAddressableStorage\]\[build.bazel.remote.execution.v2.ContentAddressableStorage\]
    /// (CAS). The server will enumerate the `Directory` tree recursively and
    /// return every node descended from the root.
    ///
    /// The GetTreeRequest.page_token parameter can be used to skip ahead in
    /// the stream (e.g. when retrying a partially completed and aborted request),
    /// by setting it to a value taken from GetTreeResponse.next_page_token of the
    /// last successfully processed GetTreeResponse).
    ///
    /// The exact traversal order is unspecified and, unless retrieving subsequent
    /// pages from an earlier request, is not guaranteed to be stable across
    /// multiple invocations of `GetTree`.
    ///
    /// If part of the tree is missing from the CAS, the server will return the
    /// portion present and omit the rest.
    ///
    /// Errors:
    ///
    /// * `NOT_FOUND`: The requested tree root is not present in the CAS.
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call (until the response stream is returned);
    /// message fields are read directly on it (zero-copy). Data the
    /// returned stream needs must be copied out or converted via
    /// `.to_owned_message()`.
    fn get_tree(
        &self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::GetTreeRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            ::connectrpc::ServiceStream<
                impl ::connectrpc::Encodable<
                    crate::proto::build::bazel::remote::execution::v2::GetTreeResponse,
                > + Send + use<Self>,
            >,
        >,
    > + Send;
    /// SplitBlob retrieves information about how a blob is split into chunks.
    ///
    /// This call returns information about how a blob is split into chunks, and
    /// returns a list of the chunk digests. Using the returned list of chunk digests,
    /// a client can check which chunks are locally available and only fetch the
    /// missing ones. The desired blob can be assembled by concatenating the fetched
    /// chunks in the order of the digests in the list. The chunks SHOULD all be
    /// available in the CAS.
    ///
    /// This API can be used to reduce the required data to download a large blob
    /// from CAS if some chunks from similar blobs are locally available. For this
    /// procedure to work properly, blobs SHOULD be split in a content-defined way,
    /// rather than with fixed-sized chunking.
    ///
    /// If a split request is answered successfully, a client can expect the
    /// following guarantees from the server:
    ///  1. The blob chunks are stored in CAS.
    ///  2. Concatenating the blob chunks in the order of the digest list returned
    /// ```text
    /// by the server results in the original blob.
    /// ```
    ///
    /// Servers which implement this functionality MUST declare that they support
    /// it by setting the
    /// \[CacheCapabilities.split_blob_support\]\[build.bazel.remote.execution.v2.CacheCapabilities.split_blob_support\]
    /// field accordingly.
    ///
    /// Clients MUST check that the server supports this capability, before using
    /// it.
    ///
    /// Clients SHOULD verify that the digest of the blob assembled by the fetched
    /// chunks is equal to the requested blob digest.
    ///
    /// The lifetimes of the generated chunk blobs MAY be independent of the
    /// lifetime of the original blob. In particular:
    ///  * A blob and any chunk derived from it MAY be evicted from the CAS at
    ///    different times.
    ///  * A call to \[SplitBlob\]\[build.bazel.remote.execution.v2.ContentAddressableStorage.SplitBlob\]
    ///    extends the lifetime of the original blob, and sets the lifetimes of
    ///    the resulting chunks (or extends the lifetimes of already-existing
    ///    chunks).
    ///  * Touching a chunk extends its lifetime, but the server MAY choose not
    ///    to extend the lifetime of the original blob.
    ///  * Touching the original blob extends its lifetime, but the server MAY
    ///    choose not to extend the lifetimes of chunks derived from it.
    ///
    /// When blob splitting and splicing is used at the same time, the clients and
    /// the server SHOULD agree out-of-band upon a chunking algorithm used by both
    /// parties to benefit from each other's chunk data and avoid unnecessary data
    /// duplication.
    ///
    /// Errors:
    ///
    /// * `NOT_FOUND`: The requested blob is not present in the CAS, OR there is no
    ///   split information available for the blob, OR at least one chunk needed to
    ///   reconstruct the blob is missing from the CAS.
    /// * `RESOURCE_EXHAUSTED`: There is insufficient disk quota to store the blob
    ///   chunks.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn split_blob<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::SplitBlobRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::SplitBlobResponse,
            > + Send + use<'a, Self>,
        >,
    > + Send;
    /// SpliceBlob tells the CAS how chunks can compose a blob.
    ///
    /// This is the complementary operation to the
    /// \[ContentAddressableStorage.SplitBlob\]\[build.bazel.remote.execution.v2.ContentAddressableStorage.SplitBlob\]
    /// function to handle the chunked upload of large blobs to save upload
    /// traffic.
    ///
    /// When uploading a large blob using chunked upload, clients MUST first upload
    /// all chunks to the CAS, then call this RPC to tell the server how those chunks
    /// compose the original blob. The chunks referenced in the SpliceBlob call SHOULD be
    /// available in the CAS before calling this RPC.
    ///
    /// If a client needs to upload a large blob and is able to split a blob into
    /// chunks in such a way that reusable chunks are obtained, e.g., by means of
    /// content-defined chunking, it can first determine which parts of the blob
    /// are already available in the remote CAS and upload the missing chunks, and
    /// then use this API to store information on how the chunks compose the
    /// original blob.
    ///
    /// Servers which implement this functionality MUST declare that they support
    /// it by setting the
    /// \[CacheCapabilities.splice_blob_support\]\[build.bazel.remote.execution.v2.CacheCapabilities.splice_blob_support\]
    /// field accordingly.
    ///
    /// Clients MUST check that the server supports this capability, before using
    /// it.
    ///
    /// In order to ensure data consistency of the CAS, the server MUST only add
    /// blobs to the CAS after verifying their digests. In particular, servers MUST NOT
    /// trust digests provided by the client. The server MAY accept a request as no-op
    /// if the client-specified blob is already in CAS or if information on how to
    /// construct the blob from chunks is available. If the client-specified blob is
    /// not already in the CAS, the server MUST verify that the digest of the newly
    /// created blob assembled from chunks matches the digest specified by the
    /// client, and reject the request if they differ. Servers MAY choose to allow
    /// overwriting existing chunk mappings or to store multiple chunk mappings for
    /// the same blob.
    ///
    /// When blob splitting and splicing is used at the same time, the clients and
    /// the server SHOULD agree out-of-band upon a chunking algorithm used by both
    /// parties to benefit from each other's chunk data and avoid unnecessary data
    /// duplication.
    ///
    /// Errors:
    ///
    /// * `NOT_FOUND`: At least one of the blob chunks is not present in the CAS.
    /// * `RESOURCE_EXHAUSTED`: There is insufficient disk quota to store the
    ///   spliced blob.
    /// * `INVALID_ARGUMENT`: The digest of the spliced blob is different from the
    ///   provided expected digest.
    /// * `ALREADY_EXISTS`: The blob already exists in CAS and the server did not
    ///   extend the lifetime of the chunks specified in the request, e.g. because
    ///   it prefers a different chunking and extended those instead. Clients can
    ///   call \[SplitBlob\]\[build.bazel.remote.execution.v2.ContentAddressableStorage.SplitBlob\]
    ///   to check what chunk mapping the server is using.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn splice_blob<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::SpliceBlobRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::SpliceBlobResponse,
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
pub trait ContentAddressableStorageExt: ContentAddressableStorage {
    /// Register this service implementation with a Router.
    ///
    /// Takes ownership of the `Arc<Self>` and returns a new Router with
    /// this service's methods registered.
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router;
}
impl<S: ContentAddressableStorage> ContentAddressableStorageExt for S {
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router {
        router
            .route_view(
                CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME,
                "FindMissingBlobs",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.find_missing_blobs(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(CONTENT_ADDRESSABLE_STORAGE_FIND_MISSING_BLOBS_SPEC)
            .route_view(
                CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME,
                "BatchUpdateBlobs",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.batch_update_blobs(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(CONTENT_ADDRESSABLE_STORAGE_BATCH_UPDATE_BLOBS_SPEC)
            .route_view(
                CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME,
                "BatchReadBlobs",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.batch_read_blobs(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(CONTENT_ADDRESSABLE_STORAGE_BATCH_READ_BLOBS_SPEC)
            .route_view_server_stream::<
                _,
                _,
                crate::proto::build::bazel::remote::execution::v2::GetTreeResponse,
            >(
                CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME,
                "GetTree",
                ::connectrpc::view_streaming_handler_fn({
                    let svc = ::std::sync::Arc::clone(&self);
                    move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeRequestView<
                                'static,
                            >,
                        >|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::GetTreeRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.get_tree(ctx, sreq).await
                        }
                    }
                }),
            )
            .with_spec(CONTENT_ADDRESSABLE_STORAGE_GET_TREE_SPEC)
            .route_view(
                CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME,
                "SplitBlob",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::SplitBlobRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.split_blob(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::SplitBlobResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(CONTENT_ADDRESSABLE_STORAGE_SPLIT_BLOB_SPEC)
            .route_view(
                CONTENT_ADDRESSABLE_STORAGE_SERVICE_NAME,
                "SpliceBlob",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::SpliceBlobRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.splice_blob(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::SpliceBlobResponse,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(CONTENT_ADDRESSABLE_STORAGE_SPLICE_BLOB_SPEC)
    }
}
/// Type-inference marker used by [`Router::add_service`](::connectrpc::Router::add_service).
#[doc(hidden)]
pub struct ContentAddressableStorageRegisterMarker;
impl<
    S: ContentAddressableStorage,
> ::connectrpc::ServiceRegister<ContentAddressableStorageRegisterMarker>
for ::std::sync::Arc<S> {
    fn register_service(self, router: ::connectrpc::Router) -> ::connectrpc::Router {
        <S as ContentAddressableStorageExt>::register(self, router)
    }
}
/// Monomorphic dispatcher for `ContentAddressableStorage`.
///
/// Unlike `.register(Router)` which type-erases each method into an `Arc<dyn ErasedHandler>` stored in a `HashMap`, this struct dispatches via a compile-time `match` on method name: no vtable, no hash lookup.
///
/// # Example
///
/// ```rust,ignore
/// use connectrpc::ConnectRpcService;
///
/// let server = ContentAddressableStorageServer::new(MyImpl);
/// let service = ConnectRpcService::new(server);
/// // hand `service` to axum/hyper as a fallback_service
/// ```
pub struct ContentAddressableStorageServer<T> {
    inner: ::std::sync::Arc<T>,
}
impl<T: ContentAddressableStorage> ContentAddressableStorageServer<T> {
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
impl<T> Clone for ContentAddressableStorageServer<T> {
    fn clone(&self) -> Self {
        Self {
            inner: ::std::sync::Arc::clone(&self.inner),
        }
    }
}
impl<T: ContentAddressableStorage> ::connectrpc::Dispatcher
for ContentAddressableStorageServer<T> {
    #[inline]
    fn lookup(
        &self,
        path: &str,
    ) -> Option<::connectrpc::dispatcher::codegen::MethodDescriptor> {
        let method = path
            .strip_prefix("build.bazel.remote.execution.v2.ContentAddressableStorage/")?;
        match method {
            "FindMissingBlobs" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(CONTENT_ADDRESSABLE_STORAGE_FIND_MISSING_BLOBS_SPEC),
                )
            }
            "BatchUpdateBlobs" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(CONTENT_ADDRESSABLE_STORAGE_BATCH_UPDATE_BLOBS_SPEC),
                )
            }
            "BatchReadBlobs" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(CONTENT_ADDRESSABLE_STORAGE_BATCH_READ_BLOBS_SPEC),
                )
            }
            "GetTree" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::server_streaming()
                        .with_spec(CONTENT_ADDRESSABLE_STORAGE_GET_TREE_SPEC),
                )
            }
            "SplitBlob" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(CONTENT_ADDRESSABLE_STORAGE_SPLIT_BLOB_SPEC),
                )
            }
            "SpliceBlob" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(CONTENT_ADDRESSABLE_STORAGE_SPLICE_BLOB_SPEC),
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ContentAddressableStorage/")
        else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "FindMissingBlobs" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsRequest,
                    >::from_parts(&req, &body);
                    svc.find_missing_blobs(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsResponse,
                        >(format)
                })
            }
            "BatchUpdateBlobs" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsRequest,
                    >::from_parts(&req, &body);
                    svc.batch_update_blobs(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsResponse,
                        >(format)
                })
            }
            "BatchReadBlobs" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsRequest,
                    >::from_parts(&req, &body);
                    svc.batch_read_blobs(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsResponse,
                        >(format)
                })
            }
            "SplitBlob" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::SplitBlobRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::SplitBlobRequest,
                    >::from_parts(&req, &body);
                    svc.split_blob(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::SplitBlobResponse,
                        >(format)
                })
            }
            "SpliceBlob" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::SpliceBlobRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::SpliceBlobRequest,
                    >::from_parts(&req, &body);
                    svc.splice_blob(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::SpliceBlobResponse,
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ContentAddressableStorage/")
        else {
            return ::connectrpc::dispatcher::codegen::unimplemented_streaming(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "GetTree" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::GetTreeRequest,
                    >(request, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::GetTreeRequest,
                    >::from_parts(&req, &body);
                    let resp = svc.get_tree(ctx, req).await?;
                    Ok(
                        resp
                            .map_body(|s| ::connectrpc::dispatcher::codegen::encode_response_stream::<
                                crate::proto::build::bazel::remote::execution::v2::GetTreeResponse,
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ContentAddressableStorage/")
        else {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.ContentAddressableStorage/")
        else {
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
/// let client = ContentAddressableStorageClient::new(conn, config);
/// let response = client.find_missing_blobs(request).await?;
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
/// let client = ContentAddressableStorageClient::new(http, config);
/// let response = client.find_missing_blobs(request).await?;
/// ```
///
/// # Working with the response
///
/// Unary calls return [`UnaryResponse<OwnedView<FooView>>`](::connectrpc::client::UnaryResponse).
/// [`view()`](::connectrpc::client::UnaryResponse::view) borrows the response
/// message, so field access is zero-copy:
///
/// ```rust,ignore
/// let resp = client.find_missing_blobs(request).await?;
/// let name: &str = resp.view().name;  // borrow into the response buffer
/// ```
///
/// If you need the owned struct (e.g. to store or pass by value), use
/// [`into_owned()`](::connectrpc::client::UnaryResponse::into_owned):
///
/// ```rust,ignore
/// let owned = client.find_missing_blobs(request).await?.into_owned();
/// ```
///
/// [`into_view()`](::connectrpc::client::UnaryResponse::into_view) keeps the
/// zero-copy decoded body (an `OwnedView`) without copying; field access on it
/// goes through `.reborrow()`. Streaming responses yield one
/// [`StreamMessage`](::connectrpc::StreamMessage) per received message from
/// `.message().await` — read fields zero-copy through the generated accessor
/// methods (`msg.name()`) or `.view()`, or convert with `.to_owned_message()`.
#[derive(Clone)]
pub struct ContentAddressableStorageClient<T> {
    transport: T,
    config: ::connectrpc::client::ClientConfig,
}
impl<T> ContentAddressableStorageClient<T>
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
    /// Call the FindMissingBlobs RPC. Sends a request to /build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs.
    pub async fn find_missing_blobs(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.find_missing_blobs_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the FindMissingBlobs RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn find_missing_blobs_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::FindMissingBlobsRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::FindMissingBlobsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                CONTENT_ADDRESSABLE_STORAGE_FIND_MISSING_BLOBS_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the BatchUpdateBlobs RPC. Sends a request to /build.bazel.remote.execution.v2.ContentAddressableStorage/BatchUpdateBlobs.
    pub async fn batch_update_blobs(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.batch_update_blobs_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the BatchUpdateBlobs RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn batch_update_blobs_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::BatchUpdateBlobsRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchUpdateBlobsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                CONTENT_ADDRESSABLE_STORAGE_BATCH_UPDATE_BLOBS_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the BatchReadBlobs RPC. Sends a request to /build.bazel.remote.execution.v2.ContentAddressableStorage/BatchReadBlobs.
    pub async fn batch_read_blobs(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.batch_read_blobs_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the BatchReadBlobs RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn batch_read_blobs_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::BatchReadBlobsRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::BatchReadBlobsResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                CONTENT_ADDRESSABLE_STORAGE_BATCH_READ_BLOBS_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the GetTree RPC. Sends a request to /build.bazel.remote.execution.v2.ContentAddressableStorage/GetTree.
    pub async fn get_tree(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::GetTreeRequest,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeResponseView<
                'static,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.get_tree_with_options(request, ::connectrpc::client::CallOptions::default())
            .await
    }
    /// Call the GetTree RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn get_tree_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::GetTreeRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::ServerStream<
            T::ResponseBody,
            crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetTreeResponseView<
                'static,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_server_stream(
                &self.transport,
                &self.config,
                CONTENT_ADDRESSABLE_STORAGE_GET_TREE_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the SplitBlob RPC. Sends a request to /build.bazel.remote.execution.v2.ContentAddressableStorage/SplitBlob.
    pub async fn split_blob(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::SplitBlobRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.split_blob_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the SplitBlob RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn split_blob_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::SplitBlobRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::SplitBlobResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                CONTENT_ADDRESSABLE_STORAGE_SPLIT_BLOB_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
    /// Call the SpliceBlob RPC. Sends a request to /build.bazel.remote.execution.v2.ContentAddressableStorage/SpliceBlob.
    pub async fn splice_blob(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::SpliceBlobRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.splice_blob_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the SpliceBlob RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn splice_blob_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::SpliceBlobRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::SpliceBlobResponseView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                CONTENT_ADDRESSABLE_STORAGE_SPLICE_BLOB_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
}
/// Full service name for this service.
pub const CAPABILITIES_SERVICE_NAME: &str = "build.bazel.remote.execution.v2.Capabilities";
/// Static [`Spec`](::connectrpc::Spec) for the `GetCapabilities` RPC, as seen by the server; the generated client passes it with [`origin`](::connectrpc::Spec::origin) `Client` (compare across sides with [`Spec::same_method`](::connectrpc::Spec::same_method)).
pub const CAPABILITIES_GET_CAPABILITIES_SPEC: ::connectrpc::Spec = ::connectrpc::Spec::server(
        "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities",
        ::connectrpc::StreamType::Unary,
    )
    .with_idempotency_level(::connectrpc::IdempotencyLevel::Unknown);
/// The Capabilities service may be used by remote execution clients to query
/// various server properties, in order to self-configure or return meaningful
/// error messages.
///
/// The query may include a particular `instance_name`, in which case the values
/// returned will pertain to that instance.
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
pub trait Capabilities: Send + Sync + 'static {
    /// GetCapabilities returns the server capabilities configuration of the
    /// remote endpoint.
    /// Only the capabilities of the services supported by the endpoint will
    /// be returned:
    /// * Execution + CAS + Action Cache endpoints should return both
    ///   CacheCapabilities and ExecutionCapabilities.
    /// * Execution only endpoints should return ExecutionCapabilities.
    /// * CAS + Action Cache only endpoints should return CacheCapabilities.
    ///
    /// There are no method-specific errors.
    ///
    /// `'a` lets the response body borrow from `&self` (e.g. server-resident state).
    ///
    /// `request` is borrowed from the request body and is valid for the
    /// duration of the call; message fields are read directly on it
    /// (zero-copy). The response cannot borrow from `request` — use
    /// `.to_owned_message()` (or copy the specific fields) for anything
    /// returned, stored, or moved into `tokio::spawn`.
    fn get_capabilities<'a>(
        &'a self,
        ctx: ::connectrpc::RequestContext,
        request: ::connectrpc::ServiceRequest<
            '_,
            crate::proto::build::bazel::remote::execution::v2::GetCapabilitiesRequest,
        >,
    ) -> impl ::std::future::Future<
        Output = ::connectrpc::ServiceResult<
            impl ::connectrpc::Encodable<
                crate::proto::build::bazel::remote::execution::v2::ServerCapabilities,
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
pub trait CapabilitiesExt: Capabilities {
    /// Register this service implementation with a Router.
    ///
    /// Takes ownership of the `Arc<Self>` and returns a new Router with
    /// this service's methods registered.
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router;
}
impl<S: Capabilities> CapabilitiesExt for S {
    fn register(
        self: ::std::sync::Arc<Self>,
        router: ::connectrpc::Router,
    ) -> ::connectrpc::Router {
        router
            .route_view(
                CAPABILITIES_SERVICE_NAME,
                "GetCapabilities",
                {
                    let svc = ::std::sync::Arc::clone(&self);
                    ::connectrpc::view_handler_fn(move |
                        ctx,
                        req: ::buffa::view::OwnedView<
                            crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetCapabilitiesRequestView<
                                'static,
                            >,
                        >,
                        format|
                    {
                        let svc = ::std::sync::Arc::clone(&svc);
                        async move {
                            let sreq = ::connectrpc::ServiceRequest::<
                                crate::proto::build::bazel::remote::execution::v2::GetCapabilitiesRequest,
                            >::from_parts(req.reborrow(), req.bytes());
                            svc.get_capabilities(ctx, sreq)
                                .await?
                                .encode::<
                                    crate::proto::build::bazel::remote::execution::v2::ServerCapabilities,
                                >(format)
                        }
                    })
                },
            )
            .with_spec(CAPABILITIES_GET_CAPABILITIES_SPEC)
    }
}
/// Type-inference marker used by [`Router::add_service`](::connectrpc::Router::add_service).
#[doc(hidden)]
pub struct CapabilitiesRegisterMarker;
impl<S: Capabilities> ::connectrpc::ServiceRegister<CapabilitiesRegisterMarker>
for ::std::sync::Arc<S> {
    fn register_service(self, router: ::connectrpc::Router) -> ::connectrpc::Router {
        <S as CapabilitiesExt>::register(self, router)
    }
}
/// Monomorphic dispatcher for `Capabilities`.
///
/// Unlike `.register(Router)` which type-erases each method into an `Arc<dyn ErasedHandler>` stored in a `HashMap`, this struct dispatches via a compile-time `match` on method name: no vtable, no hash lookup.
///
/// # Example
///
/// ```rust,ignore
/// use connectrpc::ConnectRpcService;
///
/// let server = CapabilitiesServer::new(MyImpl);
/// let service = ConnectRpcService::new(server);
/// // hand `service` to axum/hyper as a fallback_service
/// ```
pub struct CapabilitiesServer<T> {
    inner: ::std::sync::Arc<T>,
}
impl<T: Capabilities> CapabilitiesServer<T> {
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
impl<T> Clone for CapabilitiesServer<T> {
    fn clone(&self) -> Self {
        Self {
            inner: ::std::sync::Arc::clone(&self.inner),
        }
    }
}
impl<T: Capabilities> ::connectrpc::Dispatcher for CapabilitiesServer<T> {
    #[inline]
    fn lookup(
        &self,
        path: &str,
    ) -> Option<::connectrpc::dispatcher::codegen::MethodDescriptor> {
        let method = path.strip_prefix("build.bazel.remote.execution.v2.Capabilities/")?;
        match method {
            "GetCapabilities" => {
                Some(
                    ::connectrpc::dispatcher::codegen::MethodDescriptor::unary(false)
                        .with_spec(CAPABILITIES_GET_CAPABILITIES_SPEC),
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Capabilities/") else {
            return ::connectrpc::dispatcher::codegen::unimplemented_unary(path);
        };
        let _ = (&ctx, &request, &format);
        match method {
            "GetCapabilities" => {
                let svc = ::std::sync::Arc::clone(&self.inner);
                Box::pin(async move {
                    let body = ::connectrpc::dispatcher::codegen::request_proto_bytes::<
                        crate::proto::build::bazel::remote::execution::v2::GetCapabilitiesRequest,
                    >(request.encoded()?, format)?;
                    let req: crate::proto::build::bazel::remote::execution::v2::__buffa::view::GetCapabilitiesRequestView<
                        '_,
                    > = ::connectrpc::dispatcher::codegen::decode_borrowed_request_view(
                        &body,
                        ctx.decode_options(),
                    )?;
                    let req = ::connectrpc::ServiceRequest::<
                        crate::proto::build::bazel::remote::execution::v2::GetCapabilitiesRequest,
                    >::from_parts(&req, &body);
                    svc.get_capabilities(ctx, req)
                        .await?
                        .encode::<
                            crate::proto::build::bazel::remote::execution::v2::ServerCapabilities,
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Capabilities/") else {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Capabilities/") else {
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
        let Some(method) = path
            .strip_prefix("build.bazel.remote.execution.v2.Capabilities/") else {
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
/// let client = CapabilitiesClient::new(conn, config);
/// let response = client.get_capabilities(request).await?;
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
/// let client = CapabilitiesClient::new(http, config);
/// let response = client.get_capabilities(request).await?;
/// ```
///
/// # Working with the response
///
/// Unary calls return [`UnaryResponse<OwnedView<FooView>>`](::connectrpc::client::UnaryResponse).
/// [`view()`](::connectrpc::client::UnaryResponse::view) borrows the response
/// message, so field access is zero-copy:
///
/// ```rust,ignore
/// let resp = client.get_capabilities(request).await?;
/// let name: &str = resp.view().name;  // borrow into the response buffer
/// ```
///
/// If you need the owned struct (e.g. to store or pass by value), use
/// [`into_owned()`](::connectrpc::client::UnaryResponse::into_owned):
///
/// ```rust,ignore
/// let owned = client.get_capabilities(request).await?.into_owned();
/// ```
///
/// [`into_view()`](::connectrpc::client::UnaryResponse::into_view) keeps the
/// zero-copy decoded body (an `OwnedView`) without copying; field access on it
/// goes through `.reborrow()`. Streaming responses yield one
/// [`StreamMessage`](::connectrpc::StreamMessage) per received message from
/// `.message().await` — read fields zero-copy through the generated accessor
/// methods (`msg.name()`) or `.view()`, or convert with `.to_owned_message()`.
#[derive(Clone)]
pub struct CapabilitiesClient<T> {
    transport: T,
    config: ::connectrpc::client::ClientConfig,
}
impl<T> CapabilitiesClient<T>
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
    /// Call the GetCapabilities RPC. Sends a request to /build.bazel.remote.execution.v2.Capabilities/GetCapabilities.
    pub async fn get_capabilities(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::GetCapabilitiesRequest,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::ServerCapabilitiesView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        self.get_capabilities_with_options(
                request,
                ::connectrpc::client::CallOptions::default(),
            )
            .await
    }
    /// Call the GetCapabilities RPC with explicit per-call options. Options override [`ClientConfig`](::connectrpc::client::ClientConfig) defaults.
    pub async fn get_capabilities_with_options(
        &self,
        request: crate::proto::build::bazel::remote::execution::v2::GetCapabilitiesRequest,
        options: ::connectrpc::client::CallOptions,
    ) -> Result<
        ::connectrpc::client::UnaryResponse<
            ::buffa::view::OwnedView<
                crate::proto::build::bazel::remote::execution::v2::__buffa::view::ServerCapabilitiesView<
                    'static,
                >,
            >,
        >,
        ::connectrpc::ConnectError,
    > {
        ::connectrpc::client::call_unary(
                &self.transport,
                &self.config,
                CAPABILITIES_GET_CAPABILITIES_SPEC
                    .with_origin(::connectrpc::SpecOrigin::Client),
                request,
                options,
            )
            .await
    }
}
