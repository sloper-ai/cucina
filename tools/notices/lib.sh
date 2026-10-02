#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# lib.sh: helpers for generate.sh (sourced; also tested by test.sh).
#
# classify_text reads a licence text on stdin and prints its SPDX identifier(s),
# joined with " AND " when the file holds several licences, or "UNKNOWN". It looks
# for the distinctive sentences of each licence; it is a classifier for the table
# and the policy check, while the full text is always reproduced in the notices.

# normalize: lower-case and collapse white space.
normalize() {
    tr '[:upper:]' '[:lower:]' | tr -s '[:space:]' ' '
}

# has <pattern>: does the normalized text on $text match the extended regex?
has() {
    printf '%s' "$text" | grep -Eq -- "$1"
}

classify_text() {
    local text ids=""
    text="$(normalize)"
    add() { ids="${ids:+$ids AND }$1"; }

    if has 'apache license' && has 'version 2\.0'; then add Apache-2.0; fi
    if has 'mozilla public license,? (version|v\.?) ?2\.0'; then add MPL-2.0; fi
    if has 'gnu affero general public license'; then
        add AGPL-3.0
    elif has 'gnu lesser general public license'; then
        add LGPL
    elif has 'gnu general public license'; then
        add GPL
    fi
    if has 'functional source license'; then add FSL-1.1-ALv2; fi
    if has 'redistribution and use in source and binary forms'; then
        if has '(neither the name|the names? of .{0,60} may not be used to endorse|may not be used to endorse or promote)'; then
            add BSD-3-Clause
        else
            add BSD-2-Clause
        fi
    fi
    if has 'permission is hereby granted, free of charge, to any person obtaining a copy'; then add MIT; fi
    if has 'permission to use, copy, modify, and/or distribute this software for any purpose with or without fee'; then
        if has 'provided that the above copyright notice and this permission notice appear in all copies'; then
            add ISC
        else
            add 0BSD
        fi
    fi
    if has 'this is free and unencumbered software released into the public domain'; then add Unlicense; fi
    if has 'cc0 1\.0 universal'; then add CC0-1.0; fi
    if has "provided 'as-is', without any express or implied warranty" && has 'altered source versions must be plainly marked'; then add Zlib; fi
    if has 'boost software license'; then add BSL-1.0; fi

    printf '%s\n' "${ids:-UNKNOWN}"
}

# spdx_allowed <policy.json> <expression>: every identifier of an " AND " list is allowed.
# policy.json holds {"allowed": ["Apache-2.0", ...]}; read without jq so that the tests need no tools.
spdx_allowed() {
    local policy="$1" expr="$2" id allowed count=0
    allowed="$(sed -n '/"allowed"/,/\]/p' "$policy")" || return 1
    for id in $(printf '%s' "$expr" | sed 's/ AND / /g'); do
        printf '%s\n' "$allowed" | grep -Fq "\"$id\"" || return 1
        count=$((count + 1))
    done
    [ "$count" -gt 0 ]
}
