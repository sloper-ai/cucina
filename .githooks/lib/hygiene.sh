#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# hygiene.sh <git rev-list args>: public-repository hygiene scan.
#
# This repository is public. Besides secrets (gitleaks covers those), it must not
# publish the identifiers of the environment it was tested in. This script scans
# the commit messages and the added lines of every commit selected by the
# arguments for:
#   * AWS account IDs: in ARNs (arn:aws:iam::<12 digits>:...), in ECR registry
#     hosts (<12 digits>.dkr.ecr.<region>.amazonaws.com);
#   * EC2 public DNS names (ec2-<a>-<b>-<c>-<d>.<region>.compute...amazonaws.com);
#   * IAM Identity Center start URLs (<id>.awsapps.com/start).
# Each match is judged on its own: the well-known documentation account IDs
# (123456789012 and friends) and the documentation addresses of RFC 5737
# (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, e.g. ec2-203-0-113-7.<region>
# .compute.amazonaws.com) are allowed, but a line that also holds a real identifier
# is still refused.
#
# Exit status: 0 = clean, 1 = findings (printed to stderr), 2 = usage error or
# the scan could not run (a scan that cannot run never passes).
# Example:  hygiene.sh origin/main..HEAD
set -euo pipefail

[ "$#" -ge 1 ] || {
    echo "usage: hygiene.sh <git rev-list args>" >&2
    exit 2
}

account='[0-9]{12}'
patterns="arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:${account}:"
patterns="${patterns}|${account}\\.dkr\\.ecr(-fips)?\\.[a-z0-9-]+\\.amazonaws\\.com"
patterns="${patterns}|ec2-[0-9]+-[0-9]+-[0-9]+-[0-9]+\\.[a-z0-9.-]*amazonaws\\.com"
patterns="${patterns}|[a-z0-9-]+\\.awsapps\\.com/start"
allowed_accounts=' 123456789012 111122223333 444455556666 777788889999 000000000000 999999999999 210987654321 '

tab="$(printf '\t')"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/cucina-hygiene.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

# is_allowed <match>: 0 when the matched text is a documentation placeholder.
is_allowed() {
    local m="$1" ip acct
    case "$m" in
    ec2-*)
        ip="$(printf '%s' "$m" | sed -E 's/^ec2-([0-9]+)-([0-9]+)-([0-9]+)-([0-9]+)\..*$/\1.\2.\3.\4/')"
        case "$ip" in
        192.0.2.* | 198.51.100.* | 203.0.113.*) return 0 ;;
        esac
        return 1
        ;;
    *.awsapps.com/start)
        return 1
        ;;
    *)
        acct="$(printf '%s' "$m" | grep -oE "$account" | head -n 1 || true)"
        case "$allowed_accounts" in
        *" $acct "*) [ -n "$acct" ] && return 0 ;;
        esac
        return 1
        ;;
    esac
}

git -c core.quotepath=false log -p -U0 --no-color --no-ext-diff --no-textconv --no-merges \
    --src-prefix=a/ --dst-prefix=b/ --format='commit %h%n%B' "$@" >"$tmp/log" || {
    echo "hygiene: cannot read the commits of: $* (unknown revision?)" >&2
    exit 2
}

# One "<commit> TAB <file> TAB <line>" record per added line and message line.
awk '
    /^commit [0-9a-f]+$/ { c = $2; in_diff = 0; file = "(commit message)"; next }
    /^diff --git / { in_diff = 1; next }
    in_diff && /^\+\+\+ / { file = substr($0, 7); next }
    in_diff && /^\+/ { print c "\t" file "\t" substr($0, 2); next }
    !in_diff && NF { print c "\t" file "\t" $0 }
' "$tmp/log" >"$tmp/records"

# Cheap pre-filter, then judge every match of the few candidate lines on its own.
grep -E "$patterns" "$tmp/records" >"$tmp/candidates" || true

: >"$tmp/hits"
while IFS= read -r record; do
    [ -n "$record" ] || continue
    line="${record#*"$tab"}"
    line="${line#*"$tab"}"
    while IFS= read -r match; do
        if ! is_allowed "$match"; then
            printf '%s\n' "$record" >>"$tmp/hits"
            break
        fi
    done <<EOF
$(printf '%s\n' "$line" | grep -oE "$patterns" || true)
EOF
done <"$tmp/candidates"

if [ ! -s "$tmp/hits" ]; then
    exit 0
fi

{
    echo "hygiene: this repository is public; these lines look like environment identifiers:"
    cut -c1-200 "$tmp/hits" | while IFS="$tab" read -r commit where line; do
        printf '  %s  %s\n      %s\n' "$commit" "$where" "$line"
    done
    cat <<'EOT'

Remove the identifier from the commit (rewrite the commit, not just the tip) and
use a placeholder such as 123456789012, <account-id> or a documentation address
(203.0.113.7). Environment-specific values belong under ~/.config/cucina/, never
in the repository.
EOT
} >&2
exit 1
