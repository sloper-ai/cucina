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
# Match package + name first; then pair uniquely named unmatched targets across
# changed files (moves). Identical names in different packages must not mask
# each other's tier changes. POSIX awk (macOS ships BWK awk).

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

# Read one attribute expression up to its comma/closing parenthesis, respecting
# quoted strings and nested calls. In a compact rule the rest of the line is NOT
# part of name. Comprehensions retain the expression, e.g. "test_%s" % suite.
function attribute_value(s,    i, c, quote, nesting, escape, value) {
    quote = ""
    nesting = 0
    escape = 0
    for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (quote != "") {
            if (escape) escape = 0
            else if (c == "\\") escape = 1
            else if (c == quote) quote = ""
        } else {
            if (nesting == 0 && (c == "," || c == ")")) break
            if (c == "\"" || c == "'") quote = c
            else if (c ~ /[[({]/) nesting++
            else if (c ~ /[])}]/) nesting--
        }
    }
    value = substr(s, 1, i - 1)
    sub(/^[ \t]+/, "", value)
    sub(/[ \t]+$/, "", value)
    return value
}

# One complete top-level statement is in stmt.
function end_stmt(    kind, lines, n, i, line, key, address, tier_here, marks_here, rest, t) {
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
            key = attribute_value(rest)
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
    address = cur_path SUBSEP key
    if (!((side, address) in seen)) {
        cnt[side]++
        order[side, cnt[side]] = address
    }
    seen[side, address] = 1
    names[address] = key
    marks[side, address] += marks_here
    tier[side, address] = tier_here
    kind_of[side, address] = kind
    where[side, address] = cur_path
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

function compare(old, new) {
    if (marks["new", new] > marks["old", old]) {
        flag("skip-added", where["new", new] " (" kind_of["old", old] " " names[new] "): " (marks["new", new] - marks["old", old]) " skip marker(s) added")
    }
    if (tier["old", old] != tier["new", new]) {
        flag("tier-changed", where["new", new] " (" names[new] "): " tier["old", old] " -> " tier["new", new])
    }
}

END {
    close_section()
    removed = 0
    added = 0
    example = ""
    for (i = 1; i <= cnt["old"]; i++) {
        k = order["old", i]
        if (("new", k) in seen) {
            compare(k, k)
        } else {
            removed++
            missing_old[names[k]]++
            old_address[names[k]] = k
            if (example == "") example = where["old", k] ": " names[k]
        }
    }
    for (i = 1; i <= cnt["new"]; i++) {
        k = order["new", i]
        if (!(("old", k) in seen)) {
            added++
            missing_new[names[k]]++
            new_address[names[k]] = k
        }
    }
    # Pair unambiguous moves only; same-named targets in existing packages were
    # compared independently above. An ambiguous move still needs human review.
    for (name in missing_old) {
        if (missing_old[name] == 1 && missing_new[name] == 1) {
            compare(old_address[name], new_address[name])
        }
    }
    if (removed > added) {
        flag("test-rule-removed", (removed - added) " more test target(s) removed than added in BUILD files, e.g. " example)
    }
    for (i = 1; i <= nfind; i++) print finding[i]
}
