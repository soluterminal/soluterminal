package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ST-192 (phone criterion C10a): a request that widens the agent's permissions
// must be marked, whatever its one-line preview looks like.
func TestApprovalWidening(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tool, input, want string
	}{
		// The bug: the preview shows only "git status".
		{"bash sandbox off", "Bash", `{"command":"git status","dangerouslyDisableSandbox":true}`, "sandbox off"},
		{"bash sandbox off as string", "Bash", `{"command":"git status","dangerouslyDisableSandbox":"true"}`, "sandbox off"},
		{"bash sandbox explicitly on", "Bash", `{"command":"git status","dangerouslyDisableSandbox":false}`, ""},
		{"plain bash", "Bash", `{"command":"git status","description":"Show status"}`, ""},
		{"nested in an MCP tool", "mcp__x__run", `{"opts":{"dangerously_disable_sandbox":true},"cmd":"ls"}`, "sandbox off"},
		{"codex escalated", "Bash", `{"command":"ls","sandbox_permissions":"require_escalated"}`, "escalated permissions"},
		{"codex default sandbox", "Bash", `{"command":"ls","sandbox_permissions":"use_default"}`, ""},
		{"codex with_escalated", "shell", `{"command":["ls"],"with_escalated_permissions":true}`, "escalated permissions"},
		{"codex extra permissions", "Bash", `{"command":"ls","additionalPermissions":{"network":true}}`, "extra permissions"},
		{"codex empty extra permissions", "Bash", `{"command":"ls","additionalPermissions":{}}`, ""},
		{"codex network", "Bash", `{"command":"npm view x","networkApprovalContext":{"host":"registry.npmjs.org"}}`, "network access"},
		{"codex grant root", "ApplyPatch", `{"itemId":"p1","grantRoot":"/home/u","changes":[]}`, "write access grant"},
		{"codex null grant root", "ApplyPatch", `{"itemId":"p1","grantRoot":null,"changes":[]}`, ""},
		{"codex patch with unknown files", "ApplyPatch", `{"itemId":"p1"}`, "patch files unknown"},
		{"bypass mode value", "mcp__x__set", `{"mode":"bypassPermissions"}`, "bypasses permissions"},
		{"exit plan mode", "ExitPlanMode", `{"plan":"do things"}`, "leaves plan mode"},
		{"answer another pane", "mcp__soluterminal__answer_prompt", `{"pane_id":"p2","choice":"1"}`, "answers another pane's permission prompt"},
		{"write claude settings", "Write", `{"file_path":"/repo/.claude/settings.json","content":"{}"}`, "touches agent permission settings"},
		{"edit local settings relative", "Edit", `{"file_path":".claude/settings.local.json"}`, "touches agent permission settings"},
		{"edit windows settings", "Edit", `{"file_path":"C:\\Users\\u\\.claude\\settings.json"}`, "touches agent permission settings"},
		{"write hook", "Write", `{"file_path":"/repo/.claude/hooks/pre.sh"}`, "touches agent permission settings"},
		{"write mcp json", "Write", `{"file_path":"/repo/.mcp.json"}`, "touches agent permission settings"},
		{"write codex config", "Edit", `{"file_path":"/home/u/.codex/config.toml"}`, "touches agent permission settings"},
		{"write codex rules", "Write", `{"file_path":"/home/u/.codex/rules/default.rules"}`, "touches agent permission settings"},
		{"plain edit", "Edit", `{"file_path":"/repo/src/settings.json"}`, ""},
		{"plain claude doc", "Edit", `{"file_path":"/repo/.claude/agents/x.md"}`, ""},
		{"bash skip permissions", "Bash", `{"command":"claude --dangerously-skip-permissions -p hi"}`, "bypasses permissions"},
		{"bash codex bypass", "Bash", `{"command":"codex exec --dangerously-bypass-approvals-and-sandbox x"}`, "bypasses permissions"},
		{"bash yolo", "Bash", `{"command":"codex --yolo"}`, "bypasses permissions"},
		{"bash sudo", "Bash", `{"command":"cd /x && sudo make install"}`, "runs as root"},
		{"bash sudo by path", "Bash", `{"command":"/usr/bin/sudo -n true"}`, "runs as root"},
		{"codex argv sudo", "shell", `{"command":["sudo","ls"]}`, "runs as root"},
		{"codex patch to settings", "ApplyPatch", `{"itemId":"p1","changes":[{"path":"/repo/src/a.go","kind":"update"},{"path":"/repo/.claude/settings.json","kind":"add"}]}`, "touches agent permission settings"},
		{"codex patch elsewhere", "ApplyPatch", `{"itemId":"p1","changes":[{"path":"/repo/src/a.go","kind":"update"}]}`, ""},
		{"bash su", "Bash", `{"command":"su - root -c id"}`, "runs as root"},
		{"windows runas", "Bash", `{"command":"runas /user:Administrator cmd"}`, "runs as root"},
		{"gsudo exe", "Bash", `{"command":"C:\\tools\\gsudo.exe winget install x"}`, "runs as root"},
		{"powershell elevate", "Bash", `{"command":"powershell -c \"Start-Process cmd -Verb RunAs\""}`, "runs as root"},
		{"claude config set", "Bash", `{"command":"claude config set -g autoUpdates false"}`, "touches agent permission settings"},
		{"codex -c override", "Bash", `{"command":"codex exec -c sandbox_mode=danger-full-access hi"}`, "touches agent permission settings"},
		{"codex --config", "Bash", `{"command":"codex --config=approval_policy=never"}`, "touches agent permission settings"},
		{"git hooks path", "Bash", `{"command":"git config core.hooksPath /tmp/h"}`, "touches agent permission settings"},
		{"write git hook", "Write", `{"file_path":"/repo/.git/hooks/pre-commit"}`, "touches agent permission settings"},
		{"bash git hook", "Bash", `{"command":"chmod +x .git/hooks/pre-push"}`, "touches agent permission settings"},
		{"codex review is not config", "Bash", `{"command":"codex review --base main"}`, ""},
		{"summary word is not su", "Bash", `{"command":"echo summary"}`, ""},
		{"bash pseudo is not sudo", "Bash", `{"command":"echo pseudo"}`, ""},
		{"bash mcp add", "Bash", `{"command":"claude mcp add evil -- node x.js"}`, "adds an MCP server"},
		{"bash writes settings", "Bash", `{"command":"echo '{}' > .claude/settings.local.json"}`, "touches agent permission settings"},
		{"bash writes home claude json", "Bash", `{"command":"jq . ~/.claude.json"}`, "touches agent permission settings"},
		{"bash codex config", "Bash", `{"command":"sed -i s/a/b/ /home/u/.codex/config.toml"}`, "touches agent permission settings"},
		{"unparseable input with key", "Bash", `{"command":"ls","dangerouslyDisableSandbox":tru`, "sandbox off"},
		{"empty input", "Read", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalWidening(strings.TrimSpace(tc.tool), json.RawMessage(tc.input))
			if got != tc.want {
				t.Fatalf("widening(%s %s) = %q, want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}
