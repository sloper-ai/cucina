# SPDX-License-Identifier: FSL-1.1-ALv2
#
# classify-diff.awk: find test-weakening changes in a unified diff.
#
# Input : a file with the output of `git diff -U0 --no-renames` (a/ and b/ path
#         prefixes), given as the awk file operand.
# Output: one finding per line, "<code><TAB><message>", for every change that
#         deletes, weakens or skips tests, or rewrites goldens. No output means
#         the change needs no Test-Change trailer.
# Vars  : shrink       = net lines that may be removed from test sources before
#                        the change counts as weakening (default 10).
#         renames_file = a file with the output of `git diff -M --diff-filter=R
#                        --name-status` ("R<score> TAB old TAB new" per line).
#                        A test file renamed away is not deleted; a file renamed
#                        in is not new (a rename may not smuggle in a skip); a
#                        rename without any content change (R100) may move a
#                        golden, never edit it on the way.
#
# BUILD files are judged by classify-build.awk, which reads whole files.
#
# A skip marker is judged only where it can silence a test that already exists:
# a new file, or a test added in the same hunk, may carry an environment guard;
# review (the admission rule in TESTING.md) judges those.
#
# The rules and their rationale are in TESTING.md ("The Test-Change trailer").
# They are heuristics on purpose: cheap, explainable, and tested by
# tools/ci/test-githooks.sh. Keep this file POSIX awk (macOS ships BWK awk).

BEGIN {
    if (shrink == "") shrink = 10
    nfind = 0
    ndel = 0
    in_hunk = 0
    path = ""
    tsrc_added = 0
    tsrc_removed = 0
    mark_added = 0
    mark_removed = 0
    nfiles = 0
    if (renames_file != "") {
        while ((getline line < renames_file) > 0) {
            n = split(line, part, "\t")
            if (n < 3) continue
            moved_path[part[2]] = 1
            moved_to[part[3]] = 1
            moved_from[part[3]] = part[2]
            if (part[1] == "R100") pure_path[part[2]] = 1
        }
        close(renames_file)
    }
}

function trim(s) {
    sub(/^[ \t]+/, "", s)
    sub(/[ \t]+$/, "", s)
    return s
}

function flag(code, msg) {
    nfind++
    finding[nfind] = code "\t" msg
}

# Test sources: files whose lines are tests (shrinking them weakens the suite).
function is_test_source(p) {
    return (p ~ /_test\.go$/ || p ~ /_test\.(rs|sh|py|cc|cpp|c|ts|js)$/ || p ~ /(^|\/)test(-[^\/]*)?\.sh$/ \
        || p ~ /\.tftest\.hcl$/ || p ~ /(^|\/)tests?\//)
}

# Goldens and scenario data: expectations that must not be rewritten silently.
function is_golden(p) {
    return (p ~ /(^|\/)testdata\// || p ~ /\.golden(\.|$)/ || p ~ /(^|\/)goldens?\// \
        || p ~ /__snapshots__\// || p ~ /\.snap$/ || p ~ /^sim\/scenarios\//)
}

function is_manual_check(p) {
    return (p ~ /^docs\/testing\/manual\/MT-[0-9]+\.md$/)
}

# A line that cannot skip anything: a comment (Go and Rust use //).
function is_comment(t) {
    return (t ~ /^\/\//)
}

function reset_file() {
    cur_added = 0
    cur_removed = 0
    cur_deleted = 0
    cur_new = 0
    cur_binary = 0
    skip_n = 0
    skip_removed = 0
    in_hunk = 0
}

function begin_file(p) {
    path = p
    reset_file()
    cur_golden = is_golden(p)
    cur_test = is_test_source(p)
    cur_go_test = (p ~ /_test\.go$/ && p !~ /^test\/e2e\//)
    cur_marker_lang = (p ~ /_test\.go$/ || p ~ /\.rs$/)
    cur_rust = (p ~ /\.rs$/)
    cur_manual = is_manual_check(p)
}

function finish_file(    i) {
    if (path == "") return
    if (cur_manual && cur_deleted && !(path in pure_path)) {
        flag("manual-check-deleted", path)
    }
    if (cur_golden) {
        if (cur_deleted) {
            if (!(path in pure_path)) flag("golden-deleted", path)
        } else if (cur_binary && !cur_new) {
            flag("golden-changed", path " (binary file replaced)")
        } else if (cur_removed > 0) {
            flag("golden-changed", path " (" cur_removed " line(s) removed or rewritten)")
        }
    }
    if (cur_test) {
        tsrc_added += cur_added
        tsrc_removed += cur_removed
        if (cur_deleted) {
            ndel++
            deleted_path[ndel] = path
        }
    }
    # Skip markers are judged at the end, when a renamed file can be compared with its old self.
    nfiles++
    file_path[nfiles] = path
    skip_count[path] = skip_n
    skip_gone[path] = skip_removed
    for (i = 1; i <= skip_n; i++) skip_line[path, i] = skip_text[i]
    path = ""
}

# Test-case markers: counted to catch a commit that removes more cases than it adds.
function is_marker(text) {
    if (path ~ /_test\.go$/) {
        if (text ~ /^[ \t]*func[ \t]+(\([^)]*\)[ \t]*)?(Test|Fuzz)[A-Za-z0-9_]*[ \t]*\(/) return 1
        if (text ~ /(^|[^A-Za-z0-9_])t\.Run\(/) return 1
        if (text ~ /rapid\.(Check|MakeCheck)\(/) return 1
    }
    if (cur_rust) {
        if (text ~ /#\[(tokio::)?(test|rstest)/) return 1
        if (text ~ /proptest!/) return 1
    }
    return 0
}

# A Go or Rust line that skips a test.
function is_skip(t) {
    if (cur_go_test && t ~ /\.Skip(f|Now)?\(/) return 1
    if (cur_rust && t ~ /#\[ignore/) return 1
    return 0
}

/^diff --git / {
    finish_file()
    # "diff --git a/P b/P" with --no-renames: both names are identical, so the
    # path length follows from the line length even when P contains spaces.
    n = (length($0) - 16) / 2
    begin_file(substr($0, 14, n))
    next
}

/^new file mode/ && !in_hunk { cur_new = 1; next }
/^deleted file mode/ && !in_hunk { cur_deleted = 1; next }
/^Binary files / && !in_hunk { cur_binary = 1; next }

/^@@ / {
    in_hunk = 1
    hunk_new_cases = 0
    hunk_removed_cases = 0
    next
}

in_hunk && /^\+/ {
    text = substr($0, 2)
    cur_added++
    if (cur_marker_lang) {
        if (is_marker(text)) {
            mark_added++
            hunk_new_cases++
        }
        t = trim(text)
        # A skip on a new file, or inside a test added by this very hunk, silences nothing.
        # A renamed file is not new: it is compared with its old self at the end.
        if (!is_comment(t) && is_skip(t) && ((path in moved_to) || (!cur_new && hunk_new_cases <= hunk_removed_cases))) {
            skip_n++
            skip_text[skip_n] = t
        }
    }
    next
}

in_hunk && /^-/ {
    text = substr($0, 2)
    cur_removed++
    if (cur_marker_lang) {
        if (is_marker(text)) {
            mark_removed++
            hunk_removed_cases++
        }
        t = trim(text)
        if (!is_comment(t) && is_skip(t)) skip_removed++
    }
    next
}

END {
    finish_file()
    # Skip markers: more added than removed in a file (moving one is not adding one);
    # for a renamed file the markers of the file it came from count as removed.
    for (j = 1; j <= nfiles; j++) {
        f = file_path[j]
        gone = skip_gone[f]
        if ((f in moved_from) && (moved_from[f] in skip_gone)) gone += skip_gone[moved_from[f]]
        for (i = gone + 1; i <= skip_count[f]; i++) flag("skip-added", f ": " skip_line[f, i])
    }
    for (i = 1; i <= ndel; i++) {
        if (!(deleted_path[i] in moved_path)) {
            flag("test-deleted", deleted_path[i])
        }
    }
    net = tsrc_removed - tsrc_added
    if (net > shrink) {
        flag("tests-shrunk", "net " net " line(s) removed from test sources (limit " shrink ")")
    }
    if (mark_removed > mark_added) {
        flag("tests-removed", (mark_removed - mark_added) " more test case(s) removed than added (" mark_removed " removed, " mark_added " added)")
    }
    for (i = 1; i <= nfind; i++) print finding[i]
}
