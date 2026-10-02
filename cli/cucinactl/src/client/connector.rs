// SPDX-License-Identifier: FSL-1.1-ALv2

//! gRPC channels through HTTP proxies (T10a). connect-rust has no proxy support, so
//! when a proxy applies this connector opens an HTTP `CONNECT` tunnel (to an `http://`
//! or `https://` proxy, with Basic `Proxy-Authorization` from the proxy URL's user
//! info) and runs TLS with ALPN `h2` to the target inside it.
//!
//! Which proxy applies is decided by hyper-util's matcher, the one reqwest uses for
//! discovery, the STS and the identity providers: `HTTPS_PROXY`/`https_proxy` (and
//! `HTTP_PROXY` for plaintext test endpoints), `ALL_PROXY`/`all_proxy`,
//! `NO_PROXY`/`no_proxy` (domains with subdomains, IPs, CIDRs, `*`), then the macOS
//! and Windows system settings. The target's name is resolved by the proxy.

use std::future::Future;
use std::io;
use std::path::Path;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use http::{HeaderValue, Uri};
use hyper_util::client::proxy::matcher::Matcher;
use hyper_util::rt::TokioIo;
use rustls_pki_types::ServerName;
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};
use tokio::net::TcpStream;

/// Largest proxy response head accepted.
const MAX_RESPONSE_HEAD: usize = 16 * 1024;

/// The proxy to use for one target.
#[derive(Clone, Debug, PartialEq)]
pub struct ProxyRoute {
    /// `http://host:port` or `https://host:port` (user info removed).
    pub proxy: Uri,
    /// `Basic …` from the proxy URL's user info.
    pub authorization: Option<HeaderValue>,
}

/// The proxy for `target` (`https://…` or `http://…`) from the environment and the
/// system settings, if one applies.
pub fn proxy_for(target: &Uri) -> Option<ProxyRoute> {
    route(&Matcher::from_system(), target)
}

fn route(matcher: &Matcher, target: &Uri) -> Option<ProxyRoute> {
    matcher.intercept(target).map(|i| ProxyRoute {
        proxy: i.uri().clone(),
        authorization: i.basic_auth().cloned(),
    })
}

/// A byte stream the tunnel and TLS layers run over.
pub trait Io: AsyncRead + AsyncWrite + Send + Unpin {}
impl<T: AsyncRead + AsyncWrite + Send + Unpin> Io for T {}

/// connect-rust's custom transport (`Http2Connection::lazy_with_connector`): a
/// `CONNECT` tunnel through [`ProxyRoute`], then TLS to the target (or h2c for
/// plaintext test endpoints).
#[derive(Clone)]
pub struct TunnelConnector {
    route: ProxyRoute,
    /// TLS to an `https://` proxy (ALPN http/1.1).
    proxy_tls: Option<Arc<rustls::ClientConfig>>,
    /// TLS to the target (ALPN h2); `None` for h2c.
    target_tls: Option<Arc<rustls::ClientConfig>>,
    target_host: String,
    target_port: u16,
}

fn strip_brackets(host: &str) -> &str {
    host.strip_prefix('[')
        .and_then(|h| h.strip_suffix(']'))
        .unwrap_or(host)
}

fn invalid(msg: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, msg.into())
}

impl TunnelConnector {
    /// `tls` is the client configuration for TLS targets (`None`: plaintext target);
    /// its ALPN is narrowed to `h2` (and `http/1.1` towards an `https://` proxy).
    /// For a plaintext target, `ca_file` still applies to TLS with an HTTPS proxy.
    pub fn new(
        route: ProxyRoute,
        target: &Uri,
        tls: Option<rustls::ClientConfig>,
        ca_file: Option<&Path>,
    ) -> io::Result<TunnelConnector> {
        match route.proxy.scheme_str() {
            Some("http") | Some("https") => {}
            other => {
                return Err(invalid(format!(
                    "unsupported proxy scheme {:?} for gRPC (use an http:// or https:// proxy)",
                    other.unwrap_or("")
                )));
            }
        }
        let host = target
            .host()
            .ok_or_else(|| invalid("endpoint without a host"))?;
        let default_port = if tls.is_some() { 443 } else { 80 };
        let alpn = |mut cfg: rustls::ClientConfig, proto: &[u8]| {
            cfg.alpn_protocols = vec![proto.to_vec()];
            Arc::new(cfg)
        };
        let proxy_tls = match (route.proxy.scheme_str(), &tls) {
            (Some("https"), Some(cfg)) => Some(alpn(cfg.clone(), b"http/1.1")),
            (Some("https"), None) => Some(alpn(
                crate::tls::client_config(ca_file).map_err(io::Error::other)?,
                b"http/1.1",
            )),
            _ => None,
        };
        Ok(TunnelConnector {
            proxy_tls,
            target_tls: tls.map(|cfg| alpn(cfg, b"h2")),
            target_host: strip_brackets(host).to_string(),
            target_port: target.port_u16().unwrap_or(default_port),
            route,
        })
    }

    /// `host:port` for the CONNECT request (IPv6 in brackets).
    fn authority(&self) -> String {
        if self.target_host.contains(':') {
            format!("[{}]:{}", self.target_host, self.target_port)
        } else {
            format!("{}:{}", self.target_host, self.target_port)
        }
    }

    async fn connect(&self) -> io::Result<Box<dyn Io>> {
        let proxy = &self.route.proxy;
        let proxy_host = strip_brackets(
            proxy
                .host()
                .ok_or_else(|| invalid("proxy URL without a host"))?,
        )
        .to_string();
        let proxy_port =
            proxy
                .port_u16()
                .unwrap_or(if self.proxy_tls.is_some() { 443 } else { 80 });
        let tcp = TcpStream::connect((proxy_host.as_str(), proxy_port))
            .await
            .map_err(|e| io::Error::new(e.kind(), format!("connecting to proxy {proxy}: {e}")))?;
        tcp.set_nodelay(true)?;
        let mut stream: Box<dyn Io> = match &self.proxy_tls {
            Some(cfg) => {
                let name = ServerName::try_from(proxy_host.clone())
                    .map_err(|e| invalid(format!("proxy host {proxy_host:?}: {e}")))?;
                Box::new(
                    tokio_rustls::TlsConnector::from(cfg.clone())
                        .connect(name, tcp)
                        .await?,
                )
            }
            None => Box::new(tcp),
        };

        let authority = self.authority();
        let mut request = format!("CONNECT {authority} HTTP/1.1\r\nHost: {authority}\r\n");
        if let Some(auth) = &self.route.authorization {
            let value = auth
                .to_str()
                .map_err(|_| invalid("proxy credentials are not ASCII"))?;
            request.push_str(&format!("Proxy-Authorization: {value}\r\n"));
        }
        request.push_str("\r\n");
        stream.write_all(request.as_bytes()).await?;
        stream.flush().await?;
        read_connect_response(&mut stream, &authority, proxy).await?;

        let Some(cfg) = &self.target_tls else {
            return Ok(stream);
        };
        let name = ServerName::try_from(self.target_host.clone())
            .map_err(|e| invalid(format!("TLS server name {:?}: {e}", self.target_host)))?;
        let tls = tokio_rustls::TlsConnector::from(cfg.clone())
            .connect(name, stream)
            .await?;
        if tls.get_ref().1.alpn_protocol() != Some(b"h2") {
            return Err(io::Error::other(format!(
                "{authority} did not negotiate HTTP/2 (ALPN h2) through the proxy"
            )));
        }
        Ok(Box::new(tls))
    }
}

/// Reads the proxy's response head one byte at a time (the TLS handshake that follows
/// must not lose bytes) and accepts any 2xx status.
async fn read_connect_response(
    stream: &mut Box<dyn Io>,
    authority: &str,
    proxy: &Uri,
) -> io::Result<()> {
    let mut head = Vec::with_capacity(128);
    let mut byte = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") {
        if head.len() >= MAX_RESPONSE_HEAD {
            return Err(io::Error::other(format!(
                "proxy {proxy}: response head too large"
            )));
        }
        if stream.read(&mut byte).await? == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                format!("proxy {proxy} closed the connection during CONNECT {authority}"),
            ));
        }
        head.push(byte[0]);
    }
    let status_line = String::from_utf8_lossy(head.split(|b| *b == b'\n').next().unwrap_or(&[]))
        .trim()
        .to_string();
    let status: Option<u16> = status_line
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok());
    match status {
        Some(200..=299) if status_line.starts_with("HTTP/1.") => Ok(()),
        Some(407) => Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            format!(
                "proxy {proxy} requires authentication (HTTP 407): put the credentials in the proxy URL (http://user:password@host:port)"
            ),
        )),
        _ => Err(io::Error::new(
            io::ErrorKind::ConnectionRefused,
            format!("proxy {proxy} refused CONNECT {authority}: {status_line:?}"),
        )),
    }
}

impl tower_service::Service<Uri> for TunnelConnector {
    type Response = TokioIo<Box<dyn Io>>;
    type Error = io::Error;
    type Future = Pin<Box<dyn Future<Output = io::Result<Self::Response>> + Send>>;

    fn poll_ready(&mut self, _cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, _authority: Uri) -> Self::Future {
        let this = self.clone();
        Box::pin(async move { this.connect().await.map(TokioIo::new) })
    }
}
