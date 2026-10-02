#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Local port-forwards the scenario harness expects on the dev Mac (environment descriptor):
#   127.0.0.1:9090  in-cluster Prometheus           (endpoints.prometheus = http://127.0.0.1:9090)
#   127.0.0.1:18443 mock-oauth2-server (T10, ADR 1003) (idp.localAddr = 127.0.0.1:18443)
#
#   port-forwards.sh up|down|status
#
# Environment: KUBECONFIG (default ~/.config/cucina/aws-e2e/kubeconfig), PROM_NAMESPACE (monitoring),
# PROM_SERVICE (kube-prometheus-stack-prometheus), MOCK_NAMESPACE (cucina-e2e). PID files and logs go to
# $CUCINA_DEV_STORAGE/e2e/port-forwards (no secrets there).
set -eu
export KUBECONFIG="${KUBECONFIG:-${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e/kubeconfig}"
STATE=${CUCINA_DEV_STORAGE:-$HOME/.cache/cucina}/e2e/port-forwards
PROM_NS=${PROM_NAMESPACE:-monitoring}
PROM_SVC=${PROM_SERVICE:-kube-prometheus-stack-prometheus}
MOCK_NS=${MOCK_NAMESPACE:-cucina-e2e}

start() { # name namespace target ports
	name=$1
	if [ -f "$STATE/$name.pid" ] && kill -0 "$(cat "$STATE/$name.pid")" 2>/dev/null; then
		echo "$name: already running"
		return 0
	fi
	nohup kubectl -n "$2" port-forward --address 127.0.0.1 "$3" "$4" >"$STATE/$name.log" 2>&1 &
	echo $! >"$STATE/$name.pid"
	echo "$name: started ($3 → 127.0.0.1:${4%%:*})"
}

stop() {
	for f in "$STATE"/*.pid; do
		[ -f "$f" ] || continue
		kill "$(cat "$f")" 2>/dev/null || true
		rm -f "$f"
		echo "stopped $(basename "$f" .pid)"
	done
}

mkdir -p "$STATE"
case ${1:-} in
up)
	start prometheus "$PROM_NS" "svc/$PROM_SVC" 9090:9090
	if kubectl get ns "$MOCK_NS" >/dev/null 2>&1; then
		start mock-idp "$MOCK_NS" svc/mock-oauth2-server 18443:30443
	fi
	;;
down) stop ;;
status)
	for f in "$STATE"/*.pid; do
		[ -f "$f" ] || continue
		if kill -0 "$(cat "$f")" 2>/dev/null; then echo "$(basename "$f" .pid): running"; else echo "$(basename "$f" .pid): dead"; fi
	done
	;;
*) echo "usage: port-forwards.sh up|down|status" >&2 && exit 2 ;;
esac
