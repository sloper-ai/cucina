# SPDX-License-Identifier: FSL-1.1-ALv2
#
# classify-build.awk: find changes that hide, skip or drop test targets in BUILD files.
#
# Input : sections made by test-change.sh, an OLD and a NEW section per changed
#         BUILD file. A section starts with the line "<US>OLD<TAB>path" or
#         "<US>NEW<TAB>path" (<US> = ASCII 037) and is followed by the file as
#         it was before and as it is after the change (empty when it did not
#         exist on that side).
# Output: "<code><TAB><message>" findings, like classify-diff.awk:
#           skip-added         a test target that existed before has more skip
#                              markers ("manual", "quarantine",
#                              @platforms//:incompatible, flaky = True) than before
#           tier-changed       a test target that existed before has another tier
#                              (some tiers are tagged manual: they stop running
#                              in a plain `bazel test //...`)
#           test-rule-removed  more test targets removed than added
#
# A test target is a statement whose rule ends in _test, or is cucina_scenario
# or test_suite, found by reading the file statement by statement (brackets are
# balanced), so a rule inside a list comprehension is read like any other. New
# targets are not judged: review (the admission rule in TESTING.md) does that.
# Targets are matched by their name attribute over ALL changed BUILD files, so
# moving a target (or the whole file) to another package is not removing it, and
# moving it does not hide a marker added on the way. POSIX awk (macOS ships BWK awk).

BEGIN {
    us = sprintf("%c", 31)
    nfind = 0
    cur_path = ""
    side = ""
    in_stmt = 0
    stmt = ""
    depth = 0
    cnt["old"] = 0
    cnt["new"] = 0
}

function flag(code, msg) {
    nfind++
    finding[nfind] = code "\t" msg
}

function is_test_rule(r) {
    return (r ~ /_test$/ || r == "cucina_scenario" || r == "test_suite")
}

# Empty out string literals so that brackets and # inside them do not count.
function strip_strings(s) {
    gsub(/"[^"]*"/, "\"\"", s)
    gsub(/'[^']*'/, "''", s)
    return s
}

# A line without its comment: a comment starts at a # that opens the line or follows blanks.
function uncomment(s) {
    sub(/(^|[ \t])#.*$/, "", s)
    return s
}

function marker_count(s,    n) {
    n = 0
    if (s ~ /"manual"/) n++
    if (s ~ /"quarantine"/) n++
    if (s ~ /@platforms\/\/:incompatible/) n++
    if (s ~ /flaky[ \t]*=[ \t]*(True|1)/) n++
    return n
}

# One complete top-level statement is in stmt.
function end_stmt(    kind, lines, n, i, line, key, tier_here, marks_here, rest, t) {
    if (stmt == "") return
    kind = ""
    if (match(stmt, /[A-Za-z_][A-Za-z0-9_.]*[ \t\n]*\(/)) {
        kind = substr(stmt, RSTART, RLENGTH)
        sub(/[ \t\n]*\($/, "", kind)
    }
    if (!is_test_rule(kind)) return
    n = split(stmt, lines, "\n")
    key = ""
    tier_here = ""
    marks_here = 0
    for (i = 1; i <= n; i++) {
        line = uncomment(lines[i])
        if (key == "" && match(line, /(^|[^A-Za-z0-9_.])name[ \t]*=[ \t]*/)) {
            rest = substr(line, RSTART + RLENGTH)
            sub(/,?[ \t]*$/, "", rest)
            key = rest
        }
        if (tier_here == "" && match(line, /(^|[^A-Za-z0-9_.])tier[ \t]*=[ \t]*"[^"]*"/)) {
            t = substr(line, RSTART, RLENGTH)
            sub(/^[^"]*"/, "", t)
            sub(/"$/, "", t)
            tier_here = t
        }
        marks_here += marker_count(line)
    }
    if (key == "") key = "(unnamed in " cur_path ")"
    if (!((side, key) in seen)) {
        cnt[side]++
        order[side, cnt[side]] = key
        seen[side, key] = 0
    }
    seen[side, key]++
    marks[side, key] += marks_here
    tier[side, key] = tier_here
    kind_of[side, key] = kind
    where[side, key] = cur_path
}

function body(raw,    t, o, c) {
    t = strip_strings(raw)
    sub(/[ \t]*#.*$/, "", t)
    if (!in_stmt) {
        if (t ~ /^[ \t]*$/) return
        in_stmt = 1
        stmt = ""
        depth = 0
    }
    stmt = stmt raw "\n"
    o = gsub(/[[({]/, "&", t)
    c = gsub(/[])}]/, "&", t)
    depth += o - c
    if (depth <= 0) {
        end_stmt()
        in_stmt = 0
        stmt = ""
        depth = 0
    }
}

function close_section() {
    if (in_stmt) end_stmt()
    in_stmt = 0
    stmt = ""
    depth = 0
}

$0 ~ "^" us "(OLD|NEW)\t" {
    close_section()
    side = (substr($0, 2, 3) == "OLD") ? "old" : "new"
    cur_path = substr($0, 6)
    next
}

{ body($0) }

END {
    close_section()
    removed = 0
    added = 0
    example = ""
    for (i = 1; i <= cnt["old"]; i++) {
        k = order["old", i]
        no = seen["old", k]
        nn = (("new", k) in seen) ? seen["new", k] : 0
        if (nn < no) {
            removed += no - nn
            if (example == "") example = where["old", k] ": " k
        }
        if (nn > 0) {
            if (marks["new", k] > marks["old", k]) {
                flag("skip-added", where["new", k] " (" kind_of["old", k] " " k "): " (marks["new", k] - marks["old", k]) " skip marker(s) added")
            }
            if (no == 1 && nn == 1 && tier["old", k] != tier["new", k]) {
                flag("tier-changed", where["new", k] " (" k "): " tier["old", k] " -> " tier["new", k])
            }
        }
    }
    for (i = 1; i <= cnt["new"]; i++) {
        k = order["new", i]
        no = (("old", k) in seen) ? seen["old", k] : 0
        if (seen["new", k] > no) added += seen["new", k] - no
    }
    if (removed > added) {
        flag("test-rule-removed", (removed - added) " more test target(s) removed than added in BUILD files, e.g. " example)
    }
    for (i = 1; i <= nfind; i++) print finding[i]
}
