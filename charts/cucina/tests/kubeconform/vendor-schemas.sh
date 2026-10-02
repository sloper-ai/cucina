#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Vendors JSON schemas of the third-party custom resources the chart renders, for
# kubeconform -strict without network access (R-TEST-6 "Chart"). Source:
# datreeio/CRDs-catalog at a pinned commit (Apache-2.0). Core Kubernetes schemas
# are vendored by tools/kubeconform/vendor-schemas.sh; the chart's own CRDs are
# converted from api/crds at test time. ScrapeConfig (680 KB) and GRPCRoute are
# skipped by the test instead of vendored.
# Usage: charts/cucina/tests/kubeconform/vendor-schemas.sh
set -euo pipefail
COMMIT="d373c2da9702bc9509a004db83e57263fe3bdfc1"   # 2026-10-02
SCHEMAS=(
  monitoring.coreos.com/servicemonitor_v1
  monitoring.coreos.com/prometheusrule_v1
  cert-manager.io/certificate_v1
)
dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/schemas"
for s in "${SCHEMAS[@]}"; do
  mkdir -p "${dir}/$(dirname "$s")"
  curl -fsSL --retry 3 -o "${dir}/${s}.json" "https://raw.githubusercontent.com/datreeio/CRDs-catalog/${COMMIT}/${s}.json"
done
echo "vendored ${#SCHEMAS[@]} schemas into ${dir}"
