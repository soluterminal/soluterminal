package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"image/color"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ST-18 (2026-11-04): a Claude prompt echo read as a menu stopped an
// agent's message after the paste. See prompt_echo.go.

const (
	claudeEchoSGR = "48;2;55;55;55" // Claude Code 2.1.310, dark theme
	codexEchoSGR  = "48;5;8"        // Codex 0.166.0
	echoTestCols  = 67
	echoTestRows  = 23
)

// setDetectedAgentVersions stubs the version running in a test pane's
// agent process (runningAgentVersion reads procfs).
func setDetectedAgentVersions(t *testing.T, claude, codex string) {
	t.Helper()
	prev := runningAgentVersion
	versions := map[string]string{agentClaude: cliVersionRE.FindString(claude), agentCodex: cliVersionRE.FindString(codex)}
	runningAgentVersion = func(_ *Pane, agent string) string { return versions[agent] }
	t.Cleanup(func() { runningAgentVersion = prev })
}

var cliVersionRE = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)

// sgrRow paints text, padded to the full width, in sgr.
func sgrRow(sgr, text string) string {
	pad := echoTestCols - utf8.RuneCountInString(text)
	if pad < 0 {
		pad = 0
	}
	return "\x1b[" + sgr + "m" + text + strings.Repeat(" ", pad) + "\x1b[0m"
}

// echoScreen is a Claude 2.1.310 frame mid-turn under an echoed prompt,
// built from the rows of Benjamin's screenshot. Each option bends one
// property the veto depends on.
type echoScreen struct {
	echo     []string // the echoed prompt's rows, painted with echoSGR
	echoSGR  string
	partial  bool     // only the glyph cell on the band
	reverse  bool     // one reverse-video cell in the first echo row
	between  []string // rows between the echo and the spinner
	noBox    bool
	afterBox string // a row after the footer
}

// st18Wrapped puts a row starting "2." straight under the echo, the closest
// sibling a menu check can see. (Claude itself keeps "faceid 2." together with
// a no-break space; the recorded 66-column capture has the sibling in the
// reply instead, TestRealPromptEchoCapture.) st18Unwrapped has none.
var (
	st18Wrapped   = []string{"❯ 1. i thought ive already tested approving browser with faceid", "  2. go with ur recommendation"}
	st18Unwrapped = []string{"❯ 1. i thought ive already tested approving browser with faceid 2.", "  go with ur recommendation"}
)

func (s echoScreen) raw() []byte {
	sgr := s.echoSGR
	if sgr == "" {
		sgr = claudeEchoSGR
	}
	rule := strings.Repeat("─", echoTestCols)
	rows := []string{
		"  3. Show HN is tonight at 21:30. Your first comment goes to the",
		"     marketing lead (@cmo) by 21:15.",
		"",
		"✻ Cooked for 4m 51s · done 2:04 AM",
		"",
	}
	for i, line := range s.echo {
		switch {
		case i == 0 && s.partial:
			rows = append(rows, "\x1b["+sgr+"m❯ \x1b[0m"+strings.TrimPrefix(line, "❯ "))
		case i == 0 && s.reverse:
			rows = append(rows, strings.Replace(sgrRow(sgr, line), "thought", "\x1b[7mthought\x1b[27m", 1))
		default:
			rows = append(rows, sgrRow(sgr, line))
		}
	}
	rows = append(rows, s.between...)
	rows = append(rows,
		"",
		"● Calling soluterminal 2 times… (ctrl+o to expand)",
		"",
		"✶ Zigzagging… (1m 8s · ↓ 661 tokens)",
		"  ⎿  Tip: Use /permissions to pre-approve and pre-deny bash, edit,",
		"",
	)
	if !s.noBox {
		rows = append(rows, rule, "❯ ", rule,
			"  ~/wsl-projects/soluterminal [Opus 5.5 | ctx: 13% | solutermina…",
			"  ⏸ manual mode on")
	}
	if s.afterBox != "" {
		rows = append(rows, s.afterBox)
	}
	return []byte("\x1b[H\x1b[2J" + strings.Join(rows, "\r\n"))
}

func echoGridPane(t *testing.T, id string, raw []byte) *Pane {
	t.Helper()
	p := testPaneWithTail(t, id, id, string(raw))
	p.Agent = agentClaude
	p.Cols, p.Rows = echoTestCols, echoTestRows
	p.ensureGrid()
	t.Cleanup(p.shutdownGrid)
	return p
}

// readsAsMenu is the verdict of every check prompt_echo.go feeds: the
// delivery's menu text, the grid menu text and the pane state.
func readsAsMenu(t *testing.T, p *Pane, codex bool) (delivery, grid bool, state PaneRuntimeState) {
	t.Helper()
	prev := deliveryScreenText
	deliveryScreenText = paneVisibleScreen
	defer func() { deliveryScreenText = prev }()
	text, ok := p.gridMenuText(codex)
	if !ok {
		t.Fatal("no rendered grid")
	}
	return isGridChoicePromptText(deliveryMenuText(p, codex), codex), isGridChoicePromptText(text, codex), p.computeStateWithHasCodex(codex)
}

func TestPromptEchoIsNotAMenu(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310 (Claude Code)", "codex-cli 0.166.0")
	cases := map[string]echoScreen{
		// The ST-18 echo with a row starting "2." under it.
		"wrapped echo mid-turn": {echo: st18Wrapped},
		// 00:44 the same night: a numbered reply below a "1.06" echo.
		"numbered reply under an echo": {echo: []string{"❯ 1.06 refers to mobile soluterminal?"}, between: []string{
			"", "● Yes. Two things:", "  1. The phone app approves.", "  2. Shinoque cannot.",
			"", sgrRow(claudeEchoSGR, "❯ tested, phone soluterminal can approve"),
		}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			p := echoGridPane(t, "echo-"+name, s.raw())
			plain, _ := p.gridVisibleText()
			if !isGridChoicePromptText(plain, false) {
				t.Fatalf("the unaltered screen no longer reads as a menu, so this test proves nothing\n%s", plain)
			}
			if d, g, st := readsAsMenu(t, p, false); d || g || st == StateChoicePrompt {
				t.Fatalf("prompt echo read as a menu: delivery=%v grid=%v state=%s\n%s", d, g, st, plain)
			}
		})
	}
}

// failSafeCases each keep today's verdict (a menu): the veto needs every
// one of its conditions.
func failSafeCases() map[string]struct {
	s             echoScreen
	agent         string
	claude, codex string
} {
	ok := "2.1.310 (Claude Code)"
	return map[string]struct {
		s             echoScreen
		agent         string
		claude, codex string
	}{
		"unknown Claude version":             {echoScreen{echo: st18Wrapped}, agentClaude, "2.1.311 (Claude Code)", ""},
		"no Claude version probed":           {echoScreen{echo: st18Wrapped}, agentClaude, "", ""},
		"agent not recorded as Claude":       {echoScreen{echo: st18Wrapped}, "", ok, ""},
		"light theme band":                   {echoScreen{echo: st18Wrapped, echoSGR: "48;2;240;240;240"}, agentClaude, ok, ""},
		"256-colour downgrade":               {echoScreen{echo: st18Wrapped, echoSGR: "48;5;237"}, agentClaude, ok, ""},
		"16-colour downgrade":                {echoScreen{echo: st18Wrapped, echoSGR: "100"}, agentClaude, ok, ""},
		"focus row on a non-echo background": {echoScreen{echo: st18Wrapped, echoSGR: "48;2;58;58;58"}, agentClaude, ok, ""},
		"Codex band on a Claude pane":        {echoScreen{echo: st18Wrapped, echoSGR: codexEchoSGR}, agentClaude, ok, "codex-cli 0.166.0"},
		"band on the glyph only":             {echoScreen{echo: st18Wrapped, partial: true}, agentClaude, ok, ""},
		"reverse-video cell in the row":      {echoScreen{echo: st18Wrapped, reverse: true}, agentClaude, ok, ""},
		"no input box":                       {echoScreen{echo: st18Wrapped, noBox: true}, agentClaude, ok, ""},
		"input box not the last structure":   {echoScreen{echo: st18Wrapped, afterBox: "● more output"}, agentClaude, ok, ""},
		"permission wording on screen":       {echoScreen{echo: st18Wrapped, between: []string{"", "● Do you want to proceed with the merge?"}}, agentClaude, ok, ""},
		"don't-ask-again wording on screen":  {echoScreen{echo: st18Wrapped, between: []string{"", "   3. Yes, and don't ask again for ls"}}, agentClaude, ok, ""},
		"tell-Claude wording on screen":      {echoScreen{echo: st18Wrapped, between: []string{"", "   4. No, and tell Claude what to do"}}, agentClaude, ok, ""},
		"dialog footer on screen":            {echoScreen{echo: st18Wrapped, between: []string{"", " Esc to cancel · Tab to amend"}}, agentClaude, ok, ""},
		"Codex approval wording on screen":   {echoScreen{echo: st18Wrapped, between: []string{"", "  Would you like to run the following command?"}}, agentClaude, ok, ""},
	}
}

// failSafeFailures returns the fail-safe cases that did not read as a menu.
func failSafeFailures(t *testing.T) []string {
	t.Helper()
	var failed []string
	for name, c := range failSafeCases() {
		setDetectedAgentVersions(t, c.claude, c.codex)
		p := echoGridPane(t, "failsafe-"+name, c.s.raw())
		p.Agent = c.agent
		if d, g, st := readsAsMenu(t, p, false); !d || !g || st != StateChoicePrompt {
			failed = append(failed, fmt.Sprintf("%s: delivery=%v grid=%v state=%s", name, d, g, st))
		}
	}
	sort.Strings(failed)
	return failed
}

func TestPromptEchoVetoFailsSafe(t *testing.T) {
	for _, f := range failSafeFailures(t) {
		t.Errorf("not read as a menu: %s", f)
	}
}

// A mutation that widens the colour match (any background counts as the
// band) must turn the fail-safe tests red: they are what guards the colour.
func TestPromptEchoBandMutationIsCaught(t *testing.T) {
	prev := promptEchoBgMatch
	promptEchoBgMatch = func(_, bg color.Color, reverse bool) bool { return bg != nil }
	t.Cleanup(func() { promptEchoBgMatch = prev })
	failed := strings.Join(failSafeFailures(t), "\n")
	for _, want := range []string{"light theme band", "256-colour downgrade", "16-colour downgrade", "focus row on a non-echo background", "reverse-video cell in the row"} {
		if !strings.Contains(failed, want+":") {
			t.Errorf("widened colour match left %q a menu: the fail-safe test does not guard it", want)
		}
	}
}

// repaintFocusRow redraws a live menu's focused row on the agent's echo band.
func repaintFocusRow(t *testing.T, raw []byte, screen, sgr string, cols int) []byte {
	t.Helper()
	lines := strings.Split(screen, "\n")
	y := -1
	for i, line := range lines {
		if choicePromptRE.MatchString(line) {
			y = i
		}
	}
	if y < 0 {
		t.Fatalf("no focused row on screen\n%s", screen)
	}
	row := lines[y]
	pad := cols - utf8.RuneCountInString(row)
	if pad < 0 {
		pad = 0
	}
	out := append([]byte{}, raw...)
	return append(out, fmt.Sprintf("\x1b[%d;1H\x1b[%sm%s%s\x1b[0m", y+1, sgr, row, strings.Repeat(" ", pad))...)
}

// A focused row on the echo band inside a real menu's frame stays a menu.
func TestPromptEchoBandInsideARealMenuStaysAMenu(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310 (Claude Code)", "codex-cli 0.166.0")
	cases := []struct {
		session, step, agent, sgr string
		codex                     bool
	}{
		{"claude_2_1_310_100x40", "permission", agentClaude, claudeEchoSGR, false},
		{"claude_2_1_310_49x40", "permission_amend", agentClaude, claudeEchoSGR, false},
		{"claude_2_1_310_100x40", "ask_step1", agentClaude, claudeEchoSGR, false},
		{"codex_0166_100x40", "approval", agentCodex, codexEchoSGR, true},
	}
	for _, tc := range cases {
		t.Run(tc.session+"/"+tc.step, func(t *testing.T) {
			s, raw := loadMenuScreenSession(t, tc.session)
			at := 0
			for _, step := range s.Steps {
				if step.Name == tc.step {
					at = step.Bytes
				}
			}
			first := menuScreenPane(t, s, raw[:at], tc.agent)
			first.ensureGrid()
			screen, _ := first.gridVisibleText()
			first.shutdownGrid()
			p := menuScreenPane(t, s, repaintFocusRow(t, raw[:at], screen, tc.sgr, int(s.Cols)), tc.agent)
			p.ensureGrid()
			defer p.shutdownGrid()
			if d, g, st := readsAsMenu(t, p, tc.codex); !d || !g || st != StateChoicePrompt {
				text, _ := p.gridVisibleText()
				t.Fatalf("menu with its focused row on the band: delivery=%v grid=%v state=%s\n%s", d, g, st, text)
			}
		})
	}
}

// menuDeliveryPane is a delivery test pane whose grid shows raw.
func menuDeliveryPane(t *testing.T, id, agent string, cols, rows uint16, raw []byte) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames := newTypingHoldPane(t, id)
	p.Agent = agent
	p.forceCodexForTest = agent == agentCodex
	p.Cols, p.Rows = cols, rows
	p.buffer = NewRingBuffer(1 << 20)
	if _, err := p.buffer.Write(raw); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, int64(len(stripANSI(raw))))
	p.ensureGrid()
	return p, frames
}

func feedPaneOutput(t *testing.T, p *Pane, b []byte) {
	t.Helper()
	if _, err := p.buffer.Write(b); err != nil {
		t.Fatal(err)
	}
	atomic.AddInt64(&p.TotalStrippedChars, int64(len(stripANSI(b))))
	p.subsMu.Lock()
	p.feedGridPayloadLocked(b, true)
	_ = p.gridEmu.DrainReplies()
	p.subsMu.Unlock()
}

// stubMenuDelivery runs the delivery on the pane's real screen. Its first
// sleep (the gap after the paste) waits for fed, so the test can change the
// screen between the paste and the submit key.
func stubMenuDelivery(t *testing.T, codex bool, fed <-chan struct{}) {
	t.Helper()
	prev := []any{deliveryScreenText, deliverySleep, deliveryMenuLive, deliveryVisibleTimeout, deliverySubmitTimeout, deliveryRetryAfter, deliveryPollInterval, deliveryQueueHintWait, deliveryCodexTurnOpen}
	deliveryScreenText = paneVisibleScreen
	deliverySleep = func(time.Duration) { <-fed; time.Sleep(time.Millisecond) }
	deliveryMenuLive = func(p *Pane) bool { return p.computeStateWithHasCodex(codex) == StateChoicePrompt }
	deliveryVisibleTimeout, deliverySubmitTimeout, deliveryRetryAfter, deliveryPollInterval = 200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond, time.Millisecond
	deliveryQueueHintWait = 50 * time.Millisecond
	deliveryCodexTurnOpen = func(*Pane) bool { return true }
	t.Cleanup(func() {
		deliveryScreenText = prev[0].(func(*Pane) string)
		deliverySleep = prev[1].(func(time.Duration))
		deliveryMenuLive = prev[2].(func(*Pane) bool)
		deliveryVisibleTimeout, deliverySubmitTimeout, deliveryRetryAfter, deliveryPollInterval = prev[3].(time.Duration), prev[4].(time.Duration), prev[5].(time.Duration), prev[6].(time.Duration)
		deliveryQueueHintWait = prev[7].(time.Duration)
		deliveryCodexTurnOpen = prev[8].(func(*Pane) bool)
	})
}

type deliveryResult struct {
	outcome deliveryOutcome
	err     error
}

func deliverDuring(p *Pane, body string, claude bool) <-chan deliveryResult {
	done := make(chan deliveryResult, 1)
	go func() {
		_, outcome, err := submitAgentMessage(p, body, claude, "@st-mobile · #13 Claude")
		done <- deliveryResult{outcome, err}
	}()
	return done
}

// A real permission or approval menu that opens while a queued delivery's
// paste lands still stops it before the submit key, with blanking on, and
// the pane stays choice-prompt.
func TestRealMenuAfterThePasteStillStopsTheDelivery(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310 (Claude Code)", "codex-cli 0.166.0")
	cases := []struct {
		session, before, menu, agent string
		codex                        bool
	}{
		{"claude_2_1_310_100x40", "echo_idle", "permission", agentClaude, false},
		{"claude_2_1_310_49x40", "echo_draft", "permission", agentClaude, false},
		{"codex_0166_100x40", "echo_idle", "approval", agentCodex, true},
	}
	const body = "ST-18 probe: say PLUM"
	for _, tc := range cases {
		t.Run(tc.session+"/"+tc.menu, func(t *testing.T) {
			stubClaudeBusy(t, true)
			s, raw := loadMenuScreenSession(t, tc.session)
			at := map[string]int{}
			for _, step := range s.Steps {
				at[step.Name] = step.Bytes
			}
			p, frames := menuDeliveryPane(t, "menu-after-paste", tc.agent, s.Cols, s.Rows, raw[:at[tc.before]])
			fed := make(chan struct{})
			stubMenuDelivery(t, tc.codex, fed)
			done := deliverDuring(p, body, !tc.codex)
			pasted := body
			if tc.codex {
				pasted = "\x1b[200~" + body + "\x1b[201~"
			}
			expectFrame(t, frames, "paste", pasted)
			feedPaneOutput(t, p, raw[at[tc.before]:at[tc.menu]])
			close(fed)
			r := <-done
			if !errors.Is(r.err, errDeliveryChoicePrompt) {
				t.Fatalf("delivery = %q, %v; want it stopped by the menu", r.outcome, r.err)
			}
			expectNoFrame(t, frames, "no submit key into the menu")
			if st := p.computeStateWithHasCodex(tc.codex); st != StateChoicePrompt {
				t.Fatalf("state = %s, want choice-prompt", st)
			}
		})
	}
}

// ST-18 itself: the echo reflows into the menu shape while the paste lands.
// With the recorded version the message is submitted; with any other, the
// delivery stops as it did on 2026-11-04 (today's fail-safe).
func TestPromptEchoReflowDuringDelivery(t *testing.T) {
	const body = "ST-18 probe: say PLUM"
	for _, tc := range []struct {
		version string
		enter   bool
	}{{"2.1.310 (Claude Code)", true}, {"2.1.311 (Claude Code)", false}} {
		t.Run(tc.version, func(t *testing.T) {
			setDetectedAgentVersions(t, tc.version, "")
			stubClaudeBusy(t, true)
			stubQueueWatch(t)
			p, frames := menuDeliveryPane(t, "st18-reflow", agentClaude, echoTestCols, echoTestRows, echoScreen{echo: st18Unwrapped}.raw())
			fed := make(chan struct{})
			stubMenuDelivery(t, false, fed)
			done := deliverDuring(p, body, true)
			expectFrame(t, frames, "paste", body)
			feedPaneOutput(t, p, echoScreen{echo: st18Wrapped}.raw())
			close(fed)
			if !tc.enter {
				r := <-done
				if !errors.Is(r.err, errDeliveryChoicePrompt) {
					t.Fatalf("delivery = %q, %v; want today's menu stop", r.outcome, r.err)
				}
				expectNoFrame(t, frames, "no Enter")
				return
			}
			expectFrame(t, frames, "Enter", "\r")
			if r := <-done; r.err != nil || (r.outcome != deliverySubmitted && r.outcome != deliveryQueued) {
				t.Fatalf("delivery = %q, %v; want it sent", r.outcome, r.err)
			}
			if msg := p.Snapshot().UnsentMessage; msg != nil {
				t.Fatalf("a sent message left an unsent receipt: %+v", msg)
			}
		})
	}
}

// The first verdict blanking turns is logged once per pane, with the pane,
// agent version and row count.
func TestPromptEchoVetoIsLoggedOncePerPane(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310 (Claude Code)", "")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	p := echoGridPane(t, "echo-log", echoScreen{echo: st18Wrapped}.raw())
	for i := 0; i < 3; i++ {
		p.gridMenuText(false)
		p.computeStateWithHasCodex(false)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var veto []string
	for _, l := range lines {
		if strings.Contains(l, "a prompt echo is not a menu") {
			veto = append(veto, l)
		}
	}
	if len(veto) != 1 {
		t.Fatalf("logged %d times, want once:\n%s", len(veto), buf.String())
	}
	for _, want := range []string{"pane=echo-log", "agent=claude", "version=2.1.310", "rows=1"} {
		if !strings.Contains(veto[0], want) {
			t.Errorf("log line lacks %q: %s", want, veto[0])
		}
	}
}

// The version comes from the binary the pane's process runs, never from
// what the pane shows.
func TestAgentVersionFromExe(t *testing.T) {
	pkg := map[string]string{
		"/n/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/package.json": `{"name":"@openai/codex-linux-x64","version":"0.166.0"}`,
		"/n/other/package.json": `{"name":"left-pad","version":"0.166.0"}`,
	}
	read := func(path string) ([]byte, error) {
		if b, ok := pkg[path]; ok {
			return []byte(b), nil
		}
		return nil, os.ErrNotExist
	}
	for _, tc := range []struct{ agent, exe, want string }{
		{agentClaude, "/home/u/.local/share/claude/versions/2.1.310", "2.1.310"},
		// A pane started before an update still runs the old build.
		{agentClaude, "/home/u/.local/share/claude/versions/2.1.301 (deleted)", "2.1.301"},
		{agentClaude, "/usr/bin/node", ""},
		{agentClaude, "/home/u/claude/versions/latest", ""},
		{agentCodex, "/n/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex", "0.166.0"},
		// Replaced on disk: package.json may describe the new build.
		{agentCodex, "/n/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex (deleted)", ""},
		{agentCodex, "/n/other/bin/codex", ""},
		{agentCodex, "codex", ""},
	} {
		if got := agentVersionFromExe(tc.agent, tc.exe, read); got != tc.want {
			t.Errorf("%s %q: version %q, want %q", tc.agent, tc.exe, got, tc.want)
		}
	}
}

// Only the pane-state and delivery menu checks read the blanked view; ARIA
// and auto-approve decide on the unaltered screen.
func TestPromptEchoViewOnlyInStateAndDelivery(t *testing.T) {
	allowed := map[string]bool{"prompt_echo.go": true, "state.go": true, "pane_delivery.go": true, "pane_unsent.go": true}
	view := map[string]bool{"gridMenuText": true, "gridMenuTextWithGeneration": true, "screenMenuText": true, "deliveryMenuText": true, "promptEchoMenuText": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var id string
			switch x := n.(type) {
			case *ast.Ident:
				id = x.Name
			case *ast.SelectorExpr:
				id = x.Sel.Name
			}
			if view[id] {
				t.Errorf("%s uses %s: only the state and delivery menu checks may read the blanked view", fset.Position(n.Pos()), id)
			}
			return true
		})
	}
}

// The recorded shape: Claude 2.1.310 mid-turn at 66 columns, a "❯ 1.06 …"
// echo on the band over the reply's "1. Yes" / "2. No". The text alone reads
// as a menu; the band clears it for the recorded version only, and a
// delivery then types and submits.
func TestRealPromptEchoCapture(t *testing.T) {
	s, raw := loadMenuScreenSession(t, "claude_2_1_310_66x40")
	at := 0
	for _, step := range s.Steps {
		if step.Name == "list_running" {
			at = step.Bytes
		}
	}
	const body = "ST-18 probe: say PLUM"
	for _, tc := range []struct {
		version string
		menu    bool
	}{{"2.1.310 (Claude Code)", false}, {"2.1.311 (Claude Code)", true}, {"", true}} {
		t.Run("version "+tc.version, func(t *testing.T) {
			setDetectedAgentVersions(t, tc.version, "")
			p := menuScreenPane(t, s, raw[:at], agentClaude)
			p.ensureGrid()
			defer p.shutdownGrid()
			plain, _ := p.gridVisibleText()
			if !isGridChoicePromptText(plain, false) {
				t.Fatalf("the recorded screen no longer reads as a menu as text, so this test proves nothing\n%s", plain)
			}
			if d, g, st := readsAsMenu(t, p, false); d != tc.menu || g != tc.menu || (st == StateChoicePrompt) != tc.menu {
				t.Fatalf("delivery=%v grid=%v state=%s, want menu=%v\n%s", d, g, st, tc.menu, plain)
			}

			stubClaudeBusy(t, true)
			stubQueueWatch(t)
			dp, frames := menuDeliveryPane(t, "real-echo-delivery", agentClaude, s.Cols, s.Rows, raw[:at])
			fed := make(chan struct{})
			close(fed)
			stubMenuDelivery(t, false, fed)
			r := <-deliverDuring(dp, body, true)
			if tc.menu {
				if !errors.Is(r.err, errDeliveryChoicePrompt) {
					t.Fatalf("delivery = %q, %v; want today's menu refusal", r.outcome, r.err)
				}
				expectNoFrame(t, frames, "nothing typed")
				return
			}
			expectFrame(t, frames, "paste", body)
			expectFrame(t, frames, "Enter", "\r")
			if r.err != nil {
				t.Fatalf("delivery error: %v", r.err)
			}
		})
	}
}

// codexEchoScreen is a Codex 0.166 frame: a "› 1. …" echo on Codex's band,
// a numbered reply under it (a sibling row), then the idle composer with its
// faint placeholder and status footer.
func codexEchoScreen() []byte {
	rows := []string{
		sgrRow(codexEchoSGR, "› 1. yes it works in desktop"),
		"",
		"• 1. #931 merged the lead notices fix",
		"  2. The release notes need a line",
		"",
		"─ Worked for 8s ─────────────────────────────────────────",
		"",
		"› \x1b[2mAsk Codex to do anything\x1b[22m",
		"",
		"  Context 1% used · GPT-6.1-Sol high · ~/repo",
	}
	return []byte("\x1b[H\x1b[2J" + strings.Join(rows, "\r\n"))
}

// Review round 2: Codex's raw tail shows the same echo and sibling rows, so
// computeState's raw-tail path must set the match aside too, and an agent's
// ask_pane-style delivery (deliveryNoSteer) must type and submit.
func TestCodexPromptEchoIsNotAMenu(t *testing.T) {
	const body = "ST-18 probe: say PLUM"
	for _, tc := range []struct {
		version string
		menu    bool
	}{{"codex-cli 0.166.0", false}, {"codex-cli 0.167.0", true}, {"", true}} {
		t.Run("version "+tc.version, func(t *testing.T) {
			setDetectedAgentVersions(t, "", tc.version)
			raw := codexEchoScreen()

			// Raw tail only: no rendered screen, so the raw verdict stands.
			bare := testPaneWithTail(t, "codex-echo-raw", "codex", string(raw))
			bare.Agent = agentCodex
			if st := bare.computeStateWithHasCodex(true); st != StateChoicePrompt {
				t.Fatalf("raw tail alone: state=%s; the screen no longer reads as a menu as text, so this test proves nothing", st)
			}

			p := echoGridPane(t, "codex-echo", raw)
			p.Agent = agentCodex
			plain, _ := p.gridVisibleText()
			if !isGridChoicePromptText(plain, true) {
				t.Fatalf("the unaltered screen no longer reads as a menu\n%s", plain)
			}
			if d, g, st := readsAsMenu(t, p, true); d != tc.menu || g != tc.menu || (st == StateChoicePrompt) != tc.menu {
				t.Fatalf("delivery=%v grid=%v state=%s, want menu=%v\n%s", d, g, st, tc.menu, plain)
			}

			dp, frames := menuDeliveryPane(t, "codex-echo-delivery", agentCodex, echoTestCols, echoTestRows, raw)
			fed := make(chan struct{})
			close(fed)
			stubMenuDelivery(t, true, fed)
			deliveryCodexTurnOpen = func(*Pane) bool { return false } // an idle Codex: Enter submits
			done := make(chan deliveryResult, 1)
			go func() {
				_, outcome, err := deliverAgentMessage(dp, body, false, deliveryNoSteer, "@secure · #8 Claude")
				done <- deliveryResult{outcome, err}
			}()
			r := <-done
			if tc.menu {
				if !errors.Is(r.err, errDeliveryChoicePrompt) {
					t.Fatalf("delivery = %q, %v; want today's menu refusal", r.outcome, r.err)
				}
				expectNoFrame(t, frames, "nothing typed")
				return
			}
			expectFrame(t, frames, "paste", "\x1b[200~"+body+"\x1b[201~")
			expectFrame(t, frames, "Enter", "\r")
			if r.err != nil || r.outcome != deliverySubmitted {
				t.Fatalf("delivery = %q, %v; want submitted", r.outcome, r.err)
			}
		})
	}
}
