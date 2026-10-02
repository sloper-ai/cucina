#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Host-side acceptance of a built worker image (R-VER-3 smoke, NFR-P1 boot-to-ready, sizes):
#   test-image.sh <image> [report.json]
# 1. Clones <image> to a throwaway VM `cucina-imgtest-<random>` sized like hostd would size it on this host
#    ((cores-2)/SLOTS vCPUs, (RAM-8 GiB)/SLOTS memory; SLOTS=2), so nothing touches the image itself.
# 2. Boots it BOOTS times (first boot after clone, then restarts of the persistent VM) with hostd's flags
#    (`tart run --no-graphics --root-disk-opts=caching=cached,sync=none`) and records, per boot:
#    ip (tart ip --wait), agent (first `tart exec true`), session (build user owns the console = bb_runner can start).
# 3. Runs the in-guest smoke test (/usr/local/cucina/libexec/cucina-smoke) via `tart exec` on the first boot.
# 4. Records image size (logical and allocated), Xcode/macOS builds, then stops and DELETES the throwaway VM.
set -euo pipefail

die() { printf 'test-image: %s\n' "$*" >&2; exit 1; }
[ $# -ge 1 ] || die "usage: test-image.sh <image> [report.json]"
image="$1"
report="${2:-}"
BOOTS="${BOOTS:-3}"
SLOTS="${SLOTS:-2}"
READY_TIMEOUT="${READY_TIMEOUT:-180}"
: "${TART_HOME:?TART_HOME must point at the Tart home holding the image}"
export TART_HOME

now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }
with_timeout() { local s="$1"; shift; perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$s" "$@"; }
since() { perl -e 'printf "%.1f", $ARGV[1] - $ARGV[0]' "$1" "$2"; }

tart get "$image" >/dev/null 2>&1 || die "image $image not found in $TART_HOME"
vm="cucina-imgtest-$(openssl rand -hex 4)"
logdir="${LOG_DIR:-${CUCINA_DEV_STORAGE:-$HOME/Library/Caches/cucina}/macimage/logs}"
mkdir -p "$logdir"
runlog="$logdir/$vm.run.log"
run_pid=""

cleanup() {
  set +e
  if [ -n "$run_pid" ] && kill -0 "$run_pid" 2>/dev/null; then
    tart stop "$vm" --timeout 30 >/dev/null 2>&1
    wait "$run_pid" 2>/dev/null
  fi
  tart delete "$vm" >/dev/null 2>&1 && echo "deleted throwaway VM $vm"
}
trap cleanup EXIT

cores="$(sysctl -n hw.ncpu)"
mem_gib=$(($(sysctl -n hw.memsize) / 1073741824))
vcpus=${VCPUS:-$(((cores - 2) / SLOTS))}
mem_mb=${MEMORY_MIB:-$((((mem_gib - 8) / SLOTS) * 1024))}
[ "$(tart list --format json | jq '[.[] | select(.Running)] | length')" -lt 2 ] || die "both VM slots are occupied"
tart clone "$image" "$vm"
tart set "$vm" --cpu "$vcpus" --memory "$mem_mb"
# Tart rejects an equal-size resize; a clone already has the golden image's capacity.
disk_gb="$(tart get "$image" --format json | jq -r .Disk)"
echo "cloned $image -> $vm ($vcpus vCPUs, $((mem_mb / 1024)) GiB)"

boot_json=""
smoke_json='null'
guest_facts='null'
for i in $(seq 1 "$BOOTS"); do
  t0="$(now)"
  tart run "$vm" --no-graphics --root-disk-opts=caching=cached,sync=none >"$runlog" 2>&1 &
  run_pid=$!
  ip="$(tart ip "$vm" --wait "$READY_TIMEOUT" 2>/dev/null || true)"
  t_ip="$(now)"
  until with_timeout 10 tart exec "$vm" /usr/bin/true >/dev/null 2>&1; do
    [ "$(since "$t0" "$(now)" | cut -d. -f1)" -lt "$READY_TIMEOUT" ] || die "boot $i: guest agent not ready after ${READY_TIMEOUT}s"
    kill -0 "$run_pid" 2>/dev/null || die "boot $i: tart run exited: $(tail -n 3 "$runlog")"
    sleep 0.25
  done
  t_agent="$(now)"
  # Use hostd's actual manifest operation (cat), not a cold guest CoreFoundation/plutil process with a 10s deadline.
  # Guest startup/RPC reconnects can be transient; retry under the same overall readiness deadline.
  until guest_facts="$(with_timeout 30 tart exec "$vm" /bin/cat /usr/local/cucina/image.json 2>/dev/null)" &&
    build_user="$(printf '%s' "$guest_facts" | jq -er 'select(.schema == 1) | .buildUser | select(type == "string" and length > 0)' 2>/dev/null)"; do
    [ "$(since "$t0" "$(now)" | cut -d. -f1)" -lt "$READY_TIMEOUT" ] || die "boot $i: image manifest not readable after ${READY_TIMEOUT}s"
    kill -0 "$run_pid" 2>/dev/null || die "boot $i: tart run exited"
    sleep 0.25
  done
  until [ "$(with_timeout 30 tart exec "$vm" /usr/bin/stat -f %Su /dev/console 2>/dev/null)" = "$build_user" ]; do
    [ "$(since "$t0" "$(now)" | cut -d. -f1)" -lt "$READY_TIMEOUT" ] || die "boot $i: $build_user not logged in after ${READY_TIMEOUT}s"
    sleep 0.25
  done
  t_session="$(now)"
  echo "boot $i: ip ${ip:-?} after $(since "$t0" "$t_ip")s, guest agent $(since "$t0" "$t_agent")s, $build_user session $(since "$t0" "$t_session")s"
  boot_json="$boot_json${boot_json:+,}{\"boot\":$i,\"ip_s\":$(since "$t0" "$t_ip"),\"agent_s\":$(since "$t0" "$t_agent"),\"session_s\":$(since "$t0" "$t_session")}"

  if [ "$i" = 1 ]; then
    if out="$(with_timeout 300 tart exec "$vm" /usr/bin/sudo -n -- /usr/local/cucina/libexec/cucina-smoke 2>&1)"; then
      smoke_ok=true
    else
      smoke_ok=false
    fi
    printf '%s\n' "$out" | sed 's/^/  smoke: /'
    smoke_json="$(printf '%s\n' "$out" | tail -n 1)"
    case "$smoke_json" in "{"*) ;; *) smoke_json="{\"passed\":0,\"failed\":1,\"failures\":\"no summary\"}" ;; esac
    $smoke_ok || echo "SMOKE TEST FAILED" >&2
    render_ok=null
    if [ "$(printf '%s' "$guest_facts" | jq -r .workerAgent)" = present ]; then
      # The in-VM render call site with sample settings and a throwaway CA (this clone is deleted afterwards).
      ca="$(openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout /dev/null -subj /CN=imgtest-ca -days 1 2>/dev/null)"
      printf '%s\n' "$ca" | with_timeout 30 tart exec -i "$vm" /usr/bin/sudo -n -- /bin/sh -c 'cat > /etc/cucina/pki/ca.crt'
      settings='{"pool":"imgtest","node":"imgtest/'"$vm"'","runners":[{"name":"xcode","platform":[{"name":"OSFamily","value":"macos"},{"name":"ISA","value":"arm-a64"},{"name":"xcode-version","value":"'"$(printf '%s' "$guest_facts" | jq -r .xcode.version)"'"}]},{"name":"generic","platform":[{"name":"OSFamily","value":"macos"},{"name":"ISA","value":"arm-a64"}]}],"schedulerEndpoint":"192.0.2.1:8983","storageEndpoint":"192.0.2.1:8981","serverName":"workers.cucina.example","buildDirectory":"native","l1Placement":"vm-disk","maximumMessageSizeBytes":"16777216","sizeClass":1,"instanceNamePrefixes":["main"]}'
      if printf '%s' "$settings" | with_timeout 60 tart exec -i "$vm" /usr/bin/sudo -n -- /usr/local/cucina/libexec/cucina-render >"$logdir/$vm.render.json" 2>&1 &&
        with_timeout 20 tart exec "$vm" /usr/bin/plutil -convert xml1 -o /dev/null /etc/cucina/bb/worker.json; then
        render_ok=true
        echo "  render call site: ok ($(jq -c '[.directories[].Path]' "$logdir/$vm.render.json" 2>/dev/null))"
      else
        render_ok=false
        echo "  render call site: FAILED ($(tail -n 2 "$logdir/$vm.render.json"))" >&2
      fi
    fi
  fi
  tart stop "$vm" --timeout 60 >/dev/null
  wait "$run_pid" 2>/dev/null || true
  run_pid=""
done

vmdir="$TART_HOME/vms/$image"
disk_logical=$((disk_gb * 1000000000)) # Tart reports virtual capacity in decimal GB.
disk_backing=$(stat -f %z "$vmdir/disk.img") # Container EOF is not necessarily its virtual capacity.
disk_alloc=$(($(command du -k "$vmdir/disk.img" | awk '{print $1}') * 1024))
p50_agent="$(printf '%s' "[$boot_json]" | jq '[.[].agent_s] | sort | .[(length - 1) / 2 | floor]')"
max_agent="$(printf '%s' "[$boot_json]" | jq '[.[].agent_s] | max')"
p50_session="$(printf '%s' "[$boot_json]" | jq '[.[].session_s] | sort | .[(length - 1) / 2 | floor]')"
max_session="$(printf '%s' "[$boot_json]" | jq '[.[].session_s] | max')"

result="$(jq -n \
  --arg image "$image" --arg vm "$vm" --argjson vcpus "$vcpus" --argjson mem_mb "$mem_mb" \
  --argjson boots "[$boot_json]" --argjson smoke "$smoke_json" --argjson guest "$guest_facts" --argjson render "${render_ok:-null}" \
  --argjson logical "$disk_logical" --argjson backing "$disk_backing" --argjson alloc "$disk_alloc" \
  --argjson p50a "$p50_agent" --argjson maxa "$max_agent" --argjson p50s "$p50_session" --argjson maxs "$max_session" \
  '{image: $image, testVm: $vm, vm: {vcpus: $vcpus, memoryMiB: $mem_mb},
    boots: $boots, bootToReady: {agentP50: $p50a, agentMax: $maxa, sessionP50: $p50s, sessionMax: $maxs},
    disk: {logicalBytes: $logical, backingFileBytes: $backing, allocatedBytes: $alloc},
    guest: $guest, smoke: $smoke, renderCallSite: $render}')"
if [ -n "$report" ]; then
  mkdir -p "$(dirname "$report")"
  printf '%s\n' "$result" >"$report"
  echo "report: $report"
fi
printf '%s\n' "$result" | jq -c '{image, bootToReady, disk, smoke: {passed: .smoke.passed, failed: .smoke.failed}}'
[ "$(printf '%s' "$smoke_json" | jq -r .failed)" = "0" ] && [ "${render_ok:-false}" = true ]
