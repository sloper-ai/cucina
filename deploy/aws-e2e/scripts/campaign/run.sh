#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Run acceptance phases against an environment the lead provisioned. Scenarios
# can scale workers, inject faults and destroy tagged resources; this is NOT an
# offline check. Use `e2e check` for local prerequisite validation first.
# Usage: run.sh [--env NAME] PHASE...
# `all` attempts teardown even after failure/interruption; skipped/failed/error
# results return nonzero, never an "ok" campaign. Report generation still runs.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH='' cd -- "$here/../../../.." && pwd)
env=${CUCINA_E2E_ENV:-aws-e2e}
if [ "${1:-}" = --env ]; then env=$2; shift 2; fi
[ $# -gt 0 ] || { echo 'usage: run.sh [--env NAME] preflight|install|baselines|linux|windows|concurrency|faults|security|upgrade|rollout|mac|pkg|cross|cli|data|dogfood|canaries|teardown|report|all ...' >&2; exit 2; }
logs=${CUCINA_DEV_STORAGE:?source .work/env.sh first}/logs
mkdir -p "$logs"
desc=$HOME/.config/cucina/e2e/$env.json
[ -f "$desc" ] || { echo 'No descriptor: generate it with e2e env first.' >&2; exit 1; }
results=$(jq -er '.artifactsDir+"/"+.runId+"/results"' "$desc")
status=0
teardown_due=0

ids() {
 case $1 in
 install) echo T0 ;;
 baselines) echo baseline-linux,baseline-windows,baseline-macos ;;
 linux) echo T1,T2,T3 ;;
 windows) echo T4,T5,T6 ;;
 concurrency) echo T7,T8 ;;
 faults) echo T9a,T9b,T9c,T9d,T9e,T9f ;;
 security) echo T10a,T10b,T10c,T10d,T10e,T10f,T10g,T10h,T10i ;;
 upgrade) echo T11 ;; rollout) echo T12 ;; mac) echo T13 ;; pkg) echo T14 ;;
 cross) echo T16,T17,T18,T19 ;; cli) echo T20 ;; data) echo T21 ;; dogfood) echo T22 ;;
 canaries) echo canary-cache,canary-exec,zero-scale ;; teardown) echo T15 ;;
 *) return 1 ;;
 esac
}
cli() { (cd "$repo" && go run ./test/e2e/cmd/e2e "$@"); }
scenario() {
 phase=$1
 list=$(ids "$phase") || { echo "unknown phase $phase" >&2; status=1; return; }
 echo "== $phase: $list"
 if ! (cd "$repo" && go test ./test/e2e -count=1 -timeout 24h -run '^TestScenario$' -args -env "$env" -id "$list") >"$logs/campaign-$phase.log" 2>&1; then
  status=1
  echo "   test process failed; see $logs/campaign-$phase.log"
 fi
 for id in $(printf '%s' "$list" | tr ',' ' '); do
  result=$results/result-$id.json
  if [ ! -f "$result" ]; then echo "   $id ERROR: no result written"; status=1; continue; fi
  jq -r '"   "+.id+" "+(.status|ascii_upcase)+": "+(.skipReason // .error // "")' "$result"
  if ! jq -e '.status=="pass" or (.status=="functional-pass" and .measurementScope=="small-functional")' "$result" >/dev/null; then status=1; fi
 done
}
preflight() {
 cli check --env "$env" --id T0 || return 1
 aws --profile default --region us-west-1 sts get-caller-identity >/dev/null || { echo 'AWS session unavailable: stop and ask the lead to arrange login.' >&2; return 1; }
 export KUBECONFIG
 KUBECONFIG=$(jq -er '.kubernetes.kubeconfig' "$desc")
 kubectl --kubeconfig "$KUBECONFIG" get nodes >/dev/null || return 1
 # The lead owns Prometheus deployment and may already forward it. Require
 # the descriptor's exact URL rather than silently starting a different one.
 prom=$(jq -er '.endpoints.prometheus' "$desc")
 curl --fail --silent --show-error --max-time 10 "$prom/-/ready" >/dev/null || { echo 'Prometheus is not reachable; run the configured port-forward.' >&2; return 1; }
 echo 'preflight ready (connectivity only; not an acceptance verdict)'
}
security() {
 if ! "$here/mock-oauth2-server.sh" up || ! "$here/port-forwards.sh" up mock-idp; then status=1; return; fi
 scenario security
}
# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
 incoming=$?
 trap - EXIT INT TERM
 if [ "$teardown_due" = 1 ]; then
  teardown_due=0
  scenario teardown
  cli report --env "$env" || status=1
 fi
 if [ "$incoming" -ne 0 ]; then exit "$incoming"; fi
 exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for phase in "$@"; do
 case $phase in
 preflight) preflight || status=1 ;;
 report) cli report --env "$env" || status=1 ;;
 all)
  # Do not start a campaign whose mandatory cleanup would be skipped.
  jq -e '.kind=="aws-e2e" and .safety.allowDestructive==true and (.capabilities|index("destructive")!=null)' "$desc" >/dev/null || { echo 'all requires explicit destructive/teardown permission in the descriptor; refusing workloads' >&2; exit 2; }
  teardown_due=1
  if preflight; then
   for p in install baselines linux windows concurrency faults security upgrade rollout mac pkg cross cli data dogfood canaries; do
    if [ "$p" = security ]; then security; else scenario "$p"; fi
   done
  else status=1
  fi
  teardown_due=0
  scenario teardown
  cli report --env "$env" || status=1
  ;;
 security) security ;;
 *) scenario "$phase" ;;
 esac
done
exit "$status"
