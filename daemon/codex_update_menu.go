package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Codex's own start-up update menu (ST-289), as the official Codex 0.168.0
// and 0.169.0 builds draw it (testdata/menu_screens/codex_*_update_*):
//
//	  Update available · 0.168.0 → 0.169.0
//	  Release notes: https://github.com/openai/codex/releases/latest
//	› 1. Update now (runs `npm install -g @openai/codex`)
//	  2. Skip
//	  3. Skip until next version
//	  enter continue · esc skip
//
// It replaces the input box until the person answers it, and its focus can
// be on any option (choicePromptRE only knows a focused "1."). Typed text
// would choose an option there: a "1" runs a global npm install. So it is a
// menu wherever its focus is, it is the person's to answer (no auto-approve,
// attention says so), and an agent's message waits behind it.
var codexUpdateMenuRE = regexp.MustCompile(`(?m)^[ \t]*Update available · (\S+) → (\S+)[ \t]*$(?:\n.*){0,4}?\n[ \t]*[❯›]?[ \t]*1\. Update now\b.*\n[ \t]*[❯›]?[ \t]*2\. Skip[ \t]*(?:\n[ \t]*[❯›]?[ \t]*3\. Skip until next version[ \t]*)?(?:\n[ \t]*)*\n[ \t]*enter continue · esc skip[ \t]*$`)

// codexUpdateMenuMatches is the live update menu on screen, if any: from its
// option list (where choicePromptRE anchors a menu focused on "1.") to its
// footer. A Codex input box drawn after it means the menu is old output.
func codexUpdateMenuMatches(screen string) [][]int {
	var matches [][]int
	for _, loc := range codexUpdateMenuLocs(screen) {
		matches = append(matches, []int{loc[2], loc[1]})
	}
	return matches
}

// codexUpdateMenuLocs is each live menu's [header, end, option list] offsets.
func codexUpdateMenuLocs(screen string) [][3]int {
	var locs [][3]int
	for _, loc := range codexUpdateMenuRE.FindAllStringIndex(screen, -1) {
		if looksLikeCodexPrompt(screen[loc[1]:]) {
			continue
		}
		menu := screen[loc[0]:loc[1]]
		options := loc[0] + strings.LastIndex(menu[:strings.Index(menu, "1. Update now")], "\n") + 1
		locs = append(locs, [3]int{loc[0], loc[1], options})
	}
	return locs
}

// codexUpdateMenuVersions reports the versions a live update menu offers.
func codexUpdateMenuVersions(screen string) (from, to string, ok bool) {
	locs := codexUpdateMenuLocs(screen)
	if len(locs) == 0 {
		return "", "", false
	}
	loc := locs[len(locs)-1]
	m := codexUpdateMenuRE.FindStringSubmatch(screen[loc[0]:loc[1]])
	return m[1], m[2], true
}

func paneShowsCodexUpdateMenu(p *Pane) bool {
	return len(codexUpdateMenuMatches(paneVisibleScreen(p))) > 0
}

const codexUpdateMenuName = "codex_update"

func codexUpdateMenuHint(from, to string) string {
	return fmt.Sprintf("Codex is showing its own update menu (Update available %s → %s), so its input box is not open yet. The person chooses Update now or Skip in that pane.", from, to)
}
