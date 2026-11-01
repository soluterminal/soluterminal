package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newClippedClaudeAutoFireHarness(t *testing.T, id string) (*Aria, *Pane, <-chan directAutoFrameRead) {
	t.Helper()
	a, p, frames := newClaudeVerifyHarness(t, id)
	p.Cols, p.Rows = 62, 14
	p.resizeGridEmu(62, 14)
	return a, p, frames
}

// The command scrolls out of a normal grid tile. Each distinct command leaves
// precisely the same warning, question and options visible, with no DEC 2026
// frame markers to advance GridChoicePromptGeneration.
func clippedClaudeCommand(command string) string {
	return "Bash command\n│ " + command + "\n" +
		strings.Repeat("Multiple directory changes require approval for clarity\n", 10) +
		"Do you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel · Tab to amend"
}

func paintClippedClaudePrompt(t *testing.T, p *Pane, command string) uint64 {
	t.Helper()
	menu := clippedClaudeCommand(command)
	paintClaude(t, p, menu, "\n"+menu+"\n")
	visible, ok := p.gridVisibleText()
	if !ok || strings.Contains(visible, command) || !isGridChoicePromptText(visible, false) {
		t.Fatalf("fixture must show the menu with the command clipped: %q", visible)
	}
	if atomic.LoadUint64(&p.GridChoicePromptGeneration) != 0 {
		t.Fatal("Claude fixture manufactured a committed prompt generation")
	}
	return autoFireObservationIdentity(p, "")
}

func waitForAutoFireRearm(t *testing.T, a *Aria, p *Pane) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		rearmed := a.autoFireRepeats[p.ID] == nil
		a.mu.Unlock()
		if rearmed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("answered prompt did not rearm its repeat budget")
}

func TestAutoFireRearmClippedClaudePrompts(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(fmt.Sprintf("identical-command=%v", identical), func(t *testing.T) {
			a, p, frames := newClippedClaudeAutoFireHarness(t, "clipped-claude-rearm")
			var firstHash uint64
			for i := range 3 {
				command := "cd /repo && git show HEAD"
				if !identical {
					command += fmt.Sprintf("~%d", i)
				}
				hash := paintClippedClaudePrompt(t, p, command)
				if i == 0 {
					firstHash = hash
				}
				if hash == 0 || hash != firstHash {
					t.Fatal("fixture must reproduce identical visible observation hashes")
				}
				assertDirectAutoPromptReady(t, p, command)
				a.directAutoFire(context.Background(), p.ID, p.Title)
				expectFrame(t, frames, command, "\r")
				paintClaude(t, p, "Command finished\nWorking…", "\nCommand finished\n")
				waitForAutoFireRearm(t, a, p)
			}
		})
	}
}

// Missing a fast exit is deliberately conservative. Neither an acknowledged
// Enter nor a raw redraw proves turnover, so the original two-fire bound wins.
func TestAutoFireRearmMissedExitKeepsLoopGuard(t *testing.T) {
	for _, missedExit := range []bool{false, true} {
		t.Run(fmt.Sprintf("missed-exit=%v", missedExit), func(t *testing.T) {
			a, p, frames := newClippedClaudeAutoFireHarness(t, "clipped-claude-missed-exit")
			// Neither prompt exit can overlap a scheduled verifier check.
			a.autoFirePacingOverride.verify = []time.Duration{300 * time.Millisecond, 350 * time.Millisecond}
			for i := range 3 {
				paintClippedClaudePrompt(t, p, "cd /repo && git status")
				assertDirectAutoPromptReady(t, p, "redrawn menu")
				a.directAutoFire(context.Background(), p.ID, p.Title)
				if i == 2 {
					expectNoFrame(t, frames, "exhausted repeat budget")
					break
				}
				expectFrame(t, frames, "bounded approval", "\r")
				if missedExit {
					paintClaude(t, p, "Command finished", "\nCommand finished\n")
				}
			}
			time.Sleep(200 * time.Millisecond)
			expectNoFrame(t, frames, "verifier must preserve exhausted budget")
			a.mu.Lock()
			repeat := a.autoFireRepeats[p.ID]
			bounded := repeat != nil && repeat.fires == 2
			a.mu.Unlock()
			if !bounded {
				t.Fatal("unverified prompt exit reset the repeat budget")
			}
			if att, _ := p.AttentionNow(); att != AttentionNeedsUser {
				t.Fatalf("exhausted menu attention = %q, want needs-user", att)
			}
		})
	}
}

func TestAutoFireRearmOldVerifierCannotEraseLaterFire(t *testing.T) {
	t.Parallel()
	a, p, _ := newClippedClaudeAutoFireHarness(t, "clipped-claude-stale-verifier")
	paintClaude(t, p, "Command finished", "\nCommand finished\n")
	atomic.StoreInt64(&p.LastInputMs, time.Now().UnixMilli())
	a.recordUnchangedAutoFire(p.ID, 123, 0)
	a.verifyAutoFireLanded(p.ID, p.Title, atomic.LoadInt64(&p.LastInputMs))
	// A second Enter can share LastInputMs with the first. Simulate its
	// successful write bookkeeping after the old verifier has been scheduled.
	a.recordUnchangedAutoFire(p.ID, 123, 0)
	time.Sleep(200 * time.Millisecond)
	if !a.unchangedAutoFireExhausted(p.ID, 123, 0) {
		t.Fatal("old verifier erased a newer fire's repeat budget")
	}
}

func TestAutoFireRearmPreservesCommittedGenerationBudget(t *testing.T) {
	t.Parallel()
	a, p, _ := newClippedClaudeAutoFireHarness(t, "tracked-generation-budget")
	paintClaude(t, p, "Command finished", "\nCommand finished\n")
	atomic.StoreInt64(&p.LastInputMs, time.Now().UnixMilli())
	for range 2 {
		a.recordUnchangedAutoFire(p.ID, 123, 7)
	}
	a.verifyAutoFireLanded(p.ID, p.Title, atomic.LoadInt64(&p.LastInputMs))
	time.Sleep(200 * time.Millisecond)
	if !a.unchangedAutoFireExhausted(p.ID, 123, 7) {
		t.Fatal("verifier changed the committed-generation repeat budget")
	}
	if a.unchangedAutoFireExhausted(p.ID, 123, 8) {
		t.Fatal("new committed generation did not rearm normally")
	}
}

func TestAutoFireRearmMissingRenderIsNotExit(t *testing.T) {
	t.Parallel()
	a, p, _ := newClippedClaudeAutoFireHarness(t, "missing-render-budget")
	p.shutdownGrid()
	atomic.StoreInt64(&p.LastInputMs, time.Now().UnixMilli())
	for range 2 {
		a.recordUnchangedAutoFire(p.ID, 123, 0)
	}
	a.verifyAutoFireLanded(p.ID, p.Title, atomic.LoadInt64(&p.LastInputMs))
	time.Sleep(200 * time.Millisecond)
	if !a.unchangedAutoFireExhausted(p.ID, 123, 0) {
		t.Fatal("missing rendered evidence reset the repeat budget")
	}
}
