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

Recorded 2026-11-04 for ST-18 (prompt echoes read as menus, `prompt_echo.go`):

- `claude_2_1_310_66x40`: Claude Code 2.1.310, `claude --model haiku
  --permission-mode default --strict-mcp-config --setting-sources project`
  (no MCP servers or user hooks, so nothing reached a live daemon). The turn
  is still running at `list_running`: a `❯ 1.06 …` echo on Claude's band,
  the reply's `1. Yes` / `2. No` list, then the idle input box. The text
  alone reads as a menu; only the band tells it apart.

Recorded 2026-11-14 for ST-263 (a repaint read as an approval menu,
`message_hold_screens_test.go`), Claude Code 2.1.322, `claude --model haiku
--strict-mcp-config --setting-sources project`, in a fresh git repo whose
`.claude/settings.json` sets a status line (`~/repo [Haiku 4.5 | ctx: 4%]`),
as the affected panes had one. The plan name and usage percentage in these
two were also replaced at equal length.

- `claude_2_1_322_100x40`: `--permission-mode default`. Two background
  shells; when the first ends, Claude repaints only the changed cells of
  "2 shells still running", and the `1` follows the input box's `❯` in the
  byte stream (`❯ … 1  stil  run ing`): a focused `❯ 1` row to the raw-tail
  scan, with no menu on screen at any step.
- `claude_2_1_322_auto_120x40`: `--permission-mode auto`. At `teach_dialog`
  Claude's "Teach auto mode about your environment?" dialog is open below the
  idle input box: a real menu. Claude offers it only in some sessions.

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

# Claude, 66x40 (claude_2_1_310_66x40)
settle 4
key down
settle 1
key enter
settle 4
save idle
send 1.06 refers to mobile soluterminal? Use no tools. First reply with exactly a two item markdown numbered list: Yes, then No. Then write the word PLUM two hundred times, separated by spaces.
settle 1
key enter
wait PLUM PLUM
save list_running

# Claude 2.1.322, 100x40 (claude_2_1_322_100x40)
settle 4
key down
settle 1
key enter
settle 4
save idle
send Reply with just the word ok.
settle 1
key enter
wait ●\s+ok
settle 4
save warm
send Use the Bash tool two times, each with run_in_background set to true. First command exactly: sleep 20. Second command exactly: sleep 50. Then reply with just the word started. When a background command later completes, reply with just the word noted.
settle 1
key enter
wait ●\s+started
settle 4
save two_shells
wait sleep 20.{1,3}completed
settle 4
save one_shell
wait sleep 50.{1,3}completed
settle 4
save no_shells

# Claude 2.1.322, auto mode, 120x40 (claude_2_1_322_auto_120x40; the first two saves)
settle 4
key down
settle 1
key enter
settle 4
save idle
send Reply with just the word ok.
settle 1
key enter
wait ●\s+ok
settle 4
save teach_dialog
```

Recorded 2026-11-14 for ST-265 (terminal one-tap, `terminal_one_tap.go`), in
fresh git repos. Usernames and the plan name were replaced at equal length.
Every permission prompt was cancelled with Esc; nothing was approved.

- `claude_2_1_322_perm_100x40`: Claude Code 2.1.322, `claude --model haiku
  --permission-mode default --strict-mcp-config --setting-sources project`, no
  sandbox. Bash, Write, Edit, WebFetch, sudo and a `.claude/settings.local.json`
  write, plus the folder-trust menu.
- `claude_2_1_322_sandbox_100x40`: the same with a project
  `.claude/settings.json` of `{"sandbox":{"enabled":true,"allowUnsandboxedCommands":true}}`.
  The sandbox stayed disabled on the recording machine (`/sandbox` reports
  socat not installed), so `sandbox_off_permission`, a Bash call asked to set
  dangerouslyDisableSandbox, shows exactly what a plain Bash prompt shows. That
  is why one-tap judges widening on the PreToolUse hook's tool input, never on
  the screen.
- `codex_0169_ro_100x40`: Codex 0.169.0, `codex --no-alt-screen --no-daemon -s
  read-only -a on-request -c 'mcp_servers={}' -c 'model_reasoning_effort="low"'`.
  A plain write and an explicitly escalated command render identically, with a
  model-written "Reason:" line, so every Codex approval goes to Review. (curl
  ran without a prompt even under read-only on that machine.)

Step scripts:

```
# Claude, no sandbox (claude_2_1_322_perm_100x40)
settle 6
save trust
key down
settle 1
key enter
settle 6
save idle
send Use the Bash tool to run exactly this command: touch perm_probe.txt
settle 1
key enter
settle 14
save bash_permission
key esc
settle 2
key esc
settle 5
save bash_cancelled
send Use the Write tool to create notes.txt containing the word hi
settle 1
key enter
settle 14
save write_permission
key esc
settle 2
key esc
settle 5
send Use the Edit tool to change hi to hello in README.md
settle 1
key enter
settle 14
save edit_permission
key esc
settle 2
key esc
settle 5
send Use the WebFetch tool to fetch https://example.com and tell me its title
settle 1
key enter
settle 14
save webfetch_permission
key esc
settle 2
key esc
settle 5
send Use the Bash tool to run exactly this command: sudo -n true
settle 1
key enter
settle 14
save sudo_permission
key esc
settle 2
key esc
settle 5
send Use the Write tool to create the file .claude/settings.local.json containing {}
settle 1
key enter
settle 14
save settings_write_permission
key esc
settle 2
key esc
settle 5

# Claude, sandbox settings (claude_2_1_322_sandbox_100x40)
settle 6
save trust
key down
settle 1
key enter
settle 6
save idle
send Use the Bash tool with the dangerouslyDisableSandbox parameter set to true to run exactly this command: touch outside_probe.txt
settle 1
key enter
settle 16
save sandbox_off_permission
key esc
settle 2
key esc
settle 5
send Use the Bash tool to run exactly this command: curl -sI https://example.com
settle 1
key enter
settle 16
save sandboxed_network
key esc
settle 2
key esc
settle 5
send Use the Bash tool to run exactly this command: touch /tmp/st265_probe_outside
settle 1
key enter
settle 16
save sandboxed_outside_write
key esc
settle 2
key esc
settle 5

# Codex read-only (codex_0169_ro_100x40)
settle 8
save idle
send Create a file named approvaltest.txt containing hi, using the shell command: echo hi > approvaltest.txt
settle 1
key enter
settle 25
save write_command
key esc
settle 6
send Run this shell command exactly, nothing else: curl -sI https://example.com
settle 1
key enter
settle 25
save network_command
key esc
settle 6
send Use apply_patch to add a new file notes.txt containing hi
settle 1
key enter
settle 25
save patch_approval
key esc
settle 6
send Run this shell command exactly with escalated permissions, explaining why: touch ../st265_outside_probe.txt
settle 1
key enter
settle 25
save escalated_command
key esc
settle 6
```
