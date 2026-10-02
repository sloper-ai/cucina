#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# chart-testing lint (R-TEST-6 "Chart"): `ct lint` over every ci values file, offline
# (no maintainer/version-bump checks, which need GitHub and git history). ct discovers
# ci/*-values.yaml, so the profiles are linted from a scratch copy that renames
# ci/values-<size>.yaml to ci/<size>-values.yaml. `ct install` (kind) is the system tier.
set -euo pipefail
chart="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ct="${CT:-ct}"
command -v "$ct" >/dev/null || { echo "SKIP: chart-testing (ct) not found; set CT"; exit 0; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export HELM_CACHE_HOME="$tmp/cache" HELM_CONFIG_HOME="$tmp/config" HELM_DATA_HOME="$tmp/data"
mkdir -p "$tmp/charts"
cp -RL "$chart" "$tmp/charts/cucina"
for f in "$tmp"/charts/cucina/ci/values-*.yaml; do
  base="$(basename "$f" .yaml)"
  mv "$f" "$tmp/charts/cucina/ci/${base#values-}-values.yaml"
done
cd "$tmp"
yaml_args=(--validate-yaml=false)
if command -v yamllint >/dev/null && [[ -n "${CT_LINTCONF:-}" ]]; then
  yaml_args=(--validate-yaml=true --lint-conf "$CT_LINTCONF")
fi
"$ct" lint --charts charts/cucina --chart-dirs charts \
  --validate-maintainers=false --check-version-increment=false --validate-chart-schema=false "${yaml_args[@]}"
