#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# R-TEST-6 "Chart": `helm lint --strict` over the three test profiles and the samples,
# with Helm 4 ($HELM, default `helm`) and, when $HELM3 names a Helm 3.x binary, Helm 3
# (the chart supports both, apiVersion v2). Also `helm package` (what the release ships).
set -euo pipefail
chart="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "${TEST_SRCDIR:-}" && -d "${TEST_SRCDIR}/${TEST_WORKSPACE:-_main}/charts/cucina" ]]; then
  chart="${TEST_SRCDIR}/${TEST_WORKSPACE:-_main}/charts/cucina"
fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export HELM_CACHE_HOME="$tmp/cache" HELM_CONFIG_HOME="$tmp/config" HELM_DATA_HOME="$tmp/data"

lint() {
  local helm="$1"
  for values in "$chart"/ci/values-small.yaml "$chart"/ci/values-medium.yaml "$chart"/ci/values-large.yaml; do
    "$helm" lint --strict "$chart" -f "$values" >"$tmp/lint.log" 2>&1 || { cat "$tmp/lint.log"; echo "FAIL: $helm lint --strict -f $values"; exit 1; }
  done
  "$helm" lint --strict "$chart" -f "$chart/samples/pools.yaml" -f "$chart/samples/trust-policies.yaml" >"$tmp/lint.log" 2>&1 \
    || { cat "$tmp/lint.log"; echo "FAIL: $helm lint --strict with the samples"; exit 1; }
  echo "ok: $("$helm" version --short) lint --strict (3 profiles + samples)"
}

lint "${HELM:-helm}"
if [[ -n "${HELM3:-}" ]]; then
  lint "$HELM3"
fi
"${HELM:-helm}" package "$chart" --destination "$tmp/pkg" >/dev/null
if tar tzf "$tmp"/pkg/cucina-*.tgz | grep -qE '^cucina/(tests|ci)/'; then
  echo "FAIL: the packaged chart contains tests/ or ci/"; exit 1
fi
echo "ok: helm package"
