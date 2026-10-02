<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0017 — D7: Virtual build directories on every OS where they work

* Status: accepted (2026-10-02)

## Context
Upstream ranks hardlink-based build directories as the least efficient mode, and NTFS caps a file at
1023 hardlinks, which breaks Windows workers under load. A virtual build directory fetches only the bytes an
action reads, which is the main lever on worker-side transfer.

## Decision
FUSE on Linux, WinFSP on Windows (case-insensitive, as in upstream's Windows CI), and NFSv4 or the
native mode on macOS depending on what proves reliable in Tart guests (the measured choice is
recorded in the macOS image ADRs). Falling back to native on any OS needs a recorded reason.

## Consequences
* WinFSP is GPLv3 with a FLOSS exception. We install it in our own images and do not redistribute it; do not publish a worker
  image that contains it (`THIRD_PARTY_NOTICES.md`).
* Linux images need FUSE; macOS guests need the case-sensitive build volume described in the image docs.
