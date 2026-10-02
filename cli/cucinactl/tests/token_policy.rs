// SPDX-License-Identifier: FSL-1.1-ALv2

use base64::Engine as _;
use cucinactl::auth::token::{CachedToken, RENEW_BEFORE_SECS};
use cucinactl::config::AuthMethod;

// Guards: R-AUTH-6/8 — reuse at exactly 300 seconds remaining, renew below it;
// expiry uses JWT exp when present, otherwise now + expires_in. Explicit times
// close the three non-equivalent survivors of the d7be2c4 credential-policy run.
#[test]
fn cached_token_expiry_and_renewal_policy() {
    // Synthetic payload only: no signing key, real credential, clock or I/O.
    let payload = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .encode(serde_json::json!({"exp": 1800}).to_string());
    let jwt = format!("synthetic-header.{payload}.synthetic-signature");
    let cases = [
        (
            "expires_in fallback",
            "synthetic-not-a-jwt",
            Some(600),
            [(1300, true), (1301, false)],
        ),
        (
            "JWT exp overrides expires_in",
            jwt.as_str(),
            Some(600),
            [(1500, true), (1501, false)],
        ),
    ];
    for (name, access_token, expires_in, decisions) in cases {
        let token =
            CachedToken::from_access_token(access_token.into(), expires_in, 1000, AuthMethod::Oidc);
        for (now, reusable) in decisions {
            assert_eq!(
                token.valid_for(now, RENEW_BEFORE_SECS),
                reusable,
                "{name}: reuse at now={now}"
            );
        }
    }
}
