#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runs the acceptance campaign (PROMPT §10.3) with the scenario harness, phase by phase. It creates no AWS resources
# itself: the lead brings the environment up first (deploy/aws-e2e/scripts/up.sh all, helm values, kubeconfig, keys,
# `e2e env` descriptor). Scenarios that need what the environment lacks SKIP with a reason; the budget governor
# ($300, §12) holds or skips scenarios whose projected spend would exceed the budget.
#
#   run.sh [--env NAME] PHASE...
#
# Phases (in campaign order; "all" runs every phase, teardown last):
#   preflight  install  baselines  linux  windows  concurrency  faults  security  upgrade  rollout  mac  pkg
#   cross  cli  data  dogfood  canaries  teardown  report
#
# Each phase logs to $CUCINA_DEV_STORAGE/logs/campaign-<phase>.log; results land in
# <artifactsDir>/<runId>/results (result-<ID>.json, budget.json). `report` writes docs/reports/e2e-<date>.md (redacted).
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH='' cd -- "$here/../../../.." && pwd)
env=${CUCINA_E2E_ENV:-aws-e2e}
if [ "${1:-}" = "--env" ]; then env=$2 && shift 2; fi
[ $# -gt 0 ] || { sed -n '3,20p' "$0" >&2; exit 2; }
logs=${CUCINA_DEV_STORAGE:?source .work/env.sh first}/logs
mkdir -p "$logs"

ids() {
	case $1 in
	install) echo T0 ;;
	baselines) echo baseline-linux,baseline-windows,baseline-macos ;;
	linux) echo T1,T2,T3 ;;
	windows) echo T4,T5,T6 ;;
	concurrency) echo T7,T8 ;;
	faults) echo T9a,T9b,T9c,T9d,T9e,T9f ;;
	security) echo T10a,T10b,T10c,T10d,T10e,T10f,T10g,T10h,T10i ;;
	upgrade) echo T11 ;;
	rollout) echo T12 ;;
	mac) echo T13 ;;
	pkg) echo T14 ;;
	cross) echo T16,T17,T18,T19 ;;
	cli) echo T20 ;;
	data) echo T21 ;;
	dogfood) echo T22 ;;
	canaries) echo canary-cache,canary-exec,zero-scale ;;
	teardown) echo T15 ;;
	*) return 1 ;;
	esac
}

scenario() { # phase
	list=$(ids "$1") || { echo "unknown phase $1" >&2; exit 2; }
	echo "== $1: $list"
	(cd "$repo" && go test ./test/e2e -count=1 -timeout 24h -run TestScenario -args -env "$env" -id "$list") \
		>"$logs/campaign-$1.log" 2>&1 && echo "   ok" || echo "   see $logs/campaign-$1.log (failures do not stop the campaign)"
	grep -E '^\s+scenario_test.go:[0-9]+: ' "$logs/campaign-$1.log" | sed 's/^ *scenario_test.go:[0-9]*: /   /' || true
}

preflight() {
	desc=$HOME/.config/cucina/e2e/$env.json
	[ -f "$desc" ] || { echo "no $desc: run go run ./test/e2e/cmd/e2e env --base … --env …" >&2; exit 1; }
	aws sts get-caller-identity --query Arn --output text >/dev/null || { echo "AWS session expired: aws sso login (ask the user)" >&2; exit 1; }
	kubectl --kubeconfig "${KUBECONFIG:-$HOME/.config/cucina/aws-e2e/kubeconfig}" get nodes >/dev/null || { echo "cluster unreachable" >&2; exit 1; }
	"$here/port-forwards.sh" up
	echo "preflight ok ($env)"
}

for phase in "$@"; do
	case $phase in
	preflight) preflight ;;
	report) (cd "$repo" && go run ./test/e2e/cmd/e2e report --env "$env") ;;
	all)
		preflight
		for p in install baselines linux windows concurrency faults security upgrade rollout mac pkg cross cli data dogfood canaries teardown; do
			scenario "$p"
		done
		(cd "$repo" && go run ./test/e2e/cmd/e2e report --env "$env")
		;;
	security)
		"$here/mock-oauth2-server.sh" up
		"$here/port-forwards.sh" up
		scenario security
		;;
	*) scenario "$phase" ;;
	esac
done
