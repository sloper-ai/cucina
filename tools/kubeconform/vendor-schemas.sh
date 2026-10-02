#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Vendors Kubernetes JSON schemas for kubeconform so chart tests never touch the
# network (R-BUILD-1 "Chart tests"). Source: yannh/kubernetes-json-schema at a
# pinned commit. Add a kind by appending to KINDS and re-running.
# Upstream has no strict CustomResourceDefinition schema: run kubeconform with
# `-skip CustomResourceDefinition` (CRDs are validated by envtest instead).
# Usage: tools/kubeconform/vendor-schemas.sh
set -euo pipefail

COMMIT="8df8a883b68a24a104b4a9e43c1288090ae60b3b"   # 2026-09-29
K8S_VERSION="v1.36.5"                                 # k3s v1.36.5+k3s1 (R-LIB-4)
FLAVOUR="standalone-strict"
KINDS=(
  clusterrole-rbac-v1 clusterrolebinding-rbac-v1 configmap-v1 cronjob-batch-v1
  deployment-apps-v1
  horizontalpodautoscaler-autoscaling-v2 ingress-networking-v1 job-batch-v1
  lease-coordination-v1 namespace-v1 networkpolicy-networking-v1
  persistentvolumeclaim-v1 pod-v1 poddisruptionbudget-policy-v1 priorityclass-scheduling-v1
  role-rbac-v1 rolebinding-rbac-v1 secret-v1 service-v1 serviceaccount-v1 statefulset-apps-v1
  validatingwebhookconfiguration-admissionregistration-v1
)

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/schemas/${K8S_VERSION}-${FLAVOUR}"
mkdir -p "${dir}"
for kind in "${KINDS[@]}"; do
  curl -fsSL --retry 3 -o "${dir}/${kind}.json" \
    "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/${COMMIT}/${K8S_VERSION}-${FLAVOUR}/${kind}.json"
done
echo "vendored ${#KINDS[@]} schemas into ${dir}"
