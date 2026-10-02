// SPDX-License-Identifier: FSL-1.1-ALv2

//! `hello_rs`: the cross-build proof binary (R-BUILD-1, R-BUILD-3). It links the
//! crates with C code that cucinactl needs (aws-lc via rustls, zstd) plus the
//! generated buffa/connect-rust code, and runs one request in-process.

use buffa::{Message, MessageView};
use connectrpc::{RequestContext, ServiceRequest};
use hello_greeter::Server;
use hellopb_rs::connect::cucina::hello::v1::Greeter;
use hellopb_rs::proto::cucina::hello::v1::{HelloRequest, HelloRequestView};

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    rustls::crypto::aws_lc_rs::default_provider()
        .install_default()
        .map_err(|_| "a rustls crypto provider was already installed")?;
    let request = HelloRequest {
        name: std::env::args().nth(1).unwrap_or_default(),
        ..Default::default()
    };
    let body = buffa::bytes::Bytes::from(request.encode_to_vec());
    let view = HelloRequestView::decode_view(&body)?;
    let ctx = RequestContext::new(Default::default());
    let reply = Server
        .say_hello(ctx, ServiceRequest::from_parts(&view, &body))
        .await?;
    let packed = zstd::encode_all(reply.body.message.as_bytes(), 3)?;
    println!("{} ({} bytes zstd)", reply.body.message, packed.len());
    Ok(())
}
