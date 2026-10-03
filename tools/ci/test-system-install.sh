#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Noninteractive POSIX sh does not evaluate BASH_ENV before this containment code.
set -eu
unset BASH_ENV ENV PYTHONHOME PYTHONPATH
case "$0" in
    */*) here=${0%/*} ;;
    *) here=. ;;
esac
exec /usr/bin/python3 -I "$here/system_install_test.py"
