package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ST-297 (tracker #72): on Codex 0.169.1, agent messages to a never-touched
// idle pane were held as "input box has text" behind the placeholder, and
// Send on the held message typed nothing. The reporter's Codex status line
// has no working directory and shows the context item as remaining
// ("Context 100% left · GPT-6-Astra medium"); the footer shape knew only
// "Context N% used" or a footer ending in a directory, so the footer and the
// hint row under it read as text in the box. Send now reads the box with the
// same reader under the writer lock, so it was refused (409, "your input box
// has text") and nothing was typed. These are real 0.169.1 recordings
// (testdata/menu_screens/README.md, testdata/unsent_clear).

// recordedCodexScreen feeds a recording up to step into a replay pane and
// returns a reader of its screen with the faint text blanked, as
// draftGuardScreen reads a live pane.
func recordedCodexScreen(t *testing.T, session, step string) func(*Pane) (string, string, bool) {
	t.Helper()
	s, raw := loadMenuScreenSession(t, session)
	end := -1
	for _, st := range s.Steps {
		if st.Name == step {
			end = st.Bytes
		}
	}
	if end < 0 {
		t.Fatalf("%s has no step %q", session, step)
	}
	rp := recordedPane(t, s, agentCodex)
	feedRecorded(rp, raw[:end])
	return func(*Pane) (string, string, bool) { return paneScreenFaintBlanked(rp) }
}

// Send now on a held message types it into the idle box the person sees as
// empty.
func TestHeldSendNowTypesIntoAnIdleCodex01691Box(t *testing.T) {
	for _, session := range []string{"codex_01691_left_100x40", "codex_01691_leftwarn_60x40"} {
		t.Run(session, func(t *testing.T) {
			p, frames, screen := newDraftGuardPane(t, "st297-send-now")
			a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
			read := recordedCodexScreen(t, session, "idle")
			prev := draftGuardScreen
			draftGuardScreen = read
			t.Cleanup(func() { draftGuardScreen = prev })
			plain, _, _ := read(p)
			screen.Store(plain)
			none := func() (int, int64) { return 0, 0 }
			if reason := p.draftGuardReason(true, false, time.Now(), none); reason != "" {
				t.Fatalf("idle box read as held (%s)\n%s", reason, plain)
			}

			// Held only because the person is typing, as a held message
			// is when they press Send now.
			p.noteHumanInput(time.Now())
			if got := <-agentDelivery(p, codexProbeMessage, "@lead"); got != deliveryHeld {
				t.Fatalf("outcome = %q, want held while the person types", got)
			}
			expectNoFrame(t, frames, "nothing typed while held")
			h := p.heldDeliveryByID(p.Snapshot().HeldMessages[0].ID)
			sent := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				a.actOnHeldDelivery(rec, p, h, "send")
				sent <- rec
			}()
			expectFrame(t, frames, "body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
			expectFrame(t, frames, "Enter", "\r")
			if rec := <-sent; rec.Code != http.StatusOK {
				t.Fatalf("send now = %d %s", rec.Code, rec.Body)
			}
			if status := heldStatusOf(p, codexProbeMessage); status != "sent" {
				t.Fatalf("status = %q, want sent", status)
			}
		})
	}
}

// Send now still never types into a draft with these status lines: the
// recorded box with "hello draft text" typed refuses it.
func TestHeldSendNowRefusesACodex01691Draft(t *testing.T) {
	for _, session := range []string{"codex_01691_left_100x40", "codex_01691_leftwarn_60x40"} {
		t.Run(session, func(t *testing.T) {
			p, frames, screen := newDraftGuardPane(t, "st297-send-draft")
			a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
			read := recordedCodexScreen(t, session, "typed")
			prev := draftGuardScreen
			draftGuardScreen = read
			t.Cleanup(func() { draftGuardScreen = prev })
			plain, _, _ := read(p)
			screen.Store(plain)
			if got := <-agentDelivery(p, codexProbeMessage, "@lead"); got != deliveryHeld {
				t.Fatalf("outcome = %q, want held behind the draft", got)
			}
			h := p.heldDeliveryByID(p.Snapshot().HeldMessages[0].ID)
			rec := httptest.NewRecorder()
			a.actOnHeldDelivery(rec, p, h, "send")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "input box has text") {
				t.Fatalf("send now into a draft = %d %s", rec.Code, rec.Body)
			}
			expectNoFrame(t, frames, "nothing typed into the draft")
		})
	}
}

// A message put back in a 0.169.1 box under the reporter's status line is
// read as exactly one line, so main's single-line Clear empties it (the keys
// and what Codex drew after them are recorded), and the box reads empty
// before and after.
func TestPutBackClearCodex01691StatusLineRecorded(t *testing.T) {
	const body = "ST297 short queued"
	for _, name := range []string{"codex_0_169_1_left_100x40", "codex_0_169_1_leftwarn_66x40"} {
		t.Run(name, func(t *testing.T) {
			session, raw, at := st291RecordingNamed(t, name)
			for _, step := range []string{"idle", "short_clear"} {
				q := menuScreenPane(t, session, raw[:at[step]], agentCodex)
				q.ensureGrid()
				plain, blanked, _ := putBackClearScreen(q)
				if draft, known := composerDraft(typedComposerScreen(plain, blanked, true), true); !known || draft != "" {
					t.Fatalf("%s: draft %q known=%v:\n%s", step, draft, known, plain)
				}
				q.shutdownGrid()
			}

			p, frames, _ := st11ClearPane(t, true, body, "", false)
			setDetectedAgentVersions(t, "2.1.310", "0.169.1")
			st11UseRealClearReader(p)
			p.Cols, p.Rows = session.Cols, session.Rows
			st291Feed(p, raw[:at["put_back_short"]], true)
			t.Cleanup(p.shutdownGrid)
			plain, blanked, ok := putBackClearScreen(p)
			if !ok {
				t.Fatal("no grid")
			}
			if line, one := composerOneLine(typedComposerScreen(plain, blanked, true), true); !one || line != body {
				t.Fatalf("box line = %q (one=%v), want %q:\n%s", line, one, body, plain)
			}
			none := func() (int, int64) { return 0, 0 }
			if reason := p.draftGuardReason(true, false, time.Now(), none); !strings.HasPrefix(reason, unsentInBoxPrefix) {
				t.Fatalf("reason = %q, want the unsent message in the box\n%s", reason, plain)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				expectFrame(t, frames, "main's single-line Clear keys", "\x01\x0b")
				st291Feed(p, raw[at["put_back_short"]:at["short_clear"]], false)
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
