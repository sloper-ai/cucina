<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Manual-test steps an agent may pre-run

Every step of a manual check ([`docs/testing/manual/`](../../docs/testing/manual/README.md)) is marked `[agent]` or `[human]`. An `[agent]` step is a command or a
read-only observation that a coding agent (or a script) can run and whose output a person can review. A `[human]` step needs a physical action, a browser, or a judgement.
The marker is the source of truth; this page only explains the convention. To list the pre-runnable steps of every check:

```sh
tools/ci/check-manual-tests.sh --list-agent-steps
```

## Rules for pre-running

1. **A person signs off, always.** Pre-running saves time; it does not replace the check. Attach the output to the release issue and let the human review it.
2. **Only against the right deployment.** Run against a staging or acceptance deployment. Steps that destroy data or interrupt hardware (MT-004's disk fill, MT-008's storage deletion) are marked
   `[human]` for that reason: an agent does not run them unattended.
3. **Read before you write.** Run the observation steps first (`status`, `list`, `describe`, `whoami`), then the ones that change state.
4. **Redact before attaching.** Tokens, keys and JWTs never leave the terminal; serial numbers, addresses and account IDs become placeholders ([evidence rules](../../docs/testing/manual/README.md#evidence)).
5. **Stop on a surprise.** If a step does not behave as the check says, record it and stop; do not improvise a workaround.

## What an agent needs

| Need | For |
| --- | --- |
| An admin session: `cucinactl login <sts address>` (a person completes any browser step) | every `cucinactl` step |
| `kubectl` access to the deployment's namespace | cluster-side observations |
| A read-only AWS role in the deployment's account (tag-filtered describes) | MT-007 and the cost and orphan checks |
| SSH or the MDM's remote tool to the Mac host | MT-001, MT-004, MT-005 |
| The ability to run `bazelisk` against the deployment | the build steps |
