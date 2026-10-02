// SPDX-License-Identifier: FSL-1.1-ALv2

//! Management client `Watch*` streams (task: "server streams with reconnect/backoff,
//! timeouts, cancellation"; R-CLI-4 live views): a broken stream is resumed with
//! backoff and reported to the consumer, a permanent error ends the stream.

mod support;

use std::time::Duration;

use connectrpc::ErrorCode;
use cucina_api::proto::cucina::v1 as pb;
use cucinactl::client::{
    ConnectOptions, ManagementClient, TokenHandle, WatchItem, WatchOptions, channel,
};
use futures::StreamExt as _;
use support::fake_mgmt::FakeMgmt;

fn kind(item: &WatchItem<pb::OperationEvent>) -> String {
    match item {
        WatchItem::Data(ev) => format!(
            "{:?}:{}",
            ev.kind.as_known().unwrap(),
            ev.operation
                .as_option()
                .map(|o| o.name.as_str())
                .unwrap_or_default()
        ),
        WatchItem::Reconnecting { attempt, .. } => format!("reconnecting#{attempt}"),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn watch_reconnects_after_breaks_and_stops_on_permanent_errors() {
    let mgmt = FakeMgmt::new("token-1");
    mgmt.state
        .watch_breaks
        .store(2, std::sync::atomic::Ordering::SeqCst);
    let url = mgmt.serve().await;
    let ch = channel(&ConnectOptions {
        endpoint: url,
        ca_file: None,
        connect_timeout: Duration::from_secs(5),
    })
    .unwrap();
    let client = ManagementClient::new(ch, TokenHandle::new("token-1"), Duration::from_secs(5));
    let fast = WatchOptions {
        initial_backoff: Duration::from_millis(1),
        max_backoff: Duration::from_millis(5),
        max_attempts: None,
    };

    let items: Vec<String> = client
        .watch_operations(pb::ListOperationsRequest::default(), fast.clone())
        .take(7)
        .map(|r| kind(&r.expect("no terminal error")))
        .collect()
        .await;
    assert_eq!(
        items,
        [
            "KIND_ADDED:op-before-break-0",
            "reconnecting#1",
            "KIND_ADDED:op-before-break-1",
            "reconnecting#1",
            "KIND_ADDED:op-2",
            "KIND_CHANGED:op-2",
            "KIND_REMOVED:op-2",
        ]
    );

    mgmt.fail_next(
        "watch_operations",
        ErrorCode::PermissionDenied,
        "not an admin",
    );
    let rest: Vec<_> = client
        .watch_operations(pb::ListOperationsRequest::default(), fast)
        .collect()
        .await;
    assert_eq!(rest.len(), 1, "a permanent error ends the stream");
    assert_eq!(
        rest[0].as_ref().unwrap_err().code,
        ErrorCode::PermissionDenied
    );
}
