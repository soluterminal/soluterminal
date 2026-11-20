package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// PTY submit timing — see feedback_pty_submit_split.md.
//
// When we feed a user-authored line into a PTY-hosted TUI, we MUST
// split the body and the trailing \r across two writes. Bubble Tea
// (Claude Code) and other paste-burst detectors will swallow a \r
// that arrives in the same read() as the body bytes. The fix is:
// write body, sleep `gap`, write \r as its own frame.
//
// Numbers were proven on the voice-auto-send path:
//   - Claude / plain-text PTY input: 400ms — Bubble Tea's paste
//     window is ~50ms but WS+PTY buffering adds headroom, and
//     shorter values regressed in field testing.
//   - Bracketed-paste path (Codex/other TUIs/shells wrapping body
//     in \x1b[200~ ... \x1b[201~): 100ms — paste mode itself
//     resets burst state, so a small margin is enough.
//
// Callers in this package: ARIA send_input tool, ARIA auto-direct
// fire path. The web counterpart (web/src/ptySubmit.ts) MUST stay
// in sync — same numbers, same semantics. If you change one, change
// the other.
const (
	EnterGapClaude         = 400 * time.Millisecond
	EnterGapBracketedPaste = 100 * time.Millisecond
)

// Runtime-tunable enter-gap overrides — let Benjamin tune the 400ms with
// data WITHOUT a rebuild. Read once from the environment at startup so the
// hot submit path stays allocation-free:
//
//	SOLU_ENTER_GAP_CLAUDE_MS  — overrides the 400ms Claude/raw gap.
//	SOLU_ENTER_GAP_DEFAULT_MS — overrides the 100ms bracketed-paste gap.
//
// Both parse to an int count of milliseconds; an unset, blank, non-numeric,
// or negative value leaves the compiled-in default in place. So with NO env
// set the behavior is byte-identical to before (400 / 100). The web side
// mirrors these via /api/enter-gap → web/src/ptySubmit.ts.
var (
	enterGapClaude         = resolveEnterGap("SOLU_ENTER_GAP_CLAUDE_MS", EnterGapClaude)
	enterGapBracketedPaste = resolveEnterGap("SOLU_ENTER_GAP_DEFAULT_MS", EnterGapBracketedPaste)
)

// resolveEnterGap reads an integer-millisecond override from env, falling
// back to `def` when the var is unset/blank/invalid/negative. Zero is a
// valid override (callers that pass body!="" still gate on >0 before
// sleeping), so we accept 0..N and only reject negatives as nonsense.
func resolveEnterGap(envKey string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Millisecond
}

// EnterGap returns the gap to wait between writing the body and
// the trailing \r. `hasClaude` reflects whether the pane is running
// Claude Code (or any TUI that needs the longer Bubble-Tea-safe
// window); everything else uses the bracketed-paste timing. Honors the
// SOLU_ENTER_GAP_* env overrides resolved above; defaults to 400 / 100.
func EnterGap(hasClaude bool) time.Duration {
	if hasClaude {
		return enterGapClaude
	}
	return enterGapBracketedPaste
}

// submitEnterGapWait is the pause between a submitted line and its Enter.
// A test replaces it to open a menu at exactly that moment.
var submitEnterGapWait = time.Sleep

// submitLineToPane types ONE line into a pane and submits it, the way every
// daemon-authored injection already does by hand (the prompt-schedule fire,
// ARIA's mention delivery, the send_input tool). Returns the bytes written.
//
// WHY the body and the \r are two writes: Bubble Tea (Claude Code) treats
// "body + \r" arriving in a single read() as a paste burst and swallows the
// Enter, so a line typed that way sits unsent in the composer forever. Write
// the body, wait EnterGap, then write the carriage return as its own frame.
//
// Claude and Codex terminal panes go through submitDaemonMessage, which
// also confirms the text left the input box (pane_delivery.go). Other
// non-Claude terminal TUIs get bracketed paste so multi-word input survives;
// Claude deliberately does not (paste mode confuses its composer).
//
// A terminal pane is never typed into while the person is typing there or
// has a draft in it (tracker #35: a prompt schedule submitted a half-written
// prompt with its own): the error then wraps errDraftGuardHeld and nothing
// is typed. Retrying is the caller's choice.
func submitLineToPane(p *Pane, body string, hasClaude bool) (int, error) {
	body = strings.TrimRight(body, "\r\n")
	if agent, claude := paneAgentProvider(p); agent {
		wrote, outcome, err := submitDaemonMessage(p, body, claude)
		if err == nil && outcome == deliveryLeftInComposer {
			err = errDeliveryLeftInComposer
		}
		return wrote, err
	}
	first := []byte(body)
	if !hasClaude {
		first = []byte("\x1b[200~" + body + "\x1b[201~")
	}
	// Its callers write unattended (schedules, ARIA mentions): neither the
	// line nor its Enter may land in a permission menu (unattendedMenuCheck).
	// Any other terminal program: its input line cannot be read off the
	// screen, so the person's keystroke recency and the prompt recorder's
	// draft decide. All of it is read under the writer lock with the paste.
	var held string
	var rev uint64
	wrote, err := p.writeWithPrecondition(first, true, false, bothChecks(unattendedMenuCheck(p), func() error {
		held = shellDraftReason(p, time.Now())
		if held != "" {
			return errDraftGuardHeld
		}
		rev = p.inputRevision.Load()
		return nil
	}))
	if errors.Is(err, errDeliveryChoicePrompt) {
		return wrote, err
	}
	if errors.Is(err, errDraftGuardHeld) {
		return 0, draftGuardHeldError(held)
	}
	if err != nil {
		return wrote, errors.New("write failed: " + err.Error())
	}
	submitEnterGapWait(EnterGap(hasClaude))
	// Enter only if nothing was typed since the paste (it would submit the
	// person's keystrokes with it; the paste itself is one revision) and no
	// menu opened over it.
	n, err := p.writeWithPrecondition([]byte("\r"), true, false, bothChecks(func() error {
		if p.inputRevision.Load() != rev+1 {
			return errDeliveryLeftInComposer
		}
		return nil
	}, unattendedMenuCheck(p)))
	wrote += n
	if errors.Is(err, errDeliveryChoicePrompt) || errors.Is(err, errDeliveryLeftInComposer) {
		// The line waits in the box for the person, as an agent pane's
		// delivery leaves it: callers must not also answer elsewhere.
		return wrote, errDeliveryLeftInComposer
	}
	if err != nil {
		return wrote, errors.New("submit-Enter failed: " + err.Error())
	}
	return wrote, nil
}

// EnterGapClaudeMS / EnterGapDefaultMS expose the resolved (env-overridden)
// gaps in integer milliseconds for the /api/enter-gap config the web client
// fetches to keep web/src/ptySubmit.ts in sync without a rebuild.
func EnterGapClaudeMS() int  { return int(enterGapClaude / time.Millisecond) }
func EnterGapDefaultMS() int { return int(enterGapBracketedPaste / time.Millisecond) }

// shellDraftReason is draftGuardReason for a terminal program whose input
// line SoluTerminal cannot read (a shell, an editor): the person typed in
// the last draftGuardQuiet, or the prompt recorder holds a draft they typed.
// The draft never goes stale here: with no screen to check, a line left
// unsent for minutes is still theirs, and Enter would run it. A recorder
// draft that is not really there costs a skipped run, never their command.
// Caller holds the writer lock (agentWriteMu).
func shellDraftReason(p *Pane, now time.Time) string {
	if last := p.lastHumanInputMs.Load(); last > 0 && now.Sub(time.UnixMilli(last)) < draftGuardQuiet {
		return "the person is typing"
	}
	if n, _ := p.promptRec.humanDraft(); n > 0 {
		return heldBoxTextReason
	}
	return ""
}
