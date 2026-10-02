# SPDX-License-Identifier: FSL-1.1-ALv2
#
# check-quarantine.awk: the BUILD-file scanner behind check-quarantine.sh.
# Reads BUILD files (one record per file), prints "Q<TAB>target<TAB>until" for every
# quarantined test target and "E<TAB>target<TAB>message" for every policy violation.
# Variables: root, today (YYYY-MM-DD), max_days.

function days(y, m, d,    era, yoe, doy, doe) {
    if (m <= 2) y--
    era = int((y >= 0 ? y : y - 399) / 400)
    yoe = y - era * 400
    doy = int((153 * (m + (m > 2 ? -3 : 9)) + 2) / 5) + d - 1
    doe = yoe * 365 + int(yoe / 4) - int(yoe / 100) + doy
    return era * 146097 + doe - 719468
}

# Cut a "#" comment off a line, honouring string quotes.
function strip_comment(line,    i, c, q, out) {
    q = ""
    out = ""
    for (i = 1; i <= length(line); i++) {
        c = substr(line, i, 1)
        if (q != "") {
            out = out c
            if (c == "\\") { i++; out = out substr(line, i, 1) }
            else if (c == q) q = ""
        } else if (c == "\"" || c == "\047") {
            q = c
            out = out c
        } else if (c == "#") {
            break
        } else {
            out = out c
        }
    }
    return out
}

function label(file,    dir) {
    dir = file
    sub(/\/BUILD(\.bazel)?$/, "", dir)
    if (dir == root) return "//"
    if (index(dir, root "/") == 1) dir = substr(dir, length(root) + 2)
    return "//" dir
}

function err(file, target, msg) {
    print "E\t" label(file) ":" target "\t" msg
}

# Examine the text of one rule call.
function check_rule(file, text,    name, rest, block, tag, tags, ntag, i, has_q, until, issue, owner, n_until, n_issue, n_owner, y, m, d, d_until, d_today, depth, j, c) {
    name = "?"
    if (match(text, /name[ \t\n]*=[ \t\n]*"[^"]*"/)) {
        name = substr(text, RSTART, RLENGTH)
        sub(/^name[ \t\n]*=[ \t\n]*"/, "", name)
        sub(/"$/, "", name)
    }
    if (text ~ /flaky[ \t\n]*=[ \t\n]*(True|1)/) {
        err(file, name, "flaky = True is banned (a flaky test is a bug: fix it or quarantine it)")
    }
    # Collect the literal tags of every `tags = [ ... ]` in the rule.
    ntag = 0
    rest = text
    while (match(rest, /tags[ \t\n]*=[ \t\n]*\[/)) {
        rest = substr(rest, RSTART + RLENGTH)
        block = ""
        depth = 1
        for (j = 1; j <= length(rest) && depth > 0; j++) {
            c = substr(rest, j, 1)
            if (c == "[") depth++
            else if (c == "]") depth--
            if (depth > 0) block = block c
        }
        while (match(block, /"[^"]*"/)) {
            tag = substr(block, RSTART + 1, RLENGTH - 2)
            tags[++ntag] = tag
            block = substr(block, RSTART + RLENGTH)
        }
    }
    has_q = 0; n_until = 0; n_issue = 0; n_owner = 0; until = ""
    for (i = 1; i <= ntag; i++) {
        tag = tags[i]
        if (tag == "exclusive") err(file, name, "tag exclusive disables remote execution; use exclusive-if-local")
        if (tag == "quarantine") has_q = 1
        else if (tag ~ /^quarantine-until-/) { n_until++; until = substr(tag, 18) }
        else if (tag ~ /^quarantine-issue-/) { n_issue++; issue = substr(tag, 18) }
        else if (tag ~ /^quarantine-owner-/) { n_owner++; owner = substr(tag, 18) }
    }
    if (!has_q) {
        if (n_until + n_issue + n_owner > 0) err(file, name, "quarantine-* tags without the \"quarantine\" tag")
        return
    }
    print "Q\t" label(file) ":" name "\t" until
    if (n_until != 1 || until !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/) {
        err(file, name, "quarantined without exactly one \"quarantine-until-YYYY-MM-DD\" tag")
    } else {
        y = substr(until, 1, 4) + 0; m = substr(until, 6, 2) + 0; d = substr(until, 9, 2) + 0
        d_until = days(y, m, d)
        y = substr(today, 1, 4) + 0; m = substr(today, 6, 2) + 0; d = substr(today, 9, 2) + 0
        d_today = days(y, m, d)
        if (d_until < d_today) {
            err(file, name, "quarantine expired on " until " (today " today "): fix the test or delete it")
        } else if (d_until - d_today > max_days) {
            err(file, name, "quarantine until " until " is more than " max_days " days away (today " today ")")
        }
    }
    if (n_issue != 1 || issue !~ /^[0-9]+$/) err(file, name, "quarantined without exactly one \"quarantine-issue-<number>\" tag")
    if (n_owner != 1 || owner == "") err(file, name, "quarantined without exactly one \"quarantine-owner-<handle>\" tag")
}

BEGIN { RS = "\001" }

{
    # One record = one BUILD file. Strip comments, then walk the text and cut
    # it into top-level calls: ident( ... ) at parenthesis depth 0.
    n = split($0, lines, "\n")
    buf = ""
    for (i = 1; i <= n; i++) buf = buf strip_comment(lines[i]) "\n"
    depth = 0; q = ""; start = 0
    for (i = 1; i <= length(buf); i++) {
        c = substr(buf, i, 1)
        if (q != "") {
            if (c == "\\") i++
            else if (c == q) q = ""
            continue
        }
        if (c == "\"" || c == "\047") { q = c; continue }
        if (c == "(") {
            if (depth == 0) start = i
            depth++
        } else if (c == ")") {
            depth--
            if (depth == 0 && start > 0) {
                check_rule(FILENAME, substr(buf, start, i - start + 1))
                start = 0
            }
        }
    }
}
