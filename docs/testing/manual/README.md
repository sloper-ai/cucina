<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Manual tests

Some risks cannot be covered by an automated test at an acceptable cost: a brand-new Mac mini coming out of Apple Business, a real Google consent screen, a power cable pulled out of a
host, the feel of a TUI in six terminals, a production install done by someone who did not write the docs. Those are **manual checks**: written procedures that a human runs,
with required evidence and a signature. They are the `manual` tier of [`TESTING.md`](../../../TESTING.md).

## The checks

| ID | Check | Run when |
| --- | --- | --- |
| [MT-001](MT-001.md) | Mac mini enrollment through Apple Business, from a brand-new device, including hostd's root-daemon to `cucina`-user Tart path | Before the first production host; changes to `macos/` or hostd enrollment or privilege drop; macOS major releases |
| [MT-002](MT-002.md) | Real Google Desktop-app login, including `--manual` and the SSH forward | Changes to the STS, trust policies, the credential helper or login; OAuth client changes |
| [MT-003](MT-003.md) | Real GitHub Actions OIDC run in this repository, including fork-PR denial | Changes to the STS, trust policies or the CI workflow |
| [MT-004](MT-004.md) | Mac host power pull, network pull and disk full | Before the first production host; changes to hostd lifecycle, reconnect or disk handling |
| [MT-005](MT-005.md) | macOS and Xcode upgrade on a host and in the guest image | Every macOS and Xcode release; changes to the update or image procedures |
| [MT-006](MT-006.md) | TUI feel: resize, terminals, tmux and SSH, colour, small sizes | Changes to the TUI |
| [MT-007](MT-007.md) | Fresh production install on EKS using only the README | Every minor release; material README, chart or EKS guide changes |
| [MT-008](MT-008.md) | Disaster drill: storage loss and cold-cache recovery time | Quarterly; every minor release; storage or retention changes |

## Rules

* **A human signs off.** An agent may pre-run the steps marked `[agent]` and attach the output, which saves the human time and gives them a clean starting point. Steps marked `[human]` need a person
  (a physical action, a browser, a judgement). The sign-off is the person's: record it in the file's front matter (`last_run: <date>`, `sign_off: <name> <date>`) in the same change that attaches
  the evidence. A file with `last_run: never` and `sign_off: pending` has never been run.
* **A manual failure that can be automated becomes a regression test**, in the lowest tier that can hold it. The manual step then shrinks to what a person must still judge.
* **A new check** needs a real risk that no automated tier can cover. Copy the template below, number it, add it to the table above, and run `tools/ci/check-manual-tests.sh`.
* **Deleting or weakening a check** needs a `Test-Change:` line like any other test.

### Evidence

The release issue lives in a public repository, so evidence must be safe to publish. Before attaching anything:

* remove tokens, keys, refresh tokens and JWTs entirely (never mask part of one);
* replace account IDs, addresses, hostnames and serial numbers with placeholders;
* keep timestamps, versions, command lines and exit codes, which are what a reviewer needs;
* prefer text transcripts to screenshots; blur names in screenshots you must take.

## Releases

A release issue lists the manual checks the release needs and links their evidence. The first release (0.1.0) needs all eight. Later releases need every check whose *Run when* column matches the changes since the previous release,
plus MT-007 for a minor release. Copy this into a new issue titled `Release x.y.z: manual verification`:

```markdown
## Release x.y.z manual verification

Previous release: x.y.(z-1)   Changes reviewed: <link to the compare view>

| Check | Required? | Why | Evidence | Signed off by | Date |
| --- | --- | --- | --- | --- | --- |
| MT-001 Mac mini enrollment | yes / no | <what changed> | <link> | <name> | <date> |
| MT-002 Google login | yes / no | | | | |
| MT-003 GitHub Actions OIDC | yes / no | | | | |
| MT-004 Mac host power, network, disk | yes / no | | | | |
| MT-005 macOS and Xcode upgrade | yes / no | | | | |
| MT-006 TUI feel | yes / no | | | | |
| MT-007 EKS install from the README | yes / no | | | | |
| MT-008 Storage-loss drill | yes / no | | | | |

- [ ] Every required check is signed off, or the release notes say which check was waived and why.
- [ ] Every defect found is an issue; defects that can be automated have a regression-test issue.
- [ ] Exploratory charters run for this release are listed below with their debriefs.
```

## Exploratory testing

Scripted checks find what you expected. Exploratory sessions find the rest. They are **time-boxed charters**: a mission, a time limit, notes taken as you go, and a short debrief. Run a few before each
release; they are cheap and they find different bugs.

**Rules.** A session is 60 to 90 minutes, one person, one charter. Take notes while you work. Do not fix bugs during the session; file them. End with the debrief.

**Charters to start with:**

| Charter | Mission |
| --- | --- |
| Cold start under mixed load | Explore what a developer sees when several builds hit pools that are at zero at the same time: queue time, error messages, `cucinactl` views. Find confusing states |
| Operator mistakes | Explore misconfigurations: a pool with a missing image, a deleted `WorkerPool` during scale-out, an invalid trust policy, `max: 0`. Find failures that are silent, slow or unclear |
| Login in hostile conditions | Explore `cucinactl login` and the credential helper with clock skew, a proxy, no network, an expired refresh token, two terminals at once. Find the worst message |
| Why is my build queued? | Starting only from `cucinactl`, answer "why is this action not running?" for a stuck platform. Find what a new engineer cannot discover |
| Runbooks, cold | Follow two runbooks from `docs/operations/` against a staging deployment without prior knowledge. Find missing commands and wrong assumptions |
| Windows client quirks | Explore a Windows developer's first hour: install, login, a Windows and a cross build. Find path, quoting and terminal problems |

**Debrief template** (put it in the release issue, or a comment on it):

```markdown
### Charter: <name>
- Tester / date / duration:
- Environment (version, deployment, client OS):
- What I covered:
- What I did not cover:
- Bugs filed: <links>
- Surprises and questions:
- Risks I now worry about:
- Follow-ups (new automated test, doc fix, new charter):
```

## Writing a check

A check is `MT-NNN.md` with front matter and four sections. [`tools/ci/check-manual-tests.sh`](../../../tools/ci/check-manual-tests.sh) verifies the structure.

```markdown
---
# SPDX-License-Identifier: FSL-1.1-ALv2
id: MT-009
title: <what it proves, in a line>
risks: [<requirement IDs or risk names>]
trigger: <when to run it>
owner: unassigned
last_run: never
sign_off: pending
---

# MT-009: <title>

<Two or three sentences: what real-world thing this proves that no automated tier can.>

## Preconditions
- <hardware, accounts, deployment state, tools, who must run it>

## Steps
1. [agent] <a step an agent may pre-run, with the exact command>
2. [human] <a step that needs a person>

## Expected results
- <observable outcomes, one per risk>

## Required evidence
- <what to attach, and what to redact>

## Sign-off
Run on: never. Signed off by: pending.
```

Which steps an agent can pre-run, listed from the files themselves: `tools/ci/check-manual-tests.sh --list-agent-steps` (see [`tools/ci/scriptable-steps.md`](../../../tools/ci/scriptable-steps.md)).
