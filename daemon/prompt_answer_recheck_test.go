package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// ST-52: the grant is decided again under the writer lock when the answer is
// written, so a revoke between admission and write refuses it with nothing
// written.
func TestAnswerPromptRevokeBeforeWriteWritesNothing(t *testing.T) {
	p := choicePromptPane(t, "st52-terminal")
	daemonEnd, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = daemonEnd.Close(); _ = agentEnd.Close() })
	p.mu.Lock()
	p.agentConn = daemonEnd
	p.mu.Unlock()
	frames := make(chan string, 4)
	go func() {
		for {
			_, payload, err := ReadFrame(agentEnd)
			if err != nil {
				return
			}
			frames <- string(payload)
		}
	}()
	grant := grantedAgentAuthority(t)
	server := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grant}
	meta := mcpMetaFromCallerPane("other-pane")
	if v := server.decidePromptAnswer(meta, false, p); !v.allowed {
		t.Fatalf("fixture caller not admitted: %s", v.refusal)
	}
	recheck := server.promptAnswerRecheck(meta, false, p)
	if err := grant.set(agentAuthorityRecord{}); err != nil {
		t.Fatal(err)
	}
	res, prompt := server.mcpToolAnswerPromptChecked(map[string]any{"pane_id": p.ID, "option": "1"}, recheck)
	text, isErr := mcpResultText(t, res)
	if !isErr || !strings.Contains(text, "Agent permissions") || !strings.Contains(text, "Nothing was written") || prompt != "" {
		t.Fatalf("answer after revoke = %q (err=%v, prompt=%q)", text, isErr, prompt)
	}
	select {
	case got := <-frames:
		t.Fatalf("answer after revoke wrote %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	// With the grant back on, the same admitted answer is written.
	if err := grant.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	res, _ = server.mcpToolAnswerPromptChecked(map[string]any{"pane_id": p.ID, "option": "1"}, recheck)
	if text, isErr := mcpResultText(t, res); isErr {
		t.Fatalf("answer under the grant = %q", text)
	}
	select {
	case got := <-frames:
		if got != "1" {
			t.Fatalf("answer under the grant wrote %q, want 1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answer under the grant wrote nothing")
	}
}
