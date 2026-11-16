package main

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// approval_widening.go decides whether approving a tool-permission
// request would let the call run with MORE permission than the agent's normal
// sandbox and approval settings grant (ST-192, phone public-release criterion
// C10a).
//
// Why it exists: the one-line preview (nativeApprovalInputPreview) shows only
// the most meaningful field, usually `command`. A Bash call carrying
// dangerouslyDisableSandbox:true therefore previews as a plain `git status`,
// and the phone offered one-tap Approve for it. Every surface that offers a
// quick approve (the push payload, the pane list's pendingApproval hint) now
// carries widensPermissions, and the summary carries a visible label, so the
// phone sends the person to the full request instead.
//
// The check is deliberately broad: a false positive costs one extra tap
// (Review instead of Approve), a false negative is the bug this fixes.
//
// Since the Focus (native) panes were removed, nothing auto-allows from a tool
// or prefix allowlist and no surface offers a quick approve, so nothing calls
// approvalWidening today. It stays, with its tests, for the terminal-pane
// one-tap task: any future allowlist auto-allow or quick-approve surface must
// call it first.

// approvalWideningKeys are input fields that ask for extra permission, keyed by
// their normalised name (lower case, no '_' or '-'). They are matched at any
// depth, so an MCP tool or a future input shape that nests them is still
// caught.
var approvalWideningKeys = map[string]string{
	"dangerouslydisablesandbox": "sandbox off",           // Claude Code Bash
	"withescalatedpermissions":  "escalated permissions", // Codex shell (older)
	"sandboxpermissions":        "escalated permissions", // Codex shell: "require_escalated"
	"additionalpermissions":     "extra permissions",     // Codex app-server
	"networkapprovalcontext":    "network access",        // Codex app-server
	"grantroot":                 "write access grant",    // Codex fileChange
}

// approvalWideningCommand matches shell text that turns permission checks off
// or runs as root. Matched on the lower-cased command.
var approvalWideningCommand = []struct {
	re    *regexp.Regexp
	label string
}{
	{regexp.MustCompile(`--dangerously-skip-permissions|--allow-dangerously-skip-permissions|--dangerously-bypass-approvals-and-sandbox|bypasspermissions|(^|\s)--yolo(\s|$)`), "bypasses permissions"},
	{regexp.MustCompile(`(^|[\s;&|(` + "`" + `])(\S*[/\\])?(sudo|doas|pkexec|su|runas|gsudo)(\.exe)?(\s|$)`), "runs as root"},
	{regexp.MustCompile(`start-process\b.*-verb\s+['"]?runas`), "runs as root"},
	{regexp.MustCompile(`\b(claude|codex)\s+mcp\s+add`), "adds an MCP server"},
	{regexp.MustCompile(`\bclaude\s+config\s+(set|add)\b`), "touches agent permission settings"},
	{regexp.MustCompile(`\bcodex\b.*\s(-c|--config)(\s|=)`), "touches agent permission settings"},
	{regexp.MustCompile(`\bcore\.hookspath\b`), "touches agent permission settings"},
}

// approvalPermissionConfigFragments are paths whose contents decide what an
// agent may do without asking: Claude Code and Codex settings, permission
// rules, hooks (a hook can approve tool calls) and MCP server lists. Matched
// on a lower-cased, forward-slashed path with a leading "/".
var approvalPermissionConfigFragments = []string{
	"/.claude/settings.json",
	"/.claude/settings.local.json",
	"/.claude/hooks/",
	"/managed-settings.json",
	"/.claude.json",
	"/.mcp.json",
	"/.codex/config.toml",
	"/.codex/rules/",
	// A git hook runs outside the agent's sandbox on the next git command.
	"/.git/hooks/",
}

func approvalWidening(toolName string, raw json.RawMessage) string {
	switch {
	case toolName == "ExitPlanMode":
		return "leaves plan mode"
	case strings.HasPrefix(toolName, "mcp__") && strings.HasSuffix(toolName, "__answer_prompt"):
		return "answers another pane's permission prompt"
	}
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Unparseable input: fall back to a text scan for the key names so a
		// malformed request never comes out more trusted than a parsed one.
		low := normaliseWideningKey(string(raw))
		for key, label := range approvalWideningKeys {
			if strings.Contains(low, key) {
				return label
			}
		}
		return ""
	}
	if label := wideningInValue(v, 0); label != "" {
		return label
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	// A Codex patch approval whose item changes the adapter never saw (no
	// item/started, or the bound was hit) can't be checked by path: fail closed.
	if toolName == "ApplyPatch" {
		if _, ok := m["changes"]; !ok {
			return "patch files unknown"
		}
	}
	if cmd := commandText(m["command"]); cmd != "" {
		if label := commandWidening(cmd); label != "" {
			return label
		}
	}
	return ""
}

// commandText returns a shell command given as a string or, as Codex sends
// it, an argv array.
func commandText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, a := range t {
			if s, ok := a.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// approvalPathKeys name a file a tool reads or writes: Claude's file tools
// and Codex patch changes (changes[].path, move_path).
var approvalPathKeys = map[string]bool{"filepath": true, "notebookpath": true, "path": true, "movepath": true}

// wideningInValue walks a decoded JSON value for a widening key with a value
// that asks for something. depth bounds hostile nesting.
func wideningInValue(v any, depth int) string {
	if depth > 16 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			nk := normaliseWideningKey(k)
			if label, ok := approvalWideningKeys[nk]; ok && wideningValueSet(k, child) {
				return label
			}
			if p, ok := child.(string); ok && approvalPathKeys[nk] && isPermissionConfigPath(p) {
				return "touches agent permission settings"
			}
		}
		for _, child := range t {
			if label := wideningInValue(child, depth+1); label != "" {
				return label
			}
		}
	case []any:
		for _, child := range t {
			if label := wideningInValue(child, depth+1); label != "" {
				return label
			}
		}
	case string:
		if strings.EqualFold(strings.TrimSpace(t), "bypassPermissions") {
			return "bypasses permissions"
		}
	}
	return ""
}

// wideningValueSet reports whether a widening key's value actually asks for
// the extra permission: false, null, "", "use_default" and empty objects do not.
func wideningValueSet(key string, v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		switch s {
		case "", "false", "0", "no", "none", "null", "use_default", "usedefault", "default":
			return false
		}
		if normaliseWideningKey(key) == "sandboxpermissions" {
			return strings.Contains(s, "escalat")
		}
		return true
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}

func normaliseWideningKey(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("_", "", "-", "").Replace(s)
}

func isPermissionConfigPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	p = "/" + strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, frag := range approvalPermissionConfigFragments {
		if strings.HasSuffix(frag, "/") {
			if strings.Contains(p+"/", frag) {
				return true
			}
			continue
		}
		if strings.HasSuffix(p, frag) {
			return true
		}
	}
	return false
}

// commandWidening checks a shell command for permission bypass flags, root
// escalation, and writes or reads that name a permission config file.
func commandWidening(cmd string) string {
	low := strings.ToLower(cmd)
	for _, rule := range approvalWideningCommand {
		if rule.re.MatchString(low) {
			return rule.label
		}
	}
	lowSlash := strings.ReplaceAll(low, `\`, "/")
	for _, frag := range approvalPermissionConfigFragments {
		bare := strings.TrimPrefix(frag, "/")
		if strings.Contains(lowSlash, frag) || strings.HasPrefix(lowSlash, bare) || strings.Contains(lowSlash, " "+bare) || strings.Contains(lowSlash, "~/"+bare) {
			return "touches agent permission settings"
		}
	}
	return ""
}
