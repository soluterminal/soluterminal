package main

import (
	"image/color"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// A prompt echo is not a menu.
//
// 2026-11-04 (ST-18): Benjamin's prompt "1. i thought … faceid 2. go with ur
// recommendation" was echoed in @release-lead's transcript as "❯ 1. …". Mid
// turn the pane read as choice-prompt: an agent's message was pasted and
// left without Enter ("a choice menu opened"), and later messages were held
// behind it. At 00:44 the same night it happened under a "1.06 refers to …"
// echo. "❯ 1." with any row starting "2." below it is the shape of a focused
// menu, and a numbered reply under the echo is one such row (recorded:
// testdata/menu_screens/claude_2_1_310_66x40). The text-only veto
// (dropIdleComposerMatches) cannot tell those from a menu.
//
// The cells can. Claude Code and Codex paint the person's echoed prompt on a
// band: every cell of the row carries one background colour, and the TUI
// draws the glyph itself, so no agent text can choose it. A live menu's
// focused row is never on that band (Claude: foreground colour only; Codex:
// reverse video). The band is checked only for a colour recorded
// (testdata/menu_screens) at the version the pane's own process runs;
// anything else keeps today's verdict, because a missed menu lets a
// delivery's Enter answer a permission prompt, which is far worse than a held
// message.
//
// Only the pane-state and delivery menu checks read this view. ARIA and
// auto-approve keep reading the unaltered screen for their decisions.

// promptEchoBand is one recorded echo rendering.
type promptEchoBand struct {
	agent   string
	version string // as the daemon's own CLI probe reports it
	glyph   string
	// bg is compared with ==, so the colour's type is part of the match: a
	// truecolor band is color.RGBA, and the 256- or 16-colour downgrade of
	// it (ansi.IndexedColor, ansi.BasicColor) does not match.
	bg color.Color
}

// promptEchoBands lists the renderings recorded in testdata/menu_screens. A
// new CLI version or theme is added only with a capture of it, and the real
// menu captures must still be detected (TestRealMenuScreens).
var promptEchoBands = []promptEchoBand{
	// Claude Code 2.1.310, dark theme: 48;2;55;55;55 across the row.
	{agentClaude, "2.1.310", "❯", color.RGBA{0x37, 0x37, 0x37, 0xff}},
	// Codex 0.166.0: 48;5;8 across the row.
	{agentCodex, "0.166.0", "›", ansi.IndexedColor(8)},
}

// promptEchoBgMatch is the cell test for a band. A variable so the mutation
// test can widen it and see the menu tests fail.
var promptEchoBgMatch = func(band, bg color.Color, reverse bool) bool {
	return !reverse && bg == band
}

// claudeExeVersionRE: Claude Code's native installer runs each build from
// ~/.local/share/claude/versions/<version>. An updated pane's old build shows
// as deleted and is still the build that runs.
var claudeExeVersionRE = regexp.MustCompile(`/claude/versions/(\d+\.\d+\.\d+)(?: \(deleted\))?$`)

// promptEchoBandFor is the recorded band for the pane's agent, as the daemon
// recorded it, at the version running in the pane; nil otherwise. It reads
// procfs, so callers resolve it only once a screen reads as a menu, and
// never under subsMu.
func (p *Pane) promptEchoBandFor(hasCodex bool) *promptEchoBand {
	p.mu.Lock()
	agent, claudeSession := strings.TrimSpace(p.Agent), p.ClaudeSessionID
	p.mu.Unlock()
	want := ""
	switch {
	case !hasCodex && (agent == agentClaude || agent == "" && claudeSession != ""):
		want = agentClaude
	case hasCodex && agent == agentCodex:
		want = agentCodex
	default:
		return nil
	}
	version := runningAgentVersion(p, want)
	for i := range promptEchoBands {
		if b := &promptEchoBands[i]; b.agent == want && b.version == version {
			return b
		}
	}
	return nil
}

// mayBeEcho is the cheap first test: only a focused-row match can be an
// echo. It runs on the state poller's path, before any procfs read.
func mayBeEcho(text string, hasCodex bool) bool {
	return choicePromptRE.MatchString(text) && isGridChoicePromptText(text, hasCodex)
}

// rowMatcher is the emulator read promptEchoMenuText needs.
type rowMatcher interface {
	VisibleTextRowsMatching(match func(bg color.Color, reverse bool) bool) (string, []bool, bool)
}

// promptEchoDenyRE: wording of a permission or approval dialog. While any
// row off the band shows it, no echo is set aside.
var promptEchoDenyRE = regexp.MustCompile(`(?i)do\s+you\s+want\s+to|yes,\s+and\s+don'?t\s+ask\s+again|no,\s+and\s+tell\s+(?:claude|codex)|would\s+you\s+like\s+to|yes,\s+proceed`)

// promptEchoMenuText is text, the pane's screen, for the menu checks: when
// text reads as a menu only because of the agent's own prompt echoes, it is
// the same frame with those echoes' glyphs blanked. In every other case it
// is text unchanged: no recorded band for the agent and version, no
// emulator, a frame that no longer reads as a menu, or any check below.
func (p *Pane) promptEchoMenuText(text string, hasCodex bool, band *promptEchoBand, em rowMatcher) string {
	if band == nil || em == nil || !mayBeEcho(text, hasCodex) {
		return text
	}
	frame, rows, ok := em.VisibleTextRowsMatching(func(bg color.Color, reverse bool) bool {
		return promptEchoBgMatch(band.bg, bg, reverse)
	})
	if !ok || !isGridChoicePromptText(frame, hasCodex) {
		return text
	}
	blanked, n := blankPromptEchoes(frame, rows, band, hasCodex)
	if n == 0 || isGridChoicePromptText(blanked, hasCodex) {
		return text
	}
	if p.promptEchoLogged.CompareAndSwap(false, true) {
		slog.Info("menu check: a prompt echo is not a menu", "pane", p.ID, "agent", band.agent, "version", band.version, "rows", n)
	}
	return blanked
}

// blankPromptEchoes blanks the glyph of each echo block on screen and
// returns how many it blanked. An echo block is a run of whole rows on the
// band whose first row starts with the agent's glyph. Nothing is blanked
// unless the agent's idle input box is the last structure on the screen,
// below every block and off the band, and no row off the band shows a
// dialog's wording or footer.
func blankPromptEchoes(screen string, rows []bool, band *promptEchoBand, codex bool) (string, int) {
	lines := strings.Split(screen, "\n")
	if len(rows) < len(lines)-1 {
		return screen, 0
	}
	var prompt, end int
	var ok bool
	if codex {
		prompt, end, ok = lastCodexComposer(screen)
		ok = ok && idleComposerTrailer(screen[end:], codexTrailerLineRE)
	} else {
		prompt, end, ok = lastClaudeInputBox(screen)
		ok = ok && idleComposerTrailer(screen[end:], claudeFooterLineRE)
	}
	if !ok {
		return screen, 0
	}
	box := strings.Count(screen[:prompt], "\n")
	if box >= len(rows) || rows[box] {
		return screen, 0
	}
	var off strings.Builder
	for y := 0; y < len(lines) && y < len(rows); y++ {
		if !rows[y] {
			off.WriteString(lines[y])
			off.WriteByte('\n')
		}
	}
	if promptEchoDenyRE.MatchString(off.String()) || menuDialogFooterRE.MatchString(off.String()) || codexApprovalHeaderRE.MatchString(off.String()) {
		return screen, 0
	}
	n := 0
	for y := 0; y < box; y++ {
		if !rows[y] || (y > 0 && rows[y-1]) {
			continue
		}
		k := y
		for k+1 < len(rows) && rows[k+1] {
			k++
		}
		if k >= box {
			continue // the run reaches the input box
		}
		lead := strings.TrimLeftFunc(lines[y], unicode.IsSpace)
		if !strings.HasPrefix(lead, band.glyph) {
			continue
		}
		at := len(lines[y]) - len(lead)
		lines[y] = lines[y][:at] + " " + lines[y][at+len(band.glyph):]
		n++
	}
	return strings.Join(lines, "\n"), n
}

// gridMenuTextWithGeneration is gridVisibleTextWithGeneration for the menu
// checks (promptEchoMenuText), the text and generation read under one lock.
// Only a screen that may be an echo resolves the band, outside the lock, and
// is then read again.
func (p *Pane) gridMenuTextWithGeneration(hasCodex bool) (string, uint64, bool) {
	text, generation, ok := p.gridVisibleTextWithGeneration()
	if !ok || !mayBeEcho(text, hasCodex) {
		return text, generation, ok
	}
	band := p.promptEchoBandFor(hasCodex)
	if band == nil {
		return text, generation, ok
	}
	p.subsMu.RLock()
	defer p.subsMu.RUnlock()
	em := p.gridEmu
	if em == nil {
		return "", 0, false
	}
	text, ok = em.VisibleText()
	generation = atomic.LoadUint64(&p.GridChoicePromptGeneration)
	if !ok {
		return text, generation, false
	}
	return p.promptEchoMenuText(text, hasCodex, band, em), generation, true
}

func (p *Pane) gridMenuText(hasCodex bool) (string, bool) {
	text, _, ok := p.gridMenuTextWithGeneration(hasCodex)
	return text, ok
}

// screenMenuText is screenVisibleText for the menu checks.
func (p *Pane) screenMenuText(hasCodex bool) (string, bool) {
	p.subsMu.RLock()
	em := p.replayEmu
	p.subsMu.RUnlock()
	if em == nil {
		return "", false
	}
	text, ok := em.VisibleText()
	if !ok || !mayBeEcho(text, hasCodex) {
		return text, ok
	}
	return p.promptEchoMenuText(text, hasCodex, p.promptEchoBandFor(hasCodex), em), true
}

// codexMenuIsEcho reports that a Codex pane's rendered screen reads as a
// menu only through prompt echoes the band verifies (promptEchoMenuText
// turns its verdict). computeState then sets the raw-tail match aside, as
// claudeMenuOffScreen does for Claude: Codex redraws whole rows, so its raw
// tail shows the same echo and the same sibling rows. Any other screen, or
// no rendered screen, keeps the raw verdict.
func (p *Pane) codexMenuIsEcho(hasCodex bool) bool {
	if !hasCodex {
		return false
	}
	if plain, ok := p.gridVisibleText(); ok {
		text, _ := p.gridMenuText(true)
		return isGridChoicePromptText(plain, true) && !isGridChoicePromptText(text, true)
	}
	if plain, ok := p.screenVisibleText(); ok {
		text, _ := p.screenMenuText(true)
		return isGridChoicePromptText(plain, true) && !isGridChoicePromptText(text, true)
	}
	return false
}

// deliveryMenuText is the delivery's screen (deliveryScreenText) for its
// menu checks, read from the emulator paneVisibleScreen reads first.
func deliveryMenuText(p *Pane, codex bool) string {
	return deliveryMenuTextFrom(p, codex, deliveryScreenText(p))
}

// deliveryMenuTextFrom is deliveryMenuText for a screen already read.
func deliveryMenuTextFrom(p *Pane, codex bool, text string) string {
	if !mayBeEcho(text, codex) {
		return text
	}
	p.subsMu.RLock()
	var em rowMatcher
	if p.gridEmu != nil {
		em = p.gridEmu
	} else if p.replayEmu != nil {
		em = p.replayEmu
	}
	p.subsMu.RUnlock()
	return p.promptEchoMenuText(text, codex, p.promptEchoBandFor(codex), em)
}
