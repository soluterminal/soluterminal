package main

import (
	"slices"
	"strings"
	"testing"
)

func TestOrchestratorHostSupport(t *testing.T) {
	for _, agent := range []string{"", agentClaude, agentCodex} {
		if !orchestratorHostSupported(agent) {
			t.Fatalf("%q should support the orchestrator role", agent)
		}
	}
	for _, agent := range []string{agentMuse, "bash", "unknown"} {
		if orchestratorHostSupported(agent) || orchestratorHostUnsupportedReason(agent) == "" {
			t.Fatalf("%q unexpectedly supported or has no reason", agent)
		}
	}
	if !strings.Contains(orchestratorHostUnsupportedReason(agentMuse), "MCP") {
		t.Fatal("Muse reason must explain the missing MCP client")
	}
}

func TestWithCodexOrchestratorBrief(t *testing.T) {
	id := "019f5697-154c-7a92-8b61-ad2f9b68476d"
	got := withCodexOrchestratorBrief([]string{"codex", "resume", id})
	if len(got) != 5 || got[1] != "resume" || got[2] != id || resumeThreadIDFromCodexCmd(got) != id {
		t.Fatalf("decorated resume = %#v", got)
	}
	if stripped := stripCodexResumeArgv(got); !slices.Equal(stripped, []string{"codex"}) {
		t.Fatalf("stripped resume = %#v", stripped)
	}
	stale := []string{"codex", "-c", codexDeveloperInstructionsKey + orchestratorBriefMarker + " old"}
	got = withCodexOrchestratorBrief(stale)
	if strings.Count(strings.Join(got, "\x00"), codexDeveloperInstructionsKey) != 1 {
		t.Fatalf("stale policy was duplicated: %#v", got)
	}
	custom := withCodexOrchestratorBrief([]string{"codex", "-c", codexDeveloperInstructionsKey + "custom"})
	if len(custom) != 5 || custom[2] != codexDeveloperInstructionsKey+"custom" {
		t.Fatalf("custom override was not preserved: %#v", custom)
	}
	for _, untouched := range [][]string{{"codex", "app-server", "--stdio"}, {"claude"}, {"bash", "-lc", "exec 'codex'"}} {
		if actual := withCodexOrchestratorBrief(untouched); !slices.Equal(actual, untouched) {
			t.Fatalf("non-bare command changed: %#v", actual)
		}
	}
}

func TestSpawnCmdForRoleHosts(t *testing.T) {
	claude := spawnCmdForRole("", paneRoleOrchestrator, agentClaude, []string{"claude"})
	if !slices.Equal(claude, []string{"claude", "--append-system-prompt", orchestratorBrief}) {
		t.Fatalf("Claude argv = %#v", claude)
	}
	codex := spawnCmdForRole("", paneRoleOrchestrator, agentCodex, []string{"codex"})
	if !slices.Equal(codex, []string{"codex", "-c", codexDeveloperInstructionsKey + orchestratorBrief}) {
		t.Fatalf("Codex argv = %#v", codex)
	}
	for _, kind := range []string{""} {
		for _, role := range []string{"", paneRoleOrchestrator} {
			for _, agent := range []string{"", agentClaude, agentCodex, agentMuse} {
				cmd := []string{orchestratorHostLabel(agent)}
				switch effectiveAgentKind(agent) {
				case agentCodex:
					cmd = []string{"codex"}
				default:
					cmd = []string{"claude"}
				}
				got := spawnCmdForRole(kind, role, agent, cmd)
				decorated := len(got) > len(cmd)
				wantDecorated := kind == "" && role == paneRoleOrchestrator && (agent == "" || agent == agentClaude || agent == agentCodex)
				if decorated != wantDecorated {
					t.Fatalf("kind=%q role=%q agent=%q argv=%#v decorated=%v want=%v", kind, role, agent, got, decorated, wantDecorated)
				}
			}
		}
	}

	wrapped, ok := tryLoginShellWrap(withCodexOrchestratorBrief([]string{"codex", "resume", "019f5697-154c-7a92-8b61-ad2f9b68476d"}), "", nil)
	if !ok {
		t.Skip("bash is unavailable for login-shell transport test")
	}
	joined := strings.Join(wrapped, "\x00")
	if !strings.Contains(joined, "; exec ") || !strings.Contains(joined, " -i") {
		t.Fatalf("interactive Codex wrapper lost its shell tail: %#v", wrapped)
	}
	if !strings.Contains(joined, codexDeveloperInstructionsKey+orchestratorBriefMarker) {
		t.Fatalf("Codex wrapper lost the developer instructions: %#v", wrapped)
	}
	if strings.Contains(joined, `'\''`) {
		t.Fatalf("Codex policy required an escaped ASCII apostrophe: %#v", wrapped)
	}
}
