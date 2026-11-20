package main

import "testing"

func TestRecordingPaneAgent(t *testing.T) {
	for _, c := range []struct {
		agent string
		cmd   []string
		want  string
	}{
		{"", []string{"claude"}, agentClaude},
		{"", []string{"/home/u/.nvm/versions/node/v24/bin/claude", "--resume", "x"}, agentClaude},
		{"", []string{"bash", "-lc", "cd '/w' && exec 'claude' --resume 'x'"}, agentClaude},
		{"claude", []string{"bash"}, agentClaude},
		{"codex", []string{"codex"}, agentCodex},
		{"", []string{"codex"}, agentCodex},
		{"", []string{"bash", "-lc", "exec 'codex' resume"}, agentCodex},
		{"muse", []string{"muse"}, agentMuse},
		{"", []string{"bash"}, ""},
		{"", nil, ""},
	} {
		if got := recordingPaneAgent(c.agent, c.cmd); got != c.want {
			t.Errorf("recordingPaneAgent(%q, %q) = %q, want %q", c.agent, c.cmd, got, c.want)
		}
	}
}
