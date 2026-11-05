package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ariaTailAdaptiveMaxLines bounds the expanded scrollback window when
// the base tail looks blind. See tailOfPaneAdaptive.
const ariaTailAdaptiveMaxLines = 120

// tailOfPane copy-window bounds. The window scales with the requested
// line count (16 KB/line) so heavy ANSI/spinner-redraw lines still fit,
// but is clamped to [256 KB, 2 MB] — far below the 8 MB ring — so a pane
// parked on an approval menu can't drive per-tick multi-MB garbage.
const (
	tailOfPaneMinWindowBytes = 256 * 1024
	tailOfPaneMaxWindowBytes = 2 * 1024 * 1024
)

// startAriaWatcher publishes "aria-observation" uiBus events when a
// pane transitions into an attention-requiring state (awaiting-input
// or choice-prompt). Each observation carries a short tail excerpt
// captured at the moment of transition so the UI can show
// "<pane> is waiting on: …" inline without an extra round-trip.
//
// This is step 1 of ARIA's monitoring roadmap: NOTIFY ONLY. No LLM
// call, no auto-reply. The goal is to prove the state-change signal
// is reliable enough to build scoped standing orders on top of in
// step 2 — not to act autonomously yet.
//
// Plumbing: we subscribe to the same uiBus the activity watcher
// publishes into. That means the upstream filter (drop sub-1s
// bounces, drop non-attention transitions, drop the noisy `recent`
// intermediate) is already applied before we see the event — we
// only add a cheap per-pane debounce on top in case the same pane
// flaps between awaiting-input and choice-prompt in quick succession.
//
// Feedback-loop safety: we publish `aria-observation` events, and
// filter incoming to `pane-state` only, so we never recurse on our
// own output.
func startAriaWatcher(ctx context.Context, mgr *Manager, bus *uiBus, aria *Aria) {
	ch := bus.subscribe()
	defer bus.unsubscribe(ch)

	// Per-pane debounce. Tuned per-state because they have very
	// different semantics:
	//   - awaiting-input flaps a lot (Claude returns to its idle
	//     `❯ … [Opus 4.7]` box after every reply), so 20s suppresses
	//     duplicate cards from a single user-visible event.
	//   - choice-prompt is rare AND each occurrence is a fresh
	//     interactive prompt the user/auto-mode needs to answer. Two
	//     back-to-back approvals (e.g. always-mode answers prompt A,
	//     prompt B appears 10s later) MUST both fire — so we use a
	//     short 2s debounce just to coalesce the sub-second redraw
	//     flicker the upstream activity.go can sometimes emit.
	// Bug history: a single 20s debounce ate the second of two
	// back-to-back approvals in always-mode panes; pane sat in
	// choice-prompt forever because no further state event ever came.
	const awaitingInputMinGap = 20 * time.Second
	const choicePromptMinGap = 2 * time.Second
	var (
		mu         sync.Mutex
		lastFor    = map[string]time.Time{}
		lastForPos = map[string]int64{}
	)

	// fireForPane publishes an observation + (maybe) auto-evaluates.
	// Returns false if the per-pane debounce blocked the fire.
	// Shared between the SSE event loop and the recovery sweeper so
	// "stuck choice-prompt" panes (no further state-transition events
	// will ever come) still get serviced.
	fireForPane := func(paneID, paneTitle, toState string, fastWake bool) bool {
		p, found := mgr.Get(paneID)
		if !found {
			return false
		}
		if toState == string(StateChoicePrompt) && aria.completionRefused(p) {
			return false
		}
		minGap := awaitingInputMinGap
		var pos int64
		if toState == string(StateChoicePrompt) {
			minGap = choicePromptMinGap
			pos = atomic.LoadInt64(&p.LastAckedChoicePromptPos)
		}
		mu.Lock()
		previousPos, hadPos := lastForPos[paneID]
		newChoice := toState == string(StateChoicePrompt) && (!hadPos || pos != previousPos)
		// A new menu may arrive while the pane stays in choice-prompt.
		// Bypass the old 2s debounce for that distinct instance, while a
		// fast output wake fires at most once for each acknowledged menu.
		if fastWake && !newChoice {
			mu.Unlock()
			return false
		}
		if t, seen := lastFor[paneID]; seen && !newChoice && time.Since(t) < minGap {
			mu.Unlock()
			return false
		}
		lastFor[paneID] = time.Now()
		if toState == string(StateChoicePrompt) {
			lastForPos[paneID] = pos
		}
		mu.Unlock()
		tail := tailOfPaneAdaptive(p, 30)
		// Native panes keep raw stream-json JSONL in the ring buffer, which is
		// useless as an observation. Replace the tail with a structured summary
		// of the pending approval(s). evaluateObservation's native branch builds
		// its own per-approval prompt, so this mainly improves the UI
		// observation card; the terminal branch still gets its grid tail.
		if p.Snapshot().Kind == "native" {
			if nt := nativeObservationText(p); nt != "" {
				tail = nt
			}
		}
		bus.publish(uiEvent{
			Type:      "aria-observation",
			PaneID:    paneID,
			PaneTitle: paneTitle,
			ToState:   toState,
			Ts:        time.Now().UnixMilli(),
			Text:      tail,
		})
		// One-shot resume banner: if a persisted pause survived the
		// last restart, surface it the first time anything happens
		// post-boot. Quietly cleared after firing so we don't nag.
		if aria != nil {
			aria.maybeFireResumePrompt()
		}
		// Auto-eval gate: only choice-prompt (numbered menu / explicit
		// y/n). awaiting-input fires after every Claude reply and
		// answering those is the false-positive class.
		if aria != nil && toState == string(StateChoicePrompt) {
			if p.Snapshot().AutoMode != "" {
				go aria.evaluateObservation(ctx, paneID, paneTitle, tail)
			}
		}
		return true
	}

	// Recovery sweeper: every 5s walk every pane, and for any in
	// choice-prompt with no recent fire (older than the debounce
	// window) re-fire the eval. Catches the "pane stuck in
	// choice-prompt because the transition event was eaten by the
	// debounce" case — the SSE event loop is purely transition-driven
	// and can't recover from a missed edge on its own.
	//
	// Tuned 30s → 5s on 2026-04-27 after observing back-to-back
	// approval bursts where Claude emits menu N+1 within ~5s of
	// answering menu N. The 2s choice-prompt debounce eats the
	// transition; 30s sweeper meant ~30s of user-visible lag before
	// the sweeper caught it. 5s polling is still essentially free
	// (just walks the panes map) and bounds the worst case to ~5s.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		// awaiting-approval surfacing: a pane sitting in StateChoicePrompt
		// for ≥15s without auto-approval needs human attention. Track
		// entry time per-pane and fire pane.awaiting_approval once per
		// distinct menu.
		const awaitingApprovalThreshold = 15 * time.Second
		enteredChoicePromptAt := map[string]time.Time{}
		// Position of the menu we last fired awaiting_approval for. Keying
		// the "already notified" guard on the menu position (rather than a
		// bare per-pane bool) closes the edge-fire pitfall: when the user
		// answers menu A and Claude immediately presents menu B, the pane
		// never leaves StateChoicePrompt, so a bool guard would suppress
		// the second notification forever. LastAckedChoicePromptPos
		// advances whenever computeState sees a genuinely new menu, so a
		// changed position re-arms both the dwell timer and the notify.
		notifiedForPos := map[string]int64{}
		choicePromptPos := map[string]int64{}
		clearAwaiting := func(id string) {
			delete(enteredChoicePromptAt, id)
			delete(notifiedForPos, id)
			delete(choicePromptPos, id)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				current := map[string]struct{}{}
				for _, p := range mgr.List() {
					snap := p.Snapshot()
					current[snap.ID] = struct{}{}
					if snap.Status != PaneRunning {
						clearAwaiting(snap.ID)
						continue
					}
					if p.computeState() != StateChoicePrompt {
						clearAwaiting(snap.ID)
						continue
					}
					// Re-arm on a new menu: first sighting, or the menu
					// changed under us while staying in choice-prompt.
					pos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
					if prev, had := choicePromptPos[snap.ID]; !had || prev != pos {
						choicePromptPos[snap.ID] = pos
						enteredChoicePromptAt[snap.ID] = time.Now()
						delete(notifiedForPos, snap.ID)
					}
					// Fire once per distinct menu after the dwell threshold —
					// but never while the attention engine says auto-approval
					// is expected to clear it (auto-pending). When the engine
					// flips to needs-user (decline / never-auto tool /
					// escalation), the notification fires on the next tick if
					// the dwell has already elapsed.
					att, _ := p.AttentionNow()
					if _, notified := notifiedForPos[snap.ID]; !notified && att != AttentionAutoPending &&
						time.Since(enteredChoicePromptAt[snap.ID]) >= awaitingApprovalThreshold {
						// Resolve the named workspace so the aria-watcher's
						// "Approval needed — X" title can show it instead of a
						// generic pane title. "" when not in a named workspace.
						workspace := ""
						if aria != nil && aria.ws != nil {
							workspace = aria.ws.NameForPane(snap.ID)
						}
						bus.publish(uiEvent{
							Type:      "pane.awaiting_approval",
							PaneID:    snap.ID,
							PaneTitle: snap.Title,
							Workspace: workspace,
							Ts:        time.Now().UnixMilli(),
							Text:      tailOfPaneAdaptive(p, 30),
						})
						notifiedForPos[snap.ID] = pos
					}
					// Sweeper backoff: if ARIA has already declined /
					// errored on this exact menu position N times in a
					// row, stop hammering it. Re-arms when the pane's
					// acked choice-prompt position advances (i.e. a
					// genuinely new menu appears). The transition-driven
					// path (line ~145 below) is unaffected.
					if aria != nil {
						ackedPos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
						if aria.shouldSkipSweeper(snap.ID, ackedPos) {
							continue
						}
					}
					fireForPane(snap.ID, snap.Title, string(StateChoicePrompt), false)
				}
				// Cleanup: drop tracking for panes that no longer exist.
				for id := range enteredChoicePromptAt {
					if _, ok := current[id]; !ok {
						clearAwaiting(id)
					}
				}
				mu.Lock()
				for id := range lastFor {
					if _, ok := current[id]; !ok {
						delete(lastFor, id)
						delete(lastForPos, id)
					}
				}
				mu.Unlock()
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case paneID := <-mgr.autoPromptWake:
			p, found := mgr.Get(paneID)
			if !found {
				continue
			}
			snap := p.Snapshot()
			if snap.Status != PaneRunning || snap.AutoMode == "" || snap.Kind == "native" {
				continue
			}
			// Output wakeups are only hints. Recheck after the frame has
			// settled, using the direct state path so a cached old state
			// cannot admit a vanished menu.
			if p.computeState() == StateChoicePrompt {
				fireForPane(paneID, snap.Title, string(StateChoicePrompt), true)
			}
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if evt.Type != "pane-state" {
				continue
			}
			if evt.ToState != string(StateAwaitingInput) && evt.ToState != string(StateChoicePrompt) {
				continue
			}
			if evt.PaneID == "" {
				continue
			}
			fireForPane(evt.PaneID, evt.PaneTitle, evt.ToState, false)
		}
	}
}

// tailOfPane returns the last nLines of a pane's ANSI-stripped
// scrollback, trimmed of trailing whitespace. Shared with
// toolReadPaneTail so the two code paths stay in lockstep when we
// tune the tail shape.
//
// Trailing bottom-status spinner lines are skipped before counting
// back — Claude Code redraws its animated spinner ("✶ thinking…") in
// place using cursor-position escapes, and stripANSI loses the
// in-place overwrite, so each redraw becomes a fresh line. A pane
// stuck in choice-prompt for ~30s can accumulate hundreds of these
// between the actual menu and the buffer's tail, pushing the menu
// out of any fixed N-line window. Without this, `pane_7603fcb...` on
// 2026-04-27 had 484 trailing junk lines hiding a real "Do you want
// to proceed? ❯ 1. Yes" menu, and the auto-eval saw nothing
// actionable in its 30-line tail.
func tailOfPane(p *Pane, nLines int) string {
	// Bound the copy+strip to a recent tail window instead of the full
	// 8 MB ring. The full Snapshot()+stripANSI here (run by the 5 s
	// recovery sweeper per choice-prompt pane) produced the same
	// GC-death-spiral class that computeState was fixed for in v0.0.484:
	// ~4 transient 8 MB allocations per call, multiplied across panes
	// parked on approval menus. The window scales with the requested line
	// count to keep headroom for heavy trailing spinner-redraw junk (the
	// 484-junk-line case below), but caps well under the ring so the
	// allocation stays bounded. Menus are always in the most recent
	// output, so a recent-tail window never loses them.
	windowBytes := nLines * 16 * 1024
	if windowBytes < tailOfPaneMinWindowBytes {
		windowBytes = tailOfPaneMinWindowBytes
	}
	if windowBytes > tailOfPaneMaxWindowBytes {
		windowBytes = tailOfPaneMaxWindowBytes
	}
	raw := p.buffer.SnapshotTail(windowBytes)
	stripped := stripANSI(raw)
	bytesEnd := trimTrailingStatusJunk(stripped)
	lineCount := 0
	start := bytesEnd
	for i := bytesEnd - 1; i >= 0; i-- {
		if stripped[i] == '\n' {
			lineCount++
			if lineCount > nLines {
				start = i + 1
				break
			}
		}
		start = i
	}
	return capPaneTail(strings.TrimRight(string(stripped[start:bytesEnd]), " \t\r\n"))
}

// tailOfPaneAdaptive returns tailOfPane(p, baseN), but if the result
// looks blind (no menu anchor visible OR fewer than 5 surviving lines
// after junk-trim) it retries with a larger window before returning.
// Spinner-redraw frames can fill the 30-line window; expand on
// detection so we don't blind-decline real menus that landed older
// than 30 lines ago.
func tailOfPaneAdaptive(p *Pane, baseN int) string {
	out := tailOfPane(p, baseN)
	if hasMenuAnchor(out) && countNonEmptyLines(out) >= 5 {
		return out
	}
	expanded := tailOfPane(p, ariaTailAdaptiveMaxLines)
	if expanded != out {
		log.Printf("aria_watch: tail expanded from %d to %d lines for pane %s (base looked blind)", baseN, ariaTailAdaptiveMaxLines, p.ID)
	}
	return expanded
}

// hasMenuAnchor reports whether s contains at least one line that
// trimTrailingStatusJunk would consider actionable (menu marker /
// numbered option / long-enough alphanumeric content).
func hasMenuAnchor(s string) bool {
	if s == "" {
		return false
	}
	for _, line := range strings.Split(s, "\n") {
		if !isStatusJunkLine(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

func countNonEmptyLines(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

const (
	ariaTailMaxBytes     = 64 * 1024
	ariaTailMaxLineRunes = 4000
)

func capPaneTail(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	truncated := false
	for i, line := range lines {
		if len([]rune(line)) > ariaTailMaxLineRunes {
			r := []rune(line)
			lines[i] = "[line truncated] " + string(r[len(r)-ariaTailMaxLineRunes:])
			truncated = true
		}
	}
	s = strings.Join(lines, "\n")
	if len(s) > ariaTailMaxBytes {
		start := len(s) - ariaTailMaxBytes
		for start < len(s) && (s[start]&0xc0) == 0x80 {
			start++
		}
		s = "[tail truncated]\n" + s[start:]
		truncated = true
	}
	if truncated {
		return s
	}
	return s
}

func currentChoicePromptIdentity(p *Pane, visibleTail string) (state PaneRuntimeState, pos int64, fp uint64, ok bool) {
	state = p.computeState()
	if state != StateChoicePrompt {
		return state, 0, 0, false
	}
	pos = atomic.LoadInt64(&p.LastAckedChoicePromptPos)
	hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()
	// Cell-diff Claude menus leave stale or incomplete requests in the raw
	// tail. Use the same currently rendered menu as admission and the final
	// approval classifier, including for Claude (not just Codex/Muse).
	if gridText, visible := p.gridVisibleText(); visible && isGridChoicePromptText(gridText, hasCodex) {
		fp = approvalPromptFingerprint(gridText, hasCodex)
		if fp != 0 {
			return state, pos, fp, true
		}
	}
	if visibleTail != "" {
		fp = approvalPromptFingerprint(visibleTail, hasCodex)
	}
	if fp == 0 {
		fp = approvalPromptFingerprint(tailOfPaneAdaptive(p, 30), hasCodex)
	}
	if fp == 0 && hasCodex {
		p.ensureGrid()
		if gridText, ok := p.gridVisibleText(); ok {
			fp = approvalPromptFingerprint(gridText, hasCodex)
		}
	}
	return state, pos, fp, true
}

func choicePromptIdentityMatches(p *Pane, expectedPos int64, expectedHash uint64) bool {
	_, pos, fp, ok := currentChoicePromptIdentity(p, "")
	if !ok {
		return false
	}
	if expectedPos > 0 && pos != expectedPos {
		return false
	}
	if expectedHash != 0 && fp != 0 && fp != expectedHash {
		return false
	}
	return true
}

// trimTrailingStatusJunk returns the byte offset just past the last
// line of `stripped` that carries actionable TUI content, walking
// backward over empty/whitespace-only lines and short bottom-status
// spinner artifacts (e.g. mangled "✶ thinking 4k" redraws after
// stripANSI). A line is considered actionable if it's >= 20 chars
// after trim, OR contains a menu marker (`❯`/`›`/`?`/`(y/n)`/`\d.`),
// which preserves real prompts like "❯  1.  Yes" and "Do you want
// to proceed?" while dropping spinner noise.
func trimTrailingStatusJunk(stripped string) int {
	end := len(stripped)
	for end > 0 {
		lineStart := end
		for lineStart > 0 && stripped[lineStart-1] != '\n' {
			lineStart--
		}
		line := strings.TrimSpace(stripped[lineStart:end])
		if !isStatusJunkLine(line) {
			return end
		}
		if lineStart == 0 {
			return 0
		}
		end = lineStart - 1
	}
	return 0
}

// isStatusJunkLine reports whether a trimmed stripped-ANSI line is
// likely bottom-status spinner noise rather than meaningful TUI
// content. Conservative: a line is preserved as actionable if it
// contains a menu marker, digit-period option, or has alphanumeric
// content with enough length to be plausible status text. Lines with
// no alphanumeric content at all (pure decoration: bullets, box
// borders, cursor-position residue) are junk regardless of length.
func isStatusJunkLine(t string) bool {
	if t == "" {
		return true
	}
	// Menu markers always preserve the line.
	if strings.ContainsAny(t, "?❯›") {
		return false
	}
	if strings.Contains(t, "y/n") || strings.Contains(t, "Y/N") {
		return false
	}
	// Numbered-menu-option backstop: "1.", "2." anywhere on the line.
	for i := 0; i < len(t)-1; i++ {
		if t[i] >= '0' && t[i] <= '9' && t[i+1] == '.' {
			return false
		}
	}
	// A line with NO alphanumeric content is decoration regardless of
	// length: codex's "    •         •         •" cursor-position
	// residue can stretch the full column width (>80 chars) but carries
	// no information. The previous length-only gate kept these,
	// which buried the menu in the 2026-05-02 limits/caps regression.
	hasAlnum := false
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			hasAlnum = true
			break
		}
	}
	if !hasAlnum {
		return true
	}
	// Has alphanumerics and no markers — preserve only if long enough
	// to plausibly be a real status/prompt line. Short lines like
	// "✶ thinking 4k" stay classified as junk, matching prior behavior.
	return len(t) < 20
}
