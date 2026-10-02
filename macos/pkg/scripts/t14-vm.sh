#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# T14 host-side runner: sign/install/reboot/upgrade/uninstall ONLY in a throwaway Tart VM (§12).
# Uses one VM slot; coordinate with other operators (Apple's maximum is two running macOS VMs total).
# Signing material stays in the guest; SSH keys, enrollment tokens, logs and exported certificates stay under
# ~/.config/cucina on the host. No host keychain changes, host installs, pulls or publishing.
# Full enrollment needs --controller-url/--ca-cert/--token-file; the default dummy endpoint tests packaging only.
# usage: t14-vm.sh run --hostd PATH [--vm cucina-pkgtest-N] [--image REF] [--controller-url URL]
#        [--ca-cert FILE] [--token-file FILE] [--notices FILE] [--keep] [--work DIR_UNDER_CONFIG_CUCINA]
#        t14-vm.sh down [--vm cucina-pkgtest-N]
# --keep preserves the VM on success OR failure for diagnosis. Otherwise an EXIT trap deletes our clone.
set -eu
umask 077

. "$(dirname -- "$0")/lib.sh"
PKG_DIR=$(cucina_pkg_dir)
REPO_DIR=$(CDPATH='' cd -- "$PKG_DIR/../.." && pwd -P)
GUEST=/Users/admin/.config/cucina/t14
[ $# -ge 1 ] || cucina_die "usage: t14-vm.sh run|down [options] (try --help)"
cmd=$1
shift
vm=cucina-pkgtest-1 image=ghcr.io/cirruslabs/macos-golden-gate-xcode:27 hostd='' keep=0
controller_url=https://cucina-controller.invalid:8445 ca_cert='' token_file='' notices=$REPO_DIR/THIRD_PARTY_NOTICES.md
work=''
while [ $# -gt 0 ]; do
	case $1 in
	--vm) vm=$2 && shift 2 ;;
	--image) image=$2 && shift 2 ;;
	--hostd) hostd=$2 && shift 2 ;;
	--controller-url) controller_url=$2 && shift 2 ;;
	--ca-cert) ca_cert=$2 && shift 2 ;;
	--token-file) token_file=$2 && shift 2 ;;
	--notices) notices=$2 && shift 2 ;;
	--keep) keep=1 && shift ;;
	--work) work=$2 && shift 2 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
case $vm in *[!a-z0-9-]* | cucina-pkgtest-) cucina_die "unsafe VM name" ;; esac
case $vm in cucina-pkgtest-*) ;; *) cucina_die "--vm must start with cucina-pkgtest- (throwaway VMs only)" ;; esac
case $cmd in run | down) ;; *) cucina_die "unknown command: $cmd" ;; esac
[ -n "${TART_HOME:-}" ] || cucina_die "TART_HOME is not set (source .work/env.sh)"
cucina_need tart ssh scp ssh-keygen tar jq
ip='' created=0 failed=0

ssh_vm() { ssh -i "$key" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$work/known_hosts" -o LogLevel=ERROR \
	-o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o BatchMode=yes "admin@$ip" "$@"; }
sudo_vm() {
	remote='sudo -n'
	for arg in "$@"; do remote="$remote '$(printf '%s' "$arg" | sed "s/'/'\\\\''/g")'"; done
	ssh_vm "$remote"
}
down() {
	tart stop "$vm" --timeout 60 >/dev/null 2>&1 || true
	tart delete "$vm" >/dev/null 2>&1 || cucina_die "could not delete VM $vm"
	cucina_info "deleted VM $vm"
}
[ "$cmd" != down ] || { down; exit 0; }

# Resolve symlinks BEFORE creating anything: credentials must never land on the bulk volume or in the repo.
work=${work:-$HOME/.config/cucina/t14/$vm}
case $work in "$HOME/.config/cucina/"*) ;; *) cucina_die "--work must be under ~/.config/cucina (contains credentials)" ;; esac
case /$work/ in */../* | */./*) cucina_die "--work cannot contain dot path components" ;; esac
ancestor=$work
while [ ! -e "$ancestor" ]; do ancestor=$(dirname -- "$ancestor"); done
physical=$(CDPATH='' cd -- "$ancestor" && pwd -P)
case $physical in "$HOME" | "$HOME/.config" | "$HOME/.config/cucina" | "$HOME/.config/cucina/"*) ;; *) cucina_die "--work resolves outside ~/.config/cucina" ;; esac
[ -f "$hostd" ] || cucina_die "--hostd PATH is required"
[ -f "$notices" ] || cucina_die "missing $notices (pass --notices FILE)"
[ -z "$token_file" ] || [ -f "$token_file" ] || cucina_die "missing token file"
[ -z "$ca_cert" ] || [ -f "$ca_cert" ] || cucina_die "missing CA file"
tart list --format json | jq -e --arg n "$vm" 'any(.[]; .Name == $n)' >/dev/null && cucina_die "VM $vm exists; refusing to touch it"
tart list --format json | jq -e --arg n "$image" 'any(.[]; .Name == $n)' >/dev/null || cucina_die "base image is not pulled yet"
running=$(tart list --format json | jq '[.[] | select(.State == "running")] | length')
[ "$running" -lt 2 ] || cucina_die "two macOS VMs are already running"
[ ! -e "$work/ssh-key" ] || cucina_die "workspace has an existing SSH identity; use a new --work"
mkdir -p "$HOME/.config/cucina" "$work"
chmod 0700 "$HOME/.config/cucina" "$work"
key=$work/ssh-key
cleanup() {
	code=$?
	trap - EXIT INT TERM
	if [ "$created" = 1 ] && [ "$keep" = 0 ]; then down || code=1; fi
	if [ "$keep" = 0 ]; then rm -rf "$work/kit" "$work/kit.tgz"; rm -f "$key" "$key.pub"; fi
	exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_ssh() {
	ws_end=$(($(date +%s) + 300))
	while :; do
		ip=$(tart ip "$vm" 2>/dev/null || true)
		if [ -n "$ip" ] && ssh_vm true 2>/dev/null; then return 0; fi
		[ "$(date +%s)" -lt "$ws_end" ] || cucina_die "VM not reachable over SSH within 300 s"
		sleep 5
	done
}
restart_vm() {
	boot=$(ssh_vm sysctl -n kern.boottime)
	sudo_vm shutdown -r now >/dev/null 2>&1 || true
	rb_end=$(($(date +%s) + 300))
	while :; do
		ip=$(tart ip "$vm" 2>/dev/null || true)
		new_boot=$(ssh_vm sysctl -n kern.boottime 2>/dev/null || true)
		if [ -n "$new_boot" ] && [ "$new_boot" != "$boot" ]; then break; fi
		[ "$(date +%s)" -lt "$rb_end" ] || cucina_die "VM did not reboot within 300 s"
		sleep 5
	done
	# Wait on the expected console/daemon state rather than an arbitrary post-boot delay.
	rb_end=$(($(date +%s) + 120))
	while ! sudo_vm sh -c "[ \"\$(stat -f %Su /dev/console)\" = '$1' ] && launchctl print system/ai.sloper.cucina.hostd | grep -q 'state = running'" >/dev/null 2>&1; do
		[ "$(date +%s)" -lt "$rb_end" ] || break
		sleep 3
	done
}
up() {
	cucina_info "cloning base -> $vm (one slot, 2 CPUs/8 GiB)"
	tart clone "$image" "$vm"
	created=1
	tart set "$vm" --cpu 2 --memory 8192
	nohup tart run --no-graphics "$vm" >"$work/vm.run.log" 2>&1 &
	ip=$(tart ip --wait 300 "$vm")
	ssh-keygen -q -t ed25519 -N '' -C "t14-$vm" -f "$key"
	tries=0
	until tart exec -i "$vm" /bin/sh -c 'umask 077; mkdir -p ~/.ssh; cat >> ~/.ssh/authorized_keys' <"$key.pub" 2>/dev/null; do
		tries=$((tries + 1))
		[ "$tries" -lt 36 ] || cucina_die "Tart guest agent did not become ready; no password fallback"
		sleep 5
	done
	wait_ssh
	cucina_info "$vm reachable (macOS $(ssh_vm sw_vers -productVersion)); address retained only in private workspace"
}
kit() {
	deps=$("$PKG_DIR/scripts/fetch-deps.sh" --print)
	k=$work/kit
	mkdir -p "$k/macos/pkg/test" "$k/in"
	(cd "$PKG_DIR" && COPYFILE_DISABLE=1 tar -cf - pins.env scripts resources payload install-scripts test/t14-guest-checks.sh) | (cd "$k/macos/pkg" && tar -xf -)
	cp "$REPO_DIR/LICENSE.md" "$k/LICENSE.md"
	cp "$notices" "$k/THIRD_PARTY_NOTICES.md"
	cp "$hostd" "$k/in/cucina-hostd"
	cp "$(printf '%s\n' "$deps" | sed -n 's/^BB_STORAGE_BIN=//p')" "$k/in/bb_storage"
	cp "$(printf '%s\n' "$deps" | sed -n 's/^TART_TARBALL=//p')" "$k/in/tart.tar.gz"
	[ -z "$ca_cert" ] || cp "$ca_cert" "$k/in/ca.pem"
	if [ -n "$token_file" ]; then cp "$token_file" "$k/in/token"; else printf 't14-dummy-token\n' >"$k/in/token"; fi
	COPYFILE_DISABLE=1 tar -czf "$work/kit.tgz" -C "$k" .
	ssh_vm "umask 077; mkdir -p '$GUEST'"
	scp -q -i "$key" -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$work/known_hosts" "$work/kit.tgz" "admin@$ip:$GUEST/kit.tgz"
	ssh_vm "tar -xzf '$GUEST/kit.tgz' -C '$GUEST'; rm '$GUEST/kit.tgz'"
	# Evidence belongs to this input build and guest, not a prior package run. facts redacts the serial itself.
	shasum -a 256 "$hostd" >"$work/input-hostd.sha256"
	ssh_vm "$GUEST/in/cucina-hostd facts" >"$work/guest-facts.json"
	ssh_vm '/usr/sbin/sysctl -n hw.ncpu hw.memsize' >"$work/guest-resources.txt"
	# Both VM hardware and this root-owned sentinel are required by the destructive guest helpers.
	sudo_vm install -o root -g wheel -m 0600 /dev/null /var/db/cucina-t14-throwaway
}
sign_in_vm() {
	# shellcheck disable=SC2016 # literal guest shell script, no host interpolation
	ssh_vm 'set -eu; cd "$HOME/.config/cucina/t14"; w=$PWD; cd macos/pkg
		scripts/make-signing-cert.sh --name "Cucina T14 Application" --purpose application --days 7 --out-dir "$w/certs/application" \
			--p12-out "$w/signing.p12" >"$w/cert.txt" 2>"$w/cert.log"
		scripts/make-signing-cert.sh --name "Cucina T14 Installer" --purpose installer --days 7 --out-dir "$w/certs/installer" \
			--p12-out "$w/installer.p12" >>"$w/cert.txt" 2>>"$w/cert.log"
		scripts/ci-keychain.sh create --p12 "$w/signing.p12" --pass-file "$w/signing.p12.pass" \
			--installer-p12 "$w/installer.p12" --installer-pass-file "$w/installer.p12.pass" \
			--keychain "$w/signing.keychain-db" >"$w/kc.env"
		. "$w/kc.env"
		trap '\''scripts/ci-keychain.sh delete --keychain "$KEYCHAIN" >/dev/null 2>&1; rm -f "$w/signing.p12" "$w/signing.p12.pass" "$w/installer.p12" "$w/installer.p12.pass"'\'' EXIT
		mkdir -p "$w/out"
		cp "$w"/certs/installer/*.pem "$w/out/installer.pem"
		cp "$w"/certs/application/*.pem "$w/out/application.pem"
		# Apple requires installer trust on the signing machine as well as the destination. Guest ONLY.
		sudo -n security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "$w/out/installer.pem"
		for v in 0.1.0 0.1.1; do
			scripts/sign.sh --identity "$IDENTITY_SHA1" --installer-identity "$INSTALLER_IDENTITY_SHA1" --keychain "$KEYCHAIN" -- --hostd "$w/in/cucina-hostd" \
				--bb-storage "$w/in/bb_storage" --tart-tarball "$w/in/tart.tar.gz" \
				--license "$w/LICENSE.md" --notices "$w/THIRD_PARTY_NOTICES.md" --version "$v" \
				--out "$w/out/cucina-host-$(printf %s "$v" | tr . -).pkg" >"$w/sign-$v.log" 2>&1
		done
		scripts/build-pkg.sh --uninstall --version 0.1.1 --installer-identity "Cucina T14 Installer" \
			--keychain "$KEYCHAIN" --out "$w/out/cucina-host-uninstall-0-1-1.pkg" >"$w/sign-uninstall.log" 2>&1'
}
guest() {
	if sudo_vm sh "$GUEST/macos/pkg/test/t14-guest-checks.sh" "$@" >"$work/phase-$1.log" 2>&1; then :; else failed=1; fi
	grep -E '^(PASS|FAIL|INFO) ' "$work/phase-$1.log" | tee -a "$work/results.txt" || true
	if ! grep -q '^PASS' "$work/phase-$1.log"; then failed=1; printf 'FAIL  guest phase %s did not produce evidence\n' "$1" | tee -a "$work/results.txt"; fi
}
install_pkg() {
	if sudo_vm installer -pkg "$GUEST/out/$1.pkg" -target / >"$work/$1.install.log" 2>&1; then
		printf 'PASS  installer %s (no -allowUntrusted)\n' "$1" | tee -a "$work/results.txt"
	else
		printf 'FAIL  installer %s; see private install log\n' "$1" | tee -a "$work/results.txt"
		cucina_die "installer failed; aborting lifecycle"
	fi
}

: >"$work/results.txt"
up
kit
cucina_info "signing inside the VM (timestamps enabled)"
if ! sign_in_vm >"$work/signing.log" 2>&1; then
	ssh_vm "tar -czf - -C '$GUEST' cert.txt cert.log kc.env sign-0.1.0.log sign-0.1.1.log sign-uninstall.log 2>/dev/null" >"$work/signing-logs.tgz" || true
	cucina_die "signing failed (logs in private workspace)"
fi
mkdir -p "$work/artifacts"
ssh_vm "tar -cf - -C '$GUEST/out' ." | tar -xf - -C "$work/artifacts"
printf 'PASS  private codesign/productbuild signatures and unchanged Tart verified\n' | tee -a "$work/results.txt"
set -- --signer-cert "$GUEST/out/installer.pem" --controller-url "$controller_url" --token-file "$GUEST/in/token"
if [ -n "$ca_cert" ]; then set -- "$@" --ca-cert "$GUEST/in/ca.pem"; else set -- "$@" --ca-pin aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; fi
sudo_vm sh "$GUEST/macos/pkg/scripts/simulate-mdm.sh" "$@" >"$work/simulate.log" 2>&1
install_pkg cucina-host-0-1-0
guest after-install 0.1.0
restart_vm cucina
guest after-reboot
sudo_vm sysadminctl -autologin off >"$work/autologin-off.log" 2>&1
restart_vm root
guest no-login
sudo_vm /usr/local/cucina/bin/cucina-host-setup autologin --reset-password >"$work/autologin-repair.log" 2>&1
restart_vm cucina
guest after-reboot
sudo_vm sh "$GUEST/macos/pkg/test/t14-guest-checks.sh" mark >"$work/mark.log" 2>&1
install_pkg cucina-host-0-1-1
guest after-upgrade 0.1.1
sudo_vm /usr/local/cucina/bin/cucina-host-uninstall --yes >"$work/uninstall-script.log" 2>&1
guest after-uninstall
install_pkg cucina-host-0-1-1
sudo_vm sh "$GUEST/macos/pkg/test/t14-guest-checks.sh" mark >"$work/mark.log" 2>&1
install_pkg cucina-host-uninstall-0-1-1
guest after-uninstall
printf '\nT14 package checks: %s passed, %s failed (private evidence: %s)\n' \
	"$(grep -c '^PASS' "$work/results.txt" || true)" "$(grep -c '^FAIL' "$work/results.txt" || true)" "$work/results.txt"
[ "$failed" = 0 ] && ! grep -q '^FAIL' "$work/results.txt"
