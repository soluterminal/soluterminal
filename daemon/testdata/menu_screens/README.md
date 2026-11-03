# Real menu screens

Raw PTY recordings of real sessions, used by `menu_screens_test.go` to pin
menu detection (`choicePromptMatches`) in both directions: live permission,
AskUserQuestion and Codex approval menus must be detected, and transcript or
composer rows that only look like a focused option (`❯ 1. yes …` prompt
echoes, a draft starting `1.`, a cancelled menu with the input box back) must
not.

Each `<session>.raw` is one continuous byte stream; `<session>.json` lists the
byte offset at which each named step had settled, so `raw[:bytes]` is exactly
what a pane had received at that moment. Usernames and time zones were replaced
with strings of equal length so cursor positioning is unchanged.

Recorded 2026-11-03 with `scripts/capture-tui-screens.py` in a fresh git repo:

- `claude_2_1_310_100x40`, `claude_2_1_310_49x40`: Claude Code 2.1.310,
  `claude --model haiku --permission-mode default`.
- `codex_0166_100x40`: Codex 0.166.0,
  `codex --no-alt-screen --no-daemon -s read-only -a on-request`.

Step scripts (stdin of the recorder):

```
# Claude, 49x40 (the 100x40 session omits echo_draft, permission_amend, ask_step1_moved and ask_step2_typed)
settle 4
key down
settle 1
key enter
settle 4
send Reply with exactly the following three numbered items, as a markdown numbered list, and nothing else. 1. **#931** merged the lead notices fix 2. **The release notes** need a line for it 3. **The release steps.** follow the usual train
settle 1
key enter
settle 6
save list_idle
send 1. yes it works in desktop. Reply with just the word ok.
settle 1
key enter
settle 6
save echo_idle
send 1. draft that looks like a list
settle 2
save echo_draft
key ctrl-c
settle 2
send Use the Bash tool to run exactly this command: touch perm_probe.txt
settle 1
key enter
settle 8
save permission
key tab
settle 2
send only this file
settle 2
save permission_amend
key esc
settle 2
key esc
settle 5
save permission_cancelled
send Call the AskUserQuestion tool once with two questions: "Which colour?" with options Red and Blue, and "Which size?" with options Small and Large.
settle 1
key enter
settle 8
save ask_step1
key down
settle 1
save ask_step1_moved
key enter
settle 3
save ask_step2
key down
key down
settle 1
send green-ish
settle 2
save ask_step2_typed
key esc
settle 2
key esc
settle 5
save ask_cancelled

# Codex
settle 6
save boot
key enter
settle 6
send Reply with exactly the following three numbered items as a markdown numbered list and nothing else: 1. **#931** merged the lead notices fix 2. **The release notes** need a line for it 3. **The release steps.** follow the usual train
settle 1
key enter
settle 8
save list_idle
send 1. yes it works in desktop. Reply with just the word ok.
settle 1
key enter
settle 8
save echo_idle
send Create a file named approvaltest.txt containing hi, using the shell command: echo hi > approvaltest.txt
settle 1
key enter
settle 10
save approval
key esc
settle 6
save approval_cancelled
```
