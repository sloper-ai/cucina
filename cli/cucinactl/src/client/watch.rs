// SPDX-License-Identifier: FSL-1.1-ALv2

//! Server-stream supervision for `Watch*` RPCs: reconnects with jittered
//! exponential backoff on transient failures (and when the server ends a stream),
//! reports each reconnect to the consumer, stops on permanent errors, and stops as
//! soon as the consumer drops the stream (cancellation).

use std::hash::{BuildHasher as _, RandomState};
use std::time::Duration;

use connectrpc::{ConnectError, ErrorCode};
use futures::{Stream, StreamExt as _};
use tokio::sync::mpsc;

/// Backoff settings.
#[derive(Debug, Clone)]
pub struct WatchOptions {
    pub initial_backoff: Duration,
    pub max_backoff: Duration,
    /// Give up after this many consecutive failed attempts (`None`: never).
    pub max_attempts: Option<u32>,
}

impl Default for WatchOptions {
    fn default() -> Self {
        WatchOptions {
            initial_backoff: Duration::from_millis(250),
            max_backoff: Duration::from_secs(30),
            max_attempts: None,
        }
    }
}

/// An element of a supervised stream.
#[derive(Debug, Clone, PartialEq)]
pub enum WatchItem<T> {
    /// A message from the server.
    Data(T),
    /// The stream broke; reconnecting after `retry_in` (attempt numbers start at 1).
    Reconnecting {
        attempt: u32,
        retry_in: Duration,
        reason: String,
    },
}

/// Whether a status is worth reconnecting for.
pub fn retryable(code: ErrorCode) -> bool {
    matches!(
        code,
        ErrorCode::Unavailable
            | ErrorCode::DeadlineExceeded
            | ErrorCode::ResourceExhausted
            | ErrorCode::Aborted
            | ErrorCode::Internal
            | ErrorCode::Unknown
            | ErrorCode::Canceled
    )
}

fn delay(opts: &WatchOptions, attempt: u32) -> Duration {
    let exp = opts
        .initial_backoff
        .saturating_mul(1u32 << attempt.saturating_sub(1).min(16))
        .min(opts.max_backoff);
    // Jitter in [exp/2, exp] from a randomly keyed hasher (no RNG dependency).
    let r = RandomState::new().hash_one(attempt) % 1_000;
    let half = exp / 2;
    half + (exp - half).mul_f64(r as f64 / 1_000.0)
}

/// Supervises the stream `open()` returns (see module docs). Errors yielded are final.
pub fn reconnecting<T, F, Fut, S>(
    opts: WatchOptions,
    open: F,
) -> impl Stream<Item = Result<WatchItem<T>, ConnectError>>
where
    T: Send + 'static,
    F: Fn() -> Fut + Send + 'static,
    Fut: Future<Output = Result<S, ConnectError>> + Send,
    S: Stream<Item = Result<T, ConnectError>> + Send + Unpin,
{
    let (tx, rx) = mpsc::channel(64);
    tokio::spawn(async move {
        let mut attempt = 0u32;
        loop {
            let opened = tokio::select! {
                r = open() => r,
                () = tx.closed() => return,
            };
            let err = match opened {
                Ok(mut stream) => loop {
                    tokio::select! {
                        () = tx.closed() => return,
                        msg = stream.next() => match msg {
                            Some(Ok(item)) => {
                                attempt = 0;
                                if tx.send(Ok(WatchItem::Data(item))).await.is_err() {
                                    return;
                                }
                            }
                            None => break ConnectError::new(ErrorCode::Unavailable, "the server ended the stream"),
                            Some(Err(e)) => break e,
                        },
                    }
                },
                Err(e) => e,
            };
            attempt = attempt.saturating_add(1);
            let give_up = !retryable(err.code) || opts.max_attempts.is_some_and(|m| attempt > m);
            if give_up {
                let _ = tx.send(Err(err)).await;
                return;
            }
            let retry_in = delay(&opts, attempt);
            let note = WatchItem::Reconnecting {
                attempt,
                retry_in,
                reason: err.to_string(),
            };
            if tx.send(Ok(note)).await.is_err() {
                return;
            }
            tokio::select! {
                () = tokio::time::sleep(retry_in) => {}
                () = tx.closed() => return,
            }
        }
    });
    tokio_stream::wrappers::ReceiverStream::new(rx)
}
