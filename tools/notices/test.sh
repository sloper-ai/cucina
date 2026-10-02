#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Tests for tools/notices/lib.sh: the licence classifier decides whether a
# dependency may be bundled, so each supported licence has a case, and a copyleft
# licence is refused. The fixtures are the distinctive sentences of each licence,
# not full texts.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
# shellcheck source-path=SCRIPTDIR
. "$here/lib.sh"

passed=0
failed=0
check() { # check <expected> <name> <text>
    local got
    got="$(printf '%s' "$3" | classify_text)"
    if [ "$got" = "$1" ]; then
        passed=$((passed + 1))
        echo "ok - $2"
    else
        failed=$((failed + 1))
        echo "not ok - $2 (expected $1, got $got)"
    fi
}

check_policy() { # check_policy <allowed|refused> <name> <expression>
    local got=refused
    if spdx_allowed "$here/policy.json" "$3"; then got=allowed; fi
    if [ "$got" = "$1" ]; then
        passed=$((passed + 1))
        echo "ok - $2"
    else
        failed=$((failed + 1))
        echo "not ok - $2 (expected $1, got $got)"
    fi
}

check Apache-2.0 "Apache-2.0" "Apache License
Version 2.0, January 2004"
check MIT "MIT" "Permission is hereby granted, free of charge, to any person obtaining a copy of this software"
check BSD-3-Clause "BSD-3-Clause" "Redistribution and use in source and binary forms, with or without modification, are permitted provided that ... Neither the name of the copyright holder nor the names of its contributors may be used"
check BSD-3-Clause "BSD-3-Clause, Go wording" "Redistribution and use in source and binary forms, with or without modification, are permitted ... The name of Google Inc. may not be used to endorse or promote products derived from this software"
check BSD-2-Clause "BSD-2-Clause" "Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met: 1. Redistributions of source code must retain"
check ISC "ISC" "Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted, provided that the above copyright notice and this permission notice appear in all copies."
check 0BSD "0BSD (ISC without the notice clause)" "Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted."
check MPL-2.0 "MPL-2.0" "Mozilla Public License, version 2.0"
check Unlicense "Unlicense" "This is free and unencumbered software released into the public domain."
check Zlib "Zlib" "This software is provided 'as-is', without any express or implied warranty. ... Altered source versions must be plainly marked as such"
check FSL-1.1-ALv2 "FSL" "Functional Source License, Version 1.1, ALv2 Future License"
check GPL "GPL" "GNU GENERAL PUBLIC LICENSE Version 3, 29 June 2007"
check LGPL "LGPL" "GNU LESSER GENERAL PUBLIC LICENSE Version 3 ... the GNU General Public License"
check AGPL-3.0 "AGPL" "GNU AFFERO GENERAL PUBLIC LICENSE Version 3"
check "Apache-2.0 AND MIT" "a file holding two licences lists both" "Apache License Version 2.0 ... Permission is hereby granted, free of charge, to any person obtaining a copy"
check UNKNOWN "unrecognised text" "All rights reserved. Do not copy."

check_policy allowed "permissive licences are allowed" "Apache-2.0 AND MIT"
check_policy refused "GPL is refused" "GPL"
check_policy refused "an unknown licence is refused" "UNKNOWN"
check_policy refused "one refused licence in a list refuses the list" "MIT AND LGPL"

echo
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
