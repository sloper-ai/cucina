<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0851 — TUI flows as VHS goldens: deterministic demo data, manual tier

* Status: accepted (2026-10-02)

## Context
R-TEST-6 asks for 2–3 key TUI flows as VHS (v0.12.1) goldens. VHS drives a real
terminal emulator (ttyd + xterm.js in a headless Chrome) and can write the screen as
text after every command. Its dependencies (ttyd, ffmpeg, a Chromium) are not hermetic
and not in the Bazel graph; ttyd has no macOS build in mise/aqua. Live data and a wall
clock make frames differ between runs, and a frame taken right after a keystroke races
the TUI's redraw.

## Decision
* Three tapes in `cli/cucinactl/vhs/` (overview updating live; drain with confirmation;
  filter + action inspector), at 80×24 (846×486 px, font 16, padding 10).
* They run the binary's built-in fake: `CUCINA_TUI_DEMO=frozen` (static data, clock fixed
  at the fixtures' time) or `scripted` (the demo load stepping every 2 s, same fixed
  clock), so a given step always renders identically.
* Every interaction is `Hide`-den; a frame is captured only after `Wait+Screen` for the
  expected state (`Show`, short `Sleep`), so frames never race a redraw.
* `vhs/check.sh` runs the tapes, keeps the TUI frames (drops empty/prompt frames and
  duplicates, trims blanks) and diffs them with `vhs/golden/*.txt` (two frames, ≤ 50
  lines each); `--update` rewrites them. It is a **manual-tier** check (release, TUI
  changes, MT-006), not part of `bazel test //...`. ttyd comes from Homebrew, vhs from
  mise; Chrome must be installed.

## Consequences
The goldens prove the flows in a real emulator and stay stable across runs (verified
twice in a row). They do not run in CI until a lane has vhs/ttyd/ffmpeg/Chrome; the
`insta` snapshots and the PTY test cover rendering and flows hermetically in the
meantime. Fixture data changes require `check.sh --update` and a reviewed diff
(`Test-Change:` trailer).
