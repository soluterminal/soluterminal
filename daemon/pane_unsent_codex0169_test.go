package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ST-291: Clear on a message put back in a Codex 0.169 input box. 0.169
// draws a hint row (or its warning chip) under the footer, so the box no
// longer ends at the screen's last row. These are real 0.169.0 sessions at
// 66 and 100 columns (testdata/unsent_clear/README.md): a queued message put
// back by Esc, then main's single-line Clear keys. 0.169 has no recorded
// full-clear keys, so Clear either clears one exactly readable line or
// refuses without a key; it never clears part of a box.

func st291Recording(t *testing.T, cols int) (menuScreenSession, []byte, map[string]int) {
	t.Helper()
	return st291RecordingNamed(t, "codex_0_169_0_"+map[int]string{66: "66x40", 100: "100x40"}[cols])
}

// st291RecordingNamed loads testdata/unsent_clear/<name>.
func st291RecordingNamed(t *testing.T, name string) (menuScreenSession, []byte, map[string]int) {
	t.Helper()
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
	at := map[string]int{}
	for _, step := range session.Steps {
		at[step.Name] = step.Bytes
	}
	return session, raw, at
}

func st291Feed(p *Pane, raw []byte, fresh bool) {
	p.ensureGrid()
	p.subsMu.Lock()
	if fresh {
		raw = append([]byte("\x1b[H\x1b[2J"), raw...)
	}
	p.feedGridPayloadLocked(raw, true)
	_ = p.gridEmu.DrainReplies()
	p.subsMu.Unlock()
}

func TestPutBackClearCodex0169Recorded(t *testing.T) {
	const wrapped = "ST291 alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau."
	for _, cols := range []int{66, 100} {
		session, raw, at := st291Recording(t, cols)
		for _, tc := range []struct {
			name, before, after, body string
			edited                    bool
		}{
			// One readable line: main's Ctrl+A Ctrl+K empties it (recorded).
			{"short", "put_back_short", "short_clear", "ST291 short queued", false},
			{"edited single line", "put_back_edited", "edited_clear", "ST291 edit me", true},
			// More than one row: refused before any key.
			{"wrapped", "put_back_wrapped", "", wrapped, false},
			{"two queued messages", "put_back_two", "", "ST291 second queued", false},
			{"edited multiline", "put_back_edited_multiline", "", "ST291 line one", true},
		} {
			t.Run(map[int]string{66: "66", 100: "100"}[cols]+"/"+tc.name, func(t *testing.T) {
				p, frames, _ := st11ClearPane(t, true, tc.body, "", tc.edited)
				setDetectedAgentVersions(t, "2.1.310", "0.169.0")
				st11UseRealClearReader(p)
				p.Cols, p.Rows = session.Cols, session.Rows
				st291Feed(p, raw[:at[tc.before]], true)
				t.Cleanup(p.shutdownGrid)

				// The full-clear reader (pinned to 0.166.0) cannot read a
				// 0.169 box: were it ever widened unrecorded, it refuses.
				plain, blanked, ok := putBackClearScreen(p)
				if !ok {
					t.Fatal("no grid")
				}
				if _, _, complete := readPutBackClearBox(plain, true); complete {
					t.Fatalf("0.166's box reader accepted a 0.169 screen:\n%s", plain)
				}
				line, one := composerOneLine(typedComposerScreen(plain, blanked, true), true)

				before := p.unsentMessage
				if tc.after == "" {
					if one {
						t.Fatalf("a multi-row box read as one line %q:\n%s", line, plain)
					}
					if rec := actOnUnsent(t, p, "clear"); rec.Code != 409 || p.unsentMessage != before {
						t.Fatalf("clear: %d %s", rec.Code, rec.Body)
					}
					expectNoFrame(t, frames, "refused without keys")
					return
				}
				want := tc.body
				if tc.edited {
					want += " person edit"
				}
				if !one || line != want {
					t.Fatalf("box line = %q (one=%v), want %q:\n%s", line, one, want, plain)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					expectFrame(t, frames, "main's single-line Clear keys", "\x01\x0b")
					// What Codex 0.169 drew after exactly those keys.
					st291Feed(p, raw[at[tc.before]:at[tc.after]], false)
				}()
				rec := actOnUnsent(t, p, "clear")
				<-done
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleared"`) || p.Snapshot().UnsentMessage != nil {
					t.Fatalf("clear: %d %s %+v", rec.Code, rec.Body, p.Snapshot().UnsentMessage)
				}
				expectNoFrame(t, frames, "one write")
			})
		}
	}
}

// After Ctrl+A Ctrl+K, the recorded 0.169 box is empty in the single-line
// cases, holds the other line when two messages came back, and is empty for
// the wrapped one (Codex kills the whole logical line). Clear still refuses
// that one: a box over more than one row is never cleared by these keys.
func TestPutBackClearCodex0169RecordedKeyEffects(t *testing.T) {
	for _, cols := range []int{66, 100} {
		session, raw, at := st291Recording(t, cols)
		for step, want := range map[string]string{
			"idle": "", "short_clear": "", "edited_clear": "", "wrapped_after_keys": "",
			"two_cleared": "", "end": "",
		} {
			p := menuScreenPane(t, session, raw[:at[step]], agentCodex)
			p.ensureGrid()
			plain, blanked, _ := putBackClearScreen(p)
			draft, known := composerDraft(typedComposerScreen(plain, blanked, true), true)
			if !known || draft != want {
				t.Fatalf("%d/%s: draft %q known=%v:\n%s", cols, step, draft, known, plain)
			}
			p.shutdownGrid()
		}
		p := menuScreenPane(t, session, raw[:at["two_after_keys"]], agentCodex)
		p.ensureGrid()
		plain, blanked, _ := putBackClearScreen(p)
		if draft, known := composerDraft(typedComposerScreen(plain, blanked, true), true); !known || draft == "" {
			t.Fatalf("%d: two queued messages left nothing after one line's keys:\n%s", cols, plain)
		}
		p.shutdownGrid()
	}
}

// #1236's 0.169 sessions draw the footer differently once text is typed (the
// working directory can wrap onto a second footer row, the warning chip sits
// on the hint row). Clear's single-line reader takes exactly the typed line,
// never a footer row; the full-clear reader still refuses the screen.
func TestPutBackClearCodex0169FooterShapes(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310", "0.169.0")
	for _, name := range []string{"codex_0169_empty_100x40", "codex_0169_warning_60x40", "codex_0169_daybreak_100x40"} {
		session, raw := loadMenuScreenSession(t, name)
		for _, step := range session.Steps {
			if step.Name != "typed" {
				continue
			}
			p := menuScreenPane(t, session, raw[:step.Bytes], agentCodex)
			p.ensureGrid()
			plain, blanked, _ := putBackClearScreen(p)
			typed := typedComposerScreen(plain, blanked, true)
			if line, one := composerOneLine(typed, true); !one || line != "hello draft text" {
				t.Fatalf("%s: line %q one=%v:\n%s", name, line, one, plain)
			}
			if !composerShowsExactLine(typed, true, "hello draft text") || composerShowsExactLine(typed, true, "hello draft") {
				t.Fatalf("%s: exact-line check is not exact:\n%s", name, plain)
			}
			if _, _, complete := readPutBackClearBox(plain, true); complete {
				t.Fatalf("%s: 0.166's box reader accepted a 0.169 screen", name)
			}
			p.shutdownGrid()
		}
	}
}
