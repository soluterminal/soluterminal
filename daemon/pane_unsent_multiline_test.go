package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"soluterminal/grid"
)

const st11Wrapped = "ST11 alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau."
const st11Multiline = "ST11 first line\nST11 second line"

func st11Box(codex bool, text string) string {
	if codex {
		return "Earlier output\n\n› " + strings.ReplaceAll(text, "\n", "\n  ") + "\n\n  Context 0% used · GPT-6.1-Sol high · ~/project\n"
	}
	return "Earlier output\n────────────────────\n❯\u00a0" + strings.ReplaceAll(text, "\n", "\n  ") + "\n────────────────────\n  ? for shortcuts\n"
}

// These are raw outputs of the exact real agents, including the empty-box
// no-op after 4096 Backspaces. The tests replay output; they do not model
// an editor and assume that the same keys must work.
func st11Recording(t *testing.T, agent string) map[string]string {
	t.Helper()
	setDetectedAgentVersions(t, "2.1.310", "0.166.0")
	name := "claude_2_1_310"
	if agent == agentCodex {
		name = "codex_0_166_0"
	}
	base := filepath.Join("testdata", "unsent_clear", name)
	b, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var session menuScreenSession
	if err := json.Unmarshal(b, &session); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + ".raw")
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, step := range session.Steps {
		p := menuScreenPane(t, session, raw[:step.Bytes], agent)
		p.ensureGrid()
		plainScreen, blankedScreen, readable := putBackClearScreen(p)
		screen := typedComposerScreen(plainScreen, blankedScreen, agent == agentCodex)
		if !readable {
			t.Fatalf("%s/%s: no grid", agent, step.Name)
		}
		result[step.Name] = screen
		if step.Name == "wrapped_clear" || step.Name == "empty_noop" || step.Name == "multiline_clear" {
			// The pending guard must use the same chrome-preserving read as
			// Clear, even when real SGR faint blanks borders and footers.
			plain, typed, readable := paneScreenFaintBlanked(p)
			receipt := &unsentPaneMessage{clearPending: true, claude: agent == agentClaude}
			shown, known := p.putBackShownInTypedBox(receipt, plain, typed)
			if !readable || !known || shown {
				t.Fatalf("%s/%s: pending guard didn't recognize the empty real box", agent, step.Name)
			}
		}
		if agent == agentCodex {
			// A1: the recorded draft has no cell background to require.
			snap := p.gridEmu.Snapshot()
			for y, line := range strings.Split(screen, "\n") {
				if strings.HasPrefix(line, "› ") {
					for x := 0; x < snap.Cols; x++ {
						if bg := snap.Cells[y*snap.Cols+x].BG; bg != "" {
							t.Fatalf("recording acquired a composer background: %q", bg)
						}
					}
				}
			}
		}
		p.shutdownGrid()
	}
	return result
}

func TestPutBackClearRealAgentRecordings(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		t.Run(agent, func(t *testing.T) {
			recording := st11Recording(t, agent)
			for name, want := range map[string]string{
				"wrapped": st11Wrapped, "multiline": st11Multiline,
				"edited_multiline": st11Multiline + "\nperson edited line",
				"wrapped_clear":    "", "empty_noop": "", "multiline_clear": "",
			} {
				text, rows, ok := readPutBackClearBox(recording[name], agent == agentCodex)
				if !ok || conditionalDraftText(text) != conditionalDraftText(want) {
					t.Fatalf("%s: box=%q rows=%d readable=%v\n%s", name, text, rows, ok, recording[name])
				}
				if want != "" && rows < 2 {
					t.Fatalf("%s didn't exercise multiple rows", name)
				}
				if isGridChoicePromptText(recording[name], agent == agentCodex) {
					t.Fatalf("%s opened a menu", name)
				}
			}
			if recording["empty_noop"] == "" || recording["multiline_clear"] == "" {
				t.Fatal("agent did not remain at its composer after excess keys")
			}
		})
	}
}

func TestPutBackClearRecordedBoxRefusals(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		recording := st11Recording(t, agent)
		screen := recording["multiline"]
		glyph := "❯"
		if agent == agentCodex {
			glyph = "›"
		}
		at := strings.Index(screen, "\n"+glyph) + 1
		if at <= 0 {
			t.Fatal("no recorded prompt")
		}
		for name, broken := range map[string]string{
			"pasted":           strings.Replace(screen, "ST11 first line", "[Pasted text #1 +2 lines]", 1),
			"top offscreen":    screen[at:],
			"bottom offscreen": screen[:strings.Index(screen, "ST11 second line")+len("ST11 second line")],
		} {
			if _, _, ok := readPutBackClearBox(broken, agent == agentCodex); ok {
				t.Fatalf("%s %s accepted an incomplete recorded box", agent, name)
			}
		}
	}
}

func st11ClearPane(t *testing.T, codex bool, body, text string, edited bool) (*Pane, <-chan directAutoFrameRead, *atomic.Value) {
	t.Helper()
	asCaller(t, callerHuman)
	setDetectedAgentVersions(t, "2.1.310", "0.166.0")
	p, frames := newTypingHoldPane(t, "st11-clear")
	p.Agent = agentClaude
	if codex {
		p.Agent = agentCodex
	}
	p.unsentMessage = &unsentPaneMessage{ID: "st11", Sender: "@test", PutBack: true, Edited: edited, body: body, claude: !codex, connection: p.agentConn}
	screen := &atomic.Value{}
	screen.Store(st11Box(codex, text))
	stubDeliveryScreen(t, screen)
	prevClear, prevDraft := putBackClearScreen, draftGuardScreen
	putBackClearScreen = func(*Pane) (string, string, bool) { s := screen.Load().(string); return s, s, true }
	draftGuardScreen = func(*Pane) (string, string, bool) {
		s := screen.Load().(string)
		return s, s, true
	}
	t.Cleanup(func() { putBackClearScreen, draftGuardScreen = prevClear, prevDraft })
	return p, frames, screen
}

func TestPutBackClearMultilineOneWrite(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, text string
			edited           bool
		}{
			{"wrapped", st11Wrapped, strings.Replace(st11Wrapped, "lambda", "\nlambda", 1), false},
			{"shift enter", st11Multiline, st11Multiline, false},
			{"edited multiline", st11Multiline, st11Multiline + "\nperson edited line", true},
			{"A2 whitespace", "ST11 first line  \nST11 second line", "ST11first line\nST11 second line", false},
			{"capped", strings.Repeat(" ", 5000) + st11Multiline, st11Multiline, false},
			{"A3 literal single-line ellipsis", "wait...", "wait...", false},
			{"A3 literal multiline ellipsis", "wait…\nthen continue", "wait…\nthen continue", false},
			{"A3 wrapped literal ellipsis", "wait... then continue", "wait..\n. then continue", false},
		} {
			t.Run(tc.name+map[bool]string{true: "/codex", false: "/claude"}[codex], func(t *testing.T) {
				p, frames, screen := st11ClearPane(t, codex, tc.body, tc.text, tc.edited)
				count := utf8.RuneCountInString(tc.body) + strings.Count(tc.body, "\n")
				if tc.edited {
					box, rows, _ := readPutBackClearBox(screen.Load().(string), codex)
					count = utf8.RuneCountInString(box) + rows
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					expectFrame(t, frames, "one bounded Clear", strings.Repeat("\x7f", min(count+64, 4096)))
					screen.Store(st11Box(codex, ""))
				}()
				rec := actOnUnsent(t, p, "clear")
				<-done
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleared"`) || p.Snapshot().UnsentMessage != nil {
					t.Fatalf("clear: %d %s %+v", rec.Code, rec.Body, p.Snapshot().UnsentMessage)
				}
				if p.composerPendingLen() != 0 {
					t.Fatal("Clear keys leaked into prompt history")
				}
				expectNoFrame(t, frames, "no second write")
			})
		}
	}
}

// The unknown-version branch retains main's pre-#947 path. For edited single
// rows, the recorded versions must produce the same keys, response and receipt
// state, both when the box empties and when the old read still sees the body.
func TestPutBackClearEditedSingleLineMatchesLegacy(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, clears := range []bool{false, true} {
			var legacyResponse string
			for _, recorded := range []bool{false, true} {
				name := map[bool]string{true: "codex", false: "claude"}[codex]
				t.Run(name+"/clears="+fmt.Sprint(clears)+"/recorded="+fmt.Sprint(recorded), func(t *testing.T) {
					p, frames, screen := st11ClearPane(t, codex, "wait", "wait... person edit", true)
					if !recorded {
						setDetectedAgentVersions(t, "", "")
					}
					done := make(chan struct{})
					go func() {
						defer close(done)
						expectFrame(t, frames, "main's edited single-line keys", "\x01\x0b")
						if clears {
							screen.Store(st11Box(codex, ""))
						}
					}()
					rec := actOnUnsent(t, p, "clear")
					<-done
					want := `"left_in_composer"`
					if clears {
						want = `"cleared"`
					}
					if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
						t.Fatalf("legacy clear: %d %s", rec.Code, rec.Body)
					}
					if !recorded {
						legacyResponse = rec.Body.String()
					} else if rec.Body.String() != legacyResponse {
						t.Fatalf("response differs from main's path: %s vs %s", rec.Body, legacyResponse)
					}
					msg := p.Snapshot().UnsentMessage
					if clears && msg != nil || !clears && (msg == nil || !msg.Edited || msg.clearPending || msg.revision != p.inputRevision.Load()) {
						t.Fatalf("legacy receipt handling changed: %+v", msg)
					}
					expectNoFrame(t, frames, "no retry on old path")
				})
			}
		}
	}
}

func TestPutBackClearA3EllipsisAndPasteRefusals(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, text string
			edited           bool
		}{
			{"truncated Unicode", "wait for all characters", "wait…", false},
			{"truncated ASCII", "wait for all characters", "wait...", false},
			{"edited multiline", "wait", "wait...\nperson edit", true},
			{"edited multiline Unicode", "wait", "wait…\nperson edit", true},
			{"unedited paste", "[Pasted text #1]", "[Pasted text #1]", false},
			{"edited paste", "[Pasted text #1]", "[Pasted text #1]...", true},
			{"unedited wrapped paste", "[Pasted text #1]", "[Pas\nted text #1]", false},
			{"edited wrapped paste", "[Pasted text #1]", "[Pas\nted text #1]...", true},
		} {
			t.Run(tc.name+map[bool]string{true: "/codex", false: "/claude"}[codex], func(t *testing.T) {
				p, frames, _ := st11ClearPane(t, codex, tc.body, tc.text, tc.edited)
				before := p.unsentMessage
				if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 || p.unsentMessage != before {
					t.Fatalf("A3 refusal: %d %s", rec.Code, rec.Body)
				}
				expectNoFrame(t, frames, "A3 refusal before any keys")
			})
		}
	}
}

func TestPutBackClearRefusesIncompleteOrChangedBox(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, mutation := range []string{"pasted", "extra", "missing", "ellipsis", "top offscreen", "bottom offscreen", "popup", "working", "no grid"} {
			t.Run(mutation+map[bool]string{true: "/codex", false: "/claude"}[codex], func(t *testing.T) {
				p, frames, screen := st11ClearPane(t, codex, st11Multiline, st11Multiline, false)
				s := screen.Load().(string)
				switch mutation {
				case "pasted":
					s = st11Box(codex, "[Pasted text #1 +2 lines]")
				case "extra":
					s = st11Box(codex, st11Multiline+"!")
				case "missing":
					s = st11Box(codex, strings.Replace(st11Multiline, "first", "firs", 1))
				case "ellipsis":
					s = st11Box(codex, "ST11 …")
				case "top offscreen":
					lines := strings.Split(s, "\n")
					s = strings.Join(lines[2:], "\n")
				case "bottom offscreen":
					if codex {
						s = strings.Split(s, "  Context")[0]
					} else {
						at := strings.LastIndex(s, "────────────────────")
						s = s[:at]
					}
				case "popup":
					s += "Select a file\n"
				case "working":
					if codex {
						s = strings.Replace(s, "  Context 0% used · GPT-6.1-Sol high · ~/project", "  tab to queue message", 1)
					} else {
						s = strings.Replace(s, "────────────────────\n  ?", "missing border\n  ?", 1)
					}
				case "no grid":
					putBackClearScreen = func(*Pane) (string, string, bool) { return "", "", false }
				}
				screen.Store(s)
				before := p.unsentMessage
				rec := actOnUnsent(t, p, "clear")
				if rec.Code != http.StatusConflict || p.unsentMessage != before {
					t.Fatalf("clear: %d %s", rec.Code, rec.Body)
				}
				expectNoFrame(t, frames, "refused without keys")
			})
		}
	}
}

func TestPutBackClearUnknownVersionFallback(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, version := range []string{"", "0.166.1", "2.1.311"} {
			t.Run(version+map[bool]string{true: "/codex", false: "/claude"}[codex], func(t *testing.T) {
				p, frames, _ := st11ClearPane(t, codex, st11Multiline, st11Multiline, true)
				setDetectedAgentVersions(t, version, version)
				if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 {
					t.Fatalf("unknown version clear: %d %s", rec.Code, rec.Body)
				}
				expectNoFrame(t, frames, "unknown version must not guess")
			})
		}
	}
}

func TestPutBackClearReadWriteConflict(t *testing.T) {
	for _, mutation := range []string{"input", "receipt", "screen"} {
		t.Run(mutation, func(t *testing.T) {
			p, frames, screen := st11ClearPane(t, true, st11Multiline, st11Multiline, false)
			reads := 0
			putBackClearScreen = func(*Pane) (string, string, bool) {
				reads++
				if reads == 2 {
					switch mutation {
					case "input":
						p.inputRevision.Add(1)
					case "receipt":
						p.mu.Lock()
						next := *p.unsentMessage
						p.unsentMessage = &next
						p.mu.Unlock()
					case "screen":
						s := st11Box(true, st11Multiline+"new")
						return s, s, true
					}
				}
				s := screen.Load().(string)
				return s, s, true
			}
			if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 {
				t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
			}
			expectNoFrame(t, frames, "409 before writing")
		})
	}
}

func TestPutBackClearRechecksHumanUnderDeliveryLock(t *testing.T) {
	p, frames, _ := st11ClearPane(t, true, st11Multiline, st11Multiline, false)
	calls := 0
	identifyCallerHook = func(*http.Request) callerIdentity {
		calls++
		if calls <= 2 {
			return callerIdentity{Kind: callerHuman}
		}
		return callerIdentity{Kind: callerPane, PaneID: "other-pane"}
	}
	if rec := actOnUnsent(t, p, "clear"); rec.Code != http.StatusForbidden || calls < 3 {
		t.Fatalf("authority was not rechecked: calls=%d status=%d body=%s", calls, rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "no keys after authority change")
}

func TestPutBackClearPendingReceiptSurvivesDelayedQueueWatch(t *testing.T) {
	p, frames, screen := st11ClearPane(t, true, st11Multiline, st11Multiline, false)
	p.unsentMessage.clearPending = true
	p.unsentMessage.Edited = true
	before := p.unsentMessage
	screen.Store(st11Box(true, "x"))
	p.recordPutBackMessage("@late", "different queued message", false, p.inputRevision.Load())
	if p.unsentMessage != before || p.putBackInputCheck() == nil {
		t.Fatal("delayed queue watcher replaced the partial-clear guard")
	}
	expectNoFrame(t, frames, "receipt preservation sends no keys")
}

func TestPutBackClearWaitsForReadableRedrawWithoutRetry(t *testing.T) {
	p, frames, screen := st11ClearPane(t, true, st11Multiline, st11Multiline, false)
	reads := 0
	putBackClearScreen = func(*Pane) (string, string, bool) {
		reads++
		if reads == 3 {
			return "half drawn", "half drawn", true
		}
		if reads > 3 {
			s := st11Box(true, "")
			return s, s, true
		}
		s := screen.Load().(string)
		return s, s, true
	}
	rec := actOnUnsent(t, p, "clear")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleared"`) || reads < 4 {
		t.Fatalf("redraw: %d %s reads=%d", rec.Code, rec.Body, reads)
	}
	expectFrame(t, frames, "one write", strings.Repeat("\x7f", utf8.RuneCountInString(st11Multiline)+strings.Count(st11Multiline, "\n")+64))
	expectNoFrame(t, frames, "redraw does not retry keys")
}

func TestPutBackClearPartlyClearedKeepsReceiptAndGuard(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, remainder := range []string{"x", "unreadable"} {
			t.Run(remainder+map[bool]string{true: "/codex", false: "/claude"}[codex], func(t *testing.T) {
				p, frames, screen := st11ClearPane(t, codex, st11Multiline, st11Multiline, false)
				done := make(chan struct{})
				go func() {
					defer close(done)
					expectFrame(t, frames, "one write", strings.Repeat("\x7f", utf8.RuneCountInString(st11Multiline)+strings.Count(st11Multiline, "\n")+64))
					if remainder == "unreadable" {
						screen.Store("unknown layout")
					} else {
						screen.Store(st11Box(codex, remainder))
					}
				}()
				rec := actOnUnsent(t, p, "clear")
				<-done
				msg := p.Snapshot().UnsentMessage
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "partly_cleared") || msg == nil || !msg.clearPending || msg.revision != p.inputRevision.Load() {
					t.Fatalf("partial clear: %d %s %+v", rec.Code, rec.Body, msg)
				}
				if err := p.putBackInputCheck(); err == nil {
					t.Fatal("short/unreadable remainder lifted agent guard")
				}
				p.deliveryMu.Lock()
				watching := p.checkPutBack()
				p.deliveryMu.Unlock()
				if !watching || p.unsentMessage == nil {
					t.Fatal("watch dropped remnant receipt")
				}
				expectNoFrame(t, frames, "no auto retry")
				screen.Store(st11Box(codex, ""))
				p.deliveryMu.Lock()
				p.checkPutBack()
				p.deliveryMu.Unlock()
				if p.unsentMessage != nil || p.putBackInputCheck() != nil {
					t.Fatal("empty box did not release receipt")
				}
			})
		}
	}
}

func TestPutBackClearSequencesHaveNoBannedBytes(t *testing.T) {
	for _, codex := range []bool{false, true} {
		t.Run(map[bool]string{true: "codex-0.166.0", false: "claude-2.1.310"}[codex], func(t *testing.T) {
			p, frames, _ := st11ClearPane(t, codex, st11Multiline, st11Multiline, false)
			rec := actOnUnsent(t, p, "clear")
			if rec.Code != 200 {
				t.Fatal(rec.Body)
			}
			frame := <-frames
			if len(frame.payload) > 4096 || strings.ContainsAny(string(frame.payload), "\r\n\x03\x04\x1b\x1a\t") {
				t.Fatalf("unsafe sequence: %q", frame.payload)
			}
		})
	}
}

func st11FeedClearFixture(t *testing.T, p *Pane, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "unsent_clear", name+".raw"))
	if err != nil {
		t.Fatal(err)
	}
	p.ensureGrid()
	p.subsMu.Lock()
	p.feedGridPayloadLocked(append([]byte("\x1b[H\x1b[2J"), raw...), true)
	_ = p.gridEmu.DrainReplies()
	p.subsMu.Unlock()
}

func st11UseRealClearReader(p *Pane) {
	p.Cols, p.Rows = 66, 40
	putBackClearScreen = paneScreenFaintBlanked
	draftGuardScreen = paneScreenFaintBlanked
	deliveryScreenText = paneVisibleScreen
}

func TestPutBackClearRefusesUnblankedFaintPlaceholderAndTruncation(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		for _, fixture := range []string{"faint_pasted", "faint_wrapped_pasted", "faint_ellipsis", "faint_ascii_ellipsis", "faint_remnant"} {
			t.Run(agent+"/"+fixture, func(t *testing.T) {
				edited := fixture == "faint_pasted" || fixture == "faint_wrapped_pasted"
				p, frames, _ := st11ClearPane(t, agent == agentCodex, st11Multiline, st11Multiline, edited)
				st11UseRealClearReader(p)
				st11FeedClearFixture(t, p, agent+"_"+fixture)
				plain, blanked, ok := paneScreenFaintBlanked(p)
				text, _, complete := readPutBackClearBox(typedComposerScreen(plain, blanked, agent == agentCodex), agent == agentCodex)
				if !ok || complete && strings.TrimSpace(text) != "" {
					t.Fatal("fixture did not hide the unsafe text when faint-blanked")
				}
				before := p.unsentMessage
				if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 || p.unsentMessage != before {
					t.Fatalf("faint refusal: %d %s", rec.Code, rec.Body)
				}
				expectNoFrame(t, frames, "no keys after raw placeholder/truncation refusal")
			})
		}
	}
}

func TestPutBackClearCountsUnblankedEditedText(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		t.Run(agent, func(t *testing.T) {
			p, frames, _ := st11ClearPane(t, agent == agentCodex, st11Multiline, st11Multiline, true)
			st11UseRealClearReader(p)
			st11FeedClearFixture(t, p, agent+"_faint_edited_multiline")
			done := make(chan struct{})
			go func() {
				defer close(done)
				expectFrame(t, frames, "count includes faint edit", strings.Repeat("\x7f", utf8.RuneCountInString("remaining text\nsecond line")+2+64))
				st11FeedClearFixture(t, p, agent+"_faint_hint")
			}()
			rec := actOnUnsent(t, p, "clear")
			<-done
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleared"`) || p.unsentMessage != nil {
				t.Fatalf("faint edit: %d %s", rec.Code, rec.Body)
			}
			expectNoFrame(t, frames, "no retry")
		})
	}
}

func TestPutBackClearPendingSingleRowRetryUsesPairedReader(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		t.Run(agent, func(t *testing.T) {
			p, frames, _ := st11ClearPane(t, agent == agentCodex, st11Multiline, st11Multiline, true)
			p.unsentMessage.clearPending = true
			st11UseRealClearReader(p)
			st11FeedClearFixture(t, p, agent+"_faint_remnant")
			if p.putBackInputCheck() == nil {
				t.Fatal("pending faint remnant lost the guard before retry")
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				expectFrame(t, frames, "pending retry keeps full-clear keys", strings.Repeat("\x7f", utf8.RuneCountInString("remaining text")+1+64))
				st11FeedClearFixture(t, p, agent+"_faint_hint")
			}()
			rec := actOnUnsent(t, p, "clear")
			<-done
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleared"`) || p.unsentMessage != nil || p.putBackInputCheck() != nil {
				t.Fatalf("pending retry: %d %s %+v", rec.Code, rec.Body, p.unsentMessage)
			}
			expectNoFrame(t, frames, "one write per human retry")
		})
	}
}

func TestPutBackClearDetectsChangeHiddenByFaintBlanking(t *testing.T) {
	p, frames, _ := st11ClearPane(t, false, st11Multiline, st11Multiline, true)
	st11UseRealClearReader(p)
	st11FeedClearFixture(t, p, "claude_faint_edited_multiline")
	read := putBackClearScreen
	reads := 0
	putBackClearScreen = func(p *Pane) (string, string, bool) {
		plain, blanked, ok := read(p)
		reads++
		if reads == 2 {
			plain = strings.Replace(plain, "remaining text", "remaining edit", 1)
		}
		return plain, blanked, ok
	}
	if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 || p.unsentMessage == nil {
		t.Fatalf("hidden change: %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "raw change must refuse even if blanked read is unchanged")
}

func TestPutBackClearRealClaudePasteStyleAndRefusal(t *testing.T) {
	p, frames, _ := st11ClearPane(t, false, st11Multiline, st11Multiline, true)
	st11UseRealClearReader(p)
	st11FeedClearFixture(t, p, "claude_2_1_310_paste")
	plain, _, ok := paneScreenFaintBlanked(p)
	if !ok || !strings.Contains(plain, "[Pasted text #1 +9 lines]") {
		t.Fatal("missing real paste placeholder")
	}
	snap := p.gridEmu.Snapshot()
	for y, line := range strings.Split(plain, "\n") {
		if !strings.Contains(line, "[Pasted text #1 +9 lines]") {
			continue
		}
		for x := 2; x < 27; x++ {
			if snap.Cells[y*snap.Cols+x].Attrs&grid.AttrFaint != 0 {
				t.Fatal("real Claude placeholder acquired faint styling; review fixture provenance")
			}
		}
	}
	if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 {
		t.Fatalf("real paste was not refused: %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "real placeholder never clears hidden paste")
}

func TestPutBackClearFaintRemnantsKeepReceiptAndGuard(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		for _, fixture := range []string{"faint_remnant", "faint_pasted", "faint_hint_with_remnant", "solid_hint", "faint_hint"} {
			t.Run(agent+"/"+fixture, func(t *testing.T) {
				codex := agent == agentCodex
				p, frames, _ := st11ClearPane(t, codex, st11Multiline, st11Multiline, false)
				st11UseRealClearReader(p)
				p.ensureGrid()
				p.subsMu.Lock()
				p.feedGridPayloadLocked([]byte("\x1b[H\x1b[2J"+strings.ReplaceAll(st11Box(codex, st11Multiline), "\n", "\r\n")), true)
				p.subsMu.Unlock()
				done := make(chan struct{})
				go func() {
					defer close(done)
					expectFrame(t, frames, "one write", strings.Repeat("\x7f", utf8.RuneCountInString(st11Multiline)+strings.Count(st11Multiline, "\n")+64))
					st11FeedClearFixture(t, p, agent+"_"+fixture)
				}()
				rec := actOnUnsent(t, p, "clear")
				<-done
				if fixture == "faint_hint" {
					if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleared"`) || p.unsentMessage != nil {
						t.Fatalf("recorded faint suggestion was not empty: %d %s", rec.Code, rec.Body)
					}
				} else {
					if rec.Code != 200 || !strings.Contains(rec.Body.String(), "partly_cleared") || p.unsentMessage == nil {
						t.Fatalf("faint remnant lost receipt: %d %s", rec.Code, rec.Body)
					}
					if p.putBackInputCheck() == nil || p.keptPutBack() == nil {
						t.Fatal("faint remnant lifted agent guard")
					}
					p.deliveryMu.Lock()
					watching := p.checkPutBack()
					p.deliveryMu.Unlock()
					if !watching || p.unsentMessage == nil {
						t.Fatal("watcher dropped faint remnant")
					}
				}
				expectNoFrame(t, frames, "no retry after faint redraw")
			})
		}
	}
}

func TestPutBackClearSuggestionExemptionsAreVersionPinned(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		t.Run(agent, func(t *testing.T) {
			p, _, _ := st11ClearPane(t, agent == agentCodex, st11Multiline, st11Multiline, true)
			st11UseRealClearReader(p)
			st11FeedClearFixture(t, p, agent+"_faint_hint")
			plain, typed, _ := paneScreenFaintBlanked(p)
			for _, version := range []string{"", "2.1.310", "2.1.311", "0.166.0", "0.166.1", "v2.1.310"} {
				read, ok := readPutBackClearViews(plain, typed, agent == agentCodex, version)
				want := agent == agentClaude && version == "2.1.310" || agent == agentCodex && version == "0.166.0"
				if !ok || read.empty != want {
					t.Fatalf("agent=%s version=%q exemption=%v want=%v readable=%v", agent, version, read.empty, want, ok)
				}
			}
		})
	}
}

func TestPutBackClearUnknownVersionKeepsPendingSuggestion(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		for _, version := range []string{"", "2.1.311", "0.166.1"} {
			t.Run(agent+"/"+version, func(t *testing.T) {
				p, frames, _ := st11ClearPane(t, agent == agentCodex, st11Multiline, st11Multiline, true)
				st11UseRealClearReader(p)
				st11FeedClearFixture(t, p, agent+"_faint_hint")
				p.unsentMessage.clearPending = true
				setDetectedAgentVersions(t, version, version)
				if p.putBackInputCheck() == nil || p.keptPutBack() == nil {
					t.Fatal("unknown version inherited a suggestion exemption")
				}
				p.deliveryMu.Lock()
				watching := p.checkPutBack()
				p.deliveryMu.Unlock()
				if !watching || p.unsentMessage == nil {
					t.Fatal("watcher dropped receipt using another version's shape")
				}
				expectNoFrame(t, frames, "unknown-version guard sends no keys")
			})
		}
	}
}

func TestPutBackClearRechecksSuggestionVersionAfterWrite(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex} {
		t.Run(agent, func(t *testing.T) {
			codex := agent == agentCodex
			p, frames, _ := st11ClearPane(t, codex, st11Multiline, st11Multiline, false)
			st11UseRealClearReader(p)
			p.ensureGrid()
			p.subsMu.Lock()
			p.feedGridPayloadLocked([]byte("\x1b[H\x1b[2J"+strings.ReplaceAll(st11Box(codex, st11Multiline), "\n", "\r\n")), true)
			p.subsMu.Unlock()
			var current atomic.Value
			version := "2.1.310"
			if codex {
				version = "0.166.0"
			}
			current.Store(version)
			runningAgentVersion = func(*Pane, string) string { return current.Load().(string) }
			done := make(chan struct{})
			go func() {
				defer close(done)
				expectFrame(t, frames, "one version-gated write", strings.Repeat("\x7f", utf8.RuneCountInString(st11Multiline)+strings.Count(st11Multiline, "\n")+64))
				current.Store("9.9.9")
				st11FeedClearFixture(t, p, agent+"_faint_hint")
			}()
			rec := actOnUnsent(t, p, "clear")
			<-done
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "partly_cleared") || p.unsentMessage == nil || p.putBackInputCheck() == nil {
				t.Fatalf("post-write version change lost guard: %d %s", rec.Code, rec.Body)
			}
			expectNoFrame(t, frames, "no retry after a version change")
		})
	}
}
