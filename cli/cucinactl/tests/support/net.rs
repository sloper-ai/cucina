// SPDX-License-Identifier: FSL-1.1-ALv2

//! Network fakes for TLS and proxy tests, loopback only: a TLS terminator in front of
//! a plaintext fake (the fakes speak HTTP/1.1 and h2c, so the bytes pass through
//! unchanged) and an HTTP `CONNECT` proxy that resolves names itself and records what
//! it was asked.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::{TcpListener, TcpStream};

/// Serves TLS on `listener` and forwards every connection's plaintext to `backend`.
pub fn serve_tls(listener: TcpListener, backend: SocketAddr, config: Arc<rustls::ServerConfig>) {
    let acceptor = tokio_rustls::TlsAcceptor::from(config);
    tokio::spawn(async move {
        while let Ok((sock, _)) = listener.accept().await {
            let acceptor = acceptor.clone();
            tokio::spawn(async move {
                let Ok(mut tls) = acceptor.accept(sock).await else {
                    return; // e.g. the client rejected the certificate
                };
                if let Ok(mut upstream) = TcpStream::connect(backend).await {
                    let _ = tokio::io::copy_bidirectional(&mut tls, &mut upstream).await;
                }
            });
        }
    });
}

/// One `CONNECT` request the proxy received.
#[derive(Debug, Clone, PartialEq)]
pub struct Connect {
    /// `host:port` as sent by the client.
    pub authority: String,
    pub proxy_authorization: Option<String>,
}

/// An HTTP `CONNECT` proxy. `routes` maps the requested `host:port` to a local
/// address (the proxy, not the client, resolves names); anything else gets 502.
pub struct FakeProxy {
    pub addr: SocketAddr,
    pub connects: Arc<Mutex<Vec<Connect>>>,
}

impl FakeProxy {
    pub async fn start(routes: HashMap<String, SocketAddr>) -> FakeProxy {
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().unwrap();
        let connects = Arc::new(Mutex::new(Vec::new()));
        let log = connects.clone();
        let routes = Arc::new(routes);
        tokio::spawn(async move {
            while let Ok((sock, _)) = listener.accept().await {
                let (log, routes) = (log.clone(), routes.clone());
                tokio::spawn(async move {
                    let _ = handle(sock, &log, &routes).await;
                });
            }
        });
        FakeProxy { addr, connects }
    }

    /// `http://user:password@127.0.0.1:<port>`.
    pub fn url_with_credentials(&self, user: &str, password: &str) -> String {
        format!("http://{user}:{password}@{}", self.addr)
    }

    pub fn connects(&self) -> Vec<Connect> {
        self.connects.lock().unwrap().clone()
    }
}

async fn handle(
    mut sock: TcpStream,
    log: &Mutex<Vec<Connect>>,
    routes: &HashMap<String, SocketAddr>,
) -> std::io::Result<()> {
    let mut head = Vec::new();
    let mut byte = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") {
        if head.len() > 16 * 1024 || sock.read(&mut byte).await? == 0 {
            return Ok(());
        }
        head.push(byte[0]);
    }
    let text = String::from_utf8_lossy(&head).to_string();
    let mut lines = text.split("\r\n");
    let request = lines.next().unwrap_or_default();
    let mut parts = request.split_whitespace();
    let (method, authority) = (parts.next().unwrap_or(""), parts.next().unwrap_or(""));
    if method != "CONNECT" {
        sock.write_all(b"HTTP/1.1 405 Method Not Allowed\r\ncontent-length: 0\r\n\r\n")
            .await?;
        return Ok(());
    }
    let proxy_authorization = lines.find_map(|l| {
        let (k, v) = l.split_once(':')?;
        k.trim()
            .eq_ignore_ascii_case("proxy-authorization")
            .then(|| v.trim().to_string())
    });
    log.lock().unwrap().push(Connect {
        authority: authority.to_string(),
        proxy_authorization,
    });
    let Some(target) = routes.get(authority) else {
        sock.write_all(b"HTTP/1.1 502 Bad Gateway\r\ncontent-length: 0\r\n\r\n")
            .await?;
        return Ok(());
    };
    let mut upstream = TcpStream::connect(target).await?;
    sock.write_all(b"HTTP/1.1 200 Connection established\r\n\r\n")
        .await?;
    let _ = tokio::io::copy_bidirectional(&mut sock, &mut upstream).await;
    Ok(())
}
