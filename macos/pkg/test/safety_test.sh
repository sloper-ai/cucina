#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards: R-MAC-8/R-SEC-3, §12 — the T14 runner refuses unsafe names/credential directories before side effects.
set -eu
PATH=$PATH:/usr/bin:/bin:/usr/sbin:/sbin
export PATH
if [ -n "${TEST_SRCDIR:-}" ]; then
	PKG="$TEST_SRCDIR/${TEST_WORKSPACE:-_main}/macos/pkg"
else
	PKG=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
fi
w=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/pkg-safety.XXXXXX")
w=$(CDPATH='' cd -- "$w" && pwd -P)
trap 'rm -rf "$w"' EXIT

# Guards: T14 evidence loss — a public capture invocation retains its real failure/signal, bounded private stderr,
# and immutable identity even when a later invocation passes. Finite child commands only: no keychain or VM access.
capture_cli_checks() {
	mkdir -p "$w/captures"
	for scenario in error signal success; do
		case $scenario in
		error) program='print STDERR "E" x 8192; exit 37'; want=37; signal=0; saved=4096 ;;
		signal) program='kill "TERM", $$; exit 99'; want=143; signal=15; saved=0 ;;
		success) program='print "discard stdout"; print STDERR "discard successful stderr"; exit 0'; want=0; signal=0; saved=0 ;;
		esac
		if /usr/bin/perl "$PKG/scripts/capture-command.pl" --out "$w/captures/$scenario" --id "$scenario" \
			--meta target_user=fixture --meta phase="$scenario" -- /usr/bin/perl -e "$program" >"$w/capture.stdout" 2>"$w/capture.stderr"; then code=0; else code=$?; fi
		[ "$code" = "$want" ] || { printf 'FAIL: capture %s returned %s, expected %s\n' "$scenario" "$code" "$want" >&2; exit 1; }
		[ ! -s "$w/capture.stdout" ] || { printf 'FAIL: capture leaked child stdout\n' >&2; exit 1; }
		/usr/bin/perl -MJSON::PP -e '
			my ($dir,$id,$rc,$sig,$saved)=@ARGV;
			open my $f,"<","$dir/status.json" or die "missing capture status\n"; local $/; my $s=decode_json(<$f>);
			die "identity lost\n" unless $s->{id} eq $id && $s->{context}{target_user} eq "fixture" && $s->{context}{phase} eq $id;
			die "outcome lost\n" unless $s->{completed} && $s->{return_code} == $rc && $s->{signal} == $sig;
			die "exit lost\n" unless $sig ? !defined($s->{exit_code}) : $s->{exit_code} == $rc;
			die "raw status lost\n" unless $s->{raw_wait_status} == ($sig || ($rc << 8));
			die "timing missing\n" unless defined($s->{elapsed_monotonic_seconds}) && $s->{elapsed_monotonic_seconds} >= 0;
			die "stderr bound failed\n" unless -f "$dir/stderr" && -s "$dir/stderr" == $saved && $s->{stderr_saved_bytes} == $saved;
			die "truncation lost\n" if $id eq "error" && (!$s->{stderr_truncated} || $s->{stderr_seen_bytes} != 8192);
			die "private modes lost\n" unless ((stat($dir))[2] & 0777) == 0700 && ((stat("$dir/status.json"))[2] & 0777) == 0600 && ((stat("$dir/stderr"))[2] & 0777) == 0600;
		' "$w/captures/$scenario" "$scenario" "$want" "$signal" "$saved"
	done
	cp "$w/captures/error/status.json" "$w/original-status.json"
	if /usr/bin/perl "$PKG/scripts/capture-command.pl" --out "$w/captures/error" --id replacement \
		-- /usr/bin/perl -e 'open my $f, ">", $ARGV[0] or die; print $f "ran"' "$w/unexpected-child" >/dev/null 2>&1; then
		printf 'FAIL: capture overwrote an earlier invocation\n' >&2; exit 1
	fi
	cmp -s "$w/original-status.json" "$w/captures/error/status.json" && [ ! -e "$w/unexpected-child" ] || {
		printf 'FAIL: refused capture changed evidence or ran its child\n' >&2; exit 1;
	}
	printf 'PASS: capture CLI preserves failure/signal, bounded private evidence and invocation identity\n'
}
# Guards: T14 evidence retention — failed transport must stop destructive continuation and retain the original
# failure in a private recovery record. Exercise the public runner against a stateful transport, never a real VM.
transport_failure_check() {
	tf=$w/transport
	mkdir -p "$tf/bin" "$tf/state" "$tf/home" "$tf/repo/macos/pkg/scripts" "$tf/repo/macos/pkg/test" \
		"$tf/repo/macos/pkg/resources" "$tf/repo/macos/pkg/payload" "$tf/repo/macos/pkg/install-scripts" "$tf/inputs"
	cp "$PKG/scripts/t14-vm.sh" "$PKG/scripts/lib.sh" "$PKG/scripts/capture-command.pl" "$tf/repo/macos/pkg/scripts/"
	printf '# SPDX-License-Identifier: FSL-1.1-ALv2\n' >"$tf/repo/macos/pkg/pins.env"
	printf '# SPDX-License-Identifier: FSL-1.1-ALv2\n' >"$tf/repo/macos/pkg/test/t14-guest-checks.sh"
	printf 'local fixture, not a release\n' >"$tf/repo/LICENSE.md"
	cp "$tf/repo/LICENSE.md" "$tf/repo/THIRD_PARTY_NOTICES.md"
	for input in hostd bb_storage tart.tar.gz; do printf 'local fixture\n' >"$tf/inputs/$input"; done
	cat >"$tf/repo/macos/pkg/scripts/fetch-deps.sh" <<'EOF'
#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
printf 'TART_TARBALL=%s/tart.tar.gz\nBB_STORAGE_BIN=%s/bb_storage\n' "$FAKE_T14_INPUTS" "$FAKE_T14_INPUTS"
EOF
	chmod +x "$tf/repo/macos/pkg/scripts/fetch-deps.sh"
	cat >"$tf/bin/transport" <<'EOF'
#!/usr/bin/perl
# SPDX-License-Identifier: FSL-1.1-ALv2
use strict;
use warnings;
use File::Basename qw(basename);
use JSON::PP;
my $state=$ENV{FAKE_T14_STATE}; my $vm='cucina-pkgtest-transport';
sub record { open my $f,'>',"$state/$_[0]" or die; print {$f} "1\n"; }
my $tool=basename($0);
if ($tool eq 'tart') {
    my $op=$ARGV[0];
    if ($op eq 'list') {
        my @fleet=({Name=>'ghcr.io/cirruslabs/macos-golden-gate-xcode:27',State=>'stopped'});
        push @fleet,{Name=>$vm,State=>(-e "$state/stopped" ? 'stopped' : 'running')} if -e "$state/created" && !-e "$state/deleted";
        print encode_json(\@fleet);
    } elsif ($op eq 'clone') { record('created'); }
    elsif ($op eq 'stop') { record('stopped'); }
    elsif ($op eq 'delete') { record('deleted'); }
    elsif ($op eq 'ip') { print "127.0.0.1\n"; }
    elsif ($op eq 'exec') { while (<STDIN>) {} }
    elsif ($op ne 'set' && $op ne 'run') { die "unsupported fake Tart operation\n"; }
    exit 0;
}
if ($tool eq 'ssh-keygen') {
    for (my $i=0;$i<@ARGV;$i++) { if ($ARGV[$i] eq '-f') { for my $suffix ('','.pub') { open my $f,'>',$ARGV[$i+1].$suffix or die; print {$f} "not-a-key: local fixture\n"; } } }
    exit 0;
}
exit 0 if $tool eq 'scp';
$tool eq 'ssh' or die "unsupported fake transport\n";
my $remote=$ARGV[-1];
if ($remote =~ /scripts\/make-signing-cert\.sh/) { exit 0; }
if ($remote =~ /tar -cf -/) { exec '/usr/bin/tar','-cf','-','-T','/dev/null'; die; }
if ($remote =~ /probes\/([^\/]+)\/(write|cleanup)\/status\.json/) {
    my ($id,$op)=($1,$2); my $rc=$op eq 'write' ? 37 : 0;
    print encode_json({completed=>JSON::PP::true,id=>"$id-$op",exit_code=>$rc,signal=>0,raw_wait_status=>$rc<<8,return_code=>$rc}); exit 0;
}
if ($remote =~ /probes\/[^\/]+\/write\/stderr/) { print STDERR "fixture transfer failed\n"; exit 255; }
if ($remote =~ /probes\/[^\/]+\/cleanup\/stderr/) { exit 0; }
if ($remote =~ /t14-guest-checks\.sh/) {
    if ($remote =~ /after-reboot/) { print "FAIL  fixture primary write exit=37\n"; exit 1; }
    print "PASS  fixture guest observation\n"; exit 0;
}
if ($remote =~ /kern\.boottime/ || $remote =~ /shutdown/) {
    my $generation=0;
    if (open my $f,'<',"$state/boot-generation") { $generation=0+<$f>; }
    if ($remote =~ /shutdown/) { open my $f,'>',"$state/boot-generation" or die; print {$f} $generation+1; }
    else { print "boot-$generation\n"; }
    exit 0;
}
if ($remote =~ /--reset-password/) { record('reset'); exit 0; }
if ($remote =~ /^sudo -n/ && $remote =~ /cucina-host-uninstall/) { record('uninstalled'); exit 0; }
if ($remote =~ /sw_vers/) { print "27.0\n"; exit 0; }
if ($remote =~ /hw\.ncpu/) { print "2\n8589934592\n"; exit 0; }
if ($remote =~ /cucina-hostd facts/) { print "{}\n"; exit 0; }
exit 0 if $remote eq 'true' || $remote =~ /^(umask|tar -xzf)/ || $remote =~ /^sudo -n/;
die "unsupported fake SSH operation\n";
EOF
	chmod +x "$tf/bin/transport"
	for tool in tart ssh scp ssh-keygen; do ln -s transport "$tf/bin/$tool"; done
	if HOME="$tf/home" PATH="$tf/bin:$PATH" TART_HOME="$tf/fleet" FAKE_T14_STATE="$tf/state" FAKE_T14_INPUTS="$tf/inputs" \
		sh "$tf/repo/macos/pkg/scripts/t14-vm.sh" run --vm cucina-pkgtest-transport --hostd "$tf/inputs/hostd" >"$tf/run.log" 2>&1; then
		printf 'FAIL: runner accepted incomplete primary evidence\n' >&2; exit 1
	fi
	[ -e "$tf/state/created" ] || { printf 'FAIL: fixture never reached the owned clone\n' >&2; exit 1; }
	[ ! -e "$tf/state/reset" ] && [ ! -e "$tf/state/uninstalled" ] && [ ! -e "$tf/state/deleted" ] || {
		printf 'FAIL: evidence transfer failure allowed reset, uninstall or deletion\n' >&2; exit 1;
	}
	[ -e "$tf/state/stopped" ] || { printf 'FAIL: owned clone was not stopped\n' >&2; exit 1; }
	tf_work=$tf/home/.config/cucina/t14/cucina-pkgtest-transport
	/usr/bin/perl -MJSON::PP -e '
		my $work=$ARGV[0]; open my $f,"<","$work/recovery.json" or die "missing recovery manifest\n";
		local $/; my $r=decode_json(<$f>);
		die "not a recovery barrier\n" unless $r->{status} eq "cleanup-incomplete" && $r->{vm} eq "cucina-pkgtest-transport" && $r->{owned_by_run} && $r->{vm_state} eq "stopped" && $r->{stop_verified};
		die "primary phase exit lost\n" unless $r->{phase_exit_code} == 1;
		open my $p,"<","$r->{phase_evidence_dir}/write/status.json" or die "primary probe evidence lost\n";
		my $s=decode_json(<$p>); die "primary probe exit changed\n" unless $s->{exit_code} == 37;
		die "private recovery identity missing\n" unless -f $r->{ssh_key} && ((stat($r->{ssh_key}))[2] & 0777) == 0600;
		die "manifest not private\n" unless ((stat("$work/recovery.json"))[2] & 0777) == 0600;
		open my $c,"<","$work/cleanup-status" or die "cleanup status missing\n";
		die "cleanup falsely complete\n" unless <$c> eq "cleanup-incomplete\n";
	' "$tf_work"
	printf 'PASS: public runner stops and retains owned evidence before reset/uninstall/deletion\n'
}
case ${1:-} in
--capture-only) capture_cli_checks; exit 0 ;;
--transport-only) transport_failure_check; exit 0 ;;
'') capture_cli_checks; transport_failure_check ;;
*) printf 'unknown test selection\n' >&2; exit 2 ;;
esac

mkdir -p "$w/bin"
# Empty in-memory fleet: a runner must reject bad inputs before it ever needs an image.
printf '#!/bin/sh\nprintf "[]\\n"\n' >"$w/bin/tart"
chmod +x "$w/bin/tart"
export PATH="$w/bin:$PATH" TART_HOME="$w/fleet"
# shellcheck disable=SC2016 # literal shell metacharacters must be rejected, not evaluated
for name in cucina-pkgtest-safety 'cucina-pkgtest-../escape' 'cucina-pkgtest-$(false)'; do
	if "$PKG/scripts/t14-vm.sh" run --vm "$name" --work "$w/work" >"$w/out" 2>&1; then
		printf 'FAIL: unsafe runner invocation succeeded\n' >&2
		exit 1
	fi
	if [ -e "$w/work" ]; then
		printf 'FAIL: runner created a credential workspace outside ~/.config/cucina\n' >&2
		exit 1
	fi
done
printf 'PASS: unsafe T14 invocations have no filesystem side effects\n'

# Guards: §12 — failed S3 tagging must never trigger an untagged destructive cleanup.
# Stateful CLI fake: creation succeeds, tagging fails, tag lookup refuses ownership; deletion leaves evidence.
cat >"$w/bin/aws" <<'EOF'
#!/bin/sh
case "$1 $2" in
's3api create-bucket') touch "$FAKE_AWS_STATE/created" ;;
's3api put-bucket-tagging' | 's3api get-bucket-tagging') exit 1 ;;
's3 rm' | 's3api delete-bucket') touch "$FAKE_AWS_STATE/deleted" ;;
*) exit 1 ;;
esac
EOF
chmod +x "$w/bin/aws"
mkdir -p "$w/home/.config/cucina" "$w/aws"
printf 'fixture\n' >"$w/input.pkg"
if HOME="$w/home" CUCINA_SECRETS_DIR="$w/home/.config/cucina" FAKE_AWS_STATE="$w/aws" \
	AWS_REGION=us-west-1 CUCINA_RUN_ID=test-run CUCINA_EXPIRES=2026-10-09T00:00:00Z \
	"$PKG/scripts/publish-s3-temp.sh" create --pkg "$w/input.pkg" --version 1.2.3 >"$w/s3.out" 2>&1; then
	printf 'FAIL: failed bucket tagging reported success\n' >&2; exit 1
fi
[ -e "$w/aws/created" ] || { printf 'FAIL: fake did not exercise creation\n' >&2; exit 1; }
[ ! -e "$w/aws/deleted" ] || { printf 'FAIL: cleanup deleted a bucket without verified ownership tags\n' >&2; exit 1; }
printf 'PASS: failed S3 tagging never authorizes deletion\n'

# Guards: R-MAC-9 — failed CI setup does not leave an unlocked signing keychain behind.
cat >"$w/bin/security" <<'EOF'
#!/bin/sh
case $1 in
create-keychain)
	for last in "$@"; do :; done
	touch "$last" ;;
delete-keychain) rm -f "$2" ;;
set-key-partition-list) exit 1 ;;
list-keychains)
	[ "${4:-}" = -s ] || printf '    "%s/Library/Keychains/login.keychain-db"\n' "$HOME" ;;
*) : ;;
esac
EOF
chmod +x "$w/bin/security"
printf 'fixture\n' >"$w/input.p12"
printf 'fixture\n' >"$w/input.pass"
if HOME="$w/home" "$PKG/scripts/ci-keychain.sh" create --p12 "$w/input.p12" --pass-file "$w/input.pass" \
	--keychain "$w/test.keychain-db" >"$w/keychain.out" 2>&1; then
	printf 'FAIL: failed keychain setup reported success\n' >&2; exit 1
fi
[ ! -e "$w/test.keychain-db" ] || { printf 'FAIL: failed setup left its signing keychain behind\n' >&2; exit 1; }
printf 'PASS: failed CI keychain setup cleans up its private identity\n'
