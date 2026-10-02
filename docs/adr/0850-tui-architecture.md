<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0850 — `cucinactl tui`: reducer architecture, data refresh and safety rules

* Status: accepted (2026-10-02)

## Context
R-CLI-4 asks for live views (≤ 2 s, server streaming where available), keyboard-first
with optional mouse, and a confirmation before every destructive action; R-TEST-6/4 allow
only a handful of TUI snapshots, which must be deterministic; R-LIB-3 fixes ratatui 0.30
+ crossterm 0.29 with no third-party widget crates. The management API streams only the
overview (`WatchOverview`, every 2 s) and operation changes (`WatchOperations`); workers
per pool, host details, cost lines and keys are unary calls. The CLI commands
(`cli.rs`, `commands/`) and the client library belong to the `cli` agent.

## Decision
* **Elm-style split.** `State` holds everything on screen, including the clock (`Tick`
  events) and the terminal size; `update(&mut State, Event) -> Vec<Effect>` is the only
  mutator and does no I/O; `render(&State, &mut Frame)` is pure. Effects (API calls,
  stream restarts, mouse capture, repaint) run in `runtime.rs`. Geometry lives in
  `layout.rs`, shared by rendering and mouse hit-testing.
* **Port + fake.** `backend::Management` (two streams, one `call(Request)`) has the real
  adapter over `client::Session` (reusing `inspect::inspect` for drill-down) and a
  state-based fake used by the tests and the demo modes. No client changes were needed.
* **Refresh.** Streams feed the overview, pool summaries, host live fields and the
  operation list. Unary data is fetched only for the visible view: `GetPool` of the
  selected pool and `ListHosts` every 2 s, `GetCost`/keys every 10 s, never twice in
  flight. After a `WatchOperations` reconnect the TUI takes a full `ListOperations`
  snapshot, because a new stream replays current operations as ADDED but cannot report
  those that ended in between. The header shows STALE when overview data is older than
  5 s (not on every reconnect, so a quick resume does not flicker).
* **Filtering is client-side**, over the whole operation stream, so it is incremental
  and covers fields the API cannot filter (platform substrings, targets, workers).
* **Safety.** Destructive requests (drain, kill, revoke, re-image) are emitted from one
  function, `confirm`, reachable only from the dialog's "yes"; the dialog opens on
  "No", and Enter answers the highlighted button. Undrain/uncordon only revert a drain
  and do not ask.
* **Terminal hygiene.** `ratatui::try_init`/`restore` with its panic hook (plus a hook
  that disables mouse capture and stops the loop if any thread panics); input on a
  thread using crossterm's blocking reader (no extra `event-stream` feature); key
  releases ignored (Windows). The TUI renews the session itself and reports failures in
  the status line instead of `client::spawn_refresher`, whose `tracing::warn!` on stderr
  would scribble over the screen.
* **Options without flags.** `cucinactl tui` has no options of its own (the CLI
  definition is not ours); the TUI reads `CUCINA_TUI_DEMO` and `CUCINA_TUI_MOUSE`, and
  the global `--color`, `--profile`, `--timeout`.
* **Portability.** 16 ANSI colours only; `NO_COLOR`/`--color never` fall back to
  bold/reverse; outside UTF-8 locales the rendered buffer is mapped to ASCII; minimum
  60×16, below which a message is shown.

## Consequences
Snapshot tests and the confirmation table run against the pure reducer in milliseconds
with no terminal. The demo mode lets anyone try the TUI (and run MT-006) without a
cluster, at the cost of shipping invented fixture data in the binary. Key creation,
principal revocation and log streaming stay CLI-only. Very large operation queues are
streamed in full to the client.
