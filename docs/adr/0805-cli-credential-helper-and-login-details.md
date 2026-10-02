<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0805 — Credential helper and login details

* Status: accepted (2026-10-02)

## Context
R-AUTH-5..8 leave a few behaviours open: how the helper picks a session, where GitHub Actions finds the STS, where local
state lives on Windows, and how `login` avoids minting a new Google refresh token on every run.

## Decision
* The helper selects the profile whose client-endpoint host equals the request URI's host (`CUCINA_PROFILE` overrides);
  Bazel's per-host scoping keeps other hosts' URIs away. When `ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN` are set, the GitHub path
  always wins; the STS comes from a matching profile, else `CUCINA_URL`, else `https://<host>`, via discovery; nothing is
  written.
* Renewal happens when < 5 min remain; if it fails while the cached token still has > 2.5 min, the helper returns the cached
  token (Bazel is told it expires 2 min before `exp`).
* Configuration lives in `$CUCINA_CONFIG_DIR` → `$XDG_CONFIG_HOME/cucina` → `~/.config/cucina`; on Windows the last step is
  `%APPDATA%\cucina` (the platform's roaming config directory) instead of `~/.config`.
* `login` first tries the stored refresh token for the same profile and provider and only runs the browser flow if that
  fails or `--force` is given (one refresh token per machine and profile). `--browser-command` is the scenario hook for
  `mock-oauth2-server`; `CUCINA_ALLOW_INSECURE_HTTP=1` permits plaintext test IdPs.
* Service keys are exchanged with `subject_token_type=urn:cucina:params:oauth:token-type:service-key` (ADR 0603).

## Consequences
CI needs only `CUCINA_URL` and `id-token: write`. A deployment reachable under several host names needs one profile per
name (or `CUCINA_PROFILE`).
