#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Refresh and print the OpenTofu outputs of the e2e layers.
#
#   outputs.sh [base|env]            refresh <layer>-outputs.json from state, print its path and keys
#   outputs.sh base|env <key>        print one value (raw; JSON for lists/objects)
#   outputs.sh --json base|env       print the whole outputs object
#
# The files live in ~/.config/cucina/aws-e2e/ (0600). Outputs hold resource IDs and the
# environment's addresses, so they never go into the repository.

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
  e2e_usage "$0"
  exit "${1:-0}"
}

as_json=0
if [ "${1:-}" = "--json" ]; then
  as_json=1
  shift
fi
case "${1:-}" in
  base | env) layer=$1 ;;
  -h | --help) usage 0 ;;
  *) usage 1 ;;
esac
key=${2:-}

e2e_require tofu jq
e2e_setup
e2e_state_exists "$layer" || e2e_die "layer '$layer' has no state in $E2E_STATE_DIR/$layer"
e2e_tofu "$layer" init -input=false -no-color >/dev/null 2>&1 || e2e_die "tofu init failed for $layer"
e2e_write_outputs "$layer"
file="$E2E_STATE_DIR/$layer-outputs.json"

if [ -n "$key" ]; then
  jq -er --arg k "$key" 'if has($k) then .[$k] else error("no such output: " + $k) end | if type == "string" then . else tojson end' "$file"
elif [ "$as_json" = 1 ]; then
  jq . "$file"
else
  printf '%s\n' "$file"
  jq -r 'keys | join(" ")' "$file"
fi
