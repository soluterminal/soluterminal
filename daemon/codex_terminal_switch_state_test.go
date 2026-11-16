package main

import (
	"sync/atomic"
	"testing"
	"time"
)

const codexIdleTerminalScreen = "• You have 1 usage limit reset available. Run /usage to use one.\n\n" +
	"› Find and fix a bug in @filename\n\n" +
	"  gpt-5.6-sol xhigh · /mnt/c/users/alice/soluterminal · gpt-5.6-so…\n"

func codexTerminalStatePane(output string) *Pane {
	p := deferredTestPane("codex-terminal-state", "")
	p.Agent = agentCodex
	writeBuf(p, output)
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli()-500)
	return p
}

func TestIdleTerminalCodexDoesNotConfirmOrStampInterrupted(t *testing.T) {
	t.Parallel()
	p := codexTerminalStatePane(codexIdleTerminalScreen)
	if got := p.computeStateWithHasCodex(true); got != StateBusy {
		t.Fatalf("precondition: fresh idle redraw state = %q, want busy recency tail", got)
	}
	if !p.claudePromptVisible() {
		t.Fatal("idle Codex composer/footer was not recognized as handed back")
	}
	if p.agentMidTurnForState(StateBusy) || p.IsAgentWorking() {
		t.Fatal("idle Terminal Codex must not ask to switch when done or stamp interrupted")
	}
}

func TestTerminalCodexStartupIsNotAConversationTurn(t *testing.T) {
	t.Parallel()
	startup := "• Starting MCP servers (0s • esc to interrupt)\n\n" + codexIdleTerminalScreen
	p := codexTerminalStatePane(startup)
	if !p.claudePromptVisible() || p.agentMidTurnForState(StateBusy) {
		t.Fatal("Codex startup animation must not be treated as an interruptible model turn")
	}
}

func TestWorkingTerminalCodexStillConfirmsAndInterrupts(t *testing.T) {
	t.Parallel()
	working := "• Working (1m 09s • esc to interrupt) · 1 background terminal running\n\n" + codexIdleTerminalScreen
	p := codexTerminalStatePane(working)
	if p.claudePromptVisible() {
		t.Fatal("live Codex Working row was mistaken for an idle prompt")
	}
	if !p.agentMidTurnForState(StateBusy) || !p.IsAgentWorking() {
		t.Fatal("a real Terminal Codex turn must retain the switch confirmation and interrupt path")
	}
}

func TestTerminalCodexContextFooterAndAnimatedIdleComposer(t *testing.T) {
	t.Parallel()
	for _, composer := range []string{"› Ask Codex to do anything", "›⠁Ask Codex to do anything⡀"} {
		idle := "■ Conversation interrupted - tell the model what to do differently.\n" + composer + "\n  Context 62% used · gpt-6-astra xhigh · ~/wsl-projects/soluterminal\n"
		p := codexTerminalStatePane(idle)
		if !p.codexPromptVisible() || fleetPaneBusyForRestart(p) {
			t.Fatalf("interrupted idle pane still blocks fleet restart: %q", composer)
		}
		working := codexTerminalStatePane("• Working (1m 09s • esc to interrupt)\n" + idle)
		if working.codexPromptVisible() || !fleetPaneBusyForRestart(working) {
			t.Fatal("new footer must not hide a live turn")
		}
	}
}
