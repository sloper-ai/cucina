#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# helm-unittest (plugin v1.2.0) suites in tests/unittest: only conditionals and defaults
# that matter (R-TEST-6 "Chart"). $HELM_PLUGINS must contain the plugin (Bazel passes the
# pinned @helm_unittest; locally $CUCINA_DEV_STORAGE/helm-plugins).
set -euo pipefail
chart="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "${TEST_SRCDIR:-}" && -d "${TEST_SRCDIR}/${TEST_WORKSPACE:-_main}/charts/cucina" ]]; then
  chart="${TEST_SRCDIR}/${TEST_WORKSPACE:-_main}/charts/cucina"
fi
if [[ -n "${HELM_UNITTEST_PLUGIN_YAML:-}" ]]; then
  # Bazel: HELM_PLUGINS is the directory holding the plugin directory.
  plugin_dir="$(cd "$(dirname "$HELM_UNITTEST_PLUGIN_YAML")" && pwd)"
  export HELM_PLUGINS="$(dirname "$plugin_dir")"
fi
: "${HELM_PLUGINS:?set HELM_PLUGINS to a directory containing the helm-unittest plugin}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export HELM_CACHE_HOME="$tmp/cache" HELM_CONFIG_HOME="$tmp/config" HELM_DATA_HOME="$tmp/data"
# helm-unittest writes nothing into the chart; copy it anyway so read-only runfiles work.
cp -RL "$chart" "$tmp/cucina"
exec "${HELM:-helm}" unittest -f 'tests/unittest/*_test.yaml' "$tmp/cucina"
