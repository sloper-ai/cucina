# SPDX-License-Identifier: FSL-1.1-ALv2
#
# check-manual-tests.awk: structural check of one manual-test file
# (docs/testing/manual/MT-NNN.md), the machine-checkable half of the manual
# test policy in TESTING.md. Prints "E<TAB>file<TAB>message" per problem and,
# with list=1, "S<TAB>file<TAB>step" per step marked [agent].
# Variable: list (0/1).

function err(msg) {
    print "E\t" FILENAME "\t" msg
}

BEGIN {
    state = "start"       # start -> front -> body
    section = ""
    nsteps = 0
    split("id title risks trigger owner last_run sign_off", required_key, " ")
    split("Preconditions|Steps|Expected results|Required evidence", required_section, "|")
}

FNR == 1 {
    stem = FILENAME
    sub(/^.*\//, "", stem)
    sub(/\.md$/, "", stem)
    if ($0 != "---") {
        err("must start with a YAML front-matter block (---)")
        state = "body"
    }
    state = ($0 == "---") ? "front" : "body"
    next
}

state == "front" {
    if ($0 == "---") { state = "body"; next }
    if ($0 ~ /^[A-Za-z_]+:/) {
        key = $0
        sub(/:.*/, "", key)
        value = $0
        sub(/^[A-Za-z_]+:[ \t]*/, "", value)
        sub(/[ \t]+#.*$/, "", value)
        have[key] = 1
        val[key] = value
    }
    next
}

state == "body" {
    if ($0 ~ /^## /) {
        section = substr($0, 4)
        seen_section[section] = 1
        next
    }
    if (section == "Steps" && $0 ~ /^[0-9]+\. /) {
        nsteps++
        if ($0 !~ /^[0-9]+\. \[(agent|human)\] /) {
            err("step lacks an [agent] or [human] marker: " substr($0, 1, 60))
        } else if (list && $0 ~ /^[0-9]+\. \[agent\] /) {
            print "S\t" FILENAME "\t" $0
        }
    }
}

END {
    if (state == "front") err("front matter is never closed (missing second ---)")
    for (i = 1; i <= 7; i++) {
        k = required_key[i]
        if (!(k in have)) err("front matter lacks the key \"" k "\"")
    }
    if (("id" in have) && val["id"] != stem) err("id \"" val["id"] "\" does not match the file name " stem)
    if ("last_run" in have) {
        if (val["last_run"] != "never" && val["last_run"] !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/) {
            err("last_run must be \"never\" or YYYY-MM-DD, got \"" val["last_run"] "\"")
        }
        if (("sign_off" in have)) {
            if (val["last_run"] == "never" && val["sign_off"] != "pending") err("never run, so sign_off must be \"pending\"")
            if (val["last_run"] != "never" && val["sign_off"] == "pending") err("run on " val["last_run"] " but sign_off is still pending: a human must sign off")
        }
    }
    for (i = 1; i <= 4; i++) {
        if (!(required_section[i] in seen_section)) err("lacks the section \"## " required_section[i] "\"")
    }
    if (nsteps == 0) err("has no numbered steps")
}
