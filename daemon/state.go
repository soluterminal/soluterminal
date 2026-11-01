package main

import (
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// PaneRuntimeState is a finer-grained status than PaneStatus — it captures
// what the pane is currently "doing" based on recent output.
type PaneRuntimeState string

const (
	StateBusy          PaneRuntimeState = "busy"           // recent PTY output
	StateAwaitingInput PaneRuntimeState = "awaiting-input" // Claude's prompt box visible
	StateChoicePrompt  PaneRuntimeState = "choice-prompt"  // Claude asking for a numbered choice
	StateRecent        PaneRuntimeState = "recent"         // activity in last 3s but not currently busy
	StateIdle          PaneRuntimeState = "idle"           // no recent activity, no known prompt
	StateExited        PaneRuntimeState = "exited"
)

// Heuristics for Claude Code's interactive TUI. Kept deliberately narrow:
// we'd rather miss some cases than fire false notifications.
//
// Claude Code v4.7+ prompt (current):
//
//	❯
//	─────────────────
//	~/path [Opus 4.7 (1M context) | ctx: 3% | ...]
//
// Claude Code ≤v4.6 prompt (legacy, still handled):
//
//	╭──────╮
//	│ >    │
//	╰──────╯
//
// Choice prompt (both versions):
//
//	❯ 1. Yes
//	  2. No
var (
	// New-style: a line starting with `❯`. We accept a few right-angle
	// variants in case Claude Code picks a different glyph (or renders
	// the bold variant differently under ANSI attributes): ❯ U+276F,
	// ❭ U+276D, › U+203A, ▶ U+25B6. `\p{Zs}` adds Unicode space
	// separators (NBSP etc.) to the leading-whitespace class — Go's
	// plain `\s` is ASCII-only, which misses some TUI alignment chars.
	newPromptLineRE = regexp.MustCompile(`(?m)^[\s\p{Zs}]*[❯❭›▶]`)
	// New-style footer — one of Claude's model names in brackets.
	// Survives window-narrow truncation (the full "| ctx: N%" is often
	// cut off). Only matches when a model name appears RIGHT AFTER a `[`
	// so it won't fire on random prose that mentions "Opus".
	newPromptFooterRE = regexp.MustCompile(`\[(Opus|Sonnet|Haiku)\s+\d`)

	// Legacy box-drawing prompt.
	inputBoxLineRE = regexp.MustCompile(`│\s{1,3}>\s`)
	boxBottomRE    = regexp.MustCompile(`╰[─]+╯`)

	// `❯ 1.` on a line — matches Claude's numbered menus ("Do you want to
	// proceed? ❯ 1. Yes / 2. No …"). The whitespace between arrow and
	// "1." is `*` (zero-or-more), not `+`, because Claude's TUI sometimes
	// uses ANSI cursor-move sequences to advance columns instead of
	// emitting real space bytes. When we strip ANSI the space position
	// disappears, so we'd see "❯1." with no space.
	//
	// The digit-then-(period OR 2+ whitespace) trailer covers a second
	// rendering glitch seen 2026-04-28: Claude Code can re-paint the
	// focus line losing the period entirely ("❯  1  Yes" instead of
	// "❯  1.  Yes" — confirmed in raw bytes), while siblings 2./3. still
	// have periods. The 2+ whitespace branch reads as "menu padding
	// after the option digit", which prose almost never produces — bare
	// "❯ 1 thing" with single space stays unmatched.
	choicePromptRE = regexp.MustCompile(`(?m)^[\s\p{Zs}]*[❯❭›▶][\s\p{Zs}]*1(?:\.|[\s\p{Zs}]{2,})`)

	// Claude Code's idle-composer footer can put a user line beginning with
	// "❯ 1." above "new task? /clear", which choicePromptRE otherwise admits
	// as a menu. Mirror web/src/lib/actionability.ts's preview filter by requiring
	// both "new task?" and "/clear" on one line. NBSP-tolerant whitespace
	// matches the prompt regexes above. In the 2026-08-25 05:09-05:13 incident
	// this chrome on pane_eab1cef2ed51f934 repeatedly fired direct auto-mode
	// (warden run ~/.solu/warden-runs/run-1787605917630.log).
	claudeIdleHintRE = regexp.MustCompile(`(?i)new[\s\p{Zs}]*task[\s\p{Zs}]*\?[^\n]{0,160}/clear`)
	// A real Claude/Codex menu renders at least one sibling numbered option
	// below its focused row. Accept siblings 2-9 so an ANSI-collapse glitch on
	// option 2 cannot make a real approval menu look like idle composer chrome.
	menuSiblingOptionRE = regexp.MustCompile(`(?m)^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?[2-9](?:\.|\)|[\s\p{Zs}]{2,})`)

	// Codex/Ratatui sometimes renders an approval modal without the
	// focused "› 1. Yes" option row in the reconstructed grid. The
	// header + command/edit body is still an actionable approval prompt.
	// This is only consulted for panes with a Codex descendant.
	// Preserve raw offsets while tolerating word-wrapped titles and footers.
	codexApprovalPromptRE = regexp.MustCompile(`(?ms)Would\s+you\s+like\s+to\s+(?:run\s+the\s+following\s+command|make\s+the\s+following\s+edits)\?.*(?:\n[\s\p{Zs}]*\$[\s\p{Zs}]*\S|Press\s+enter\s+to\s+confirm|Esc\s+to\s+cancel)`)

	// Current Codex MCP permission modal observed in v0.0.985 on
	// pane_8a14cbd9b3ca9a83: "Allow the soluterminal MCP server to run tool
	// \"get_workflow_task\"?". Match the question only after whitespace has
	// been flattened so narrow-terminal wrapping and ANSI-strip padding do not
	// change its shape.
	codexMcpPermissionQuestionRE = regexp.MustCompile(`allow the .{1,60}? mcp server to run .{1,160}?\?`)
	// Stable scalar argument assignments rendered between an MCP permission
	// question and its option rows (for example `task_id: wft_...`). Capturing
	// assignments explicitly keeps animated status/timer prose out of prompt
	// identity while still distinguishing invocations of the same tool.
	mcpArgumentIdentityRE = regexp.MustCompile(`(?:^| )["']?[a-z_][a-z0-9_.-]{0,63}["']?\s*:\s*(?:"[^"]*"|'[^']*'|[^\s,}\]]+)`)
	// The same modal focuses its numbered option-1 "Allow" row. Keep this
	// line-anchored so prose in the permission body cannot satisfy it.
	codexMcpAllowOptionRE = regexp.MustCompile(`(?mi)^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?1(?:\.|\)|[\s\p{Zs}]{2,})[\s\p{Zs}]*allow\b`)

	// A numbered menu option whose label is "Yes, <qualifier>" — the
	// escalation option every Claude Code tool-permission prompt offers
	// beside its bare accept-once "Yes". Enumerated from the installed
	// claude binary: "Yes, allow all edits during this session", "Yes, and
	// allow Claude to edit its own settings for this session", "Yes, and
	// bypass permissions", "Yes, and don't ask again for …", "Yes, and
	// remember this directory", "Yes, and use auto mode", "Yes, for this
	// session", "Yes, run it". Selection/direction menus never phrase an
	// option that way, so this is a structural signal, not phrasing chasing.
	approvalOptionRE = regexp.MustCompile(`(?mi)^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?[0-9]+(?:\.|[\s\p{Zs}]{2,})[\s\p{Zs}]*yes,[\s\p{Zs}]`)

	// ANSI CSI sequences (ESC [ … <letter>). Covers colors, cursor moves,
	// clear-screen, etc.
	ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// OSC sequences (ESC ] … BEL or ESC ] … ESC \). Used by Claude to set
	// the terminal title ("✳ Send delayed hi message"). Previously left in
	// and surfaced as "]0;..." residue in the tail.
	ansiOSC = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
)

// computeState returns the current runtime state of the pane based on its
// last-activity timestamp plus a scan of recent PTY output for Claude-style
// prompts. Briefly takes p.mu to read Status; the buffer scan runs lock-free.
// approvalScanChars is the crop width for the approval/choice-prompt
// scan in computeStateWithHasCodex. Deliberately much wider than the
// 1200-char awaiting-input window: a multi-subagent run can stream many
// non-spinner progress lines AFTER a "Do you want to proceed?" menu is
// already up (background terminals keep emitting while the main agent
// blocks on the menu), and those lines survive trimTrailingStatusJunk,
// so a narrow window would bury the menu and auto-approve would miss it.
// Bounded so the regex stays cheap on this hot path.
const approvalScanChars = 16000

func (p *Pane) computeState() PaneRuntimeState {
	return p.computeStateWithHasCodex(p.HasCodexDescendant() || p.HasMuseDescendant())
}

// claudeMenuOffScreen reports that a raw-tail approval match on a Claude
// pane is not on the rendered screen. Claude repaints the rows an answered
// dialog occupied, and stripped of cursor motion those repaints match the
// menu again at a newer raw position, so the pane read as choice-prompt for
// as long as a command ran (2026-10-25: 9 of ~104 auto-approvals on live
// panes were Enters into such a screen, each 5-35s after the real answer;
// every genuine approval sampled had the menu on the rendered grid). Only a
// live grid can veto, and only for Claude: other TUIs keep the raw scan.
func (p *Pane) claudeMenuOffScreen(hasCodex bool) bool {
	if hasCodex {
		return false
	}
	p.mu.Lock()
	claude := p.Agent == "claude" || p.ClaudeSessionID != ""
	p.mu.Unlock()
	if !claude {
		return false
	}
	text, ok := p.gridVisibleText()
	if !ok || isGridChoicePromptText(text, false) {
		return false
	}
	if screen, ok := p.screenVisibleText(); ok && isGridChoicePromptText(screen, false) {
		return false
	}
	return true
}

func (p *Pane) computeStateWithHasCodex(hasCodex bool) PaneRuntimeState {
	p.mu.Lock()
	status := p.Status
	p.mu.Unlock()
	if status == PaneExited {
		return StateExited
	}

	// Native-renderer short-circuit (RECON C). A parked permission request is
	// the authoritative "this pane is asking for approval" signal for native
	// panes — read lock-free (one atomic int load) BEFORE the 64KB tail
	// scan. For terminal panes (Kind==""), the parser never runs and this
	// counter is always 0, so this is a single cache-hot int read and
	// then the identical existing code path — the byte-identical fallback
	// guarantee holds. Placed AFTER the PaneExited check so a dead native pane
	// still reports StateExited.
	if p.hasNativePending() {
		return StateChoicePrompt
	}

	now := time.Now().UnixMilli()
	last := atomic.LoadInt64(&p.LastActivityMs)
	age := now - last

	// Scan the last ~8KB of output for Claude-specific prompts. We do
	// this BEFORE the age-based busy short-circuit so a brand-new
	// choice-prompt is reported even when the TUI is sub-second
	// re-rendering (cursor blink, footer clock, etc.) — the
	// position-NEW guard (matchAbsPos > ackedPos) prevents stale-buffer
	// false positives, so this can't fire for a menu we've already
	// announced. Bug history: with the busy short-circuit first, panes
	// with constantly-redrawing TUI chrome never settled to
	// choice-prompt, so the watcher never saw a transition and
	// auto-mode never fired.
	// Take a generous raw window: codex's approval menu followed by a
	// long spinner-redraw run can put the menu 17 KB+ behind the
	// stripped-buffer end (observed 2026-05-02 on pane_4aa85895…).
	// stripANSI plus trimTrailingStatusJunk below recover the menu, but
	// only if the raw window is wide enough to contain the menu bytes
	// in the first place. 64 KB clears the worst-case spinner pile-up
	// we've seen with comfortable headroom; an 8 KB window did not.
	//
	// SnapshotTail (not Snapshot) is deliberate: computeState runs on
	// 1-2s tickers across every pane, and copying the full 8 MB ring
	// each time drove the daemon into a GC death-spiral. We only ever
	// look at this 64 KB tail, so only copy that.
	s, hasTail := p.stateTail()
	if hasTail {
		windowLen := len(s)

		// Walk back over trailing spinner/whitespace junk before the
		// last-screenful crop. Without this, a pane sitting on a real
		// menu while codex (or claude) emits cursor-positioned spinner
		// frames pushes the menu out of the 1200-char window — and the
		// regex below never sees it. Same fix tailOfPane uses for the
		// ARIA eval window (see aria_watch.go:204 and the
		// feedback_aria_eval_tail_window memory note).
		trimmedEnd := trimTrailingStatusJunk(s)
		totalStripped := atomic.LoadInt64(&p.TotalStrippedChars)

		// Approval-menu scan uses a WIDE window (approvalScanChars). A real
		// "Do you want to proceed?" menu can sit far behind the trimmed
		// buffer end when a multi-subagent run keeps streaming non-spinner
		// progress lines after it; a narrow window buried it and
		// auto-approve missed the menu. approvalPromptMatches returns the
		// LAST match and the position-NEW guard below suppresses stale
		// re-fires, so a wide scan cannot double-fire.
		approvalStart := 0
		if trimmedEnd > approvalScanChars {
			approvalStart = trimmedEnd - approvalScanChars
		}

		// Position-aware choice-prompt detection. Same semantics as before
		// (see the table below).
		//
		//   matchAbsPos > ackedPos   → genuinely new menu, fire.
		//   matchAbsPos == ackedPos  → same menu we already reported.
		//     inputPos <= ackedPos   → still waiting, keep firing.
		//     inputPos  > ackedPos   → user typed past it, suppress.
		if matches := approvalPromptMatches(s[approvalStart:trimmedEnd], hasCodex); len(matches) > 0 {
			last := matches[len(matches)-1]
			// match offset is relative to the approval window, whose first
			// char sits at absolute stripped offset
			//   totalStripped - windowLen + approvalStart
			// (windowLen = len(stripANSI(last 64 KB raw))).
			scanAbsStart := totalStripped - int64(windowLen) + int64(approvalStart)
			matchAbsPos := scanAbsStart + int64(last[0])
			ackedPos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
			inputPos := atomic.LoadInt64(&p.LastInputStrippedPos)
			if matchAbsPos > ackedPos {
				atomic.StoreInt64(&p.LastAckedChoicePromptPos, matchAbsPos)
				if !p.claudeMenuOffScreen(hasCodex) {
					return StateChoicePrompt
				}
			} else if inputPos <= ackedPos && !p.claudeMenuOffScreen(hasCodex) {
				return StateChoicePrompt
			}
			// Same menu, user has answered, or a Claude menu that is no longer
			// on screen. Fall through.
		}

		// TUIs can paint prompts as cell diffs in the normal buffer.
		// The raw byte tail can degrade into cursor-motion residue
		// ("•" and huge spaces) even while the visible screen clearly
		// shows a choice prompt. When the grid emulator is already live
		// (grid-mode subscriber, grid mode, or auto mode), classify from
		// its reconstructed cells. Do not initialize it from the pane
		// poll path: seeding the emulator replays scrollback and has a
		// deliberate settle delay, so doing that inside /api/panes makes
		// ordinary polling scale badly with pane count.
		if gridText, gridGeneration, ok := p.gridVisibleTextWithGeneration(); ok {
			if isGridChoicePromptText(gridText, hasCodex) {
				// Moving a trust-menu selector is input, but still needs the
				// human until the menu disappears from the current screen.
				if len(folderTrustPromptMatches(gridText)) > 0 {
					return StateChoicePrompt
				}
				fp := approvalPromptFingerprint(gridText, hasCodex)
				ackedPos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
				inputPos := atomic.LoadInt64(&p.LastInputStrippedPos)
				ackedGridGeneration := atomic.LoadUint64(&p.LastAckedGridChoicePromptGeneration)
				turnover := fp != 0 && inputPos > ackedPos && gridGeneration > ackedGridGeneration
				if fp != 0 && (fp != atomic.LoadUint64(&p.LastGridChoicePromptHash) || turnover) {
					// Genuinely NEW grid menu: its fingerprint changed, or the live
					// grid-feed generation proves the previously answered same-looking
					// instance disappeared and returned in a later committed ratatui
					// frame. Ack it just past current output and fire. Observing the
					// edge in readLoop (not this poller) fixes both the original
					// 2026-08-10 pane_8a14cbd9b3ca9a83 back-to-back MCP modal
					// incident and identical-tool repeats while keeping
					// LastAckedChoicePromptPos as the single
					// instance key used by watcher, sweeper, and attention consumers.
					atomic.StoreUint64(&p.LastGridChoicePromptHash, fp)
					nextPos := totalStripped
					if nextPos < ackedPos {
						nextPos = ackedPos
					}
					atomic.StoreInt64(&p.LastAckedChoicePromptPos, nextPos+1)
					atomic.StoreUint64(&p.LastAckedGridChoicePromptGeneration, gridGeneration)
					return StateChoicePrompt
				}
				// Same grid menu (fingerprint UNCHANGED). Mirror the raw-buffer
				// branch's "has the user answered" guard: if the most recent
				// client → PTY write landed past the position we acked for this
				// menu, the user has already answered, so fall through instead of
				// re-firing choice-prompt every poll. A still-unanswered menu
				// (inputPos <= ackedPos) keeps firing. fp==0 means no stable menu
				// identity was extractable; keep the old always-fire behaviour
				// there since we can't reason about answer position.
				if fp != 0 && inputPos > ackedPos {
					// Same menu, user has answered. Fall through.
				} else {
					// Re-confirm the generation while this instance is still
					// unanswered. A committed absent→present redraw between initial
					// admission and the eventual Enter belongs to this same admission;
					// leaving the older generation acked would turn that already-seen
					// edge into a second prompt as soon as Pane.Write advances inputPos.
					if fp != 0 {
						atomic.StoreUint64(&p.LastAckedGridChoicePromptGeneration, gridGeneration)
					}
					return StateChoicePrompt
				}
			}
		}

		// Replay-screen fallback for TUIs that paint menus as cell diffs
		// (Muse): the raw byte tail then carries cursor-motion residue the
		// regexes cannot match, while the always-on replay grid shows the
		// menu plainly (observed live: a Muse network-access menu absent
		// from the tail yet present on screen, pane stuck busy forever).
		// Gated on hasCodex — the Muse/Codex cell-diff TUI class — so
		// Claude's text-streamed menus keep their existing tail-only
		// path. screenVisibleText only snapshots the already-running
		// replay grid (no emulator init, no ring copy), so this stays
		// cheap on the 1-2s pollers. Mirrors the emulator guards above
		// with a separate hash: a new fingerprint fires and acks just
		// past current output; an answered same-fingerprint menu falls
		// through instead of re-firing every poll.
		if hasCodex || strings.Contains(s, "trust this folder") {
			if screenText, ok := p.screenVisibleText(); ok {
				if len(folderTrustPromptMatches(screenText)) > 0 {
					return StateChoicePrompt
				}
				sfp := approvalPromptFingerprint(screenText, hasCodex)
				screenAcked := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
				screenInput := atomic.LoadInt64(&p.LastInputStrippedPos)
				if sfp != 0 && sfp != atomic.LoadUint64(&p.LastScreenChoicePromptHash) {
					atomic.StoreUint64(&p.LastScreenChoicePromptHash, sfp)
					nextPos := totalStripped
					if nextPos < screenAcked {
						nextPos = screenAcked
					}
					atomic.StoreInt64(&p.LastAckedChoicePromptPos, nextPos+1)
					return StateChoicePrompt
				}
				if sfp != 0 && screenInput > screenAcked {
					// Same screen menu, user has answered. Fall through.
				} else if sfp != 0 {
					return StateChoicePrompt
				}
			}
		}

		// awaiting-input is less specific — still gate it behind the
		// busy threshold so streaming output isn't misclassified as a
		// settled idle prompt while Claude is mid-reply.
		if age >= 1500 {
			// Keep awaiting-input on the narrow last-1200-char window (the
			// approval scan above already used the wide window): prose in
			// scrollback must not false-positive here as a settled prompt.
			awaitStart := 0
			if trimmedEnd > 1200 {
				awaitStart = trimmedEnd - 1200
			}
			if pos, ok := looksLikeClaudePromptPos(s[awaitStart:trimmedEnd]); ok {
				scanAbsStart := totalStripped - int64(windowLen) + int64(awaitStart)
				matchAbsPos := scanAbsStart + int64(pos)
				ackedPos := atomic.LoadInt64(&p.LastAckedAwaitingInputPos)
				inputPos := atomic.LoadInt64(&p.LastInputStrippedPos)
				// Fire when this is the first prompt we've seen for this pane
				// (ackedPos == 0), or when the user has typed since we last
				// acknowledged (inputPos > ackedPos — i.e. a NEW prompt has
				// appeared after a user turn). Plain redraws of the same prompt
				// (footer clock, cursor blink) advance matchAbsPos but not
				// inputPos, so they fall through silently — root of the
				// notification-repeat bug fixed 2026-05-07.
				if ackedPos == 0 || inputPos > ackedPos {
					// Ack to max(prompt, input) so ONE classification always
					// closes the gate. Input with no echo after it (nothing
					// repainted past those bytes yet) leaves inputPos beyond
					// matchAbsPos; acking only matchAbsPos then kept
					// inputPos > ackedPos true on every poll — a permanently
					// open gate that re-reported this same settled prompt
					// forever (the infinite "Claude is waiting for you"
					// repeat). A NEW user turn must advance inputPos again
					// to re-open it.
					ack := matchAbsPos
					if inputPos > ack {
						ack = inputPos
					}
					atomic.StoreInt64(&p.LastAckedAwaitingInputPos, ack)
					return StateAwaitingInput
				}
				// Same prompt, user hasn't typed — fall through to age-based busy/idle.
			}
		}
	}

	// Keystroke echo isn't work: when the newest output arrived within a
	// beat of the newest user input, it's the TUI redrawing its composer
	// (echo), not the agent doing something — report idle instead of
	// busy/recent so remote clients don't raise a phantom Stop button while
	// the user types in a live terminal. A real turn self-corrects fast:
	// streaming continues past the window, so lastOut-lastIn grows beyond it
	// within ~500ms of the submit. (Trade-off: typing a steer WHILE the agent
	// streams can briefly read idle — brief, self-healing, and far less
	// harmful than idle reading busy.)
	if p.lastOutputWasInputEcho() {
		return StateIdle
	}

	// Fresh activity → busy. Moved AFTER the choice-prompt scan so
	// a sub-second-redrawing TUI can still surface a real prompt.
	if age < 1500 {
		return StateBusy
	}

	if age < 3000 {
		return StateRecent
	}
	return StateIdle
}

// echoWindowMs bounds how long after a user keystroke the resulting PTY output
// still counts as its echo. Composer redraws land within a few ms; a real turn
// keeps producing output long past this, so detection is delayed by at most one
// window after the submit keystroke.
const echoWindowMs = 500

// lastOutputWasInputEcho reports whether the newest PTY output is best explained
// as the echo of the newest user input (see computeState). Strictly ordered
// (output at-or-after input) so a keystroke landing during real streaming output
// doesn't retroactively reclassify that output as echo.
func (p *Pane) lastOutputWasInputEcho() bool {
	lastIn := atomic.LoadInt64(&p.LastInputMs)
	if lastIn == 0 {
		return false
	}
	lastOut := atomic.LoadInt64(&p.LastActivityMs)
	return lastOut >= lastIn && lastOut-lastIn <= echoWindowMs
}

func (p *Pane) gridVisibleText() (string, bool) {
	text, _, ok := p.gridVisibleTextWithGeneration()
	return text, ok
}

// gridVisibleTextWithGeneration reads the rendered text and its live
// prompt-frame generation under the same subsMu read lock. readLoop holds the
// write lock while feeding a committed frame and advancing the generation, so
// computeState cannot combine text from one prompt instance with lifecycle
// identity from another.
func (p *Pane) gridVisibleTextWithGeneration() (string, uint64, bool) {
	p.subsMu.RLock()
	em := p.gridEmu
	if em == nil {
		p.subsMu.RUnlock()
		return "", 0, false
	}
	text, ok := em.VisibleText()
	generation := atomic.LoadUint64(&p.GridChoicePromptGeneration)
	p.subsMu.RUnlock()
	return text, generation, ok
}

// screenVisibleText renders the ALWAYS-ON replay grid (the actual current
// screen of a terminal pane) as plain text lines. See Pane.ScreenSnapshot.
func (p *Pane) screenVisibleText() (string, bool) {
	p.subsMu.RLock()
	em := p.replayEmu
	p.subsMu.RUnlock()
	if em == nil {
		return "", false
	}
	return em.VisibleText()
}

func choicePromptFingerprint(s string) uint64 {
	matches := choicePromptMatches(s)
	return fingerprintPromptMatches(s, matches)
}

func approvalPromptFingerprint(s string, hasCodex bool) uint64 {
	return fingerprintPromptMatches(s, approvalPromptMatches(s, hasCodex))
}

func fingerprintPromptMatches(s string, matches [][]int) uint64 {
	if len(matches) == 0 {
		return 0
	}
	start := matches[len(matches)-1][0]
	// Drop trailing status-junk (Codex "Working…" spinner, bullet-cursor
	// residue, blank chrome) before hashing. Those lines rewrite every
	// frame, so including them makes the fingerprint flap even when the
	// real menu identity is unchanged — which trips the just-before-Enter
	// recheck in aria_auto.go and stalls auto-approve after 3 retries.
	end := trimTrailingStatusJunk(s)
	if end < start {
		end = start
	}
	// Positive filter: the shared identity consists of stable menu rows plus,
	// for Codex permission prompts, the command/reason or MCP question/argument
	// token assembled below. Do not hash animated chrome. trimTrailingStatusJunk
	// already drops the trailing spinner footer, but Codex/ratatui can
	// interleave timer-bearing status lines ("Waiting for background terminal · (1m 45s
	// · esc to interrupt)") between the menu options and the bottom of
	// the trimmed slice — those have alnum + len>=20 so isStatusJunkLine
	// preserves them, and their digit ticks animated the hash every
	// second. Symptom on 2026-05-07 (pane_356b3aa48e4123dc): every
	// pre-Enter recheck rejected with "pane prompt changed before
	// Enter" until ARIA backed off after 3 retries. Same dynamic also
	// caused the grid-path branch in computeState to bump
	// LastAckedChoicePromptPos every animation frame, which broke
	// position equality on the recheck. Both observed symptoms collapse
	// to this hash being unstable.
	var kept []string
	for _, raw := range strings.Split(s[start:end], "\n") {
		line := strings.TrimSpace(raw)
		if !isMenuIdentityLine(line) {
			continue
		}
		kept = append(kept, normalizeMenuLine(line))
		if len(kept) >= 8 {
			break
		}
	}
	if len(kept) == 0 {
		return 0
	}
	// choicePromptMatches anchors an MCP permission modal at its focused
	// "› 1. Allow" row, so the question and arguments before that row were
	// historically absent from the fingerprint. Inspect only the bounded region
	// belonging to THIS menu: at most 1000 bytes before its anchor and never
	// across the preceding prompt anchor. That prevents an older MCP question in
	// a long adaptive tail from contaminating a later non-MCP command prompt that
	// a 24-row grid sees without the old question. Flatten wrapping/padding
	// exactly like the safety classifier, and take the LAST MCP question in that
	// region. Hash the exact question plus explicitly identity-bearing scalar
	// argument assignments; never hash the raw span, which can contain animated
	// timer/status lines.
	windowStart := start - 1000
	if windowStart < 0 {
		windowStart = 0
	}
	if len(matches) > 1 {
		previousPromptEnd := matches[len(matches)-2][1]
		if previousPromptEnd > windowStart && previousPromptEnd <= start {
			windowStart = previousPromptEnd
		}
	}
	// Numbered Codex menus anchor at the focused Yes row, after the command.
	// At narrow widths even option 2's command wraps off its numbered row.
	// Bind the action above this menu too: otherwise consecutive commands
	// with the same Yes/No labels look like the already answered request.
	if action := codexCommandApprovalIdentity(s[windowStart:start]); action != "" {
		kept = append([]string{action}, kept...)
	}
	// Claude's tool body can be longer than the small Codex/MCP identity
	// window. Stay within the current modal and the bounded approval scan;
	// never inherit a tool body from a previous focused menu.
	claudeStart := start - approvalScanChars
	if claudeStart < 0 {
		claudeStart = 0
	}
	if len(matches) > 1 && matches[len(matches)-2][1] > claudeStart {
		claudeStart = matches[len(matches)-2][1]
	}
	if action := claudePermissionIdentity(s[claudeStart:start]); action != "" {
		kept = append([]string{action}, kept...)
	}
	// The MCP token is meaningful only when the current anchor is the modal's
	// focused one-time Allow action. In particular, a Codex command approval
	// begins at its own "Would you like..." header and must never inherit an MCP
	// question merely because that question remains nearby in scrollback.
	if codexMcpAllowOptionRE.MatchString(s[start:end]) {
		w := s[windowStart:start]
		flat := strings.ToLower(strings.Join(strings.Fields(w), " "))
		if questions := codexMcpPermissionQuestionRE.FindAllStringIndex(flat, -1); len(questions) > 0 {
			lastQuestion := questions[len(questions)-1]
			tokenParts := []string{flat[lastQuestion[0]:lastQuestion[1]]}
			for _, arg := range mcpArgumentIdentityRE.FindAllString(flat[lastQuestion[1]:], 4) {
				tokenParts = append(tokenParts, strings.TrimSpace(arg))
			}
			tokenRunes := []rune(strings.Join(tokenParts, " "))
			if len(tokenRunes) > 240 {
				tokenRunes = tokenRunes[:240]
			}
			kept = append([]string{string(tokenRunes)}, kept...)
		}
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(kept, "\n")))
	return h.Sum64()
}

// Bind the command and its explicitly labelled reason inside the nearest
// Codex modal. Queued calls can run the same command in different worktrees
// without an absent frame between menus; their reasons distinguish the calls.
// Normalize physical continuation rows without hashing animated chrome.
func codexCommandApprovalIdentity(s string) string {
	const header = "Would you like to run the following command?"
	flat := strings.Join(strings.Fields(s), " ")
	if !strings.Contains(flat, header) {
		return ""
	}
	// Require the command to follow the modal header, never a command from
	// earlier scrollback. Flatten the header alone to accept wrapped titles.
	lines := strings.Split(s, "\n")
	headerEnd := -1
	for i := range lines {
		for j := i; j < len(lines) && j <= i+2; j++ {
			if normalizeMenuLine(strings.Join(lines[i:j+1], " ")) == header {
				headerEnd = j
			}
		}
	}
	if headerEnd < 0 {
		return ""
	}
	var command, reason []string
	readingReason := false
	for _, line := range lines[headerEnd+1:] {
		trimmed := strings.TrimSpace(line)
		if len(command) == 0 {
			if strings.HasPrefix(trimmed, "$ ") || strings.HasPrefix(trimmed, "$\t") {
				command = append(command, trimmed)
			} else if strings.HasPrefix(trimmed, "Reason:") {
				reason = []string{strings.TrimSpace(strings.TrimPrefix(trimmed, "Reason:"))}
				readingReason = true
			} else if trimmed == "" {
				readingReason = false
			} else if readingReason {
				reason = append(reason, trimmed)
			}
			continue
		}
		if trimmed == "" || menuOptionRowRE.MatchString(line) {
			break
		}
		command = append(command, trimmed)
	}
	if len(command) == 0 {
		return ""
	}
	identity := "command:" + normalizeMenuLine(strings.Join(command, " "))
	if normalizedReason := normalizeMenuLine(strings.Join(reason, " ")); normalizedReason != "" {
		identity += "\nreason:" + normalizedReason
	}
	return identity
}

// isMenuIdentityLine reports whether a trimmed stripped-ANSI line carries
// the stable line component of menu identity rather than animated chrome.
// Matches: numbered option rows ("1.", "2)", "❯ 1. Yes"), the Codex
// approval-modal question header ("Would you like to..."), and shell-command
// body lines ("$ git push ..."). Codex command/reason and MCP question/argument
// identity is added separately by fingerprintPromptMatches because it precedes the
// focused option-row anchor. Lines outside these patterns — separators,
// status timers, "Working…" footers, freeform reason text — are excluded so
// the fingerprint stays invariant across redraw frames.
func isMenuIdentityLine(t string) bool {
	if t == "" {
		return false
	}
	// Codex approval-modal anchors.
	if strings.HasPrefix(t, "Would you like to") {
		return true
	}
	if strings.HasPrefix(t, "$ ") || strings.HasPrefix(t, "$\t") {
		return true
	}
	// Numbered option line, optionally prefixed with a focus marker.
	rest := strings.TrimLeft(t, " \t❯❭›▶")
	rest = strings.TrimSpace(rest)
	if len(rest) == 0 {
		return false
	}
	end := len(rest)
	if end > 4 {
		end = 4
	}
	for i := 0; i < end; i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			break
		}
		if i+1 >= len(rest) {
			break
		}
		n := rest[i+1]
		if n == '.' || n == ')' || n == ' ' {
			return true
		}
	}
	return false
}

// normalizeMenuLine collapses internal whitespace runs so re-renders
// that pad/un-pad option text (column-width shifts on resize, ratatui
// re-flow) produce the same hash for the same logical line.
func normalizeMenuLine(t string) string {
	return strings.Join(strings.Fields(t), " ")
}

func isChoicePromptText(s string) bool {
	return len(choicePromptMatches(s)) > 0
}

func isCodexApprovalPromptText(s string) bool {
	return codexApprovalPromptRE.MatchString(s)
}

// isCodexMcpPermissionPromptText recognizes the current Codex MCP permission
// modal: "Allow the soluterminal MCP server to run tool
// \"get_workflow_task\"?" with Allow, session/persistent grant, and cancel
// choices. Tools without persistent permission offer only Allow and Cancel.
// A clipped four-option modal may instead prove its escape path through
// Codex's option-count/submission footer below.
func isCodexMcpPermissionPromptText(block string) bool {
	flat := strings.ToLower(strings.Join(strings.Fields(block), " "))
	if !codexMcpPermissionQuestionRE.MatchString(flat) ||
		!codexMcpAllowOptionRE.MatchString(block) {
		return false
	}
	if choices := choicePromptMatches(block); len(choices) > 0 {
		menu := parseAutoApprovalMenu(block, choices[len(choices)-1][0])
		if len(menu.options) == 2 {
			return isCodexMcpOncePermissionPromptText(block)
		}
	}
	if !strings.Contains(flat, "allow for this session") &&
		!strings.Contains(flat, "always allow") {
		return false
	}
	hasCancel, _ := regexp.MatchString(`\bcancel\b`, flat)
	return hasCancel || isCodexMcpClippedPermissionPromptText(block)
}

// Codex can omit both persistent-grant rows. Require the current MCP question,
// an explicit once-only first row and the second Cancel row; a generic Allow
// label or an escape hint alone must not turn a question into a permission.
func isCodexMcpOncePermissionPromptText(block string) bool {
	choices := choicePromptMatches(block)
	if len(choices) == 0 {
		return false
	}
	focus := choices[len(choices)-1][0]
	start := 0
	if len(choices) > 1 {
		start = choices[len(choices)-2][1]
	}
	menu := parseAutoApprovalMenu(block[start:], focus-start)
	return codexMcpPermissionQuestionRE.MatchString(menu.header) &&
		autoApprovalDenyReason(menu.header, menu.menu) == "" &&
		len(menu.options) == 2 && strings.HasPrefix(menu.options[0], "allow") &&
		ariaJevOnceLabel(menu.options[0]) &&
		(menu.options[1] == "cancel" || menu.options[1] == "cancel cancel this tool call")
}

// Codex's two-column permission modal can hide option 4 (Cancel) below the
// viewport and clip "esc to cancel" at the right edge. Accept only its exact
// four-option footer together with the current MCP question and three visible
// grant rows. An arbitrary "esc" hint or an incomplete redraw is insufficient.
func isCodexMcpClippedPermissionPromptText(block string) bool {
	choices := choicePromptMatches(block)
	if len(choices) == 0 {
		return false
	}
	focus := choices[len(choices)-1][0]
	start := 0
	if len(choices) > 1 {
		start = choices[len(choices)-2][1]
	}
	menu := parseAutoApprovalMenu(block[start:], focus-start)
	if !codexMcpPermissionQuestionRE.MatchString(menu.header) ||
		autoApprovalDenyReason(menu.header, menu.menu) != "" ||
		len(menu.options) != 3 || !ariaJevOnceLabel(menu.options[0]) ||
		!strings.HasPrefix(menu.options[0], "allow") ||
		!strings.HasPrefix(menu.options[1], "allow for this ") ||
		!strings.HasPrefix(menu.options[2], "always allow ") {
		return false
	}
	footer := normalizePromptText(lastNonEmptyLines(block[focus:], 1))
	const prefix = "option 1/4 | enter to submit | "
	if !strings.HasPrefix(footer, prefix) {
		return false
	}
	escape := strings.TrimPrefix(footer, prefix)
	return strings.HasPrefix(escape, "esc") && strings.HasPrefix("esc to cancel", escape)
}

// isAutoApprovalPromptText reports whether s shows a permission prompt that
// auto-approve may answer with a bare Enter (selecting the focused row). The
// classifier lives in auto_approval_classifier.go: an explicit deny pass for
// plan-exit / question / billing / login menus runs first, then the focused
// row must be an accept-once (or session-scoped allow) row and the menu must
// offer a No/Cancel escape. History worth keeping: the "Yes, <qualifier>"
// row (approvalOptionRE) is the permission-gate signal — it generalized over
// WebFetch, settings-write ("Yes, and allow Claude to edit its own settings")
// and similar prompts no question-phrasing allowlist caught — but on its own
// it also admitted plan-exit ("Yes, auto-accept edits"), Codex "Yes,
// implement this plan", and billing ("Yes, use my reset") menus, which is why
// the focused-row and deny checks now carry the safety burden.
func isAutoApprovalPromptText(s string, hasCodex bool) bool {
	return autoApprovalDeclineReason(s, hasCodex) == ""
}

func activeChoicePromptBlock(s string) string {
	matches := choicePromptMatches(s)
	if len(matches) == 0 {
		return s
	}
	start := matches[len(matches)-1][0]
	if start > 1200 {
		start -= 1200
	} else {
		start = 0
	}
	end := trimTrailingStatusJunk(s)
	if end <= start {
		end = len(s)
	}
	if end < start {
		end = start
	}
	return s[start:end]
}

func approvalPromptMatches(s string, hasCodex bool) [][]int {
	matches := choicePromptMatches(s)
	if len(matches) == 0 && hasCodex {
		matches = codexApprovalPromptMatches(s)
	}
	return matches
}

func isGridChoicePromptText(s string, hasCodex bool) bool {
	if isChoicePromptText(s) {
		return true
	}
	return hasCodex && isCodexApprovalPromptText(s)
}

func (p *Pane) isGridChoicePrompt(s string) bool {
	return isGridChoicePromptText(s, p.HasCodexDescendant() || p.HasMuseDescendant())
}

func choicePromptMatches(s string) [][]int {
	matches := dropClaudeIdleFooterMatches(s, choicePromptRE.FindAllStringIndex(s, -1))
	matches = append(matches, folderTrustPromptMatches(s)...)
	sort.Slice(matches, func(i, j int) bool { return matches[i][0] < matches[j][0] })
	return matches
}

// dropClaudeIdleFooterMatches vetoes only a lone focused row followed by
// Claude's idle-composer hint. A real menu has a sibling option between the
// focused row and any footer, so it remains actionable even when the same hint
// is visible below it. Vetoed panes fall through computeState's choice-prompt
// branch to the awaiting-input/idle arms, the correct settled-prompt state.
func dropClaudeIdleFooterMatches(s string, matches [][]int) [][]int {
	var kept [][]int
	for _, match := range matches {
		region := s[match[0]:]
		hint := claudeIdleHintRE.FindStringIndex(region)
		if hint != nil && !menuSiblingOptionRE.MatchString(region[:hint[0]]) {
			continue
		}
		kept = append(kept, match)
	}
	return kept
}

func codexApprovalPromptMatches(s string) [][]int {
	return codexApprovalPromptRE.FindAllStringIndex(s, -1)
}

// looksLikeClaudePromptPos detects Claude Code's "awaiting user input" TUI
// state and returns the byte offset within s of the prompt-line match.
// Tries the v4.7+ pattern first, then falls back to the legacy
// box-drawing pattern for older versions.
func looksLikeClaudePromptPos(s string) (pos int, ok bool) {
	// v4.7+: a ❯ line AND the "| ctx: N%" footer appearing in the tail.
	// Both signals together are very unlikely to coincide outside Claude's
	// idle prompt, so we don't need positional checks.
	if loc := newPromptLineRE.FindStringIndex(s); loc != nil && newPromptFooterRE.MatchString(s) {
		return loc[0], true
	}
	// Legacy box: an inner `│ > ` line followed by a `╰──╯` bottom border.
	if innerIdx := inputBoxLineRE.FindStringIndex(s); innerIdx != nil {
		if boxBottomRE.MatchString(s[innerIdx[1]:]) {
			return innerIdx[0], true
		}
	}
	return 0, false
}

// looksLikeClaudePrompt is a thin wrapper preserved for tests.
func looksLikeClaudePrompt(s string) bool {
	_, ok := looksLikeClaudePromptPos(s)
	return ok
}

// claudeActiveSpinnerRE matches Claude Code's LIVE-turn spinner line — the
// positive "a turn is in flight right now" marker. Observed forms (2.1.204
// captures + older builds):
//
//	"✢ Scampering…" / "✽ Wibbling…"                (thinking phase: bare glyph+gerund)
//	"· Actioning …   (17s · ↓   677 tokens)"       (elapsed · token counter)
//	"✻ Baking… (esc to interrupt)"                  (older builds)
//	"Waiting for terminal · (1m 45s · esc to interrupt)"  (backgrounded tool)
//
// Needed because 2.1.204+ renders the COMPOSER (❯ line + ctx footer) while
// generating (steering support), so "prompt box visible" no longer implies
// "turn handed back" on its own. The bare glyph+gerund arm is line-anchored
// and requires a spinner glyph so prose can't false-positive; the
// token-counter arm requires "tokens)"; "esc to interrupt" is claude-only
// phrasing. stripANSI replaces cursor-move escapes with spaces, so whitespace
// is flexible throughout.
var claudeActiveSpinnerRE = regexp.MustCompile(`(?m)^\s*[✢✻✽✶✳❉·*+]+\s*\w+\s*…|\(\s*(?:\d+m\s*)?\d+s\s*·\s*[↓↑][^)]*tokens\s*\)|esc to interrupt`)

// claudeChromeLineRE matches the composer/footer chrome that follows the
// transcript on every claude 2.1.2xx frame — divider rules, the ❯ composer
// (and the transcript's own "❯ <prompt>" echo lines), the cwd/model footer,
// and the permission-mode strip. Used by claudeContentAfter to decide whether
// anything SUBSTANTIVE (i.e. a turn's output being laid down) follows the last
// completion marker on screen.
var claudeChromeLineRE = regexp.MustCompile(`^\s*(?:─{4,}.*|❯.*|~\s*\[.*|.*\|\s*ctx:\s*\d+%.*|⏵⏵.*|\?\s+for shortcuts.*)$`)

// claudeContentAfter reports whether any substantive (non-chrome, non-blank)
// line appears after byte offset idx in the rendered screen. idx < 0 scans the
// whole screen. This is the STREAMING-phase detector: while claude streams a
// response, 2.1.204 shows NO spinner at all — the only screen evidence of the
// live turn is transcript content accumulating BELOW the previous turn's
// completion marker (paired with recent PTY output by the caller).
func claudeContentAfter(screen string, idx int) bool {
	rest := screen
	if idx >= 0 && idx < len(screen) {
		rest = screen[idx:]
	}
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if claudeChromeLineRE.MatchString(line) {
			continue
		}
		return true
	}
	return false
}

// claudeTurnDoneRE matches the completion marker claude paints in place of the
// spinner when the turn settles: "✻ Brewed for 18s" / "* Forged for 1m 3s"
// (random past-tense verb + elapsed time). Its LAST occurrence coming after
// the LAST active-spinner match is the "this turn already finished" proof that
// ends the spinner override the instant the turn completes — even though the
// closing redraws keep PTY output recent for a few more seconds.
var claudeTurnDoneRE = regexp.MustCompile(`\w+ed for (?:\d+m\s*)?\d+s\b`)

// museTurnDoneRE matches the completion marker Muse Code paints when a turn
// settles: "◆ Worked for 5m 05s" (◆ U+25C6 + past-tense verb + elapsed
// time). It is the Muse analogue of claudeTurnDoneRE and the only positive
// "the provider handed back" signal on a Muse Code screen: the idle composer
// ("›") and the "muse-spark-… · <effort> · <cwd> · <mode>" status footer are
// always visible, even mid-turn, so they cannot settle the restart latch on
// their own. The ◆ anchor keeps ordinary prose ("worked for hours") from
// matching.
var museTurnDoneRE = regexp.MustCompile(`◆\s*\w+\s+for\s+(?:\d+h\s*)?(?:\d+m\s*)?\d+s\b`)

// museTurnDoneSinceAdmission reports whether a Muse Code turn-completion
// marker appears in the recent stripped output at a position AFTER the
// fleet-turn admission watermark — i.e. the currently admitted turn has
// demonstrably finished. Position math mirrors computeState (absolute
// stripped offsets via TotalStrippedChars); the watermark guard keeps a
// stale marker from a previous turn from settling a NEW admission.
// Fail-closed: no marker, or marker predating the admission, reads false.
func (p *Pane) museTurnDoneSinceAdmission() bool {
	watermark := p.fleetTurnOutputWatermark.Load()
	totalStripped := atomic.LoadInt64(&p.TotalStrippedChars)
	if totalStripped <= watermark {
		return false
	}
	tail := p.buffer.SnapshotTail(65536)
	if len(tail) == 0 {
		return false
	}
	s := stripANSI(tail)
	locs := museTurnDoneRE.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return false
	}
	scanAbsStart := totalStripped - int64(len(s))
	last := locs[len(locs)-1]
	return scanAbsStart+int64(last[0]) > watermark
}

// claudeSpinnerRecency gates the spinner override on RECENT PTY output. An
// active spinner ANIMATES (glyph ticks every ~100ms), so a genuinely-live turn
// always has fresh output; a pane that has been silent this long cannot have a
// live spinner, so any spinner text still in the tail is stale scrollback
// (e.g. after an ESC-interrupted turn, which paints no completion marker) and
// must not block the prompt-visible signal — the deferred-switch watcher polls
// it to fire. Matches the busy/recent recency family in computeState.
const claudeSpinnerRecency = 3 * time.Second

// lastMatchEnd returns the end offset of re's LAST match in s, or -1.
func lastMatchEnd(re *regexp.Regexp, s string) int {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return -1
	}
	return locs[len(locs)-1][1]
}

// claudePromptVisible reports whether Claude Code's "awaiting user input" prompt
// box is currently rendered at the bottom of the pane's screen — the
// authoritative "the turn has been handed back to the user" signal for the
// renderer switch. Unlike StateAwaitingInput it is NOT gated by the
// notification-dedup ack position, so it stays true for as long as the prompt is
// shown: a steady-state signal both the confirm gate (via IsAgentWorking) and the
// deferred-switch watcher can poll without missing a one-shot transition.
//
// Why it matters: a terminal Claude pane keeps LastActivityMs fresh for ~3s after
// a turn ends (the final redraws, cursor blink, footer clock), so computeState
// reads StateBusy/StateRecent — the "3s recency tail" — even though Claude has
// finished and is showing its prompt. Treating that tail as "working" made an
// idle pane wrongly offer the "switch when done" confirm and stamp a bogus
// "interrupted" notice; a visible prompt box overrides the recency tail.
//
// Cheap by construction: Claude's prompt box always sits at the very bottom of
// the screen, so an 8 KB raw tail (vs computeState's 64 KB approval-scan window)
// is plenty. Called ONLY on the rare switch paths (a toggle click, a single
// pane's 200 ms deferred watcher, and the busy/recent arm of the per-pane DTO),
// NEVER from the hot per-pane state ticker.
func (p *Pane) claudePromptVisible() bool {
	// Terminal Codex and Claude use different composer/footer chrome. Every
	// switch-state caller funnels through this predicate, so dispatch here to
	// keep the DTO confirm gate, graceful interrupt, and deferred settle in
	// agreement. Applying Claude's prompt regex to Codex made an idle Codex
	// startup redraw look busy, then stamped a bogus interrupted notice.
	p.mu.Lock()
	agent := p.Agent
	p.mu.Unlock()
	if effectiveAgentKind(agent) == agentCodex {
		return p.codexPromptVisible()
	}

	// A LIVE spinner overrides everything: claude 2.1.204+ renders the composer
	// (❯ + ctx footer) WHILE generating, so the prompt box alone stopped
	// implying "turn handed back" — mid-turn switches lost their confirm gate,
	// their graceful interrupt, and their "interrupted — Continue" notice (a
	// real user repro, 2026-07-08).
	//
	// PREFERRED SOURCE: the rendered screen (the always-on replay grid). The
	// raw ring is a DELTA stream — the TUI animates its spinner via cursor-move
	// fragments, so the assembled spinner text may never appear contiguously in
	// recent ring bytes (while the PREVIOUS turn's completion marker gets
	// repainted by frame redraws), which mis-read a live turn as handed back.
	// On the screen, a live 2.1.204 turn has exactly two phases:
	//   THINKING  — a spinner line ("✢ Scampering…") below the last completion
	//               marker;
	//   STREAMING — NO spinner at all: the only evidence is transcript content
	//               accumulating after the last completion marker while PTY
	//               output flows.
	// Settling paints "✻ Cooked for 22s" at the transcript bottom, which ends
	// both rules at once (nothing substantive after it, spinner erased) — so
	// the ~3s of closing redraws can't re-trigger the old idle-confirm bug.
	if screen, ok := p.screenVisibleText(); ok {
		lastDone := lastMatchEnd(claudeTurnDoneRE, screen)
		if spin := lastMatchEnd(claudeActiveSpinnerRE, screen); spin >= 0 && spin > lastDone {
			return false
		}
		recentOutput := time.Since(time.UnixMilli(atomic.LoadInt64(&p.LastActivityMs))) < claudeSpinnerRecency
		if recentOutput && claudeContentAfter(screen, lastDone) {
			return false
		}
		return looksLikeClaudePrompt(screen)
	}

	// FALLBACK (no replay grid — bare test panes, exotic states): the stripped
	// ring tail with stream-level guards. Working =
	//   (a) PTY output is RECENT (an active spinner animates continuously — a
	//       quiet pane cannot have a live spinner, so stale spinner scrollback
	//       after an interrupted turn can't wedge the settle signal), AND
	//   (b) the LAST spinner match comes after the LAST completion marker
	//       ("✻ Brewed for 18s"), so the override ends the instant the turn
	//       finishes even though closing redraws keep output recent for ~3s.
	// Scanned on the RAW stripped tail, not the junk-trimmed slice below —
	// trimTrailingStatusJunk exists precisely to drop spinner chrome, i.e. it
	// would eat the very evidence we need.
	tail := p.buffer.SnapshotTail(8192)
	if len(tail) == 0 {
		return false
	}
	s := stripANSI(tail)
	if time.Since(time.UnixMilli(atomic.LoadInt64(&p.LastActivityMs))) < claudeSpinnerRecency {
		if spin := lastMatchEnd(claudeActiveSpinnerRE, s); spin >= 0 && spin > lastMatchEnd(claudeTurnDoneRE, s) {
			return false
		}
	}
	// Mirror computeState's awaiting-input scan: trim trailing spinner/whitespace
	// junk, then look only at the last 1200 chars so prose higher in scrollback
	// can't false-positive as a settled prompt.
	trimmedEnd := trimTrailingStatusJunk(s)
	awaitStart := 0
	if trimmedEnd > 1200 {
		awaitStart = trimmedEnd - 1200
	}
	return looksLikeClaudePrompt(s[awaitStart:trimmedEnd])
}

// Codex keeps its composer visible both while idle and while a turn is running.
// The stable idle proof is the paired `›` composer + `model · cwd` footer with no
// live `Working (... esc to interrupt)` row. Startup's "Starting MCP servers"
// animation is intentionally not a model turn: switching during it loses no
// response and must not produce an interrupted-conversation notice.
var (
	codexComposerLineRE = regexp.MustCompile(`(?m)^\s*›(?:[\s\x{2800}-\x{28ff}]|$).*$`)
	codexFooterLineRE   = regexp.MustCompile(`(?mi)^\s*(?:Context\s+\d+%\s+used\s+·\s+)?(?:gpt|codex)[^\n·]*\s+·\s+(?:[/~]|[a-z]:[\\/]).*$`)
	codexActiveTurnRE   = regexp.MustCompile(`(?mi)^\s*[•·]?\s*Working\s*\([^\n)]*esc to interrupt[^\n)]*\)`)
)

// Codex 0.166 chrome around a running turn. With a draft in the box the footer
// reads "<key> to queue message" ("<key> to queue" when narrow) only while a
// turn runs. <key> is the configured binding as Codex labels it: "tab" by
// default, a rebound Ctrl key as "^q" (Linux), "ctrl+q" (Windows) or "⌃q"
// (macOS). Held input is listed above the box under one of the preview
// headers, each message on a "↳" row (three rows at most, then "…"), until
// Codex sends it.
var (
	codexQueueHintRE    = regexp.MustCompile(`(?i)^\s*(tab|(?:\^|⌃|ctrl\+)[a-z]) to queue(?: message)?(?:\s|·|$)`)
	codexHeldHeaderRE   = regexp.MustCompile(`(?m)^\s*•\s*(?:Queued follow-up inputs|Messages to be submitted (?:after next tool call|at end of turn))\b.*$`)
	codexHeldEditHintRE = regexp.MustCompile(`(?i)edit last queued message`)
)

// codexQueueOffered reports that the footer under Codex's input box offers a
// queue key SoluTerminal can press, i.e. a turn is running and the box is not
// empty.
func codexQueueOffered(screen string) bool {
	_, ok := codexQueueKey(screen)
	return ok
}

// codexQueueKey returns the bytes of the queue key the footer under Codex's
// input box names. Only Tab and Ctrl+letter are pressed: they are one byte
// each. Ctrl keys whose byte a terminal reads as another key (^i Tab, ^m
// Enter, which would steer) or that stop Codex (^c, ^d, ^z) are refused, and
// so is anything else (Alt starts with Esc, which interrupts the turn).
func codexQueueKey(screen string) ([]byte, bool) {
	lines := strings.Split(screen, "\n")
	start := -1
	for i, line := range lines {
		if codexComposerLineRE.MatchString(line) {
			start = i
		}
	}
	if start < 0 {
		return nil, false
	}
	for _, line := range lines[start:] {
		m := codexQueueHintRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		label := strings.ToLower(m[1])
		if label == "tab" {
			return []byte("\t"), true
		}
		c := label[len(label)-1]
		if strings.IndexByte("cdimz", c) >= 0 {
			return nil, false
		}
		return []byte{c - 'a' + 1}, true
	}
	return nil, false
}

// codexHeldMessage is one message Codex lists as held: its text without the
// sender label or whitespace, and whether the preview cut it short.
type codexHeldMessage struct {
	text string
	cut  bool
}

// codexHeldMessages returns the messages Codex lists as queued or pending
// above its input box.
func codexHeldMessages(screen string) []codexHeldMessage {
	loc := codexHeldHeaderRE.FindStringIndex(screen)
	if loc == nil {
		return nil
	}
	var held []codexHeldMessage
	var rows []string
	cut := false
	flush := func() {
		if len(rows) > 0 {
			text := conditionalDraftText(stripSenderLabel(strings.TrimSpace(strings.Join(rows, " "))))
			held = append(held, codexHeldMessage{text: text, cut: cut})
		}
		rows, cut = nil, false
	}
	for _, line := range strings.Split(screen[loc[1]:], "\n") {
		if codexComposerLineRE.MatchString(line) {
			break
		}
		switch t := strings.TrimSpace(line); {
		case codexHeldEditHintRE.MatchString(line) || codexHeldHeaderRE.MatchString(line):
			flush()
		case strings.HasPrefix(t, "↳"):
			flush()
			rows = append(rows, strings.TrimSpace(strings.TrimPrefix(t, "↳")))
		case t == "…":
			cut = true
		case len(rows) > 0 && !cut:
			rows = append(rows, t)
		}
	}
	flush()
	return held
}

// codexQueueHolds reports whether Codex lists body as held input: the whole
// text, or, when the preview cut it short, everything the preview shows. The
// "[from …]" label is skipped on both sides (callers pass the unlabelled
// text too).
func codexQueueHolds(screen, body string) bool {
	want := conditionalDraftText(stripSenderLabel(body))
	if want == "" {
		return false
	}
	for _, m := range codexHeldMessages(screen) {
		if m.text == want || (m.cut && m.text != "" && strings.HasPrefix(want, m.text)) {
			return true
		}
	}
	return false
}

// stripSenderLabel drops the "[from …] " prefix labelPaneMessage adds.
func stripSenderLabel(body string) string {
	if strings.HasPrefix(body, "[from ") {
		if i := strings.Index(body, "] "); i > 0 && strings.TrimSpace(body[i+2:]) != "" {
			return body[i+2:]
		}
	}
	return body
}

// codexTurnRunningText reports a live Codex turn on its rendered screen: the
// "Working (… esc to interrupt)" row, or the queue hint. The Working row is
// hidden while the answer streams. computeState goes by output age alone,
// which a quiet stretch or a turn's closing redraws can misread, so callers
// that act on a Codex pane check the screen too.
func codexTurnRunningText(screen string) bool {
	return codexActiveTurnRE.MatchString(screen) || codexQueueOffered(screen)
}

func looksLikeCodexPrompt(s string) bool {
	return codexComposerLineRE.MatchString(s) && codexFooterLineRE.MatchString(s)
}

func (p *Pane) codexPromptVisible() bool {
	if screen, ok := p.screenVisibleText(); ok {
		return looksLikeCodexPrompt(screen) && !codexActiveTurnRE.MatchString(screen)
	}

	tail := p.buffer.SnapshotTail(8192)
	if len(tail) == 0 {
		return false
	}
	s := stripANSI(tail)
	if len(s) > 2400 {
		s = s[len(s)-2400:]
	}
	return looksLikeCodexPrompt(s) && !codexActiveTurnRE.MatchString(s)
}

// stripANSI removes ANSI CSI + OSC escape sequences so downstream regex
// matching sees plain text. Also normalizes `\r` (carriage return) line
// endings to `\n`, because terminal programs use solo `\r` to overwrite
// the current line (e.g. for spinners) — without this conversion,
// multiline regex anchors (`^` / `$`) don't fire at those positions.
func stripANSI(b []byte) string {
	// Replace each CSI/OSC with a SPACE, not an empty string. Claude
	// Code (and other Ratatui-style TUIs) position text by emitting
	// cursor-move escapes instead of literal space bytes — so
	// "App running at" arrives on the wire as something like
	// `\x1b[32mApp\x1b[5Crunning\x1b[9Cat\x1b[0m`. Stripping to empty
	// yielded "Apprunningat", which made every ARIA observation preview
	// unreadable. Replacing with " " over-approximates (a single SGR
	// between adjacent non-space chars inside a word gets split), but
	// downstream callers collapse whitespace before display, and the
	// over-split case is rare compared to the every-prompt space-loss
	// we were shipping. The choicePrompt regex is already space-tolerant
	// ([\s\p{Zs}]*) so detection isn't affected. Stripped-char counters
	// are self-consistent (same function both sides), so position math
	// stays aligned even though the count grows faster.
	s := ansiCSI.ReplaceAllString(string(b), " ")
	s = ansiOSC.ReplaceAllString(s, " ")
	// CRLF → LF first, then lone CR → LF.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// countStrippedChars returns the byte length that stripANSI(payload) would
// produce, WITHOUT allocating the stripped string. It is the hot-path
// equivalent of `len(stripANSI(payload))` (called per PTY payload in the
// readLoop) and MUST stay byte-for-byte parity with it — TotalStrippedChars
// drives the absolute position math in computeState, so any drift would
// re-break the answered-menu suppression.
//
// It replicates stripANSI's four steps in one left-to-right pass:
//  1. ansiCSI  \x1b\[[0-9;?]*[ -/]*[@-~]   -> 1 space (count 1)
//  2. ansiOSC  \x1b\][^\x07\x1b]*(\x07|\x1b\\) -> 1 space (count 1)
//  3. "\r\n" -> "\n"   (a CRLF pair counts as 1, not 2)
//  4. lone "\r" -> "\n" (count unchanged: 1 -> 1)
//
// CSI (\x1b[) and OSC (\x1b]) starts are mutually exclusive and a CSI match
// never spans an \x1b, so the two regex passes consume disjoint byte regions
// and a single forward scan is equivalent to stripANSI's two ReplaceAll
// passes. A CSI/OSC that fails to complete (e.g. truncated at end of buffer)
// is left literal exactly as the regex would: the lone \x1b counts as 1 byte
// and the scan resumes at the next byte. The CR/LF normalization runs over
// the post-strip stream; since strip replacements never emit \r/\n, only the
// original \r\n pairs collapse.
func countStrippedChars(payload []byte) int {
	n := len(payload)
	count := 0
	prevCR := false // did the previous OUTPUT char end up being '\r'?
	for i := 0; i < n; {
		c := payload[i]
		if c == 0x1b && i+1 < n {
			switch payload[i+1] {
			case '[': // candidate CSI: \x1b[[0-9;?]*[ -/]*[@-~]
				if end, ok := matchCSI(payload, i); ok {
					// One space replaces the whole sequence.
					count++
					prevCR = false
					i = end
					continue
				}
			case ']': // candidate OSC: \x1b][^\x07\x1b]*(\x07|\x1b\\)
				if end, ok := matchOSC(payload, i); ok {
					count++
					prevCR = false
					i = end
					continue
				}
			}
			// Fell through: not a complete CSI/OSC. The leading \x1b is a
			// literal byte (regex advances one past it and retries).
			count++
			prevCR = false
			i++
			continue
		}
		// Plain byte. Apply CR normalization: "\r\n" collapses to one char.
		if c == '\n' && prevCR {
			// The preceding '\r' already counted; the "\r\n" pair becomes a
			// single "\n", so this '\n' adds nothing.
			prevCR = false
			i++
			continue
		}
		count++
		prevCR = c == '\r'
		i++
	}
	return count
}

// matchCSI reports whether payload[start:] begins with a complete CSI
// sequence (start points at \x1b, payload[start+1]=='['), and if so returns
// the exclusive end index. Mirrors ansiCSI: \x1b\[[0-9;?]*[ -/]*[@-~].
func matchCSI(payload []byte, start int) (end int, ok bool) {
	j := start + 2 // skip \x1b[
	n := len(payload)
	for j < n { // [0-9;?]*
		c := payload[j]
		if (c >= '0' && c <= '9') || c == ';' || c == '?' {
			j++
			continue
		}
		break
	}
	for j < n { // [ -/]* (0x20-0x2f)
		c := payload[j]
		if c >= 0x20 && c <= 0x2f {
			j++
			continue
		}
		break
	}
	if j < n { // final [@-~] (0x40-0x7e)
		c := payload[j]
		if c >= 0x40 && c <= 0x7e {
			return j + 1, true
		}
	}
	return 0, false
}

// matchOSC reports whether payload[start:] begins with a complete OSC
// sequence (start points at \x1b, payload[start+1]==']'), and if so returns
// the exclusive end index. Mirrors ansiOSC: \x1b\][^\x07\x1b]*(\x07|\x1b\\).
func matchOSC(payload []byte, start int) (end int, ok bool) {
	j := start + 2 // skip \x1b]
	n := len(payload)
	for j < n { // [^\x07\x1b]*
		c := payload[j]
		if c == 0x07 || c == 0x1b {
			break
		}
		j++
	}
	if j >= n {
		return 0, false // no terminator
	}
	if payload[j] == 0x07 { // BEL terminator
		return j + 1, true
	}
	if payload[j] == 0x1b && j+1 < n && payload[j+1] == '\\' { // ST: \x1b\\
		return j + 2, true
	}
	return 0, false
}
