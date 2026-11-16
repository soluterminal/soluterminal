//go:build !windows

package main

import "testing"

func TestFindAgentDescendantPIDUsesSelectedProvider(t *testing.T) {
	t.Parallel()
	tree := processTreeSnapshot{children: map[int][]int{
		10: {11, 12},
		12: {13},
	}}
	commByPID := map[int]string{
		10: "solud",
		11: "claude",
		12: "codex",
		13: "codex",
	}
	read := func(pid int) string { return commByPID[pid] }

	if got := findAgentDescendantPID(tree, 10, agentClaude, read); got != 11 {
		t.Fatalf("Claude pid = %d, want 11", got)
	}
	if got := findAgentDescendantPID(tree, 10, agentCodex, read); got != 12 {
		t.Fatalf("Codex pid = %d, want 12", got)
	}
	if got := findAgentDescendantPID(tree, 99, agentCodex, read); got != 0 {
		t.Fatalf("missing Codex pid = %d, want 0", got)
	}
}

func TestPaneDTOEmitsProviderNeutralAndCompatibilityRSS(t *testing.T) {
	t.Parallel()
	p := switchTestPane("rss_dto", "", []string{"claude"}, "session-1")
	p.agentRssMB.Store(384)

	dto := makePaneDTO(p, nil, nil)
	if dto.AgentRssMB != 384 || dto.ClaudeRssMB != 384 {
		t.Fatalf("rss DTO = agent:%d legacy:%d, want 384/384", dto.AgentRssMB, dto.ClaudeRssMB)
	}
}
