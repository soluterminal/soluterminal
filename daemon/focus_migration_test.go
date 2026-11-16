package main

import (
	"errors"
	"reflect"
	"testing"
)

// A user updating from a release that still had Focus can have saved Focus
// panes. Each must come back as a terminal pane on the same conversation, or
// as the usual "choose the original conversation" tile — never a crash, never
// a headless agent bound to a terminal, never a silently fresh conversation.
func TestRetireNativePersistedPane(t *testing.T) {
	headless := []string{"claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--resume", "sess-from-argv"}
	cases := []struct {
		name      string
		in        PersistedPane
		wantAgent string
		wantCmd   []string
		wantConv  string
	}{
		{
			name:      "Claude Focus pane",
			in:        PersistedPane{ID: "p1", Kind: "native", Cmd: headless, ClaudeSessionID: "sess-1"},
			wantAgent: agentClaude,
			wantCmd:   []string{"claude", "--resume", "sess-1"},
			wantConv:  "sess-1",
		},
		{
			name:      "session id lost but still in the headless argv",
			in:        PersistedPane{ID: "p2", Kind: "native", Cmd: headless},
			wantAgent: agentClaude,
			wantCmd:   []string{"claude", "--resume", "sess-from-argv"},
			wantConv:  "sess-from-argv",
		},
		{
			name:      "kind lost, headless argv left behind",
			in:        PersistedPane{ID: "p3", Cmd: headless},
			wantAgent: agentClaude,
			wantCmd:   []string{"claude", "--resume", "sess-from-argv"},
			wantConv:  "sess-from-argv",
		},
		{
			name:      "Codex Focus pane",
			in:        PersistedPane{ID: "p4", Kind: "native", Agent: agentCodex, Cmd: []string{"codex", "app-server"}, CodexThreadID: "thread-1"},
			wantAgent: agentCodex,
			wantCmd:   []string{"codex", "resume", "thread-1"},
			wantConv:  "thread-1",
		},
		{
			name:      "Muse Focus pane",
			in:        PersistedPane{ID: "p5", Kind: "native", Agent: agentMuse, Cmd: []string{"solud", "muse-driver"}, MuseSessionID: "muse-1"},
			wantAgent: agentMuse,
			wantCmd:   []string{"muse", "resume", "muse-1"},
			wantConv:  "muse-1",
		},
		{
			name:      "Codex Focus pane saved without an agent field",
			in:        PersistedPane{ID: "p7", Kind: "native", Cmd: []string{"codex", "app-server", "--stdio"}, CodexThreadID: "thread-7"},
			wantAgent: agentCodex,
			wantCmd:   []string{"codex", "resume", "thread-7"},
			wantConv:  "thread-7",
		},
		{
			name:      "Muse Focus pane saved without an agent field",
			in:        PersistedPane{ID: "p8", Kind: "native", Cmd: []string{"/opt/solud", "muse-driver", "--session-id", "muse-8"}, MuseSessionID: "muse-8"},
			wantAgent: agentMuse,
			wantCmd:   []string{"muse", "resume", "muse-8"},
			wantConv:  "muse-8",
		},
		{
			name:    "Focus pane that never saved a conversation",
			in:      PersistedPane{ID: "p6", Kind: "native", Cmd: []string{"claude"}},
			wantCmd: []string{"claude"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.SocketPath = "/nonexistent/" + in.ID + ".sock"
			in.AgentPid = -1
			got, ok := retireNativePersistedPane(in)
			if !ok {
				t.Fatal("a saved Focus pane must be converted")
			}
			if got.Kind != "" {
				t.Fatalf("kind = %q, want terminal", got.Kind)
			}
			if got.Agent != tc.wantAgent {
				t.Fatalf("agent = %q, want %q", got.Agent, tc.wantAgent)
			}
			if !reflect.DeepEqual(got.Cmd, tc.wantCmd) {
				t.Fatalf("cmd = %q, want %q", got.Cmd, tc.wantCmd)
			}
			if conv := persistedPaneConversationID(got); conv != tc.wantConv {
				t.Fatalf("conversation = %q, want %q", conv, tc.wantConv)
			}
			if got.AgentPid != 0 || got.SocketPath != "" {
				t.Fatalf("the headless agent must not be reattached: pid=%d socket=%q", got.AgentPid, got.SocketPath)
			}
			if isRetiredNativePane(got.Kind, got.Cmd) {
				t.Fatal("the converted pane must not read as Focus again")
			}
			if tc.wantConv == "" {
				if msg := missingPaneIdentityMessage(got); msg != "The conversation ID was not saved. Choose the original conversation to reopen this pane." {
					t.Fatalf("recovery message = %q", msg)
				}
			}
		})
	}
}

func TestRetireNativePersistedPaneLeavesTerminalPanesAlone(t *testing.T) {
	for _, in := range []PersistedPane{
		{ID: "t1", Cmd: []string{"claude", "--resume", "s"}, ClaudeSessionID: "s", AgentPid: 42, SocketPath: "/x.sock"},
		{ID: "t2", Cmd: []string{"bash", "-l"}, AgentPid: 43, SocketPath: "/y.sock"},
		{ID: "t3", Agent: agentCodex, Cmd: []string{"codex", "resume", "th"}, CodexThreadID: "th"},
	} {
		got, ok := retireNativePersistedPane(in)
		if ok || !reflect.DeepEqual(got, in) {
			t.Fatalf("terminal pane %s changed: ok=%v got=%+v", in.ID, ok, got)
		}
	}
}

func TestRetireNativeCreateRequest(t *testing.T) {
	fresh := createPaneReq{Kind: "native", Cmd: []string{"claude"}}
	if err := retireNativeCreateRequest(&fresh); !errors.Is(err, errFocusRemoved) {
		t.Fatalf("new Focus pane: err = %v, want errFocusRemoved", err)
	}

	restore := createPaneReq{Restore: true, Kind: "native", Agent: agentCodex, Cmd: []string{"codex"}, CodexThreadID: "thread-9"}
	if err := retireNativeCreateRequest(&restore); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restore.Kind != "" || !reflect.DeepEqual(restore.Cmd, []string{"codex", "resume", "thread-9"}) {
		t.Fatalf("restore became kind=%q cmd=%q", restore.Kind, restore.Cmd)
	}

	noAgent := createPaneReq{Restore: true, Kind: "native", Cmd: []string{"codex", "app-server", "--stdio"}, CodexThreadID: "thread-r"}
	if err := retireNativeCreateRequest(&noAgent); err != nil {
		t.Fatalf("agentless Codex restore: %v", err)
	}
	if noAgent.Agent != agentCodex || !reflect.DeepEqual(noAgent.Cmd, []string{"codex", "resume", "thread-r"}) {
		t.Fatalf("agentless Codex restore became agent=%q cmd=%q (must not turn into Claude)", noAgent.Agent, noAgent.Cmd)
	}

	lost := createPaneReq{Restore: true, Cmd: []string{"bash", "-lc", "claude -p --input-format stream-json --output-format stream-json '--resume' 'sess-w'"}}
	if err := retireNativeCreateRequest(&lost); err != nil {
		t.Fatalf("lost-kind restore: %v", err)
	}
	if lost.Agent != agentClaude || lost.ClaudeSessionID != "sess-w" || !reflect.DeepEqual(lost.Cmd, []string{"claude", "--resume", "sess-w"}) {
		t.Fatalf("lost-kind restore became agent=%q session=%q cmd=%q", lost.Agent, lost.ClaudeSessionID, lost.Cmd)
	}

	for _, req := range []createPaneReq{
		{Cmd: []string{"claude"}},
		{Restore: true, Cmd: []string{"claude", "--resume", "s"}, ClaudeSessionID: "s"},
	} {
		before := req
		if err := retireNativeCreateRequest(&req); err != nil || !reflect.DeepEqual(req, before) {
			t.Fatalf("terminal request changed: err=%v got=%+v", err, req)
		}
	}
}
