#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# R-CACHE-3 measurement: NFSv4 virtual build directory vs native build directory inside a Tart guest.
#   bench-build-dir.sh <image> [mode ...]   modes: native, nfsv4 (bb_worker as the image's workerUser),
#                                           <mode>-builder (diagnostic only); default: native nfsv4
# * Clones <image> to a throwaway VM (cucina-imgtest-bdx-*; deleted at exit), sized like hostd would on this host,
#   boots it with hostd's flags and configures bb_worker/bb_runner exactly like hostd's activation (docs/dev/hostd.md
#   §1.2: files under /etc/cucina, launchd bootstrap of the image's plists, bb_worker as the build user).
# * Runs a local bb_storage (frontend + memory CAS/AC/FSAC, pinned 086b011) and bb_scheduler (pinned 1a3be95) on this
#   host, listening on loopback (Bazel) and on the vmnet gateway address (the VM), plaintext: test only.
# * Per mode: L1 wiped, worker started, time to the first remote action, then RUNS Bazel runs of the workload from
#   gen-workspace.sh with --noremote_accept_cached (run 1: cold worker caches; runs 2..: warm), then bb_worker's
#   per-stage execution metrics. Any failed run counts against the mode's reliability.
# Env: RUNS (default 5), WORK (default $CUCINA_DEV_STORAGE/macimage/bench), UNITS/HEADERS (workload size),
#      SLOTS (VMs per host for sizing, default 2), KEEP_VM=1 (leave the VM for debugging).
set -euo pipefail

die() { printf 'bench: %s\n' "$*" >&2; exit 1; }
[ $# -ge 1 ] || die "usage: bench-build-dir.sh <image> [native|nfsv4 ...]"
image="$1"
shift
modes=("$@")
[ ${#modes[@]} -gt 0 ] || modes=(native nfsv4)
here="$(cd "$(dirname "$0")" && pwd)"
tmpl="$here/templates"
: "${CUCINA_DEV_STORAGE:?source .work/env.sh first}"
: "${TART_HOME:?}"
RUNS="${RUNS:-5}"
SLOTS="${SLOTS:-2}"
BAZEL="${BAZEL:-bazelisk}"
WORK="${WORK:-$CUCINA_DEV_STORAGE/macimage/bench}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
logs="$WORK/logs/$stamp"
mkdir -p "$logs"

bbs_rel=20260930T153215Z-086b011
bbre_rel=20260930T173749Z-1a3be95
storage_bin="$CUCINA_DEV_STORAGE/bb-release/$bbs_rel/bb_storage.darwin_arm64"
scheduler_bin="$CUCINA_DEV_STORAGE/bb-release/$bbre_rel/bb_scheduler.darwin_arm64"
for b in "$storage_bin" "$scheduler_bin"; do
  [ -x "$b" ] || die "missing $b (fetched by the bbconfig tooling)"
  want="$(awk -v n="assets/$(basename "$b")" '$2 == n {print $1}' "$(dirname "$b")/sha256")"
  [ "$(shasum -a 256 "$b" | awk '{print $1}')" = "$want" ] || die "$b does not match its release sha256"
done

now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }
since() { perl -e 'printf "%.1f", $ARGV[1] - $ARGV[0]' "$1" "$2"; }
with_timeout() { local s="$1"; shift; perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$s" "$@"; }
vexec() { with_timeout 120 tart exec "$vm" /usr/bin/sudo -n -- "$@"; }

vm="cucina-imgtest-bdx-$(openssl rand -hex 3)"
run_pid=""
server_pids=()
cleanup() {
  set +e
  for p in "${server_pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  if [ "${KEEP_VM:-}" = 1 ]; then echo "keeping $vm"; return; fi
  if [ -n "$run_pid" ]; then tart stop "$vm" --timeout 30 >/dev/null 2>&1; wait "$run_pid" 2>/dev/null; fi
  tart delete "$vm" >/dev/null 2>&1 && echo "deleted throwaway VM $vm"
}
trap cleanup EXIT

# --- Workload.
ws="$WORK/ws-${UNITS:-200}-${HEADERS:-2000}"
"$here/gen-workspace.sh" "$ws" "${UNITS:-200}" "${HEADERS:-2000}" >/dev/null

# --- Worker VM.
cores="$(sysctl -n hw.ncpu)"
mem_gib=$(($(sysctl -n hw.memsize) / 1073741824))
vcpus=${VCPUS:-$(((cores - 2) / SLOTS))}
mem_mb=${MEMORY_MIB:-$((((mem_gib - 8) / SLOTS) * 1024))}
[ "$(tart list --format json | jq '[.[] | select(.Running)] | length')" -lt 2 ] || die "both VM slots are occupied"
tart clone "$image" "$vm"
tart set "$vm" --cpu "$vcpus" --memory "$mem_mb"
t_boot="$(now)"
tart run "$vm" --no-graphics --root-disk-opts=caching=cached,sync=none >"$logs/tart-run.log" 2>&1 &
run_pid=$!
ready_deadline=$((SECONDS + 180))
until with_timeout 10 tart exec "$vm" /usr/bin/true >/dev/null 2>&1; do
  kill -0 "$run_pid" 2>/dev/null || die "tart run exited"
  [ "$SECONDS" -lt "$ready_deadline" ] || die "guest agent was not ready within 180s"
  sleep 0.25
done
t_agent="$(now)"
until manifest="$(with_timeout 30 tart exec "$vm" /bin/cat /usr/local/cucina/image.json 2>/dev/null)" &&
  build_user="$(printf '%s' "$manifest" | jq -er 'select(.schema == 1) | .buildUser' 2>/dev/null)"; do
  [ "$SECONDS" -lt "$ready_deadline" ] || die "image manifest was not readable within 180s"
  sleep 0.25
done
until console="$(with_timeout 30 tart exec "$vm" /usr/bin/stat -f '%Su %u %g' /dev/console 2>/dev/null)" &&
  [ "${console%% *}" = "$build_user" ]; do
  [ "$SECONDS" -lt "$ready_deadline" ] || die "build user's GUI session was not ready within 180s"
  sleep 0.25
done
t_session="$(now)"
read -r _ uid gid <<<"$console"
xcode_version="$(printf '%s' "$manifest" | jq -r .xcode.version)"
xcode_override="$(printf '%s' "$manifest" | jq -r .xcode.xcodeVersionOverride)"
vm_ip="$(tart ip "$vm" --wait 30)"
iface="$(route -n get "$vm_ip" | awk '/interface:/ {print $2}')"
# vmnet's bridge has no ipconfig network-service record. Its actual address is the guest's default gateway.
gw="$(vexec /sbin/route -n get default | awk '$1 == "gateway:" {print $2}')"
if [ -z "$gw" ]; then die "guest has no default IPv4 gateway"; fi
if ! /sbin/ifconfig "$iface" | awk -v gw="$gw" '$1 == "inet" && $2 == gw {found=1} END {exit !found}'; then
  die "guest default gateway is not an address on host interface $iface"
fi
echo "VM $vm ($vcpus vCPUs, $((mem_mb / 1024)) GiB) at $vm_ip via $iface/$gw: agent $(since "$t_boot" "$t_agent")s, session $(since "$t_boot" "$t_session")s"

# --- Host storage + scheduler (plaintext, loopback + vmnet gateway only).
sp=18980 cp=18982 wp=18983 bq=18984
for p in $sp $cp $wp $bq; do nc -z 127.0.0.1 "$p" 2>/dev/null && die "port $p is busy"; done
sed -e "s|\"@LISTEN@\"|\"127.0.0.1:$sp\", \"$gw:$sp\"|" -e "s|@SCHEDULER@|127.0.0.1:$cp|" "$tmpl/storage.json" >"$logs/storage.json"
sed -e "s|@CLIENT_LISTEN@|127.0.0.1:$cp|" -e "s|@WORKER_LISTEN@|$gw:$wp|" -e "s|@BQS_LISTEN@|127.0.0.1:$bq|" \
  -e "s|@STORAGE@|127.0.0.1:$sp|" -e "s|@XCODE@|$xcode_version|" "$tmpl/scheduler.json" >"$logs/scheduler.json"
"$storage_bin" "$logs/storage.json" >"$logs/bb_storage.log" 2>&1 &
server_pids+=($!)
"$scheduler_bin" "$logs/scheduler.json" >"$logs/bb_scheduler.log" 2>&1 &
server_pids+=($!)
for p in $sp $cp; do
  for _ in $(seq 1 100); do nc -z 127.0.0.1 "$p" 2>/dev/null && break; sleep 0.1; done
  nc -z 127.0.0.1 "$p" || die "server on :$p did not start (see $logs)"
done

bazel_flags=(
  --remote_executor="grpc://127.0.0.1:$sp" --remote_instance_name= --spawn_strategy=remote --remote_local_fallback=false
  --remote_default_exec_properties=OSFamily=macos --remote_default_exec_properties=ISA=arm-a64
  --remote_download_minimal --noremote_accept_cached --remote_timeout=600 --jobs=64 --disk_cache=
  --incompatible_strict_action_env --show_result=0 --color=no --curses=no
)
bazel_startup=(--output_user_root="$WORK/bazel-root")
(cd "$ws" && "$BAZEL" "${bazel_startup[@]}" info release >/dev/null 2>&1)

render() { # build kind (native|nfsv4), out dir -> bb/worker.json, bb/runner.json
  local mode="$1" out="$2"
  mkdir -p "$out/bb"
  jq -n --slurpfile c "$tmpl/worker-common.json" --slurpfile b "$tmpl/build-$mode.json" \
    --argjson conc "$vcpus" --arg node "bench/$vm" --arg xcode "$xcode_version" --arg mode "$mode" \
    --argjson uid "$uid" --argjson gid "$gid" --arg sp "$gw:$sp" '
    def runner($props): {endpoint: {address: "unix://@RUNNER_SOCKET@"}, concurrency: $conc, instanceNamePrefix: "",
      platform: {properties: $props}, sizeClass: 1, maximumFilePoolFileCount: "100000",
      maximumFilePoolSizeBytes: "4294967296", workerId: {pool: "bench", node: $node}}
      + (if $mode == "nfsv4" then {buildDirectoryOwnerUserId: $uid, buildDirectoryOwnerGroupId: $gid} else {} end);
    $c[0] + {buildDirectories: [$b[0] + {runners: [
      runner([{name: "ISA", value: "arm-a64"}, {name: "OSFamily", value: "macos"}, {name: "xcode-version", value: $xcode}]),
      runner([{name: "ISA", value: "arm-a64"}, {name: "OSFamily", value: "macos"}])]}]}
    + (if $mode == "nfsv4" then {prefetching: {fileSystemAccessCache: {grpc: {client: {address: $sp}}},
        bloomFilterBitsPerPath: 14, bloomFilterMaximumSizeBytes: 65536}}
       else {global: ($c[0].global + {setUmask: {umask: 0}})} end)' |
    sed -e "s|@STORAGE@|$gw:$sp|g" -e "s|@SCHEDULER@|$gw:$wp|g" -e "s|@STATE@|/var/db/cucina|g" \
      -e "s|@BUILD@|/Volumes/cucina/build|g" -e "s|@NATIVE_CACHE@|/Volumes/cucina/cache|g" \
      -e "s|@RUNNER_SOCKET@|/var/run/cucina/runner.sock|g" >"$out/bb/worker.json"
  sed -e "s|@BUILD@|/Volumes/cucina/build|g" -e "s|@RUNNER_SOCKET@|/var/run/cucina/runner.sock|g" \
    -e "s|@XCODE_OVERRIDE@|$xcode_override|g" "$tmpl/runner.json" >"$out/bb/runner.json"
  jq empty "$out/bb/worker.json" "$out/bb/runner.json"
}

activate() { # out dir, worker user (image|builder): hostd §1.2 steps 5-6 + the directories bbconfig's plan asks for
  local worker_plist=/usr/local/cucina/launchd/ai.sloper.cucina.bb-worker.plist owner=root:wheel
  (cd "$1" && tar -cf - bb) | with_timeout 60 tart exec -i "$vm" /usr/bin/sudo -n -- /usr/bin/tar -x -p -f - -C /private/etc/cucina
  if [ "$2" = builder ]; then # the same job definition, run as the build user (hostd's single-user variant)
    vexec /bin/sh -c "cp $worker_plist /private/var/run/bench-bb-worker.plist &&
      /usr/libexec/PlistBuddy -c 'Delete :UserName' -c 'Delete :GroupName' /private/var/run/bench-bb-worker.plist 2>/dev/null;
      /usr/libexec/PlistBuddy -c 'Add :UserName string $build_user' -c 'Add :GroupName string staff' /private/var/run/bench-bb-worker.plist"
    worker_plist=/private/var/run/bench-bb-worker.plist
    owner="$uid:$gid"
  fi
  vexec /bin/sh -c "set -eu
    chmod 0644 /etc/cucina/bb/worker.json /etc/cucina/bb/runner.json
    install -d -o $uid -g $gid -m 0700 /var/run/cucina
    install -d -o root -g wheel -m 0755 /var/log/cucina
    chown $owner /var/log/cucina/bb_worker.log
    chown $uid:$gid /var/log/cucina/bb_runner.log
    rm -rf /var/db/cucina/l1 /var/db/cucina/filepool /var/db/cucina/nfsv4.sock
    for d in /var/db/cucina /var/db/cucina/l1 /var/db/cucina/l1/state /var/db/cucina/filepool /Volumes/cucina/cache; do
      mkdir -p \$d && chown $owner \$d && chmod 0700 \$d
    done
    : > /var/log/cucina/bb_worker.log; : > /var/log/cucina/bb_runner.log
    launchctl bootout gui/$uid/ai.sloper.cucina.bb-runner 2>/dev/null || true
    launchctl bootout system/ai.sloper.cucina.bb-worker 2>/dev/null || true
    for i in 1 2 3 4 5 6 7 8 9 10; do launchctl bootstrap gui/$uid /usr/local/cucina/launchd/ai.sloper.cucina.bb-runner.plist 2>/dev/null && break; sleep 0.5; done
    for i in 1 2 3 4 5 6 7 8 9 10; do launchctl bootstrap system $worker_plist 2>/dev/null && break; sleep 0.5; done
    # Bootstrap is asynchronous. Loaded jobs are sufficient here; the uncached remote action below proves readiness.
    launchctl print system/ai.sloper.cucina.bb-worker >/dev/null
    launchctl print gui/$uid/ai.sloper.cucina.bb-runner >/dev/null"
}

deactivate() {
  vexec /bin/sh -c "launchctl bootout gui/$uid/ai.sloper.cucina.bb-runner 2>/dev/null; launchctl bootout system/ai.sloper.cucina.bb-worker 2>/dev/null; sleep 2; mount | grep -c ' /Volumes/cucina/build ' || true" || true
}

results="[]"
for mode in "${modes[@]}"; do
  kind="${mode%-builder}"
  user=image
  [ "$kind" = "$mode" ] || user=builder
  [ -f "$tmpl/build-$kind.json" ] || die "unknown mode $mode"
  echo "=== $mode"
  render "$kind" "$logs/$mode"
  # Invalidate Bazel's local incremental result too: --noremote_accept_cached alone would not rerun //:smoke.
  (cd "$ws" && "$BAZEL" "${bazel_startup[@]}" clean >"$logs/$mode-clean.log" 2>&1)
  t_act="$(now)"
  if ! activate "$logs/$mode" "$user"; then
    echo "  activation FAILED ($mode): $(vexec /usr/bin/tail -n 5 /var/log/cucina/bb_worker.log 2>&1 | tr '\n' ' ' | cut -c1-400)"
  fi
  ok_runs=0
  failed_runs=0
  times=()
  # First action after the worker starts: registration + one trivial action (part of NFR-P1's cold start).
  if (cd "$ws" && with_timeout 180 "$BAZEL" "${bazel_startup[@]}" build "${bazel_flags[@]}" //:smoke >"$logs/$mode-smoke.log" 2>&1); then
    first_action="$(since "$t_act" "$(now)")"
  else
    first_action=null
    echo "first action FAILED ($mode): $(tail -n 5 "$logs/$mode-smoke.log")"
  fi
  echo "  worker start -> first action done: ${first_action}s"
  xcode_smoke=false
  if [ "$first_action" != null ]; then
    (cd "$ws" && "$BAZEL" "${bazel_startup[@]}" clean >/dev/null 2>&1)
    if (cd "$ws" && with_timeout 180 "$BAZEL" "${bazel_startup[@]}" build "${bazel_flags[@]}" \
      --remote_default_exec_properties="xcode-version=$xcode_version" //:smoke >"$logs/$mode-xcode-smoke.log" 2>&1); then
      xcode_smoke=true
      echo "  Xcode runner: remote unprivileged action passed"
    else
      echo "  Xcode runner: FAILED (see $logs/$mode-xcode-smoke.log)"
    fi
  fi
  for r in $(seq 1 "$RUNS"); do
    if [ "$first_action" = null ]; then failed_runs=$((failed_runs + 1)); continue; fi # worker never served an action
    (cd "$ws" && "$BAZEL" "${bazel_startup[@]}" clean >/dev/null 2>&1) || true
    t1="$(now)"
    if (cd "$ws" && with_timeout 1800 "$BAZEL" "${bazel_startup[@]}" build "${bazel_flags[@]}" //... >"$logs/$mode-run$r.log" 2>&1); then
      ok_runs=$((ok_runs + 1))
      times+=("$(since "$t1" "$(now)")")
      echo "  run $r: ${times[${#times[@]} - 1]}s"
    else
      failed_runs=$((failed_runs + 1))
      echo "  run $r: FAILED ($(grep -m1 -E 'ERROR|error' "$logs/$mode-run$r.log" | cut -c1-200))"
    fi
  done
  vexec /usr/bin/curl -s http://127.0.0.1:9986/metrics >"$logs/$mode-metrics.txt" || true
  stage() { # stage -> mean seconds per action
    awk -v s="$1" '$1 ~ /^buildbarn_builder_build_executor_duration_seconds_(sum|count)/ && $0 ~ "stage=\"" s "\"" {
      if ($1 ~ /_sum/) sum += $NF; else cnt += $NF } END { if (cnt > 0) printf "%.4f", sum / cnt; else printf "null" }' "$logs/$mode-metrics.txt"
  }
  files_read="$(awk '$1 ~ /^buildbarn_builder_build_executor_input_root_files_read_(sum|count)/ {if ($1 ~ /_sum/) s += $NF; else c += $NF} END {if (c > 0) printf "%.1f", s / c; else printf "null"}' "$logs/$mode-metrics.txt")"
  vexec /usr/bin/tail -n 20 /var/log/cucina/bb_worker.log >"$logs/$mode-bb_worker.tail" 2>&1 || true
  deactivate
  run_times="$(printf '%s\n' "${times[@]:-}" | jq -R 'select(length > 0) | tonumber' | jq -s .)"
  results="$(jq -n --argjson acc "$results" --arg mode "$mode" --argjson first "$first_action" --argjson runs "$run_times" \
    --argjson ok "$ok_runs" --argjson failed "$failed_runs" --argjson xcode "$xcode_smoke" --argjson fetch "$(stage FetchingInputs)" \
    --argjson running "$(stage Running)" --argjson upload "$(stage UploadingOutputs)" --argjson files "$files_read" \
    '$acc + [{mode: $mode, firstActionSeconds: $first, xcodeRunnerPassed: $xcode, runSeconds: $runs, okRuns: $ok, failedRuns: $failed,
      coldRunSeconds: ($runs[0] // null), warmMedianSeconds: (($runs[1:] | sort) as $w | if ($w | length) > 0 then ($w[(($w | length) - 1) / 2 | floor] + $w[($w | length) / 2 | floor]) / 2 else null end),
      perActionSeconds: {fetchingInputs: $fetch, running: $running, uploadingOutputs: $upload}, inputRootFilesReadPerAction: $files}]')"
done

out="$WORK/results-$stamp.json"
jq -n --arg image "$image" --arg vm "$vm" --argjson vcpus "$vcpus" --argjson mem "$mem_mb" --argjson results "$results" \
  --arg units "${UNITS:-200}" --arg headers "${HEADERS:-2000}" --arg agent "$(since "$t_boot" "$t_agent")" --arg session "$(since "$t_boot" "$t_session")" \
  '{image: $image, vm: {name: $vm, vcpus: $vcpus, memoryMiB: $mem, bootAgentSeconds: ($agent | tonumber), bootSessionSeconds: ($session | tonumber)},
    workload: {compileUnits: ($units | tonumber), headersPerInputRoot: ($headers | tonumber)}, results: $results}' >"$out"
echo "results: $out (logs: $logs)"
jq -c '.results[] | {mode, firstActionSeconds, coldRunSeconds, warmMedianSeconds, okRuns, failedRuns, perActionSeconds}' "$out"
# Also used by make remote-smoke: never return success for a failed remote build or runner.
jq -e '.results | all(.okRuns > 0 and .failedRuns == 0 and .xcodeRunnerPassed == true)' "$out" >/dev/null
