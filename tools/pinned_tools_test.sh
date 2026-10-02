#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Guards R-BUILD-1/R-BUILD-2: every pinned tool binary resolves for the platform
# the test runs on and executes (catches a wrong select() branch, a bad archive
# layout or a binary for the wrong OS/CPU). Output version strings for the log.
# Usage: pinned_tools_test.sh <name>=<rootpath> ...
set -euo pipefail

export HOME="${TEST_TMPDIR}" XDG_CACHE_HOME="${TEST_TMPDIR}/cache" XDG_CONFIG_HOME="${TEST_TMPDIR}/config"
export HELM_CACHE_HOME="${TEST_TMPDIR}/helm/cache" HELM_CONFIG_HOME="${TEST_TMPDIR}/helm/config" HELM_DATA_HOME="${TEST_TMPDIR}/helm/data"

declare_tool() { eval "tool_${1//-/_}=\"$2\""; }
for arg in "$@"; do declare_tool "${arg%%=*}" "${arg#*=}"; done

failures=0
check() { # check <label> <expected substring> <command...>
  local label="$1" want="$2" out
  shift 2
  out="$("$@" 2>&1 || true)"
  if [[ "${out}" == *"${want}"* ]]; then
    printf 'ok   %-16s %s\n' "${label}" "$(printf '%s' "${out}" | head -n1)"
  else
    printf 'FAIL %-16s expected %q in output of: %s\n%s\n' "${label}" "${want}" "$*" "${out}" >&2
    failures=$((failures + 1))
  fi
}

check helm "v4.3.0" "${tool_helm}" version --short
HELM_PLUGINS="$(dirname "$(dirname "${tool_helm_unittest_plugin}")")" \
  check helm-unittest "unittest" "${tool_helm}" plugin list
check kubeconform "v0.8.0" "${tool_kubeconform}" -v
check buf "1.73.0" "${tool_buf}" --version
check controller-gen "v0.21.0" "${tool_controller_gen}" --version
check gitleaks "8.30.1" "${tool_gitleaks}" version
check promtool "3.15.0" "${tool_promtool}" --version
check tflint "0.64.0" "${tool_tflint}" --version
check tofu "v1.13.1" "${tool_tofu}" version
check etcd "etcd Version" "${tool_etcd}" --version
check kube-apiserver "Kubernetes v1.36.2" "${tool_kube_apiserver}" --version
check kubectl "v1.36.2" "${tool_kubectl}" version --client
for bb in bb_storage bb_scheduler bb_worker bb_runner; do
  var="tool_${bb}"
  check "${bb}" "Usage" "${!var}"
done

exit $((failures > 0))
