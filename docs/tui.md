<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# `cucinactl tui` — the Cucina terminal UI

`cucinactl tui` (or `cucinactl` with no arguments on a terminal) is the interactive,
live view of a Cucina deployment (R-CLI-4, UC13): queues, pools and workers, Mac hosts,
operations down to a single action, cost and service keys. It talks to the same
management API as the CLI (`cucinactl login` first; see [cli.md](cli.md)) and needs no
kubeconfig.

Contents: [Views](#views) · [Keys](#keys) · [Filter](#filter) ·
[Destructive actions](#destructive-actions) · [Live data](#live-data) ·
[Terminals](#terminals-colour-mouse) · [Environment](#environment) ·
[Known limits](#known-limits) · [Testing](#testing) · [MT-006](#manual-check-mt-006) ·
[Code](#code)

## Views

| Tab | Shows | Data (refresh) |
| --- | --- | --- |
| 1 Overview | Queue depth and time per platform (queued, executing, oldest, p95, serving pool); workers by state; pools desired vs actual (`2+1` = 2 running + 1 booting); spend today and this month, the idle projection, hosts online; a sparkline of queued and executing actions (last 4 min); alerts | `WatchOverview` stream, every 2 s |
| 2 Pools | Pools (desired/max, booting, ready, busy, draining, image generation, condition); the selected pool's VMs (state, busy threads, type, generation, idle time, uptime); its scale timeline; its cold starts (API call → running → registered → first action, p50 and max) | overview stream + `GetPool` of the selected pool every 2 s |
| 3 Hosts | Mac hosts: site, phase, cordon state, VM slots, L2 hit ratio, WAN bytes received and rate, heartbeat age, agent version; for the selected host: macOS, Tart, chip, free disk, certificate expiry, its VMs (pool, state, image, generation, registered), images, labels | `ListHosts` every 2 s; phase, VMs, L2, WAN and heartbeat from the overview stream |
| 4 Ops | Queued and executing operations: stage, age, platform, instance, target, invocation (first 8 characters), worker. `Enter` opens the **action inspector**: command (one argument per line), environment, platform, input tree, requested and produced outputs, exit code, stdout/stderr, timing, worker — the same data as `cucinactl action inspect` | `WatchOperations` stream; a full listing after each reconnect; inspection on demand |
| 5 Cost | Spend today and this month; the idle projection (standing cost per month with every pool at zero: AMI storage, Windows Fast Launch snapshots); per pool: instance-hours, compute, EBS, transfer, public IPv4, total, standing cost; the selected pool's itemised lines and the price assumptions | overview stream + `GetCost` every 10 s |
| 6 Keys | Service keys (account, description, age, expiry, last use, status) and revoked principals | `ListServiceKeys`, `ListRevocations` every 10 s (admin only) |

Unary data is fetched only for the visible view. Selections follow their row across
refreshes (a worker stays selected while others come and go).

## Keys

| Key | Action |
| --- | --- |
| `Tab` / `Shift-Tab`, `1`–`6` | switch view |
| `↑` `↓` `j` `k`, `PgUp` `PgDn`, `g` `G` (`Home` `End`) | move the selection (scroll the inspector) |
| `Enter` / `→` / `l` | drill in: queue → its operations, pool → its workers, operation → action inspector |
| `Esc` / `←` / `h` | back (workers → pools, inspector → list); `Esc` also clears the filter |
| `/` or `:` | filter as you type (see below) |
| `d` / `u` | drain / undrain the selected worker (Pools, workers table); drain / uncordon the selected host (Hosts) |
| `R` | re-image every VM of the selected host (Hosts) |
| `x` | kill the selected operation (Ops); revoke the selected service key (Keys) |
| `r` | refresh now; reconnect a stopped stream |
| `m` | mouse capture on/off |
| `Ctrl-L` | redraw the whole screen |
| `?` | help overlay (the same table) |
| `q`, `Ctrl-C` | quit (`q` first closes the inspector or a dialog) |

In a confirmation dialog: `y` confirms; `n`, `Esc` or `q` cancel; `←` `→` `Tab` move
between **No** and **Yes**; `Enter` or `Space` answers the highlighted button, which is
**No** when the dialog opens. Other keys are ignored while it is open.

With the mouse on: click a tab to switch, click a row to select it (and focus its
table), use the wheel to move the selection or scroll the inspector, click a dialog's
buttons.

## Filter

`/` or `:` opens a prompt in the status line; the table filters as you type. Terms are
separated by spaces and must all match (case-insensitive substrings):

* a bare word matches any column: `/absl/strings`, `/mini-2`;
* `field:value` (or `field=value`) matches one column: `platform:linux inv:9e8d`;
* field names may be shortened to any unambiguous prefix (`plat:`, `inst:`); an unknown
  field is matched as plain text, so digests and URLs with `:` work; quote values with
  spaces: `target:"//a b"`.

| View | Fields (aliases) |
| --- | --- |
| Overview (queues) | `platform` (`p`, `os`), `instance` (`i`, `inst`), `pool` |
| Pools | `pool` (`name`), `platform` (`p`), `provider`, `condition` (`cond`, `status`) |
| Hosts | `host` (`name`), `serial`, `site`, `phase` (`state`, `status`) |
| Ops | `platform` (`p`, `os`), `instance` (`i`, `inst`), `invocation` (`inv`), `stage` (`s`, `state`), `target` (`t`), `worker` (`w`, `node`), `name` (`op`, `operation`), `digest` (`action`) |
| Cost | `pool` |
| Keys | `account` (`sa`), `key` (`id`), `status` (`state`), `description` (`desc`) |

`Enter` keeps the filter, `Esc` restores the previous one, `Ctrl-U` clears the input.
Each view keeps its own filter; the status line shows the active one. Platforms are
written `os/isa` plus Cucina's extra properties: `linux/x86-64`, `linux/rv64g qemu`,
`macos/arm-a64 xcode27.0`.

## Destructive actions

Drain, kill, revoke and re-image always open a confirmation dialog that names the
target and says what happens; the default answer is **No** (R-CLI-4). Every mutating
call shows the API's answer (or its gRPC status and message) in the status line, and
the view refreshes. The controller audits every mutating call.

| Action | Where | Key | API call | Asks |
| --- | --- | --- | --- | --- |
| Drain a worker | Pools, workers table | `d` | `DrainWorker` | yes |
| Undrain a worker | Pools, workers table | `u` | `UndrainWorker` | no (reverts a drain) |
| Drain (cordon) a Mac host | Hosts | `d` | `DrainHost` | yes |
| Uncordon a Mac host | Hosts | `u` | `UncordonHost` | no |
| Re-image every VM of a host | Hosts | `R` | `ReimageHost` | yes |
| Kill an operation | Ops | `x` | `KillOperations` (`operation_name`) | yes |
| Revoke a service key | Keys | `x` | `RevokeServiceKey` | yes |

## Live data

* The header shows the overview stream's health: **● LIVE** (data at most 5 s old),
  **◐ STALE** *age* `· retry #n in Ns` while the client reconnects (jittered
  exponential backoff, 250 ms up to 30 s), **✖ DISCONNECTED** after a permanent error
  (for example `permission_denied`; `r` retries). The Ops title says when its stream is
  reconnecting or stopped.
* After the operations stream reconnects, the TUI re-reads the full listing, so
  operations that finished while it was down disappear.
* The 15-minute session token is renewed in the background (5 minutes before expiry);
  renewal failures appear in the status line, and a stream that failed with
  `unauthenticated` restarts by itself 10 s later.
* Nothing is written to stderr while the TUI runs (it would scribble over the screen).

## Terminals, colour, mouse

* Needs a terminal on stdin and stdout and at least **60×16**; smaller windows show a
  "terminal too small" message until they grow. Resizing reflows the layout.
* crossterm backend: works in Terminal.app, iTerm2, Windows Terminal (key releases are
  ignored), tmux and over SSH; no terminal-specific features. `ratatui::try_init` sets
  raw mode and the alternate screen, and its panic hook restores the terminal (mouse
  capture included).
* Colour: only the 16 ANSI colours, so 16-colour terminals and light or dark themes
  work. `NO_COLOR`, `--color never` and `TERM=dumb` switch to bold/reverse only;
  `--color always` forces colour.
* Outside a UTF-8 locale (`LC_ALL`, `LC_CTYPE`, `LANG`, e.g. `LANG=C`) borders,
  sparklines and symbols are drawn in ASCII.
* Mouse capture is on by default; `m` turns it off (so the terminal selects text
  again), `CUCINA_TUI_MOUSE=0` starts with it off. While capture is on, most terminals
  still select text with a modifier held (Shift; Option in iTerm2).

## Environment

| Variable | Effect |
| --- | --- |
| `CUCINA_TUI_DEMO` | Run against built-in invented data, without a cluster or login: `1` (or `live`) — a load that changes every 2 s, dated now; `scripted` — the same load at a fixed clock (each 2 s step always looks the same); `frozen` — static data at a fixed clock. For trying the TUI, terminal checks (MT-006) and the VHS tapes |
| `CUCINA_TUI_MOUSE` | `0` starts with mouse capture off |
| `NO_COLOR`, `TERM`, `LC_ALL` / `LC_CTYPE` / `LANG` | colour and ASCII fallback (above) |

The global options apply: `--profile`, `--timeout` (per unary call), `--color`.

## Known limits

* Creating service keys and revoking principals (`keys create`, `keys revoke --sub`),
  pool cordon, scale floor and GC, enrollment tokens, worker logs and host diagnostics
  are CLI-only.
* Operations are filtered on the client over the full `WatchOperations` stream: instant
  for thousands of operations, but a huge queue costs bandwidth.
* The inspector does not wrap lines; arguments and environment are one per line, and
  longer lines are cut at the window edge.
* Ages are computed with the local clock; a skewed clock shows skewed ages.
* The Windows PTY test (expectrl/ConPTY) builds (`bazel build
  //cli/cucinactl:tui_pty_test --platforms=@rules_rs//rs/platforms:x86_64-pc-windows-gnullvm
  --extra_execution_platforms=@platforms//host,@rules_rs//rs/platforms:x86_64-pc-windows-gnullvm`)
  but has not run yet: the Windows CI lane skips `//cli/...` until Rust links for MSVC.

## Testing

| What | Where | Run |
| --- | --- | --- |
| 6 key screens at 100×30 and 80×24 (`insta` + `TestBackend`): overview, pools with the drain dialog, hosts, filtered operations, action inspector, cost | `cli/cucinactl/tests/tui_snapshots.rs`, `tests/snapshots/` | `cargo test -p cucinactl --test tui_snapshots`; Bazel `//cli/cucinactl:tui_snapshots_test` (unit) |
| Destructive actions reach the API only after "yes" (one table); stale badge and resync after a reconnect | `cli/cucinactl/tests/tui_reducer.rs` | `--test tui_reducer`; `:tui_reducer_test` (unit) |
| `cucinactl tui` in a PTY against the fake management server: overview, switch view, open and cancel a drain (no API call), quit with exit 0 | `cli/cucinactl/tests/tui_pty.rs` (rexpect on Unix, expectrl on Windows) | `--test tui_pty`; `:tui_pty_test` (integration) |
| Three flows in a real terminal emulator (VHS 0.12.1: ttyd + xterm.js): overview updating live, drain with confirmation, filter + action inspector | `cli/cucinactl/vhs/*.tape`, `vhs/golden/` | `cli/cucinactl/vhs/check.sh` (manual tier) |

The tests drive the public reducer with `tui::fake::FakeManagement` (state-based,
deterministic, records every mutating request) at a fixed clock.

* Snapshots: `INSTA_UPDATE=always cargo test -p cucinactl --test tui_snapshots` (or
  `cargo insta review`), then review the diff; changing a snapshot needs a
  `Test-Change:` trailer (R-TEST-5). Bazel never writes snapshots.
* VHS goldens: needs `mise install vhs@0.12.1`, `brew install ttyd ffmpeg` and a
  Chrome/Chromium. The tapes start the TUI with `CUCINA_TUI_DEMO=frozen|scripted`,
  capture only frames taken after a `Wait+Screen` for the expected state (80×24), and
  `check.sh` keeps the TUI frames and diffs them with `golden/`; `check.sh --update`
  rewrites them, `VHS_KEEP=dir` keeps the GIFs. See ADR 0851.
* The campaign's T20 scripted session runs the same PTY flow against the live cluster.

## Manual check (MT-006)

[MT-006](testing/manual/MT-006.md) is the human check of how the TUI feels. Use
`CUCINA_TUI_DEMO=1 cucinactl tui` where no cluster is at hand. Check each terminal at
each size, then the colour and locale variants in at least one terminal per OS:

| Terminal | Sizes | Variants |
| --- | --- | --- |
| Terminal.app (macOS 27) | 200×60, 100×30, 80×24, 60×16, 59×15 (too small), 40×12 | `NO_COLOR=1`; `TERM=xterm` (16 colours); light and dark profile; `LANG=C` (ASCII) |
| iTerm2 | same | same, plus mouse: click, wheel, modifier-drag selects text, `m` |
| Windows Terminal + PowerShell | 120×30, 80×24, 60×16 | `--color never`; key repeat; `Ctrl-C` |
| Windows Terminal + WSL (bash) | 100×30, 80×24 | `LANG=C` |
| tmux 3.x over SSH to Linux (+200 ms latency) | split panes down to 60×16, zoom/unzoom | `Esc` delay, `Shift-Tab`, `PgUp`/`PgDn`, `Home`/`End` |
| Linux console or GNOME Terminal | 80×25 | `LANG=C` |

Done when: no stray characters, layouts reflow on resize, "too small" below 60×16,
every view and dialog readable in each colour mode, the terminal is left as it was after
`q` and after `Ctrl-C`, and destructive actions always ask (default No).

Tested so far (2026-10-02, by the implementing agent): tmux on macOS 27 at 100×30 and
80×24 (all views, demo mode), a PTY (rexpect) and xterm.js under VHS. The terminals in
the table above are pending the human run.

## Code

`cli/cucinactl/src/tui/` (ADR 0850):

| File | Role |
| --- | --- |
| `mod.rs` | entry point: demo/remote backend, theme, `runtime::run` |
| `model.rs` | `State` — everything on screen, including the clock |
| `update.rs` | the reducer: `update(&mut State, Event) -> Vec<Effect>` (pure) |
| `view.rs`, `inspector.rs`, `layout.rs`, `rows.rs`, `format.rs`, `theme.rs`, `filter.rs` | rendering (`render(&State, &mut Frame)`, pure), geometry shared with mouse hit-testing, derived table rows, formatting, palette, filter |
| `backend.rs` | the `Management` port and its real adapter (`client::Session`, `inspect::inspect`) |
| `fake.rs` | `FakeManagement` for tests and the demo modes |
| `runtime.rs` | terminal, input thread, clock, streams, signals, token renewal, effects |
