#!/usr/bin/perl
# SPDX-License-Identifier: FSL-1.1-ALv2
# T14 diagnostic boundary: run one command once, retaining its outcome without interpreting the cause.
# Usage: capture-command.pl --out NEW_DIRECTORY --id ID [--meta KEY=VALUE ...] -- COMMAND [ARG ...]
# The parent directory must exist. A pre-existing output path is refused, never overwritten.
# status.json records raw wait status, exit/signal, monotonic elapsed time, caller identity and non-secret context.
# stderr retains at most 4096 bytes ON FAILURE ONLY. Child stdout and successful stderr are discarded: successful
# `security delete-generic-password` can print item attributes. Never pass credentials in --meta; argv is not logged.
# No timeout or retry is added. The caller's command keeps its existing deadline (T14 uses the unchanged alarm 15).
use strict;
use warnings;
use Errno qw(EINTR EAGAIN);
use File::Basename qw(basename);
use File::Spec;
use Getopt::Long qw(GetOptions Configure);
use IO::Select;
use JSON::PP;
use POSIX qw(WNOHANG strftime _exit);
use Time::HiRes qw(clock_gettime CLOCK_MONOTONIC);

my ($out, $id, $help);
my %context;
Configure('require_order', 'no_auto_abbrev', 'no_ignore_case');
sub invalid {
    print STDERR "capture-command: $_[0]\n";
    exit 125;
}
GetOptions('out=s' => \$out, 'id=s' => \$id, 'meta=s%' => \%context, 'help' => \$help)
    or invalid('invalid options');
if ($help) {
    print "capture-command.pl --out NEW_DIRECTORY --id ID [--meta KEY=VALUE ...] -- COMMAND [ARG ...]\n";
    exit 0;
}
defined($out) && File::Spec->file_name_is_absolute($out) && @ARGV
    or invalid('an absolute --out path and command are required');
defined($id) && $id =~ /\A[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}\z/
    or invalid('invalid --id');
keys(%context) <= 16 or invalid('too many context fields');
for my $key (keys %context) {
    $key =~ /\A[a-z][a-z0-9_]{0,39}\z/ && length($context{$key}) <= 256 && $context{$key} !~ /[\x00-\x1f]/
        or invalid('invalid non-secret context field');
}
umask 0077;
mkdir($out, 0700) or invalid('output directory cannot be created (existing evidence is never replaced)');
my $json = JSON::PP->new->canonical->pretty->ascii;
my $status = {
    schema => 1, id => $id, context => \%context,
    capture_identity => { uid => 0 + $<, euid => 0 + $>, home => $ENV{HOME} // '' },
    command_name => basename($ARGV[0]),
    started_utc => strftime('%Y-%m-%dT%H:%M:%SZ', gmtime),
    completed => JSON::PP::false,
};
sub save_status {
    open my $file, '>', "$out/status.tmp" or die "capture-command: cannot write status\n";
    print {$file} $json->encode($status) or die "capture-command: cannot save status\n";
    close($file) or die "capture-command: cannot close status\n";
    rename("$out/status.tmp", "$out/status.json") or die "capture-command: cannot publish status\n";
}
save_status();
pipe(my $reader, my $writer) or die "capture-command: cannot open stderr pipe\n";
my $started = clock_gettime(CLOCK_MONOTONIC);
my $pid = fork();
defined($pid) or die "capture-command: cannot fork\n";
if ($pid == 0) {
    close($reader);
    open STDOUT, '>', '/dev/null' or _exit(126);
    open STDERR, '>&', $writer or _exit(126);
    close($writer);
    exec { $ARGV[0] } @ARGV or do {
        print STDERR "capture-command: exec failed: $!\n";
        _exit(127);
    };
}
close($writer);
my $select = IO::Select->new($reader);
my ($raw, $stderr) = (undef, '');
my $seen = 0;
my $limit = 4096;
sub read_stderr {
    my $size = sysread($reader, my $chunk, 16384);
    if (!defined $size) {
        return if $! == EINTR || $! == EAGAIN;
        die "capture-command: cannot read stderr\n";
    }
    if (!$size) {
        $select->remove($reader);
        close($reader);
        return;
    }
    $seen += $size;
    $stderr .= substr($chunk, 0, $limit - length($stderr)) if length($stderr) < $limit;
}
while (!defined $raw) {
    read_stderr() for $select->can_read(0.05);
    my $waited = waitpid($pid, $select->count ? WNOHANG : 0);
    if ($waited == $pid) {
        $raw = $?;
        # Drain available bytes, not a descendant's open pipe: collection must not extend the command's deadline.
        while ($select->count && $select->can_read(0)) {
            read_stderr();
            last if $seen > $limit;
        }
    } elsif ($waited < 0 && $! != EINTR) {
        die "capture-command: cannot wait for child\n";
    }
}
my $elapsed = clock_gettime(CLOCK_MONOTONIC) - $started;
my $signal = $raw & 127;
my $exit = $signal ? undef : $raw >> 8;
my $result = $signal ? 128 + $signal : $exit;
my $saved = $result ? $stderr : '';
open my $errors, '>:raw', "$out/stderr" or die "capture-command: cannot write bounded stderr\n";
print {$errors} $saved or die "capture-command: cannot save bounded stderr\n";
close($errors) or die "capture-command: cannot close bounded stderr\n";
$status->{completed} = JSON::PP::true;
$status->{raw_wait_status} = $raw;
$status->{exit_code} = $exit;
$status->{signal} = $signal;
$status->{core_dump} = ($raw & 128) ? JSON::PP::true : JSON::PP::false;
$status->{return_code} = $result;
$status->{elapsed_monotonic_seconds} = $elapsed;
$status->{stderr_seen_bytes} = $seen;
$status->{stderr_saved_bytes} = length($saved);
$status->{stderr_truncated} = ($result && $seen > $limit) ? JSON::PP::true : JSON::PP::false;
save_status();
exit $result;
