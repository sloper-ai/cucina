// SPDX-License-Identifier: FSL-1.1-ALv2

//! In-process fake of the identity side: an OIDC provider (`/idp`, authorization
//! code + PKCE S256, refresh tokens, ES256 ID tokens), the Cucina STS (discovery,
//! RFC 8693 `/token`, `/jwks.json`) and GitHub's Actions OIDC endpoint (`/gha`).
//! Keys are generated per run (nothing secret is checked in). It records what the
//! client sent so tests can assert on the protocol (PKCE, scopes, token types).

use std::collections::{HashMap, HashSet};
use std::net::SocketAddr;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use aws_lc_rs::signature::{ECDSA_P256_SHA256_FIXED_SIGNING, EcdsaKeyPair, KeyPair as _};
use axum::extract::{Form, Query, State};
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use base64::Engine as _;
use jsonwebtoken::{Algorithm, DecodingKey, EncodingKey, Header, Validation};
use serde_json::{Value, json};
use sha2::Digest as _;

pub const CLIENT_ID: &str = "cucinactl-test-client";
/// The service-account key the fake STS accepts (built at runtime; not a real key).
pub fn service_key() -> String {
    format!("cuc_sk_{}_{}", "a".repeat(16), "t".repeat(52))
}

/// An ES256 signing key with its public JWK.
pub struct EcKey {
    pub kid: String,
    pub encoding: EncodingKey,
    pub decoding: DecodingKey,
    pub jwk: Value,
}

impl EcKey {
    pub fn generate(kid: &str) -> EcKey {
        let pair = EcdsaKeyPair::generate(&ECDSA_P256_SHA256_FIXED_SIGNING).expect("keygen");
        let pkcs8 = pair.to_pkcs8v1().expect("pkcs8");
        let point = pair.public_key().as_ref().to_vec();
        assert_eq!(point.len(), 65, "uncompressed P-256 point");
        let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
        let (x, y) = (b64.encode(&point[1..33]), b64.encode(&point[33..65]));
        EcKey {
            kid: kid.into(),
            encoding: EncodingKey::from_ec_der(pkcs8.as_ref()),
            decoding: DecodingKey::from_ec_components(&x, &y).expect("decoding key"),
            jwk: json!({"kty": "EC", "crv": "P-256", "x": x, "y": y, "kid": kid, "alg": "ES256", "use": "sig"}),
        }
    }

    pub fn sign(&self, claims: &Value) -> String {
        let mut h = Header::new(Algorithm::ES256);
        h.kid = Some(self.kid.clone());
        jsonwebtoken::encode(&h, claims, &self.encoding).expect("sign")
    }
}

/// Switches for negative tests.
#[derive(Debug, Default, Clone)]
pub struct Behavior {
    /// The IdP puts a different nonce into the ID token.
    pub wrong_nonce: bool,
    /// Lifetime of minted Cucina JWTs (seconds; default 900).
    pub sts_ttl: Option<i64>,
    /// The STS refuses otherwise-valid exchanges with this RFC 6749 error.
    pub sts_error: Option<(u16, String)>,
    /// Discovery returns this HTTP status while the token endpoint stays available.
    pub discovery_error: Option<u16>,
}

/// One pending authorization code.
#[derive(Debug, Clone)]
struct Grant {
    redirect_uri: String,
    challenge: String,
    nonce: String,
}

pub struct AuthState {
    pub base: String,
    pub idp_key: EcKey,
    pub sts_key: EcKey,
    pub behavior: Mutex<Behavior>,
    grants: Mutex<HashMap<String, Grant>>,
    refresh_tokens: Mutex<HashSet<String>>,
    gha_tokens: Mutex<HashSet<String>>,
    /// Query of the last /idp/authorize request.
    pub last_authorize: Mutex<Option<HashMap<String, String>>>,
    /// subject_token_type of every validated STS exchange, in order.
    pub exchanges: Mutex<Vec<String>>,
    /// `audience` parameters seen by /gha.
    pub gha_audiences: Mutex<Vec<String>>,
    pub refreshes: AtomicUsize,
    counter: AtomicUsize,
    /// Endpoints published in the discovery document.
    pub management: Mutex<String>,
    pub remote_execution: Mutex<String>,
}

/// A running fake.
pub struct FakeAuth {
    pub state: Arc<AuthState>,
    pub addr: SocketAddr,
}

impl FakeAuth {
    pub async fn start() -> FakeAuth {
        FakeAuth::start_with(|addr| format!("http://{addr}")).await
    }

    /// Like [`FakeAuth::start`], with the public base URL (issuer, endpoints) derived
    /// from the listening address, e.g. a TLS front's `https://name:port`.
    pub async fn start_with(public_base: impl FnOnce(SocketAddr) -> String) -> FakeAuth {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind");
        let addr = listener.local_addr().unwrap();
        let base = public_base(addr);
        let state = Arc::new(AuthState {
            base: base.clone(),
            idp_key: EcKey::generate("idp-1"),
            sts_key: EcKey::generate("sts-1"),
            behavior: Mutex::new(Behavior::default()),
            grants: Mutex::new(HashMap::new()),
            refresh_tokens: Mutex::new(HashSet::new()),
            gha_tokens: Mutex::new(HashSet::new()),
            last_authorize: Mutex::new(None),
            exchanges: Mutex::new(Vec::new()),
            gha_audiences: Mutex::new(Vec::new()),
            refreshes: AtomicUsize::new(0),
            counter: AtomicUsize::new(0),
            management: Mutex::new("http://127.0.0.1:1".into()),
            remote_execution: Mutex::new("grpc://127.0.0.1:1".into()),
        });
        let app = Router::new()
            .route("/.well-known/cucina-configuration", get(discovery))
            .route("/token", post(sts_token))
            .route("/jwks.json", get(sts_jwks))
            .route("/idp/.well-known/openid-configuration", get(idp_discovery))
            .route("/idp/jwks", get(idp_jwks))
            .route("/idp/authorize", get(idp_authorize))
            .route("/idp/token", post(idp_token))
            .route("/gha/token", get(gha_token))
            .with_state(state.clone());
        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        FakeAuth { state, addr }
    }

    pub fn url(&self) -> String {
        self.state.base.clone()
    }

    pub fn exchange_count(&self) -> usize {
        self.state.exchanges.lock().unwrap().len()
    }

    /// Issues a refresh token the IdP will accept (seeding a logged-in machine).
    pub fn issue_refresh_token(&self) -> String {
        let rt = format!("rt-{}", self.state.counter.fetch_add(1, Ordering::Relaxed));
        self.state.refresh_tokens.lock().unwrap().insert(rt.clone());
        rt
    }
}

fn next(state: &AuthState, prefix: &str) -> String {
    format!("{prefix}-{}", state.counter.fetch_add(1, Ordering::Relaxed))
}

fn oauth_error(status: u16, error: &str, description: &str) -> Response {
    (
        StatusCode::from_u16(status).unwrap(),
        Json(json!({"error": error, "error_description": description})),
    )
        .into_response()
}

async fn discovery(State(s): State<Arc<AuthState>>) -> Response {
    if let Some(status) = s.behavior.lock().unwrap().discovery_error {
        return StatusCode::from_u16(status).unwrap().into_response();
    }
    Json(json!({
        "version": 1,
        "issuer": s.base,
        "token_endpoint": format!("{}/token", s.base),
        "jwks_uri": format!("{}/jwks.json", s.base),
        "endpoints": {
            "remote_execution": s.remote_execution.lock().unwrap().clone(),
            "instance_name": "main",
            "management": s.management.lock().unwrap().clone(),
        },
        "identity_providers": [{
            "name": "mock",
            "type": "oidc",
            "issuer": format!("{}/idp", s.base),
            "client_id": CLIENT_ID,
            "scopes": ["openid", "email", "profile"],
            "redirect_ports": [],
        }],
    }))
    .into_response()
}

async fn idp_discovery(State(s): State<Arc<AuthState>>) -> Json<Value> {
    let idp = format!("{}/idp", s.base);
    Json(json!({
        "issuer": idp,
        "authorization_endpoint": format!("{idp}/authorize"),
        "token_endpoint": format!("{idp}/token"),
        "jwks_uri": format!("{idp}/jwks"),
        "response_types_supported": ["code"],
        "subject_types_supported": ["public"],
        "id_token_signing_alg_values_supported": ["ES256"],
        "scopes_supported": ["openid", "email", "profile", "offline_access"],
        "token_endpoint_auth_methods_supported": ["none", "client_secret_post", "client_secret_basic"],
        "code_challenge_methods_supported": ["S256"],
    }))
}

async fn idp_jwks(State(s): State<Arc<AuthState>>) -> Json<Value> {
    Json(json!({"keys": [s.idp_key.jwk.clone()]}))
}

async fn sts_jwks(State(s): State<Arc<AuthState>>) -> Json<Value> {
    Json(json!({"keys": [s.sts_key.jwk.clone()]}))
}

/// Non-interactive authorization endpoint: validates the request and redirects
/// straight back to the loopback redirect URI.
async fn idp_authorize(
    State(s): State<Arc<AuthState>>,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    *s.last_authorize.lock().unwrap() = Some(q.clone());
    let get = |k: &str| q.get(k).cloned().unwrap_or_default();
    if get("response_type") != "code"
        || get("client_id") != CLIENT_ID
        || get("code_challenge_method") != "S256"
        || get("code_challenge").is_empty()
        || !get("redirect_uri").starts_with("http://127.0.0.1:")
        || get("state").is_empty()
        || get("nonce").is_empty()
    {
        return (StatusCode::BAD_REQUEST, "invalid authorization request").into_response();
    }
    let code = next(&s, "code");
    s.grants.lock().unwrap().insert(
        code.clone(),
        Grant {
            redirect_uri: get("redirect_uri"),
            challenge: get("code_challenge"),
            nonce: get("nonce"),
        },
    );
    let mut location = openidconnect::url::Url::parse(&get("redirect_uri")).unwrap();
    location
        .query_pairs_mut()
        .append_pair("code", &code)
        .append_pair("state", &get("state"));
    (
        StatusCode::FOUND,
        [(header::LOCATION, location.to_string())],
    )
        .into_response()
}

fn id_token(s: &AuthState, nonce: Option<&str>) -> String {
    let now = cucinactl::util::now_unix();
    let mut claims = json!({
        "iss": format!("{}/idp", s.base),
        "aud": CLIENT_ID,
        "sub": "user-1",
        "email": "dev@example.com",
        "email_verified": true,
        "iat": now,
        "exp": now + 3600,
    });
    if let Some(n) = nonce {
        claims["nonce"] = json!(n);
    }
    s.idp_key.sign(&claims)
}

async fn idp_token(
    State(s): State<Arc<AuthState>>,
    Form(f): Form<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| f.get(k).cloned().unwrap_or_default();
    match get("grant_type").as_str() {
        "authorization_code" => {
            let Some(grant) = s.grants.lock().unwrap().remove(&get("code")) else {
                return oauth_error(400, "invalid_grant", "unknown or reused code");
            };
            // PKCE S256: BASE64URL(SHA256(code_verifier)) == code_challenge.
            let verifier = get("code_verifier");
            let computed = base64::engine::general_purpose::URL_SAFE_NO_PAD
                .encode(sha2::Sha256::digest(verifier.as_bytes()));
            if verifier.len() < 43 || computed != grant.challenge {
                return oauth_error(400, "invalid_grant", "PKCE verification failed");
            }
            if get("redirect_uri") != grant.redirect_uri || get("client_id") != CLIENT_ID {
                return oauth_error(400, "invalid_grant", "redirect_uri/client_id mismatch");
            }
            let nonce = if s.behavior.lock().unwrap().wrong_nonce {
                "not-the-nonce".to_string()
            } else {
                grant.nonce
            };
            let rt = next(&s, "rt");
            s.refresh_tokens.lock().unwrap().insert(rt.clone());
            Json(json!({
                "access_token": next(&s, "at"),
                "token_type": "Bearer",
                "expires_in": 3600,
                "refresh_token": rt,
                "id_token": id_token(&s, Some(&nonce)),
            }))
            .into_response()
        }
        "refresh_token" => {
            if !s
                .refresh_tokens
                .lock()
                .unwrap()
                .contains(&get("refresh_token"))
            {
                return oauth_error(400, "invalid_grant", "refresh token revoked");
            }
            s.refreshes.fetch_add(1, Ordering::Relaxed);
            Json(json!({
                "access_token": next(&s, "at"),
                "token_type": "Bearer",
                "expires_in": 3600,
                "id_token": id_token(&s, None),
            }))
            .into_response()
        }
        _ => oauth_error(400, "unsupported_grant_type", "unsupported grant"),
    }
}

async fn sts_token(
    State(s): State<Arc<AuthState>>,
    Form(f): Form<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| f.get(k).cloned().unwrap_or_default();
    let behavior = s.behavior.lock().unwrap().clone();
    if get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
        return oauth_error(400, "invalid_request", "grant_type");
    }
    let token_type = get("subject_token_type");
    let subject = get("subject_token");
    let sub = match token_type.as_str() {
        "urn:ietf:params:oauth:token-type:id_token" => {
            let mut v = Validation::new(Algorithm::ES256);
            v.set_audience(&[CLIENT_ID]);
            v.set_issuer(&[format!("{}/idp", s.base)]);
            match jsonwebtoken::decode::<Value>(&subject, &s.idp_key.decoding, &v) {
                Ok(d) => format!("mock:{}", d.claims["sub"].as_str().unwrap_or_default()),
                Err(_) => return oauth_error(400, "invalid_grant", "subject token rejected"),
            }
        }
        "urn:ietf:params:oauth:token-type:jwt" => {
            if !s.gha_tokens.lock().unwrap().contains(&subject) {
                return oauth_error(400, "invalid_grant", "unknown GitHub token");
            }
            "github:1401027334:ci".to_string()
        }
        "urn:cucina:params:oauth:token-type:service-key" => {
            if subject != service_key() {
                return oauth_error(400, "invalid_grant", "unknown service key");
            }
            "sa:ci-bot".to_string()
        }
        _ => return oauth_error(400, "invalid_request", "subject_token_type"),
    };
    // Record only validated exchanges, without retaining the supplied credential.
    s.exchanges.lock().unwrap().push(token_type);
    if let Some((status, error)) = behavior.sts_error {
        return oauth_error(status, &error, "refused by test");
    }
    let now = cucinactl::util::now_unix();
    let ttl = behavior.sts_ttl.unwrap_or(900);
    let jwt = s.sts_key.sign(&json!({
        "iss": s.base,
        "aud": "buildbarn",
        "sub": sub,
        "sid": next(&s, "sid"),
        "jti": next(&s, "jti"),
        "iat": now,
        "exp": now + ttl,
        "cucina": {"cas_read": ["main"], "ac_read": ["main"], "execute": ["main"], "admin": []},
    }));
    Json(json!({
        "access_token": jwt,
        "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
        "token_type": "Bearer",
        "expires_in": ttl,
    }))
    .into_response()
}

async fn gha_token(
    State(s): State<Arc<AuthState>>,
    headers: HeaderMap,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    if headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        != Some("Bearer gha-request-token")
    {
        return (StatusCode::UNAUTHORIZED, "bad request token").into_response();
    }
    s.gha_audiences
        .lock()
        .unwrap()
        .push(q.get("audience").cloned().unwrap_or_default());
    let token = next(&s, "gha-oidc");
    s.gha_tokens.lock().unwrap().insert(token.clone());
    Json(json!({"value": token, "count": 1})).into_response()
}
