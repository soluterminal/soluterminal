package main

// AI pane names, on the user's own Claude Code (pane_title_cli.go: `claude -p`,
// Haiku, no tools). Two ways in:
//
//   POST /api/panes/{id}/name/suggest — a suggested name for the user to keep
//                                       or edit; nothing is changed.
//   Auto-naming (Settings, off by default) — a Claude or Codex pane opened
//       after it was turned on, still called "claude"/"codex", is named once
//       after its first prompt. A name the user or an agent gave is never
//       replaced.
//
// The model sees only the agent's own conversation (its Claude transcript or
// Codex rollout) and the purpose it was started for: never the terminal
// screen or the typed-line history, which can hold credentials.
//
//   GET/PUT /api/settings/pane-naming {autoName}

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	paneNamingSettingsFile = "pane-naming.json"
	// autoNamesPerHour bounds how many CLI runs auto-naming can start.
	autoNamesPerHour = 30
)

var errNothingToName = errors.New("nothing to name yet: a name is suggested from a Claude or Codex conversation in this pane")

type paneNamingRecord struct {
	AutoName bool `json:"autoName"`
	// EnabledAt (unix ms) is when auto-naming was last turned on: only panes
	// opened after it are named, never the ones already open.
	EnabledAt int64 `json:"enabledAt,omitempty"`
}

type paneNaming struct {
	mu     sync.Mutex
	path   string
	record paneNamingRecord
	// tried holds the panes auto-naming has queued or run (once each).
	tried sync.Map
	// queue feeds the one worker that runs the CLI, oldest first; pending
	// counts queued and running names. window/count is the hourly cap.
	queue       chan *Pane
	startWorker sync.Once
	pending     atomic.Int32
	windowStart time.Time
	count       int
}

// autoNameQueueSize bounds the backlog; a pane that finds it full is not
// marked tried, so its next prompt queues it again.
const autoNameQueueSize = 64

func newPaneNaming(dataDir string) *paneNaming {
	n := &paneNaming{path: filepath.Join(dataDir, paneNamingSettingsFile), queue: make(chan *Pane, autoNameQueueSize)}
	if b, err := os.ReadFile(n.path); err == nil {
		_ = json.Unmarshal(b, &n.record)
	}
	return n
}

func (n *paneNaming) enabledAt() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.record.EnabledAt
}

func (n *paneNaming) autoName() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.record.AutoName
}

// setAutoName saves first and only then takes effect: a failed save must not
// leave naming running while the app shows it off.
func (n *paneNaming) setAutoName(on bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	next := n.record
	if on && !next.AutoName {
		next.EnabledAt = time.Now().UnixMilli()
	}
	next.AutoName = on
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(n.path, b, 0o600); err != nil {
		return err
	}
	n.record = next
	return nil
}

// admit takes a slot under the hourly cap.
func (n *paneNaming) admit(now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if now.Sub(n.windowStart) >= time.Hour {
		n.windowStart, n.count = now, 0
	}
	if n.count >= autoNamesPerHour {
		return false
	}
	n.count++
	return true
}

// ownTranscript is the pane's Claude transcript only when it is certainly this
// pane's: the path its session pinned, or its own session id. Not
// transcriptForPane's newest-in-the-folder fallback, which for a pane that
// isn't running Claude (or hasn't started yet) finds another session's chat.
func ownTranscript(p *Pane) string {
	snap := p.Snapshot()
	if tp := strings.TrimSpace(snap.ClaudeTranscriptPath); tp != "" && !isSubagentTranscript(tp) {
		if fi, err := os.Stat(tp); err == nil && !fi.IsDir() {
			return tp
		}
	}
	if id := strings.TrimSpace(snap.ClaudeSessionID); id != "" {
		for _, path := range listTranscripts(claudeConfigDirs()) {
			if !isSubagentTranscript(path) && sessionIDForTranscript(path) == id {
				return path
			}
		}
	}
	return ""
}

// paneNamingText is what the model reads: what the pane was started for, and
// the agent's own conversation — the Claude transcript or the Codex rollout of
// this pane's own session, newest turns kept. Never the typed-line history
// (LastPrompts): in a shell pane that later runs an agent it holds every shell
// command typed, secrets included. Empty when there is no such conversation.
func (a *apiServer) paneNamingText(p *Pane) string {
	snap := p.Snapshot()
	conv := ""
	if path := ownTranscript(p); path != "" {
		if buf, err := tailReadBounded(path, convTailWindowBytes); err == nil {
			turns, _ := projectConversation(buf, 40)
			conv = buildTitleConvText(turns, titleConvMaxChars)
		}
	} else if path := codexRolloutPath(snap.CodexThreadID); snap.CodexThreadID != "" && path != "" {
		if buf, err := tailReadBounded(path, convTailWindowBytes); err == nil {
			conv = newestLinesWithin(codexConversationLines(buf), titleConvMaxChars)
		}
	}
	if strings.TrimSpace(conv) == "" {
		return ""
	}
	if snap.Origin != nil && strings.TrimSpace(snap.Origin.Purpose) != "" {
		return "Started for: " + strings.TrimSpace(snap.Origin.Purpose) + "\n" + conv
	}
	return conv
}

// codexConversationLines flattens a Codex rollout into "User: …" and
// "Assistant: …" lines, skipping the context Codex injects itself.
func codexConversationLines(buf []byte) []string {
	var out []string
	for _, line := range bytes.Split(buf, []byte("\n")) {
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &env) != nil || env.Type != "response_item" || env.Payload.Type != "message" {
			continue
		}
		who := map[string]string{"user": "User: ", "assistant": "Assistant: "}[env.Payload.Role]
		if who == "" {
			continue
		}
		for _, c := range env.Payload.Content {
			t := strings.Join(strings.Fields(c.Text), " ")
			if t != "" && !(env.Payload.Role == "user" && codexInjectedUserText(c.Text)) {
				out = append(out, who+t)
			}
		}
	}
	return out
}

// namingLineMax caps one line of the model's input, so a long reply can't
// crowd out the prompts around it.
const namingLineMax = 600

// clampUTF8 cuts s to at most max bytes on a rune boundary.
func clampUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// newestLinesWithin keeps the newest lines, each clamped to namingLineMax,
// that fit in max bytes, in order.
func newestLinesWithin(lines []string, max int) string {
	kept := []string{}
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := clampUTF8(lines[i], min(namingLineMax, max))
		if total+len(line)+1 > max {
			break
		}
		total += len(line) + 1
		kept = append(kept, line)
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

func (a *apiServer) suggestPaneName(ctx context.Context, p *Pane) (string, error) {
	text := a.paneNamingText(p)
	if strings.TrimSpace(text) == "" {
		return "", errNothingToName
	}
	ctx, cancel := context.WithTimeout(ctx, titleCLITimeoutSeconds*time.Second)
	defer cancel()
	return generateTitleViaCLI(ctx, text)
}

// handleSuggestPaneName answers POST /api/panes/{id}/name/suggest.
func (a *apiServer) handleSuggestPaneName(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name, err := a.suggestPaneName(r.Context(), p)
	switch {
	case errors.Is(err, errNothingToName):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, errTitleCLIUnavailable):
		http.Error(w, "Claude Code isn't installed or signed in, so it can't suggest a name", http.StatusBadGateway)
	case err != nil:
		// Most often a signed-out Claude Code: the run exits with an error.
		http.Error(w, "Claude Code couldn't suggest a name — check that it is signed in", http.StatusBadGateway)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"name": name})
	}
}

// autoNameEligible: auto-naming is on, the pane runs Claude or Codex, it has a
// prompt, its name is still the generic one, and it has not been tried.
func (a *apiServer) autoNameEligible(p *Pane) bool {
	if a.naming == nil || !a.naming.autoName() {
		return false
	}
	snap := p.Snapshot()
	if snap.Status == PaneExited || len(snap.LastPrompts) == 0 || !paneTitleIsGeneric(snap.Title) {
		return false
	}
	// Only panes opened after auto-naming was turned on: enabling it (or a
	// daemon restart) must not rename the panes already there.
	if snap.CreatedAt.UnixMilli() < a.naming.enabledAt() {
		return false
	}
	// A pane with an agent conversation of its own, from recorded fields
	// only — no process walk, since the prompt hook also runs on a sweep.
	if snap.Agent != agentCodex && agentFromCmd(snap.Cmd) == "" &&
		snap.ClaudeSessionID == "" && snap.ClaudeTranscriptPath == "" && snap.CodexThreadID == "" {
		return false
	}
	_, tried := a.naming.tried.Load(snap.ID)
	return !tried
}

// maybeAutoNamePane runs from the prompt-commit hook. It returns at once: the
// pane is queued for the one background worker, so panes prompted together
// are all named, one after another.
func (a *apiServer) maybeAutoNamePane(p *Pane) {
	if !a.autoNameEligible(p) {
		return
	}
	if _, already := a.naming.tried.LoadOrStore(p.ID, true); already {
		return
	}
	a.naming.startWorker.Do(func() { go a.autoNameWorker() })
	a.naming.pending.Add(1)
	select {
	case a.naming.queue <- p:
	default:
		a.naming.pending.Add(-1)
		a.naming.tried.Delete(p.ID)
	}
}

func (a *apiServer) autoNameWorker() {
	for p := range a.naming.queue {
		a.autoNameOne(p)
		a.pruneAutoNameTried()
		a.naming.pending.Add(-1)
	}
}

func (a *apiServer) autoNameOne(p *Pane) {
	// Re-check: the setting, the name or the pane may have changed while queued.
	if cur, ok := a.mgr.Get(p.ID); !ok || cur != p || !a.naming.autoName() || !paneTitleIsGeneric(p.Snapshot().Title) {
		a.naming.tried.Delete(p.ID)
		return
	}
	// At the first prompt the conversation often isn't on disk yet (a Codex
	// thread is found by a later sweep, a Claude transcript is written after
	// the turn). Not tried, then: the prompt hook's sweep queues it again.
	text := a.paneNamingText(p)
	if strings.TrimSpace(text) == "" {
		a.naming.tried.Delete(p.ID)
		return
	}
	if !a.naming.admit(time.Now()) {
		a.naming.tried.Delete(p.ID) // over the hourly cap: a later prompt retries
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), titleCLITimeoutSeconds*time.Second)
	name, err := generateTitleViaCLI(ctx, text)
	cancel()
	if err != nil {
		// A failed run (Claude Code missing, signed out, timed out) is not
		// retried for this pane: the user can still press Suggest.
		log.Printf("auto-name pane %s: %v", p.ID, err)
		return
	}
	a.applyAutoName(p, name)
}

// pruneTried forgets panes that have closed, so tried doesn't grow for the
// daemon's whole life.
func (a *apiServer) pruneAutoNameTried() {
	a.naming.tried.Range(func(k, _ any) bool {
		if id, _ := k.(string); id != "" {
			if _, ok := a.mgr.Get(id); !ok {
				a.naming.tried.Delete(id)
			}
		}
		return true
	})
}

// applyAutoName renames only while the name is still generic, checked and set
// under the pane's lock, so a rename while the model ran wins. It mutates and
// persists inside withCurrentPane: a restart or mode switch that replaced the
// pane meanwhile must not have its record overwritten by this old pointer.
func (a *apiServer) applyAutoName(p *Pane, name string) {
	if strings.TrimSpace(name) == "" || !a.naming.autoName() {
		return
	}
	a.mgr.withCurrentPane(p, func() {
		prev, ok := p.renameIfGeneric(name)
		if !ok || a.panes == nil {
			return
		}
		if err := a.panes.Put(p); err != nil {
			p.renameIfTitle(name, prev)
			log.Printf("auto-name pane %s: persist: %v", p.ID, err)
		}
	})
}

// renameIfGeneric sets the title only while it is still a generic one
// ("claude", "codex", a shell), atomically under the pane lock.
func (p *Pane) renameIfGeneric(title string) (prev string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !paneTitleIsGeneric(p.Title) {
		return p.Title, false
	}
	prev, p.Title = p.Title, strings.TrimSpace(title)
	return prev, true
}

// renameIfTitle undoes renameIfGeneric unless someone renamed it since.
func (p *Pane) renameIfTitle(expect, title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Title == expect {
		p.Title = title
	}
}

// paneAutoNamer is called by the prompt-commit hook in main.go, which panes
// carry from spawn — before the API server exists.
var paneAutoNamer atomic.Pointer[func(*Pane)]

func notifyPaneAutoNamer(p *Pane) {
	if f := paneAutoNamer.Load(); f != nil {
		(*f)(p)
	}
}

func (a *apiServer) handleGetPaneNaming(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, paneNamingRecord{AutoName: a.naming.autoName()})
}

// handlePutPaneNaming is local-only: auto-naming sends prompts to the user's
// Claude Code, a choice made on the computer itself.
func (a *apiServer) handlePutPaneNaming(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "this setting can only be changed on the computer itself", http.StatusForbidden)
		return
	}
	if a.naming == nil {
		http.Error(w, "pane naming is unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		AutoName *bool `json:"autoName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.AutoName == nil {
		http.Error(w, `body must be {"autoName": true|false}`, http.StatusBadRequest)
		return
	}
	if err := a.naming.setAutoName(*body.AutoName); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, paneNamingRecord{AutoName: *body.AutoName})
}
