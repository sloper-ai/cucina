# SPDX-License-Identifier: FSL-1.1-ALv2
# `cucinactl tui`: draining a worker asks first (default "No"), then shows the API's
# answer in the status line and the worker draining (R-CLI-4, UC14). Static demo data
# at a fixed clock. Run with ./check.sh (docs/tui.md).
Output out/drain.txt
Output out/drain.gif
Require cucinactl
Set Shell "bash"
Set FontSize 16
Set Width 846
Set Height 486
Set Padding 10
Set TypingSpeed 10ms
Hide
Env LC_ALL "en_US.UTF-8"
Env CUCINA_TUI_DEMO "frozen"
Type "cucinactl tui"
Enter
Wait+Screen@20s /LIVE/
Type "2"
Wait+Screen /Workers · linux-aarch64/
Down
Wait+Screen /Workers · linux-x86-64 \(3\)/
Enter
Type "d"
Wait+Screen /Drain worker/
Show
Sleep 200ms
Hide
Type "y"
Wait+Screen /i-0a1b2c3d4e5f60718 +draining/
Show
Sleep 200ms
Hide
Type "q"
Sleep 500ms
