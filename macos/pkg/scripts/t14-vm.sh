#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# T14 packaging kit, host side: drives the package lifecycle in a THROWAWAY macOS Tart VM cloned from the pulled
# golden image (never on the dev Mac itself, §12): sign inside the VM with a throwaway identity (PKCS#12 generated
# in the VM, imported into a throwaway keychain), simulate MDM (simulate-mdm.sh), `installer -pkg … -target /`
# WITHOUT -allowUntrusted, restart (auto-login), restart without login (daemon starts at boot), repair auto-login,
# upgrade in place (VMs, caches, identity preserved), uninstall, uninstaller package. Prints PASS/FAIL lines from
# test/t14-guest-checks.sh. Enrollment against a real controller is the e2e scenario's part: pass --controller-url,
# --ca-cert and --token-file to point hostd at it (default: a dummy endpoint; hostd just retries).
#
# usage: t14-vm.sh run --hostd PATH [--vm NAME] [--image REF] [--controller-url URL] [--ca-cert FILE]
#                      [--token-file FILE] [--notices FILE] [--keep] [--work DIR]
#        t14-vm.sh down [--vm NAME]
# Requires TART_HOME (source .work/env.sh), the image in `tart list`, Tart 2.40+ and an SSH client. Nested macOS
# virtualization is unavailable inside the VM (expected): the real hostd reports it; the package path is unaffected.
set -eu

. "$(dirname -- "$0")/lib.sh"
PKG_DIR=$(cucina_pkg_dir)
REPO_DIR=$(CDPATH='' cd -- "$PKG_DIR/../.." && pwd -P)

[ $# -ge 1 ] || cucina_die "usage: t14-vm.sh run|down [options] (try --help)"
cmd=$1
shift
vm=cucina-pkgtest-1 image=ghcr.io/cirruslabs/macos-golden-gate-xcode:27 hostd='' keep=0
controller_url=https://cucina-controller.invalid:8445 ca_cert='' token_file='' notices=$REPO_DIR/THIRD_PARTY_NOTICES.md
work=${CUCINA_DEV_STORAGE:-$HOME/Library/Caches}/pkg/t14
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
case $vm in cucina-pkgtest-*) ;; *) cucina_die "--vm must start with cucina-pkgtest- (throwaway VMs only)" ;; esac
[ -n "${TART_HOME:-}" ] || cucina_die "TART_HOME is not set (source .work/env.sh)"
cucina_need tart ssh scp ssh-keygen tar
mkdir -p "$work"
key=$work/ssh-key
ip=''

ssh_vm() { ssh -i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
	-o ConnectTimeout=10 -o BatchMode=yes "admin@$ip" "$@"; }
sudo_vm() { ssh_vm sudo -n "$@"; }

down() {
	tart stop "$vm" --timeout 60 >/dev/null 2>&1 || true
	tart delete "$vm" >/dev/null 2>&1 || true
	cucina_info "deleted VM $vm"
}

# wait_ssh SECONDS — waits until the VM answers on SSH (after boot or restart).
wait_ssh() {
	ws_end=$(($(date +%s) + $1))
	while :; do
		ip=$(tart ip "$vm" 2>/dev/null || true)
		if [ -n "$ip" ] && ssh_vm true 2>/dev/null; then return 0; fi
		[ "$(date +%s)" -lt "$ws_end" ] || cucina_die "VM $vm not reachable over SSH after $1 s"
		sleep 5
	done
}

# restart_vm — guest restart, wait until it is back and the console session has settled.
restart_vm() {
	sudo_vm shutdown -r now >/dev/null 2>&1 || true
	sleep 20
	wait_ssh 420
	sleep 30
}

up() {
	if tart list --format json | grep -q "\"Name\" *: *\"$vm\""; then cucina_die "VM $vm exists (t14-vm.sh down --vm $vm)"; fi
	tart list --format json | grep -q "\"Name\" *: *\"$image\"" || cucina_die "image $image is not pulled yet"
	cucina_info "cloning $image -> $vm"
	tart clone "$image" "$vm"
	nohup tart run --no-graphics "$vm" >"$work/$vm.run.log" 2>&1 &
	ip=$(tart ip --wait 300 "$vm")
	rm -f "$key" "$key.pub"
	ssh-keygen -q -t ed25519 -N '' -C "t14-$vm" -f "$key"
	# The Tart Guest Agent (Cirrus images) installs the throwaway SSH key; SSH survives the auto-login switch to cucina.
	tries=0
	until tart exec -i "$vm" /bin/sh -c 'mkdir -p ~/.ssh && chmod 700 ~/.ssh && cat >> ~/.ssh/authorized_keys' <"$key.pub" 2>/dev/null; do
		tries=$((tries + 1))
		if [ "$tries" -ge 24 ]; then
			# No Guest Agent: fall back to the image's documented default login for user admin (see the image's
			# README); pass it in CUCINA_T14_VM_PASSWORD, it is never stored.
			[ -n "${CUCINA_T14_VM_PASSWORD:-}" ] ||
				cucina_die "tart exec does not answer in $vm; set CUCINA_T14_VM_PASSWORD to the image's default admin password"
			cucina_warn "tart exec does not answer; installing the SSH key with the image's default password"
			pub=$(cat "$key.pub")
			CUCINA_T14_PW=$CUCINA_T14_VM_PASSWORD expect -c "set timeout 60
				spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PubkeyAuthentication=no admin@$ip \"mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '$pub' >> ~/.ssh/authorized_keys\"
				expect -re {[Pp]assword:} { send \"\$env(CUCINA_T14_PW)\r\" }
				expect eof" >/dev/null || cucina_die "could not install the SSH key in $vm"
			break
		fi
		sleep 5
	done
	wait_ssh 300
	cucina_info "$vm is up at $ip ($(ssh_vm sw_vers -productVersion))"
}

kit() {
	[ -f "$hostd" ] || cucina_die "--hostd PATH is required"
	[ -f "$notices" ] || cucina_die "missing $notices (pass --notices FILE)"
	deps=$("$PKG_DIR/scripts/fetch-deps.sh" --print)
	k=$work/kit
	rm -rf "$k" && mkdir -p "$k/macos/pkg/test" "$k/in"
	(cd "$PKG_DIR" && tar -cf - pins.env scripts resources payload install-scripts test/t14-guest-checks.sh) | (cd "$k/macos/pkg" && tar -xf -)
	cp "$REPO_DIR/LICENSE.md" "$k/LICENSE.md"
	cp "$notices" "$k/THIRD_PARTY_NOTICES.md"
	cp "$hostd" "$k/in/cucina-hostd"
	cp "$(printf '%s\n' "$deps" | sed -n 's/^BB_STORAGE_BIN=//p')" "$k/in/bb_storage"
	cp "$(printf '%s\n' "$deps" | sed -n 's/^TART_TARBALL=//p')" "$k/in/tart.tar.gz"
	[ -z "$ca_cert" ] || cp "$ca_cert" "$k/in/ca.pem"
	if [ -n "$token_file" ]; then cp "$token_file" "$k/in/token"; else printf 't14-dummy-token\n' >"$k/in/token"; fi
	COPYFILE_DISABLE=1 tar -czf "$work/kit.tgz" -C "$k" .
	scp -q -i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$work/kit.tgz" "admin@$ip:/tmp/kit.tgz"
	ssh_vm 'rm -rf /tmp/t14 && mkdir -p /tmp/t14 && tar -xzf /tmp/kit.tgz -C /tmp/t14'
}

# Signing inside the VM: throwaway identity (never leaves the VM), throwaway keychain, prompt-free.
sign_in_vm() {
	# shellcheck disable=SC2016 # expanded by the VM's shell
	ssh_vm 'set -eu; cd /tmp/t14/macos/pkg
		scripts/make-signing-cert.sh --name "Cucina T14 Signing (throwaway)" --days 7 --out-dir /tmp/t14/out \
			--p12-out /tmp/t14/signing.p12 >/tmp/t14/cert.txt 2>/dev/null
		scripts/ci-keychain.sh create --p12 /tmp/t14/signing.p12 --pass-file /tmp/t14/signing.p12.pass \
			--keychain /tmp/t14/signing.keychain-db >/tmp/t14/kc.env
		. /tmp/t14/kc.env
		for v in 0.1.0 0.1.1; do
			scripts/sign.sh --identity "$IDENTITY_SHA1" --keychain "$KEYCHAIN" -- --hostd /tmp/t14/in/cucina-hostd \
				--bb-storage /tmp/t14/in/bb_storage --tart-tarball /tmp/t14/in/tart.tar.gz \
				--license /tmp/t14/LICENSE.md --notices /tmp/t14/THIRD_PARTY_NOTICES.md --version "$v" \
				--out /tmp/t14/out/cucina-host-$(echo $v | tr . -).pkg >/tmp/t14/sign-$v.log 2>&1 || { tail -20 /tmp/t14/sign-$v.log; exit 1; }
		done
		scripts/build-pkg.sh --uninstall --version 0.1.1 --installer-identity "Cucina T14 Signing (throwaway)" \
			--keychain "$KEYCHAIN" --out /tmp/t14/out/cucina-host-uninstall-0-1-1.pkg >/tmp/t14/sign-uninstall.log 2>&1
		scripts/ci-keychain.sh delete --keychain "$KEYCHAIN" >/dev/null 2>&1
		rm -f /tmp/t14/signing.p12 /tmp/t14/signing.p12.pass
		grep "SHA-1" /tmp/t14/cert.txt | head -1'
}

guest() { sudo_vm sh /tmp/t14/macos/pkg/test/t14-guest-checks.sh "$@" | tee -a "$work/results.txt"; }

run() {
	: >"$work/results.txt"
	up
	kit
	cucina_info "signing inside the VM"
	signer=$(sign_in_vm)
	printf 'INFO  in-VM signer %s\n' "$signer" | tee -a "$work/results.txt"
	ssh_vm 'cd /tmp/t14/macos/pkg && scripts/check-pkg.sh /tmp/t14/out/cucina-host-0-1-0.pkg --version 0.1.0 --signed' |
		tail -1 | sed 's/^/INFO  check-pkg: /' | tee -a "$work/results.txt"
	ssh_vm 'pkgutil --check-signature /tmp/t14/out/cucina-host-0-1-0.pkg | sed -n "s/^ *Status: /INFO  before trust: /p"' |
		tee -a "$work/results.txt"

	cucina_info "simulating MDM (trust, managed preferences)"
	set -- --signer-cert "$(ssh_vm 'ls /tmp/t14/out/*.pem | head -1')" --controller-url "$controller_url" --token-file /tmp/t14/in/token
	if [ -n "$ca_cert" ]; then set -- "$@" --ca-cert /tmp/t14/in/ca.pem; else set -- "$@" --ca-pin "$(printf 'a%.0s' $(seq 1 64))"; fi
	sudo_vm sh /tmp/t14/macos/pkg/scripts/simulate-mdm.sh "$@" | sed 's/^/INFO  /' | tee -a "$work/results.txt"
	ssh_vm 'pkgutil --check-signature /tmp/t14/out/cucina-host-0-1-0.pkg | sed -n "s/^ *Status: /INFO  after trust: /p"' | tee -a "$work/results.txt"

	cucina_info "installing 0.1.0 (no -allowUntrusted)"
	if sudo_vm installer -pkg /tmp/t14/out/cucina-host-0-1-0.pkg -target / >"$work/install-0.1.0.log" 2>&1; then
		echo "PASS  installer -pkg 0.1.0 -target / (trusted private signature)" | tee -a "$work/results.txt"
	else
		echo "FAIL  installer 0.1.0: $(tail -3 "$work/install-0.1.0.log" | tr '\n' ' ')" | tee -a "$work/results.txt"
	fi
	guest after-install 0.1.0 || true

	cucina_info "restart: auto-login"
	restart_vm
	guest after-reboot || true

	cucina_info "restart without any login: the daemon must start at boot"
	sudo_vm sysadminctl -autologin off >/dev/null 2>&1 || true
	restart_vm
	guest no-login || true
	cucina_info "repair auto-login (new random password) and restart"
	sudo_vm /usr/local/cucina/bin/cucina-host-setup autologin --reset-password | sed 's/^/INFO  /' | tee -a "$work/results.txt"
	restart_vm
	guest after-reboot || true

	cucina_info "upgrade in place 0.1.0 -> 0.1.1"
	guest mark >/dev/null || true
	if sudo_vm installer -pkg /tmp/t14/out/cucina-host-0-1-1.pkg -target / >"$work/install-0.1.1.log" 2>&1; then
		echo "PASS  installer -pkg 0.1.1 (upgrade in place)" | tee -a "$work/results.txt"
	else
		echo "FAIL  installer 0.1.1: $(tail -3 "$work/install-0.1.1.log" | tr '\n' ' ')" | tee -a "$work/results.txt"
	fi
	guest after-upgrade 0.1.1 || true

	cucina_info "uninstall (script), then reinstall and uninstall with the uninstaller package"
	sudo_vm /usr/local/cucina/bin/cucina-host-uninstall --yes | sed 's/^/INFO  /' | tee -a "$work/results.txt"
	guest after-uninstall || true
	sudo_vm installer -pkg /tmp/t14/out/cucina-host-0-1-1.pkg -target / >/dev/null 2>&1 || true
	guest mark >/dev/null || true
	if sudo_vm installer -pkg /tmp/t14/out/cucina-host-uninstall-0-1-1.pkg -target / >"$work/uninstall-pkg.log" 2>&1; then
		echo "PASS  uninstaller package installs" | tee -a "$work/results.txt"
	else
		echo "FAIL  uninstaller package: $(tail -3 "$work/uninstall-pkg.log" | tr '\n' ' ')" | tee -a "$work/results.txt"
	fi
	guest after-uninstall || true

	[ "$keep" = 1 ] || down
	printf '\nT14 package checks: %s passed, %s failed (details: %s)\n' \
		"$(grep -c '^PASS' "$work/results.txt")" "$(grep -c '^FAIL' "$work/results.txt")" "$work/results.txt"
	! grep -q '^FAIL' "$work/results.txt"
}

case $cmd in
run) run ;;
down) down ;;
*) cucina_die "unknown command: $cmd" ;;
esac
