package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"soluterminal/grid"
)

// Dropped-Enter recovery for auto-approvals.
//
// computeState counts a menu as answered as soon as any input lands past the
// position it was acknowledged at, and an auto-approval Enter is such input.
// When the TUI drops that Enter (Claude does while a queued permission dialog
// is still mounting, or while it still processes the previous answer), the
// menu stays on screen but every poller reads the pane as idle, so nothing
// re-evaluates it. The same suppression hides an in-place "1 of 2" → "2 of 2"
// handover whose option rows match (two Reads in one directory): Claude's grid
// fingerprint carries only option rows, and Claude never advances the grid
// prompt generation. Only a full repaint, such as resizing or maximizing the
// pane, made the menu new again (2026-10-15, pane_37d81442d65c2c08: parked for
// 3 minutes on "Read file 2 of 2").
//
// After each auto-approval the rendered screen is checked a few times. The
// same dialog rendered on two consecutive checks while computeState reports no
// prompt, with no other input since that Enter, is re-admitted as a new prompt
// and evaluated through the normal guarded path. Checks start after the pacing
// settle window, and the second sighting rules out a TUI that is merely slow to
// repaint an accepted answer, whose late repaint a retried Enter would
// otherwise answer the next dialog through. The unchanged-screen fire limit
// still bounds a TUI that keeps ignoring Enter, and then surfaces the pane.
var ariaAutoFireVerifyAfter = []time.Duration{
	time.Second,
	1300 * time.Millisecond,
	3 * time.Second,
	3300 * time.Millisecond,
	6 * time.Second,
}

// verifyAutoFireLanded schedules the post-Enter checks. inputMs is the pane's
// LastInputMs right after the auto-approval Enter; any later input (the user,
// or a later auto-approval that schedules its own checks) ends the checks.
func (a *Aria) verifyAutoFireLanded(paneID, paneTitle string, inputMs int64) {
	delays := a.pacing().verify
	if len(delays) == 0 || inputMs == 0 {
		return
	}
	original, ok := a.mgr.Get(paneID)
	if !ok {
		return
	}
	policy := original.autoModePolicy()
	a.mu.Lock()
	repeat := a.autoFireRepeats[paneID]
	fires := 0
	if repeat != nil {
		fires = repeat.fires
	}
	a.mu.Unlock()
	start := time.Now()
	var lastKey uint64
	var lastAbsent bool
	var check func(i int)
	check = func(i int) {
		p, ok := a.mgr.Get(paneID)
		if !ok || p != original || !policy.matches(p) || atomic.LoadInt64(&p.LastInputMs) != inputMs {
			return
		}
		if p.computeState() == StateChoicePrompt {
			// Already visible to the watcher, sweeper and pacing retry.
			return
		}
		key := autoFireSettleKey(p)
		// computeState alone cannot prove an exit: our Enter makes even an
		// ignored menu read as answered. Require two rendered non-menu checks,
		// so a transient clear during a Claude repaint does not rearm the loop
		// guard. Missing emulators are not evidence of an answered prompt.
		absent := key == 0 && p.renderedChoicePromptAbsent()
		if absent && lastAbsent {
			a.mu.Lock()
			// Bind this reset to the fire we scheduled for, including same-ms
			// Enters. An old verifier must never erase a later fire's budget.
			if repeat != nil && a.autoFireRepeats[paneID] == repeat && repeat.fires == fires &&
				repeat.gridPromptGeneration == 0 && atomic.LoadInt64(&p.LastInputMs) == inputMs {
				delete(a.autoFireRepeats, paneID)
			}
			a.mu.Unlock()
			return
		}
		lastAbsent = absent
		if key != 0 && key == lastKey {
			p.reopenRenderedChoicePrompt()
			if p.computeState() != StateChoicePrompt {
				log.Printf("aria-auto-verify: pane=%s menu still rendered %v after Enter but could not be re-admitted",
					paneID, time.Since(start).Round(time.Millisecond))
				return
			}
			log.Printf("aria-auto-verify: pane=%s menu still rendered %v after Enter; re-admitted",
				paneID, time.Since(start).Round(time.Millisecond))
			ctx := context.WithValue(context.Background(), ariaAutoModePolicyKey{}, ariaAutoModePolicy{pane: p, policy: policy})
			a.evaluateObservation(ctx, paneID, paneTitle, "")
			return
		}
		lastKey = key
		if i+1 < len(delays) {
			time.AfterFunc(delays[i+1]-delays[i], func() { check(i + 1) })
		}
	}
	time.AfterFunc(delays[0], func() { check(0) })
}

// renderedChoicePromptAbsent requires a real rendered observation, with no
// choice menu in either emulator. Read both under the output-feed lock so a
// new menu cannot be combined with a previous screen's absence.
func (p *Pane) renderedChoicePromptAbsent() bool {
	hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()
	p.subsMu.RLock()
	defer p.subsMu.RUnlock()
	observed := false
	for _, em := range []*grid.Emulator{p.gridEmu, p.replayEmu} {
		if em == nil {
			continue
		}
		text, ok := em.VisibleText()
		if ok && isGridChoicePromptText(text, hasCodex) {
			return false
		}
		observed = observed || ok
	}
	return observed
}

// reopenRenderedChoicePrompt forgets which rendered menu was last admitted, so
// the next computeState admits the menu on screen as a new prompt and acks it
// past the latest input. Only for a menu proven still pending.
func (p *Pane) reopenRenderedChoicePrompt() {
	atomic.StoreUint64(&p.LastGridChoicePromptHash, 0)
	atomic.StoreUint64(&p.LastScreenChoicePromptHash, 0)
}
