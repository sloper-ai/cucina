# SPDX-License-Identifier: FSL-1.1-ALv2
# `cucinactl tui`: the overview updating live (R-CLI-4). The scripted demo load runs
# at a fixed clock, so every 2 s step renders the same; each capture waits for its
# step (today's spend grows by $0.02 per step). Run with ./check.sh (docs/tui.md).
Output out/overview.txt
Output out/overview.gif
Require cucinactl
Set Shell "bash"
Set FontSize 16
Set Width 846
Set Height 486
Set Padding 10
Set TypingSpeed 10ms
Hide
Env LC_ALL "en_US.UTF-8"
Env CUCINA_TUI_DEMO "scripted"
Type "cucinactl tui"
Enter
Wait+Screen@20s /\$12\.42/
Show
Sleep 200ms
Hide
Wait+Screen@20s /\$12\.48/
Show
Sleep 200ms
Hide
Type "q"
Sleep 500ms
