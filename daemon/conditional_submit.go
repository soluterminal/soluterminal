package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const conditionalSubmitVersion = 1
const conditionalSubmitMaxTextBytes = 950
const conditionalSubmitTimeout = time.Second
const conditionalSubmitEnterReserve = 100 * time.Millisecond
const conditionalSubmitBodyMinimum = 25 * time.Millisecond

// A separate route is essential: legacy /inject deliberately ignores unknown
// JSON fields, so adding preconditions there would fail open on old daemons.
func (a *apiServer) registerConditionalSubmitRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pane-input/capabilities", a.conditionalSubmitCapabilities)
	mux.HandleFunc("GET /api/panes/{id}/conditional-submit", a.conditionalSubmitReady)
	mux.HandleFunc("POST /api/panes/{id}/conditional-submit", a.conditionalSubmitPane)
}

func (a *apiServer) conditionalSubmitCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"conditionalSubmitVersion": conditionalSubmitVersion,
		"supported":                conditionalSubmitSupported(), "terminalOnly": true,
		"providers": []string{"claude", "codex"}, "maxTextBytes": conditionalSubmitMaxTextBytes,
		"receipt":      "transport_only",
		"enterFraming": "split",
	})
}

// PID/CreatedAt identify the daemon pane's wrapper. Foreground PID + Linux
// start ticks additionally invalidate readiness after an in-shell model restart.
// SessionID is the daemon-observed conversation identity, never a credential.
type conditionalSubmitCondition struct {
	Pid                 int       `json:"expectedPid"`
	CreatedAt           time.Time `json:"expectedCreatedAt"`
	Provider            string    `json:"expectedProvider"`
	Cwd                 string    `json:"expectedCwd"`
	ForegroundPid       int       `json:"expectedForegroundPid"`
	ForegroundStartTime string    `json:"expectedForegroundStartTime"`
	SessionID           string    `json:"expectedSessionId"`
}

type conditionalSubmitRequest struct {
	conditionalSubmitCondition
	Text string `json:"text"`
}

type conditionalSubmitProcess struct {
	Provider, Cwd, StartTime string
	Pid                      int
}

type conditionalSubmitResult struct {
	status    int
	outcome   string
	reason    string
	condition conditionalSubmitCondition
	bytes     int
}

func conditionalNotSent(status int, reason string) conditionalSubmitResult {
	return conditionalSubmitResult{status: status, outcome: "not_sent", reason: reason}
}

func (result conditionalSubmitResult) respond(w http.ResponseWriter) {
	body := map[string]any{"conditionalSubmitVersion": conditionalSubmitVersion, "outcome": result.outcome}
	if result.reason != "" {
		body["reason"] = result.reason
	}
	if result.outcome == "ready" {
		body["condition"] = result.condition
	}
	if result.outcome == "transport_written" {
		body["bytes"] = result.bytes // logical text + Enter, excludes paste delimiters
	}
	writeJSON(w, result.status, body)
}

func (a *apiServer) conditionalSubmitReady(w http.ResponseWriter, r *http.Request) {
	a.runConditionalSubmit(r.Context(), r.PathValue("id"), nil).respond(w)
}

func (a *apiServer) conditionalSubmitPane(w http.ResponseWriter, r *http.Request) {
	var req conditionalSubmitRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		conditionalNotSent(http.StatusBadRequest, "invalid_request").respond(w)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || !req.valid() {
		conditionalNotSent(http.StatusBadRequest, "invalid_request").respond(w)
		return
	}
	a.runConditionalSubmit(r.Context(), r.PathValue("id"), &req).respond(w)
}

func (req conditionalSubmitRequest) valid() bool {
	ticks, err := strconv.ParseUint(req.ForegroundStartTime, 10, 64)
	if req.Pid <= 0 || req.CreatedAt.IsZero() || req.ForegroundPid <= 0 || err != nil || ticks == 0 ||
		(req.Provider != "claude" && req.Provider != "codex") || !filepath.IsAbs(req.Cwd) ||
		req.SessionID == "" || len(req.SessionID) > 256 || strings.TrimSpace(req.Text) == "" ||
		len(req.Text) > conditionalSubmitMaxTextBytes || !utf8.ValidString(req.Text) {
		return false
	}
	for _, ch := range req.Text {
		if unicode.IsControl(ch) || ch == '\u2028' || ch == '\u2029' {
			return false
		}
	}
	// This endpoint submits prose to a model, never TUI shortcut commands or
	// an unresolved file-picker token. Legacy raw input remains unchanged.
	text := strings.TrimSpace(req.Text)
	first, _ := utf8.DecodeRuneInString(text)
	fields := strings.Fields(text)
	if strings.ContainsRune("!/#?@", first) || strings.HasPrefix(fields[len(fields)-1], "@") {
		return false
	}
	return true
}

func sameConditionalCondition(a, b conditionalSubmitCondition) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	return a == b
}

func (a *apiServer) runConditionalSubmit(parent context.Context, id string, req *conditionalSubmitRequest) conditionalSubmitResult {
	if !conditionalSubmitSupported() {
		return conditionalNotSent(http.StatusNotImplemented, "unsupported_platform")
	}
	if a.mgr == nil {
		return conditionalNotSent(http.StatusServiceUnavailable, "pane_manager_unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, conditionalSubmitTimeout)
	defer cancel()
	if err := lockPaneReader(ctx, &a.mgr.mu); err != nil {
		return conditionalNotSent(http.StatusPreconditionFailed, "membership_busy")
	}
	p, ok := a.mgr.panes[id]
	a.mgr.mu.RUnlock()
	if !ok {
		return conditionalNotSent(http.StatusNotFound, "pane_not_found")
	}
	result := conditionalNotSent(http.StatusPreconditionFailed, "pane_changed")
	var commits []string
	// Lock order: Manager membership lease -> agentWriteMu -> short p.mu
	// snapshots/transport. The lease and input lock span BOTH PTY frames and
	// their paste gap. No Manager calls or callbacks inside the lease.
	_, leaseErr := a.mgr.withCurrentPaneContext(ctx, p, func() {
		if err := lockPaneWriter(ctx, &p.agentWriteMu); err != nil {
			result = conditionalNotSent(http.StatusPreconditionFailed, "input_busy")
			return
		}
		defer p.agentWriteMu.Unlock()
		if p.workerArchiving.Load() {
			result = conditionalNotSent(http.StatusConflict, "worker_archived")
			return
		}
		if ctx.Err() != nil {
			result = conditionalNotSent(http.StatusPreconditionFailed, "deadline_exceeded")
			return
		}
		var transcriptBefore string
		condition, reason := a.conditionalConditionLocked(ctx, p, &transcriptBefore)
		if reason != "" {
			result = conditionalNotSent(http.StatusPreconditionFailed, reason)
			return
		}
		if req != nil && !sameConditionalCondition(req.conditionalSubmitCondition, condition) {
			result = conditionalNotSent(http.StatusPreconditionFailed, "condition_changed")
			return
		}
		if ctx.Err() != nil {
			result = conditionalNotSent(http.StatusPreconditionFailed, "deadline_exceeded")
			return
		}
		if req == nil {
			result = conditionalSubmitResult{status: http.StatusOK, outcome: "ready", condition: condition}
			return
		}
		// p.mu prevents daemon-observed session/connection changes between the
		// final comparison and the frame write. It cannot freeze OS processes.
		if err := lockPaneWriter(ctx, &p.mu); err != nil {
			result = conditionalNotSent(http.StatusPreconditionFailed, "pane_busy")
			return
		}
		if reason := conditionalPaneReasonLocked(p); reason != "" || !conditionalPaneMatchesLocked(p, condition) {
			p.mu.Unlock()
			result = conditionalNotSent(http.StatusPreconditionFailed, "pane_changed")
			return
		}
		if ctx.Err() != nil {
			p.mu.Unlock()
			result = conditionalNotSent(http.StatusPreconditionFailed, "deadline_exceeded")
			return
		}
		conn := p.agentConn
		deadline, _ := ctx.Deadline()
		gap := conditionalSubmitGap(condition.Provider, EnterGap(condition.Provider == "claude"))
		bodyDeadline := deadline.Add(-gap - conditionalSubmitEnterReserve)
		if !time.Now().Add(conditionalSubmitBodyMinimum).Before(bodyDeadline) {
			p.mu.Unlock()
			result = conditionalNotSent(http.StatusPreconditionFailed, "deadline_exceeded")
			return
		}
		if err := conn.SetWriteDeadline(bodyDeadline); err != nil {
			p.mu.Unlock()
			result = conditionalNotSent(http.StatusPreconditionFailed, "transport_unavailable")
			return
		}
		defer conn.SetWriteDeadline(time.Time{})
		body := []byte(req.Text)
		if condition.Provider == "codex" {
			body = []byte("\x1b[200~" + req.Text + "\x1b[201~")
		}
		releaseInput, inputErr := p.codexLiveInputLease(body)
		if inputErr != nil {
			p.mu.Unlock()
			result = conditionalNotSent(http.StatusPreconditionFailed, "input_busy")
			return
		}
		defer releaseInput()
		err := WriteFrame(conditionalFullWriter{conn}, FrameData, body)
		p.mu.Unlock()
		if err != nil {
			// A partial frame poisons framing; never append another frame to it.
			_ = conn.Close()
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "transport_failed"}
			return
		}
		// A successfully written body is a draft, NOT a committed prompt or
		// model turn. Retain it even if cancellation/identity change blocks Enter.
		commits = p.observeSuccessfulInputLocked(body, true, menuInputNone)
		if err := conn.SetWriteDeadline(time.Time{}); err != nil {
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "submit_incomplete"}
			return
		}
		wait := a.conditionalSubmitEnterWait
		if wait == nil {
			wait = waitConditionalSubmitEnter
		}
		if err := wait(ctx, gap); err != nil {
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "submit_incomplete"}
			return
		}
		// The OS can exit/exec during the gap. Never send Enter into a changed
		// foreground (including a replacement shell). Body delivery already
		// happened, so all failures from here are ambiguous, never not_sent.
		probe := a.conditionalSubmitProbe
		if probe == nil {
			probe = probeConditionalSubmitProcess
		}
		process, err := probe(ctx, condition.Pid)
		if err != nil || process != (conditionalSubmitProcess{Provider: condition.Provider, Cwd: condition.Cwd,
			Pid: condition.ForegroundPid, StartTime: condition.ForegroundStartTime}) ||
			!conditionalPromptReady(ctx, p, condition.Provider, req.Text, &transcriptBefore, nil) {
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "submit_precondition_changed"}
			return
		}
		if err := lockPaneWriter(ctx, &p.mu); err != nil {
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "submit_incomplete"}
			return
		}
		if ctx.Err() != nil || conditionalPaneReasonLocked(p) != "" ||
			!conditionalPaneMatchesLocked(p, condition) || p.agentConn != conn {
			p.mu.Unlock()
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "submit_precondition_changed"}
			return
		}
		outputBeforeWrite := atomic.LoadInt64(&p.TotalStrippedChars)
		if err := conn.SetWriteDeadline(deadline); err != nil {
			p.mu.Unlock()
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "submit_incomplete"}
			return
		}
		err = WriteFrame(conditionalFullWriter{conn}, FrameData, []byte("\r"))
		p.mu.Unlock()
		if err != nil {
			_ = conn.Close() // partial framing remains poisoned
			result = conditionalSubmitResult{status: http.StatusBadGateway, outcome: "unknown", reason: "transport_failed"}
			return
		}
		commits = append(commits, p.observeSuccessfulInputLocked([]byte("\r"), true, menuInputNone)...)
		// A very fast response can arrive before WriteFrame returns. Preserve
		// that progress instead of latching busy at the post-response watermark.
		p.fleetTurnOutputWatermark.Store(outputBeforeWrite)
		p.fleetTurnAdmitted.Store(true) // transport admission does not rely on recorder parsing
		result = conditionalSubmitResult{status: http.StatusOK, outcome: "transport_written", bytes: len(req.Text) + 1}
	})
	if leaseErr != nil {
		return conditionalNotSent(http.StatusPreconditionFailed, "membership_busy")
	}
	// Includes onPromptsCommit (which re-enters Manager) and UI bookkeeping.
	// The callback's own current-pane guard rejects a subsequently replaced p.
	if len(commits) > 0 {
		p.finishSuccessfulInput(commits)
	}
	return result
}

func conditionalSubmitGap(provider string, configured time.Duration) time.Duration {
	minimum := EnterGapBracketedPaste
	if provider == "claude" {
		minimum = EnterGapClaude
	}
	if configured < minimum {
		return minimum
	}
	return configured
}

func waitConditionalSubmitEnter(ctx context.Context, gap time.Duration) error {
	timer := time.NewTimer(gap)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// WriteFrame's legacy semantics stay unchanged; the new contract additionally
// treats a short successful Write as an ambiguous transport failure.
type conditionalFullWriter struct{ io.Writer }

func (w conditionalFullWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return n, err
}

func conditionalPaneReasonLocked(p *Pane) string {
	if p.Kind != "" && p.Kind != "terminal" {
		return "native_excluded"
	}
	if p.Status != PaneRunning || p.userClosing || p.respawning.Load() || p.codexLiveMigrating.Load() || p.agentConn == nil {
		return "pane_unavailable"
	}
	if p.Pid <= 0 || p.CreatedAt.IsZero() || p.codexIdentityBlocked.Load() {
		return "identity_unavailable"
	}
	return ""
}

func conditionalSessionLocked(p *Pane, provider string) string {
	if provider == "codex" {
		return p.CodexThreadID
	}
	return p.ClaudeSessionID
}

func conditionalPaneMatchesLocked(p *Pane, c conditionalSubmitCondition) bool {
	return p.Pid == c.Pid && p.CreatedAt.Equal(c.CreatedAt) && p.Cwd == c.Cwd &&
		conditionalSessionLocked(p, c.Provider) == c.SessionID && effectiveAgentKind(p.Agent) == c.Provider
}

// Caller holds agentWriteMu and a membership lease. Deliberately does not use
// the DTO/process-discovery pipeline: that pipeline adopts sessions and fires
// callbacks. Readiness observation must neither persist nor acknowledge input.
func (a *apiServer) conditionalConditionLocked(ctx context.Context, p *Pane, captureTranscript *string) (conditionalSubmitCondition, string) {
	if err := lockPaneWriter(ctx, &p.mu); err != nil {
		return conditionalSubmitCondition{}, "pane_busy"
	}
	reason := conditionalPaneReasonLocked(p)
	pid, created, cwd := p.Pid, p.CreatedAt, p.Cwd
	p.mu.Unlock()
	if reason != "" {
		return conditionalSubmitCondition{}, reason
	}
	if p.buffer == nil {
		return conditionalSubmitCondition{}, "not_idle"
	}
	_, outputBefore, readable := p.buffer.TrySnapshotTail(0)
	if !readable {
		return conditionalSubmitCondition{}, "screen_busy"
	}
	probe := a.conditionalSubmitProbe
	if probe == nil {
		probe = probeConditionalSubmitProcess
	}
	process, err := probe(ctx, pid)
	if err != nil {
		return conditionalSubmitCondition{}, "foreground_unavailable"
	}
	canonical, err := filepath.EvalSymlinks(cwd)
	if err != nil || canonical != cwd || canonical != process.Cwd {
		return conditionalSubmitCondition{}, "cwd_changed"
	}
	condition := conditionalSubmitCondition{Pid: pid, CreatedAt: created, Provider: process.Provider, Cwd: canonical,
		ForegroundPid: process.Pid, ForegroundStartTime: process.StartTime}
	if err := lockPaneWriter(ctx, &p.mu); err != nil {
		return conditionalSubmitCondition{}, "pane_busy"
	}
	condition.SessionID = conditionalSessionLocked(p, process.Provider)
	reason = conditionalPaneReasonLocked(p)
	matches := conditionalPaneMatchesLocked(p, condition)
	p.mu.Unlock()
	if reason != "" || !matches || condition.SessionID == "" {
		return conditionalSubmitCondition{}, "session_unavailable"
	}
	if pending, _ := p.promptRec.draft(); pending != 0 || p.promptRec.inStringControl || p.composerNonEmptyNow(time.Now()) {
		return conditionalSubmitCondition{}, "composer_not_empty"
	}
	if !conditionalPromptReady(ctx, p, process.Provider, "", nil, captureTranscript) {
		return conditionalSubmitCondition{}, "not_idle"
	}
	// Close changes during the screen/identity checks with a fresh process
	// sample. Exit/exec AFTER this sample remains possible and is not receipt.
	confirmed, err := probe(ctx, pid)
	if err != nil || confirmed != process {
		return conditionalSubmitCondition{}, "foreground_changed"
	}
	_, outputAfter, readable := p.buffer.TrySnapshotTail(0)
	if !readable || outputAfter != outputBefore {
		return conditionalSubmitCondition{}, "prompt_changed"
	}
	return condition, ""
}

func conditionalPromptIdle(ctx context.Context, p *Pane, provider string) bool {
	return conditionalPromptReady(ctx, p, provider, "", nil, nil)
}

// During a held submission's gap, the visible composer may contain our OWN
// recorded draft. Recheck busy/approval state without pretending it is empty.
func conditionalPromptReady(ctx context.Context, p *Pane, provider string, expectedDraft string, expectedTranscript, captureTranscript *string) bool {
	if p.buffer == nil || p.hasNativePending() {
		return false
	}
	if p.fleetTurnAdmitted.Load() && atomic.LoadInt64(&p.TotalStrippedChars) <= p.fleetTurnOutputWatermark.Load() {
		return false // a preceding submission has not even produced output yet
	}
	if err := lockPaneReader(ctx, &p.subsMu); err != nil {
		return false
	}
	defer p.subsMu.RUnlock()
	var screen string
	var ok bool
	if p.replayEmu != nil {
		snapshot, available := p.replayEmu.TrySnapshot()
		if !available {
			return false
		}
		screen, ok = snapshotVisibleText(snapshot)
	}
	if !ok {
		tail, _, available := p.buffer.TrySnapshotTail(16384)
		if !available {
			return false
		}
		screen = stripANSI(tail)
	}
	if isGridChoicePromptText(screen, provider == "codex") {
		return false
	}
	if provider == "codex" {
		if !looksLikeCodexPrompt(screen) || codexActiveTurnRE.MatchString(screen) {
			return false
		}
		if expectedDraft == "" {
			return codexLiveMigrationScreenSafe(screen)
		}
		return true
	}
	transcript, draft, known := conditionalClaudeComposer(screen)
	if !known || (draft != "" && (expectedDraft == "" || draft != conditionalDraftText(expectedDraft))) {
		return false
	}
	if ok && expectedDraft != "" && draft != conditionalDraftText(expectedDraft) {
		return false // rendered body echo must be positive, not assumed from an empty composer
	}
	lastDone := lastMatchEnd(claudeTurnDoneRE, transcript)
	if spin := lastMatchEnd(claudeActiveSpinnerRE, transcript); spin >= 0 && spin > lastDone {
		return false
	}
	if expectedTranscript != nil {
		// Our echo refreshes LastActivityMs. Compare actual transcript instead:
		// old hook/output text without a completion marker is not a NEW turn,
		// while newly streamed content still forbids Enter. Scrolling may drop
		// only a leading prefix, never add/change the retained transcript.
		if !conditionalTranscriptSuffix(*expectedTranscript, transcript) {
			return false
		}
	} else if ok && time.Since(time.UnixMilli(atomic.LoadInt64(&p.LastActivityMs))) < claudeSpinnerRecency && claudeContentAfter(transcript, lastDone) {
		return false
	}
	if captureTranscript != nil {
		*captureTranscript = transcript
	}
	return looksLikeClaudePrompt(screen)
}

func conditionalTranscriptSuffix(before, after string) bool {
	content := func(text string) []string {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !claudeChromeLineRE.MatchString(line) {
				lines = append(lines, line)
			}
		}
		return lines
	}
	old, current := content(before), content(after)
	if len(current) > len(old) || (len(old) > 0 && len(current) == 0) {
		return false
	}
	for i, line := range current {
		if line != old[len(old)-len(current)+i] {
			return false
		}
	}
	return true
}

// Isolate the currently rendered Claude composer from transcript streaming.
// Wrapped copies of OUR exact draft are chrome, not a new assistant response.
// Unknown layouts or an unrecognized footer fail closed.
func conditionalClaudeComposer(screen string) (transcript, draft string, known bool) {
	lines := strings.Split(screen, "\n")
	composer, legacy := -1, false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, "❭") || strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, "▶") {
			composer, legacy = i, false
		} else if inputBoxLineRE.MatchString(line) {
			composer, legacy = i, true
		}
	}
	if composer < 0 {
		return "", "", false
	}
	text := strings.TrimSpace(lines[composer])
	if legacy {
		loc := inputBoxLineRE.FindStringIndex(text)
		if loc == nil {
			return "", "", false
		}
		text = strings.TrimSuffix(strings.TrimSpace(text[loc[1]:]), "│")
	} else {
		_, size := utf8.DecodeRuneInString(text)
		text = text[size:]
	}
	parts := []string{text}
	for i, line := range lines[composer+1:] {
		if (!legacy && newPromptFooterRE.MatchString(line)) || (legacy && boxBottomRE.MatchString(line)) {
			for _, below := range lines[composer+i+2:] {
				if strings.TrimSpace(below) != "" && !claudeChromeLineRE.MatchString(below) {
					return "", "", false
				}
			}
			return strings.Join(lines[:composer], "\n") + "\n", conditionalDraftText(strings.Join(parts, "")), true
		}
		if strings.Trim(strings.TrimSpace(line), "─") == "" {
			continue
		}
		if legacy {
			line = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "│"), "│")
		}
		parts = append(parts, line)
	}
	return "", "", false
}

func conditionalDraftText(text string) string {
	return strings.Map(func(ch rune) rune {
		if unicode.IsSpace(ch) {
			return -1
		}
		return ch
	}, text)
}

var errConditionalProcessUnavailable = errors.New("conditional input foreground unavailable")
