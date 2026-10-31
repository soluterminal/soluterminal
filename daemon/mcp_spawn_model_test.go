package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestSpawnAgentModelArgv(t *testing.T) {
	for _, tt := range []struct {
		name          string
		cmd           []string
		model, effort string
		want          []string
		wantErr       string
	}{
		{name: "nothing asked keeps cmd", cmd: []string{"bash"}, want: []string{"bash"}},
		{name: "claude model and effort", cmd: []string{"claude"}, model: "opus", effort: "high", want: []string{"claude", "--model", "opus", "--effort", "high"}},
		{name: "claude long-context id", cmd: []string{"claude"}, model: "claude-fable-5-1[1m]", want: []string{"claude", "--model", "claude-fable-5-1[1m]"}},
		{name: "claude max effort", cmd: []string{"claude"}, effort: "MAX", want: []string{"claude", "--effort", "max"}},
		{name: "codex model and effort", cmd: []string{"codex"}, model: "gpt-5.5-codex", effort: "xhigh", want: []string{"codex", "-m", "gpt-5.5-codex", "-c", "model_reasoning_effort=xhigh"}},
		{name: "codex model with max", cmd: []string{"codex"}, model: "gpt-6-astra", effort: "max", want: []string{"codex", "-m", "gpt-6-astra", "-c", "model_reasoning_effort=max"}},
		{name: "codex model without max", cmd: []string{"codex"}, model: "gpt-5.4", effort: "max", wantErr: "not supported by this Codex model"},
		{name: "uncatalogued codex model", cmd: []string{"codex"}, model: "gpt-9-future", effort: "max", wantErr: "not supported by this Codex model"},
		{name: "unknown effort", cmd: []string{"claude"}, effort: "turbo", wantErr: "not one of"},
		{name: "model that reads as a flag", cmd: []string{"claude"}, model: "--dangerously-skip-permissions", wantErr: "not a model id"},
		{name: "model with shell characters", cmd: []string{"codex"}, model: "x; rm -rf ~", wantErr: "not a model id"},
		{name: "shell pane", cmd: []string{"bash"}, model: "opus", wantErr: "need cmd"},
		{name: "default shell", cmd: nil, effort: "low", wantErr: "need cmd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := spawnAgentModelArgv(tt.cmd, tt.model, tt.effort)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tt.want)
			}
		})
	}
}

func TestMCPSpawnPaneModelReachesArgvAndKeepsRoleBrief(t *testing.T) {
	var captured []SpawnOpts
	mgr := NewManager(t.TempDir())
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Put(&Workspace{Name: "Default"}); err != nil {
		t.Fatal(err)
	}
	api := &apiServer{
		mgr:   mgr,
		ws:    ws,
		panes: &failingPaneStore{},
		orchestratorHostProbe: func(agent string) orchestratorHostAvailability {
			return orchestratorHostAvailability{Agent: agent, Label: orchestratorHostLabel(agent), Supported: orchestratorHostSupported(agent), Installed: true, Available: true}
		},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			captured = append(captured, o)
			p := transactionTestPane("pane_mcp_model")
			p.Cmd = append([]string(nil), o.Cmd...)
			mgr.panes[p.ID] = p
			return p, nil
		},
	}

	got := api.mcpToolSpawnPane(map[string]any{"cmd": "bash", "model": "opus"}, "")
	if !strings.Contains(fmt.Sprint(got), "need cmd") || len(captured) != 0 {
		t.Fatalf("shell with a model: result %#v, spawned %d; want refusal and no pane", got, len(captured))
	}

	api.mcpToolSpawnPane(map[string]any{"cmd": "codex", "model": "gpt-5.5-codex", "effort": "high", "role": "orchestrator"}, "")
	if len(captured) != 1 {
		t.Fatalf("spawned %d panes, want 1", len(captured))
	}
	o := captured[0]
	if !slices.Equal(o.Cmd, []string{"codex", "-m", "gpt-5.5-codex", "-c", "model_reasoning_effort=high"}) || o.Agent != agentCodex || o.Role != paneRoleOrchestrator {
		t.Fatalf("opts = %#v, want a Codex orchestrator with the model flags", o)
	}
	// Manager.Spawn adds the role brief after the model flags; the decorated
	// argv must still carry both.
	withBrief := spawnCmdForRole("", o.Role, o.Agent, o.Cmd)
	if !slices.Contains(withBrief, "gpt-5.5-codex") || !strings.Contains(strings.Join(withBrief, "\x00"), codexDeveloperInstructionsKey+orchestratorBriefMarker) {
		t.Fatalf("decorated argv = %#v, want model flags and the brief", withBrief)
	}
	resumed, ok := codexLiveResumeCommand(o.Cmd, "019f5697-154c-7a92-8b61-ad2f9b68476d")
	if !ok || !slices.Contains(resumed, "gpt-5.5-codex") || !slices.Contains(resumed, "model_reasoning_effort=high") {
		t.Fatalf("resume argv = %#v (ok=%v), want the model flags kept", resumed, ok)
	}
}
