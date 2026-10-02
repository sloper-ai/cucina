<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# `cucinactl` — the Cucina CLI

`cucinactl` is the management interface of Cucina (there is no web UI): fleet state
(pools, workers, Mac hosts, queues, operations), action inspection, service-account
keys and revocation, cost, images, support bundles, `.bazelrc` generation, and the
Bazel credential helper. `cucinactl tui` (or `cucinactl` with no arguments on a
terminal) opens the interactive TUI.

* Talks only to the controller's **management API** (gRPC over TLS, `ManagementService`
  in `api/proto/cucina/v1/management.proto`) and, for `action inspect`, to the REAPI
  **client endpoint** with your own token. It never needs kubeconfig.
* Authenticates with a 15-minute **Cucina JWT** from the Cucina STS (RFC 8693), obtained
  with `cucinactl login` and renewed automatically.
* Built from `cli/` (Cargo workspace members `cli/cucinactl`, `cli/cucina-api`,
  `cli/xtask`); release binaries come from Bazel. The RPC stack is connect-rust +
  buffa speaking the gRPC protocol (ADR 0003); TLS is rustls with aws-lc-rs.

Contents: [Install](#install) · [Configuration](#configuration-and-profiles) ·
[Login](#login) · [Credential helper](#bazel-credential-helper) ·
[`bazelrc`](#bazelrc) · [Targets schema](#targets-schema) · [Exit codes](#exit-codes) ·
[JSON output](#json-output-and-schemas) · [Action inspection](#action-inspection) ·
[Library API](#library-api-tui) · [Generated code](#generated-code) ·
[Testing hooks](#testing-hooks) · [Command reference](#command-reference)

## Install

Release archives contain `cucinactl` and `cucina-credential-helper` (a hardlink or
copy of the same binary; it dispatches on its own name). If you only have
`cucinactl`, create the helper next to it (or in any directory):

```sh
cucinactl credential-helper install            # next to cucinactl
cucinactl credential-helper install --dir tools  # e.g. %workspace%/tools
```

Shell completions are static scripts: `cucinactl completions bash|zsh|fish|powershell|elvish`.

## Configuration and profiles

Configuration lives in `$CUCINA_CONFIG_DIR`, else `$XDG_CONFIG_HOME/cucina`, else
`~/.config/cucina` (Windows: `%APPDATA%\cucina`). The directory is created `0700`;
every file in it is `0600`:

| Path | Content |
| --- | --- |
| `config.toml` | profiles (one per Cucina deployment) and the current profile |
| `tokens/<profile>.json` | the cached Cucina JWT (replaced atomically) |
| `tokens/<profile>.lock` | advisory lock serializing renewals |
| `discovery/<profile>.json` | the deployment's `/.well-known/cucina-configuration` |
| `secrets.json` | only with the opt-in file credential store |

`login` creates or updates a profile and makes it current. Select another with
`--profile/-p` (or `CUCINA_PROFILE`), switch the default with `cucinactl config use
<name>`, inspect with `cucinactl config view`, edit fields with `cucinactl config set`.

Long-lived secrets (the identity provider's refresh token, or a service-account key)
are stored in the **OS keychain** (macOS Keychain, Windows Credential Manager, Linux
Secret Service) under the service `ai.sloper.cucina`. Hosts without a keychain (headless
Linux) can either opt in to a 0600 file (`login --credential-store file`, or
`CUCINA_CREDENTIAL_STORE=file`) or provide the secret through the environment
(`CUCINA_REFRESH_TOKEN`, `CUCINA_SERVICE_KEY`; read-only, never written). Windows caps a
credential at 2,560 bytes, which is why only the refresh token (never the JWT) goes there.

## Login

```sh
cucinactl login https://cucina.example.com
```

1. Fetches `https://cucina.example.com/.well-known/cucina-configuration` (endpoints,
   instance name, identity providers with client IDs and scopes).
2. Runs OAuth 2.0 **authorization code + PKCE (S256)** with a **loopback redirect**
   (RFC 8252): a one-shot listener on `127.0.0.1` (never `localhost`, never all
   interfaces), random `state` and `nonce`, `scope=openid email profile` (plus
   `offline_access` for non-Google providers), optional `hd` hint (`--hd`). The URL is
   always printed; a browser is opened unless `--no-browser`/`--manual` (`webbrowser`,
   hardened; on macOS `open -u`, ADR 0806).
3. Verifies the ID token (signature, `iss`, `aud`, `nonce`, `exp`) and exchanges it at
   the STS for a Cucina JWT (`subject_token_type=…:id_token`).
4. Stores the refresh token in the keychain and the JWT in the token cache.

Running `login` again reuses this machine's refresh token (Google caps refresh tokens
per account and client); `--force` runs the browser flow again. `logout` deletes local
state only (the identity provider's grant is untouched); `--all` logs out of every
profile, `--forget` also deletes the profile.

| Situation | Command |
| --- | --- |
| Desktop | `cucinactl login https://cucina.example.com` |
| SSH session, port forward | on your laptop: `ssh -L 127.0.0.1:8765:127.0.0.1:8765 host`; on the host: `cucinactl login https://cucina.example.com --port 8765 --no-browser`, then open the printed URL on the laptop |
| SSH session, no forward | `cucinactl login https://cucina.example.com --manual`: open the URL anywhere, sign in, copy the final `http://127.0.0.1:<port>/?state=…&code=…` address from the browser (the page itself fails to load) and paste it; `state` is checked |
| Provider needs registered ports (Okta, some Entra setups) | publish `redirect_ports` in the TrustPolicy login spec (tried in order) or pass `--port` |
| Provider registers a path | `--redirect-path /callback` |
| Several providers offered | `--provider <name>` |
| Private CA | `--ca-file ca.pem` or `CUCINA_CA_FILE=ca.pem` (stored in the profile; also emitted as `--tls_certificate`); see [Private CA and proxies](#private-ca-and-proxies) |
| Behind an HTTP proxy | `HTTPS_PROXY=http://[user:password@]proxy:3128` (`NO_PROXY` honoured) |
| Service account / break-glass admin | `cucinactl login https://cucina.example.com --key key.txt` (`--key -` reads stdin, `--key-env VAR` an environment variable) |

The break-glass admin key generated by `helm install` is retrieved with `kubectl` as
described in the chart's `NOTES.txt`; use it with `login --key` and rotate or disable it
once OIDC admins exist. Service keys are exchanged at the STS with
`subject_token_type=urn:cucina:params:oauth:token-type:service-key` (ADR 0603) and renewed by
exchanging the stored key again.

Provider notes: Google "Desktop app" clients publish their (non-confidential) client
secret in the discovery document and accept any loopback port; Microsoft Entra needs
`127.0.0.1` redirect URIs in the app manifest (no `[::1]`); Keycloak public clients
register `http://127.0.0.1/*`; Dex accepts any loopback port; Okta needs fixed ports.

### Private CA and proxies

Every connection — discovery, the STS, identity providers, the management API, the REAPI
client endpoint — trusts the OS store plus these PEM bundles (all of them, if several are
set):

| Source | Scope |
| --- | --- |
| `login --ca-file <pem>` / `config set ca-file <pem>` | stored (absolute) in the profile; used by every command, the credential helper and `bazelrc` (`--tls_certificate`) |
| `CUCINA_CA_FILE=<pem>` | this process; `login` stores it in the profile when `--ca-file` is absent; `bazelrc` falls back to it |
| `SSL_CERT_FILE=<pem>` | this process only (never stored or emitted) |

Proxies follow curl's variables, the same way for HTTPS and gRPC: `HTTPS_PROXY`/`https_proxy`
(then `ALL_PROXY`), `NO_PROXY`/`no_proxy` (domains with their subdomains, IP addresses,
CIDR ranges, `*`), then the macOS/Windows system proxy settings. The gRPC channels open a
`CONNECT` tunnel (to an `http://` or `https://` proxy; credentials in the proxy URL become
Basic `Proxy-Authorization`) and run TLS to Cucina inside it, so the proxy resolves
Cucina's name. SOCKS proxies are not supported for gRPC. These settings cover cucinactl
and the credential helper; Bazel's own connections to Cucina are configured in Bazel.

## Bazel credential helper

Bazel runs the helper as `<path> get`, writes `{"uri": "https://<host>/<service>"}` to
its stdin once per gRPC service (possibly concurrently), and reads exactly

```json
{"headers":{"Authorization":["Bearer <jwt>"]},"expires":"2026-10-02T10:13:00Z"}
```

`expires` is the JWT's `exp` minus 2 minutes, in whole-second RFC 3339. The helper
renews the token when fewer than 5 minutes remain (IdP refresh → fresh ID token → STS
exchange, or the service key again), serialized by a file lock so concurrent helpers
renew once. With a valid token it does no network I/O, starts no async runtime and does
not touch the keychain (~10 ms). It **never prompts**: without a valid session it writes
"run `cucinactl login`" to stderr and exits 3.

Wire it **per host** (an unscoped helper would also receive BES and download URIs):

```text
build --credential_helper=cucina.example.com=%workspace%/tools/cucina-credential-helper
```

`cucinactl bazelrc` emits this line (with the installed helper's absolute path, or
`--helper-path`). Never also set `Authorization` with `--remote_header`. Principals
without `ac-write` must set `--remote_upload_local_results=false` (`bazelrc --read-only`):
Bazel refreshes credentials once on `UNAUTHENTICATED`/`PERMISSION_DENIED`, and a denied
upload would otherwise loop.

**GitHub Actions** (no login, nothing stored): give the job `permissions: {id-token:
write, contents: read}` and set `CUCINA_URL` to the Cucina URL (and `CUCINA_CA_FILE` to a PEM bundle for a private CA). When
`ACTIONS_ID_TOKEN_REQUEST_URL`/`ACTIONS_ID_TOKEN_REQUEST_TOKEN` are present, the helper
fetches a GitHub OIDC token for `audience=cucina` on every renewal and exchanges it
(`subject_token_type=…:jwt`).

```yaml
permissions: {id-token: write, contents: read}
env:
  CUCINA_URL: ${{ vars.CUCINA_ENDPOINT }}
steps:
  - run: cucinactl bazelrc --platform linux --ci --helper-path "$(command -v cucina-credential-helper)" >> user.bazelrc
```

## `bazelrc`

`cucinactl bazelrc --platform linux|windows|macos` prints ready-to-use lines for the
current profile: the endpoint (`--remote_executor`, `--remote_cache`), TLS
(`--tls_certificate` for a private CA), the host-scoped credential helper,
`--remote_instance_name`, the platform flags (`--extra_execution_platforms`,
`--host_platform` **and** `--platforms`, which Bazel 9's default test toolchain needs),
and the transfer/resilience defaults:

```text
startup --experimental_remote_repo_contents_cache
build --remote_cache_compression
build --remote_download_outputs=toplevel        # minimal with --ci
build --disk_cache=… --experimental_disk_cache_gc_max_size=50G
build --experimental_remote_cache_eviction_retries=5
build --rewind_lost_inputs
build --remote_build_event_upload=minimal
build --nolegacy_important_outputs
build --jobs=200 --remote_retries=10 --remote_retry_max_delay=30s
build --grpc_keepalive_time=30s --noremote_local_fallback
```

Lines are `build` (or `build:<name>` with `--config-name`), never `common`: Bazel expands a
config's `common:` lines before its `build:` lines, so `common:` lines would lose against
`build:` lines of the same config elsewhere — e.g. the Cucina repository's
`build:cucina --remote_instance_name=main` or `bazel/platforms/cucina.bazelrc` (ADR 0807).
`bazel query` and `bazel mod` do not read `build` lines.

Never emitted: `--experimental_remote_cache_chunking` (Buildbarn returns Unimplemented),
`--experimental_remote_merkle_tree_cache` (removed in Bazel 9), `--remote_header`. Every
emitted flag is checked against Bazel 9.2's flag list
(`cli/cucinactl/testdata/bazel-9.2-flags.txt`).

| Option | Effect |
| --- | --- |
| `--exec-pool <pool>` | execute on another pool of that OS (e.g. `linux-aarch64`) |
| `--xcode 27.0` | macOS: the pool serving that Xcode |
| `--ci` | `--remote_download_outputs=minimal` |
| `--read-only` | `--remote_upload_local_results=false` |
| `--cache-only` | remote cache, local execution (no executor/platform flags) |
| `--config-name cucina` | emit `build:cucina …` lines, enabled with `--config=cucina` |
| `--disk-cache DIR` / `none` | local disk cache location (default: the user cache dir) |
| `--helper-path PATH` | credential helper path (default: the installed helper) |
| `--platforms-package //tools/cucina` | use workspace platforms printed by `--emit-build-file` instead of `@cucina_platforms` |
| `--emit-build-file` | print `platform()` targets whose `exec_properties` match every pool runner exactly |

Startup options cannot be scoped to a config; put the output in `user.bazelrc` (or
`.bazelrc`). Platform labels come from the `@cucina_platforms` Bzlmod module
(`bazel/platforms`); without it, use `--emit-build-file` and `--platforms-package`.

**Cross configurations** (R-XPLAT-2(d), [cross-compilation.md](cross-compilation.md)):
`cucinactl bazelrc --cross --target <P> [--exec-pool <pool>] [--client-os <os>]` emits,
from `platforms/targets.json`:

| Lines | When |
| --- | --- |
| `--platforms=<hermetic-llvm platform>`, `--extra_execution_platforms=<compile exec platforms, chosen pool first>,<test exec platform>`, `--host_platform=<chosen compile exec platform>`, `--experimental_platform_in_output_dir` | always (build-only targets have no test exec platform) |
| the compile exec platforms' `flags`, e.g. `--@cucina_platforms//apple:sdk_version=27.0` (the chosen pool's win on a name clash) | when a listed exec platform has flags |
| `--repo_env=BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1`, `--repo_env=BAZEL_WINDOWS_SDK_EULA=1` | MSVC targets (`abi: msvc`) |
| `'--test_env=SYSTEMROOT=C:\Windows'`, `'--test_env=PATH=C:\Windows\System32;…'` | Windows targets, from every client OS (identical test actions) |
| `--test_timeout=600,3000,9000,36000` (Bazel's 60/300/900/3600 s × `testTimeoutScale`) | emulated (qemu-user) test runners |
| `--action_env=PATH=/bin:/usr/bin:/usr/local/bin`, `--host_action_env=…`, `--enable_runfiles`, `startup --windows_enable_symlinks` | Windows clients (cache sharing with Linux/macOS clients) |

Linux and macOS clients targeting Windows also get a note (a comment, and on stderr): run
`tools/xplat/windows-test-overlay.sh` from the Cucina repository in the workspace and add
the `--override_repository=bazel_tools=…` line it prints to `user.bazelrc` — Bazel 9.2
accepts that `@bazel_tools` overlay, no patched Bazel is needed (ADR 0904). Excluded
targets (macOS x86_64, Windows arm64) are refused with their reason (exit 2), unknown ones
with exit 5; macOS targets compile only on macOS pools. `--cache-only` cannot be combined
with `--cross`. `cucinactl bazelrc --list-targets` lists every target with its compile
pools, test runner, timeout scale and, for excluded ones, the reason.

## Targets schema

`bazelrc --cross` and `--list-targets` embed `platforms/targets.json` and
`platforms/pools.json` at build time. The schema is documented in
[cross-compilation.md](cross-compilation.md#schema) (ADR 0900); cucinactl reads
`excludedPlatforms`, `execPlatforms[]` (`name`, `label`, `pool`, `runner`, `os`, `cpu`,
`flags`) and `targets[]` (`name`, `aliases`, `platform`, `os`, `cpu`, `abi`,
`execPlatforms`, `test`, `testTimeoutScale`, `excluded`, `reason`), ignores unknown fields
and validates on load: referenced pools and runners exist, macOS targets list only macOS
exec platforms, excluded rows have no placement, no exec platform or supported target uses
an excluded OS/CPU pair, flags are single `--` tokens. `--target` matches `name`, an alias,
the full platform label or its target name (`windows_x86_64_msvc`).
`cli/cucinactl/data/targets.json` is a minimal schema-1 document (first-version fields plus
unknown ones) that the unit tests parse to keep older and newer catalogs readable.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | any other error (server `INTERNAL`/`UNKNOWN`/`UNIMPLEMENTED`, I/O, unexpected response) |
| 2 | usage error (bad flags or arguments, `INVALID_ARGUMENT`, refused confirmation, destructive command without `--yes` off a terminal) |
| 3 | authentication required (no profile or session, expired/revoked session, `UNAUTHENTICATED`, STS `invalid_grant`, `state`/`nonce` mismatch) |
| 4 | permission denied (`PERMISSION_DENIED`, STS `access_denied`) |
| 5 | not found (`NOT_FOUND`, unknown pool/host/operation/blob) |
| 6 | unavailable (cannot connect, `UNAVAILABLE`, `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED`) |
| 7 | conflict / failed precondition (`ALREADY_EXISTS`, `FAILED_PRECONDITION`, `ABORTED`) |

Destructive commands (`pools cordon|gc`, `workers drain`, `hosts drain|re-image|remove`,
`hosts enroll-token revoke`, `ops kill`, `keys revoke`) ask for confirmation on a
terminal and require `--yes` otherwise.

## JSON output and schemas

`--output json` (or `CUCINA_OUTPUT=json`) prints one JSON document; streams (`ops
watch`, `workers logs`) print JSON lines. There is no YAML. Every document has a
`schema` field naming its JSON Schema in `cli/cucinactl/schemas/` (contract-tested
against real command output). Conventions: snake_case fields, timestamps as RFC 3339
UTC strings or `null`, durations as seconds (numbers), money as integer micro-dollars
(`*_usd_micros`). Errors go to stderr; with `--output json` as an `error.v1` document.

| Schema | Commands |
| --- | --- |
| `status.v1` | `status` |
| `pool-list.v1`, `pool-describe.v1`, `pool-floor.v1`, `pool-gc.v1` | `pools list`, `pools describe`, `pools scale-floor`, `pools gc` |
| `worker-list.v1`, `log.v1` | `workers list`, `workers logs` |
| `host-list.v1`, `host-register.v1`, `enroll-token.v1`, `enroll-token-list.v1` | `hosts list`, `hosts register`, `hosts enroll-token create`, `hosts enroll-token list` |
| `queue-list.v1` | `queues` |
| `operation-list.v1`, `operation-event.v1` | `ops list`, `ops watch` (JSON lines; `kind` is `added`, `changed`, `removed` or `reconnecting`) |
| `action.v1` | `action inspect` |
| `login.v1`, `whoami.v1` | `login`, `whoami` |
| `service-key.v1`, `service-key-list.v1`, `revocation.v1`, `revocation-list.v1` | `keys create`, `keys list`, `keys revoke --sub/--sid`, `keys revocations` |
| `cost.v1`, `image-list.v1` | `cost`, `images` |
| `file.v1` | `diag`, `hosts diag` |
| `bazelrc.v1`, `target-list.v1` | `bazelrc`, `bazelrc --list-targets` |
| `config.v1`, `path.v1` | `config view`/`profiles`, `config path` |
| `result.v1` | other mutating commands (drain, cordon, approve, revoke key, …) |
| `error.v1` | errors (stderr) |

## Action inspection

`cucinactl action inspect <subject>` shows the command, environment, platform, input
tree, requested and produced outputs, exit code, stdout/stderr, timing and worker. It
reads the CAS/AC **through the client endpoint with your own token**: blobs via
ByteStream (`compressed-blobs/zstd/…` when the server advertises ZSTD, verified by size
and SHA-256), the input tree by walking `Directory` blobs. The subject may be:

* an action digest, `<hash>/<size>` or `<hash>-<size>`;
* the link Buildbarn prints in Bazel's output for a cached result
  (`…/<instance>/blobs/sha256/action/<hash>-<size>/`) or an uncached (e.g. failed) one
  (`…/blobs/sha256/historical_execute_response/<hash>-<size>/`);
* an operation name (resolved with the management API).

Failed actions are not stored in the AC. For an operation the scheduler still remembers,
`GetOperation` returns its serialized `ExecuteResponse` (exit code, stdout/stderr digests,
timing, status) and `result.source` is `execute-response` (`action-cache` when the result
was a cache hit); otherwise the result comes from the AC. Later, use the
`historical_execute_response` link (`historical-execute-response`).

## Library API (TUI)

`cucinactl::client` is the reusable client for the TUI (`cli/cucinactl/src/tui/`):
`Session::open(profile, timeout)` returns a `ManagementClient` (one method per RPC, with
deadlines), a `TokenHandle` shared by every call, and `reapi(instance)` for CAS/AC reads;
`spawn_refresher` keeps the 15-minute JWT fresh for long sessions;
`watch_overview`/`watch_operations` return streams of `WatchItem::{Data, Reconnecting}`
that reconnect with jittered exponential backoff, end on permanent errors, and stop when
dropped. `cucinactl::inspect::inspect` is the action inspector.

## Generated code

`cli/cucina-api/src/gen/{buffa,connect}/` holds the generated protobuf messages (buffa)
and RPC stubs (connect-rust) for `api/proto/cucina/v1/*.proto` and the vendored protos in
`cli/cucina-api/proto/`: REAPI v2 and `build/bazel/semver` from `bazelbuild/remote-apis`
at `becdd8f9ff811df88a22d3eadd6341753d51d167` (the commit bb-storage pins),
`google/{bytestream,longrunning,rpc,api}` from `googleapis/googleapis` at
`ef19b7b7a73f19f33ab86c5b3603e9590025acd7` (the BCR `googleapis` module bb-storage uses),
and `buildbarn/cas/cas.proto` (`HistoricalExecuteResponse`) from
`buildbarn/bb-remote-execution` at `1a3be95748727e2f03660925916ab39ee6c79d17`; their
licences are next to them (`LICENSE-*`, Apache-2.0). Regenerate with:

```sh
cargo xtask codegen          # buf generate + protoc-gen-buffa / -buffa-packaging / -connect-rust
cargo xtask codegen --check  # CI: fails if the checked-in code is stale
```

`buf` comes from `$BUF`, else mise (`buf@1.73.0`), else `PATH`. The plugins come from
`$PROTOC_GEN_BUFFA`, `$PROTOC_GEN_BUFFA_PACKAGING` and `$PROTOC_GEN_CONNECT_RUST`, else
xtask fetches the pinned release binaries once with `curl` and checks their SHA-256 —
protoc-gen-buffa and protoc-gen-buffa-packaging 0.9.2, protoc-gen-connect-rust 0.9.1,
the same binaries Bazel uses (`tools/pinned.bzl`, ADR 0106). Options: buffa
`views=true,json=true,exclude_package=.google.api`; packaging `exclude_package=.google.api`
(plus `filter=services` for the connect tree); connect `buffa_module=crate::proto`; every
plugin with `strategy: all`. The `codegen_drift` test (no tools needed) fails when a
`.proto` changed without regenerating. `cargo xtask docs` regenerates the command
reference below.

## Testing hooks

* `--browser-command "<program> [args]"` (or `CUCINA_BROWSER_COMMAND`) runs a program
  with the authorization URL instead of the browser — e.g. `curl -fsSL -o /dev/null`
  against `navikt/mock-oauth2-server` (non-interactive login) in the acceptance campaign.
* `CUCINA_ALLOW_INSECURE_HTTP=1` permits `http://` identity providers and plaintext gRPC
  to non-loopback hosts (test environments only; loopback is always allowed).
* `CUCINA_CONFIG_DIR` isolates all local state.

## Command reference

Generated from the clap definitions by `cargo xtask docs`; do not edit by hand.

<!-- BEGIN GENERATED: cargo xtask docs -->
### `cucinactl`

Manage a Cucina deployment (a managed Buildbarn for Bazel remote execution).

Run without arguments on a terminal to open the TUI. Exit codes: 0 ok, 1 error, 2 usage, 3 auth required, 4 permission denied, 5 not found, 6 unavailable, 7 conflict/precondition.

```text
Usage: cucinactl [OPTIONS] [COMMAND]
```

| Argument | Description |
| --- | --- |
| `-p`, `--profile` | Cluster profile (default: the current profile, see `config use`) |
| `-o`, `--output` | Output format: human tables or JSON for scripts (default: `table`) [values: table, json] |
| `-y`, `--yes` | Answer "yes" to confirmations of destructive commands (required without a TTY) |
| `--timeout` | Deadline for each API call (e.g. 30s, 2m) (default: `30s`) |
| `--color` | When to use colors (NO_COLOR and non-terminals disable them under `auto`) (default: `auto`) [values: auto, always, never] |
| `-v`, `--verbose` | More logs on stderr (-v debug, -vv trace) |

#### `cucinactl status`

Overall state: components, pools, hosts, alerts, worker instances

```text
Usage: cucinactl status [OPTIONS]
```

#### `cucinactl pools`

Worker pools

```text
Usage: cucinactl pools [OPTIONS] <COMMAND>
```

##### `cucinactl pools list`

List pools with desired vs actual workers

```text
Usage: cucinactl pools list [OPTIONS]
```

##### `cucinactl pools describe`

Show one pool: spec, conditions, workers, scale timeline, cold starts

```text
Usage: cucinactl pools describe [OPTIONS] <NAME>
```

| Argument | Description |
| --- | --- |
| `<NAME>` | Pool name |

##### `cucinactl pools scale-floor`

Keep at least N workers running for a while (standing cost; always expires). `--min 0` clears the floor

```text
Usage: cucinactl pools scale-floor [OPTIONS] --min <MIN> <NAME>
```

| Argument | Description |
| --- | --- |
| `<NAME>` | Pool name |
| `--min` | Minimum number of running workers (0 clears the floor) |
| `--for` | How long the floor lasts (e.g. 2h; at most 7d); required unless --min 0 |

##### `cucinactl pools cordon`

Stop (or with --undo, resume) launching new workers for a pool

```text
Usage: cucinactl pools cordon [OPTIONS] <NAME>
```

| Argument | Description |
| --- | --- |
| `<NAME>` | Pool name |
| `--undo` | Uncordon instead |

##### `cucinactl pools gc`

Delete orphaned volumes/ENIs carrying pool tags

```text
Usage: cucinactl pools gc [OPTIONS] [NAME]
```

| Argument | Description |
| --- | --- |
| `<NAME>` | Pool name (default: all pools) |
| `--dry-run` | Only list what would be deleted |

#### `cucinactl workers`

Workers (VMs) and their logs

```text
Usage: cucinactl workers [OPTIONS] <COMMAND>
```

##### `cucinactl workers list`

List workers (VMs)

```text
Usage: cucinactl workers list [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--pool` | Only workers of this pool |

##### `cucinactl workers drain`

Drain a worker: running actions finish, no new ones start

```text
Usage: cucinactl workers drain [OPTIONS] <NODE>
```

| Argument | Description |
| --- | --- |
| `<NODE>` | Worker node (EC2 instance ID or <host>/<vm>) |

##### `cucinactl workers undrain`

Undo a drain

```text
Usage: cucinactl workers undrain [OPTIONS] <NODE>
```

| Argument | Description |
| --- | --- |
| `<NODE>` | Worker node |

##### `cucinactl workers logs`

Show a worker's logs

```text
Usage: cucinactl workers logs [OPTIONS] <NODE>
```

| Argument | Description |
| --- | --- |
| `<NODE>` | Worker node |
| `--unit` | Which log (default: `bb-worker`) [values: bb-worker, bb-runner, agent] |
| `--tail` | Number of trailing lines (default: `200`) |
| `-f`, `--follow` | Keep streaming new lines |

#### `cucinactl hosts`

macOS hosts (Mac minis running cucina-hostd) and their enrollment

```text
Usage: cucinactl hosts [OPTIONS] <COMMAND>
```

##### `cucinactl hosts list`

List Mac hosts with VM slots, images and cache statistics

```text
Usage: cucinactl hosts list [OPTIONS]
```

##### `cucinactl hosts drain`

Drain a host for maintenance (its VMs finish their actions and shut down)

```text
Usage: cucinactl hosts drain [OPTIONS] <HOST>
```

| Argument | Description |
| --- | --- |
| `<HOST>` | Serial number or name |

##### `cucinactl hosts uncordon`

Return a drained host to service

```text
Usage: cucinactl hosts uncordon [OPTIONS] <HOST>
```

| Argument | Description |
| --- | --- |
| `<HOST>` | Serial number or name |

##### `cucinactl hosts enroll-token`

Site enrollment tokens (multi-use, expiring, revocable)

```text
Usage: cucinactl hosts enroll-token [OPTIONS] <COMMAND>
```

###### `cucinactl hosts enroll-token create`

Create a site enrollment token (shown once)

```text
Usage: cucinactl hosts enroll-token create [OPTIONS] --site <SITE>
```

| Argument | Description |
| --- | --- |
| `--site` | Site the token is bound to |
| `--ttl` | Lifetime (e.g. 7d) (default: `7d`) |
| `--max-hosts` | Maximum number of hosts that may enroll with it (default: `10`) |
| `--description` | Free-form description |

###### `cucinactl hosts enroll-token list`

List enrollment tokens (never their values)

```text
Usage: cucinactl hosts enroll-token list [OPTIONS]
```

###### `cucinactl hosts enroll-token revoke`

Revoke a token (blocks new enrollments only)

```text
Usage: cucinactl hosts enroll-token revoke [OPTIONS] <ID>
```

| Argument | Description |
| --- | --- |
| `<ID>` | Token ID |

##### `cucinactl hosts re-image`

Re-clone a host's VMs from the golden image (next start, or now if stopped)

```text
Usage: cucinactl hosts re-image [OPTIONS] <HOST>
```

| Argument | Description |
| --- | --- |
| `<HOST>` | Serial number or name |
| `--vm` | Only this VM |

##### `cucinactl hosts diag`

Collect a host's diagnostics into a file

```text
Usage: cucinactl hosts diag [OPTIONS] <HOST>
```

| Argument | Description |
| --- | --- |
| `<HOST>` | Serial number or name |
| `-f`, `--file` | Output file (default: cucina-host-<host>-<time>.tar.gz) |

##### `cucinactl hosts register`

Pre-register (and approve) serial numbers, e.g. pasted from Apple Business

```text
Usage: cucinactl hosts register [OPTIONS] --site <SITE> <SERIALS>...
```

| Argument | Description |
| --- | --- |
| `<SERIALS>` | Serial numbers |
| `--site` | Site the hosts belong to |
| `--label` | Labels (key=value), repeatable |

##### `cucinactl hosts approve`

Approve a pending host enrollment

```text
Usage: cucinactl hosts approve [OPTIONS] <SERIAL>
```

| Argument | Description |
| --- | --- |
| `<SERIAL>` | Serial number |

##### `cucinactl hosts remove`

Remove a host (it must re-enroll to come back)

```text
Usage: cucinactl hosts remove [OPTIONS] <HOST>
```

| Argument | Description |
| --- | --- |
| `<HOST>` | Serial number or name |

#### `cucinactl queues`

Build queues per platform and size class

```text
Usage: cucinactl queues [OPTIONS]
```

#### `cucinactl ops`

Queued and executing operations

```text
Usage: cucinactl ops [OPTIONS] <COMMAND>
```

##### `cucinactl ops list`

List queued and executing operations

```text
Usage: cucinactl ops list [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--stage` | Only operations in this stage [values: queued, executing, completed] |
| `--instance` | Only this instance name |
| `--invocation` | Only this Bazel invocation ID |
| `--platform` | Only this queue platform (name=value), repeatable |
| `--limit` | Page size for `list` (default: `100`) |
| `--count` | `watch`: stop after this many operation events |

##### `cucinactl ops watch`

Stream operation changes until interrupted (JSON lines with -o json)

```text
Usage: cucinactl ops watch [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--stage` | Only operations in this stage [values: queued, executing, completed] |
| `--instance` | Only this instance name |
| `--invocation` | Only this Bazel invocation ID |
| `--platform` | Only this queue platform (name=value), repeatable |
| `--limit` | Page size for `list` (default: `100`) |
| `--count` | `watch`: stop after this many operation events |

##### `cucinactl ops kill`

Kill an operation, or fail every operation queued on a queue without workers

```text
Usage: cucinactl ops kill [OPTIONS] [OPERATION]
```

| Argument | Description |
| --- | --- |
| `<OPERATION>` | Operation name |
| `--queue-without-workers` | Kill everything queued on this queue (needs --platform) |
| `--platform` | Queue platform property (name=value), repeatable |
| `--instance` | Queue instance name prefix |
| `--size-class` | Queue size class (default: `0`) |
| `--message` | Message reported to the client (default: `killed by cucinactl`) |

#### `cucinactl action`

Actions in the CAS/AC

```text
Usage: cucinactl action [OPTIONS] <COMMAND>
```

##### `cucinactl action inspect`

Show an action: command, environment, platform, inputs, outputs, exit code, stdout/stderr, timing and worker

```text
Usage: cucinactl action inspect [OPTIONS] <SUBJECT>
```

| Argument | Description |
| --- | --- |
| `<SUBJECT>` | Action digest (<hash>/<size>), a Buildbarn action or historical_execute_response link, or an operation name |
| `--instance` | Instance name (default: the profile's) |
| `--max-tree-entries` | Maximum input-tree entries to list (default: `200`) |
| `--max-output-bytes` | Maximum bytes of stdout/stderr to show each (default: `65536`) |

#### `cucinactl login`

Log in to a Cucina deployment (OIDC loopback + PKCE, or a service-account key)

```text
Usage: cucinactl login [OPTIONS] [URL]
```

| Argument | Description |
| --- | --- |
| `<URL>` | Cucina URL, e.g. https://cucina.example.com (default: the profile's) |
| `--provider` | Identity provider to use when the deployment offers several |
| `--port` | Loopback port for the redirect (e.g. for `ssh -L 127.0.0.1:P:127.0.0.1:P`) |
| `--no-browser` | Do not open a browser; only print the URL |
| `--manual` | Paste the final redirect URL instead of running a local listener |
| `--hd` | Google hosted-domain hint (`hd`) |
| `--redirect-path` | Redirect path (some providers register a path) (default: `/`) |
| `--browser-command` | Run this program with the URL instead of the default browser |
| `--login-timeout` | How long to wait for the browser (default: `5m`) |
| `--force` | Log in again even if a stored session can be renewed |
| `--key` | Service-account key file (break-glass / systems without OIDC); `-` reads stdin |
| `--key-env` | Read the service-account key from this environment variable |
| `--credential-store` | Where to store the refresh token or key [values: keyring, file] |
| `--ca-file` | Extra CA bundle (PEM) for a private CA, stored in the profile (default: $CUCINA_CA_FILE; $SSL_CERT_FILE is trusted too) |

#### `cucinactl logout`

Delete this machine's session for a profile (tokens and stored secrets)

```text
Usage: cucinactl logout [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--all` | Log out of every profile |
| `--forget` | Also delete the profile from config.toml |

#### `cucinactl whoami`

Show who you are logged in as, your grants and the token's expiry

```text
Usage: cucinactl whoami [OPTIONS]
```

#### `cucinactl keys`

Service-account keys and principal revocation

```text
Usage: cucinactl keys [OPTIONS] <COMMAND>
```

##### `cucinactl keys create`

Create a service-account key (shown once; stored hashed by the server)

```text
Usage: cucinactl keys create [OPTIONS] --account <ACCOUNT>
```

| Argument | Description |
| --- | --- |
| `--account` | Service account (TrustPolicy type serviceAccount) |
| `--description` | Description |
| `--ttl` | Expiry (default: no expiry) |

##### `cucinactl keys list`

List service-account keys

```text
Usage: cucinactl keys list [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--account` | Only this account |

##### `cucinactl keys revoke`

Revoke a service key, or deny-list a principal (--sub) or session (--sid)

```text
Usage: cucinactl keys revoke [OPTIONS] [KEY_ID]
```

| Argument | Description |
| --- | --- |
| `<KEY_ID>` | Key ID |
| `--sub` | Principal (JWT `sub`) to deny-list |
| `--sid` | Session (JWT `sid`) to deny-list |
| `--reason` | Reason (audit log) |

##### `cucinactl keys revocations`

List deny-list entries

```text
Usage: cucinactl keys revocations [OPTIONS]
```

#### `cucinactl bazelrc`

Print ready-to-use .bazelrc lines for this deployment (UC9)

```text
Usage: cucinactl bazelrc [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--platform` | Execute on this OS's pool (native configuration) [values: linux, windows, macos] |
| `--cross` | Cross configuration for a hermetic-llvm target (needs --target) |
| `--target` | Target platform for --cross (see --list-targets) |
| `--exec-pool` | Pool that runs compile actions (default: the cheapest capable pool) |
| `--xcode` | macOS: the pool serving this Xcode version |
| `--ci` | CI flags (minimal downloads) |
| `--read-only` | The principal cannot write the action cache |
| `--cache-only` | Remote cache only, local execution |
| `--config-name` | Emit lines for `--config=<NAME>` instead of unconditional ones |
| `--helper-path` | Credential helper path (default: the installed cucina-credential-helper) |
| `--disk-cache` | Local disk cache directory (`none` disables it) |
| `--disk-cache-max-size` | Disk cache size limit (default: `50G`) |
| `--client-os` | Client OS (default: this one): Windows clients get the Linux/macOS action environment in cross configurations, others the @bazel_tools overlay note for Windows tests [values: linux, windows, macos] |
| `--platforms-package` | Use platforms from this package (output of --emit-build-file) instead of @cucina_platforms |
| `--emit-build-file` | Print platform() definitions for every pool runner instead of .bazelrc lines |
| `--list-targets` | List the cross targets and the pools that may compile them |

#### `cucinactl credential-helper`

Bazel credential helper (also reachable as `cucina-credential-helper`)

```text
Usage: cucinactl credential-helper [OPTIONS] <COMMAND>
```

##### `cucinactl credential-helper get`

Bazel's `get` request: JSON {"uri": …} on stdin, headers on stdout

```text
Usage: cucinactl credential-helper get [OPTIONS]
```

##### `cucinactl credential-helper install`

Create the `cucina-credential-helper` hardlink (or copy) of this binary

```text
Usage: cucinactl credential-helper install [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--dir` | Directory (default: next to cucinactl) |

#### `cucinactl cost`

Spend per pool (estimated from instance-hours and the price list)

```text
Usage: cucinactl cost [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `--pool` | Only this pool |
| `--since` | Start of the period (RFC 3339 or YYYY-MM-DD; default: start of the month) |

#### `cucinactl images`

Worker image versions per pool

```text
Usage: cucinactl images [OPTIONS]
```

#### `cucinactl diag`

Download a support bundle (secrets redacted by the server)

```text
Usage: cucinactl diag [OPTIONS]
```

| Argument | Description |
| --- | --- |
| `-f`, `--file` | Output file (default: cucina-support-<time>.tar.gz) |
| `--include-logs` | Include component logs |

#### `cucinactl config`

Profiles and local configuration

```text
Usage: cucinactl config [OPTIONS] <COMMAND>
```

##### `cucinactl config view`

Show the configuration (profiles; never secrets)

```text
Usage: cucinactl config view [OPTIONS]
```

##### `cucinactl config profiles`

List profile names

```text
Usage: cucinactl config profiles [OPTIONS]
```

##### `cucinactl config use`

Make a profile the default

```text
Usage: cucinactl config use [OPTIONS] <NAME>
```

| Argument | Description |
| --- | --- |
| `<NAME>` | Profile name |

##### `cucinactl config set`

Set a profile field (instance-name, management, remote-executor, ca-file, credential-store)

```text
Usage: cucinactl config set [OPTIONS] <KEY> <VALUE>
```

| Argument | Description |
| --- | --- |
| `<KEY>` | Field |
| `<VALUE>` | Value |

##### `cucinactl config delete`

Delete a profile and its local session

```text
Usage: cucinactl config delete [OPTIONS] <NAME>
```

| Argument | Description |
| --- | --- |
| `<NAME>` | Profile name |

##### `cucinactl config path`

Print the configuration directory

```text
Usage: cucinactl config path [OPTIONS]
```

#### `cucinactl completions`

Print a static shell completion script

```text
Usage: cucinactl completions [OPTIONS] <SHELL>
```

| Argument | Description |
| --- | --- |
| `<SHELL>` | Shell to generate completions for [values: bash, elvish, fish, powershell, zsh] |

#### `cucinactl tui`

Open the interactive terminal UI

```text
Usage: cucinactl tui [OPTIONS]
```

<!-- END GENERATED: cargo xtask docs -->
