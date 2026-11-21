package helperproto

import "strings"

// Both ends run these checks: the helper before acting on a request, the host
// before sending one, so a bad identifier never reaches a path or a socket
// name on either side (ruling 8.3).

const maxDim = 10000

func validateHello(a *HelloArgs) *Error {
	if a.Proto != Proto {
		return Errorf(CodeBadArgs, "proto %d not supported (want %d)", a.Proto, Proto)
	}
	if a.HostVersion == "" || hasNUL(a.HostVersion) || hasNUL(a.Distro) {
		return Errorf(CodeBadArgs, "hello: bad hostVersion or distro")
	}
	return nil
}

func validateSpawn(a *SpawnArgs) *Error {
	if !ValidPaneID(a.PaneID) {
		return Errorf(CodeBadArgs, "spawn: bad paneId")
	}
	if len(a.Argv) == 0 || a.Argv[0] == "" {
		return Errorf(CodeBadArgs, "spawn: empty argv")
	}
	for _, s := range a.Argv {
		if hasNUL(s) {
			return Errorf(CodeBadArgs, "spawn: NUL in argv")
		}
	}
	if hasNUL(a.Cwd) {
		return Errorf(CodeBadArgs, "spawn: NUL in cwd")
	}
	for k, v := range a.Env {
		// The agent's --env wire joins entries with commas, so neither a key
		// nor a value may carry one: "X=a,SOLU_QUOTA_WORKER=1" would reach the
		// agent as two variables (helper-protocol-v1, spawn).
		if k == "" || strings.ContainsAny(k, "=,\x00") || strings.ContainsAny(v, ",\x00") {
			return Errorf(CodeBadArgs, "spawn: bad env entry %q", k)
		}
	}
	if a.Cols < 1 || a.Cols > maxDim || a.Rows < 1 || a.Rows > maxDim {
		return Errorf(CodeBadArgs, "spawn: bad size %dx%d", a.Cols, a.Rows)
	}
	switch a.Provider {
	case "":
	case "glm":
		return Errorf(CodeBadArgs, "spawn: provider glm is not supported in Linux panes yet")
	default:
		return Errorf(CodeBadArgs, "spawn: unknown provider")
	}
	return nil
}

// validateList checks a page request and fills in the default limit.
func validateList(a *ListArgs) *Error {
	if a.After != "" && !ValidAgentID(a.After) {
		return Errorf(CodeBadArgs, "list: bad after")
	}
	if a.Limit < 0 {
		return Errorf(CodeBadArgs, "list: negative limit")
	}
	if a.Limit == 0 || a.Limit > ListLimit {
		a.Limit = ListLimit
	}
	return nil
}

func validateRef(op string, a *AgentRef) *Error {
	if !ValidAgentID(a.AgentID) {
		return Errorf(CodeBadArgs, "%s: bad agentId", op)
	}
	return nil
}

func validateKill(a *KillArgs) *Error {
	if !ValidAgentID(a.AgentID) {
		return Errorf(CodeBadArgs, "kill: bad agentId")
	}
	if a.How != KillTerm && a.How != KillKill {
		return Errorf(CodeBadArgs, "kill: how must be term or kill")
	}
	return nil
}

func hasNUL(s string) bool { return strings.IndexByte(s, 0) >= 0 }
