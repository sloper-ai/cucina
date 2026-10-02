# SPDX-License-Identifier: FSL-1.1-ALv2
# `cucinactl tui`: filter the operations as you type, then drill into the action
# inspector (R-CLI-4, UC13). Static demo data at a fixed clock. Run with ./check.sh
# (docs/tui.md).
Output out/inspect.txt
Output out/inspect.gif
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
Type "4"
Wait+Screen /Operations \(8\)/
Type "/macos"
Enter
Wait+Screen /Operations \(1 of 8\)/
Show
Sleep 200ms
Hide
Enter
Wait+Screen /Command: 13 arguments/
Show
Sleep 200ms
Hide
Escape
Type "q"
Sleep 500ms
