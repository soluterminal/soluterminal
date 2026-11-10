package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// In-app feedback (bug / idea / question) capture. The web client sends only
// intent (type/title/body + which pane); the daemon gathers the trustworthy
// context — versions, platform, the active pane's recent output, the log-ring
// tail — REDACTS every byte locally, and forwards a normalized submission to
// the cloud, which holds the GitHub token and files the issue.
//
// The daemon is the redaction choke point: it is the only node that ever sees
// raw pane bytes or raw logs, so nothing unredacted can leave the machine. The
// outbound shape mirrors cloud.FeedbackSubmission by JSON tag (the daemon and
// cloud are separate modules — same arrangement as crashReportPayload).
//
// What we'll send: POST /api/feedback/preview returns exactly the context,
// pane text and logs a submit would carry, already redacted, so the form can
// show them before anything leaves. The form sends the previewed text back
// (the user may have deleted lines); the daemon redacts it again (idempotent)
// and forwards that instead of capturing anew. It never needs cloud
// credentials, so signed-out and unpaired computers get the same preview for
// Copy report / Email it.

const (
	feedbackMaxTitle         = 200
	feedbackMaxBody          = 8 << 10
	feedbackMaxSnapshotLines = 40
	feedbackMaxSnapshotBytes = 16 << 10

	feedbackCaptureNone     = "none"
	feedbackCapturePaneText = "pane_text"
	feedbackCaptureLogsOnly = "logs_only"
)

type feedbackReq struct {
	Type   string `json:"type"` // "bug" | "idea" | "question"
	Title  string `json:"title"`
	Body   string `json:"body"`
	PaneID string `json:"paneId"` // optional; the pane whose text to send
	// IncludeSnapshot sends the pane's recent text. Default false: pane text
	// and logs go only when asked.
	IncludeSnapshot *bool `json:"includeSnapshot"`
	// IncludeLogs sends recent app logs. Nil follows IncludeSnapshot: older
	// clients had one box for both.
	IncludeLogs *bool `json:"includeLogs"`
	// Snapshot and Logs are the previewed text the user saw and may have
	// trimmed. Nil captures now (older clients). Ignored unless included.
	Snapshot      *string `json:"snapshot"`
	Logs          *string `json:"logs"`
	ClientVersion string  `json:"clientVersion"` // low-trust; label/meta only
	// Screenshots are not forwarded until private storage exists (ST-103 D3):
	// any attachments a client sends are dropped here, for every account.
}

// feedbackSubmission mirrors cloud.FeedbackSubmission (same JSON shape).
type feedbackSubmission struct {
	Schema   int                   `json:"schema"`
	Channel  string                `json:"channel"`
	Type     string                `json:"type"`
	Title    string                `json:"title"`
	Body     string                `json:"body"`
	Snapshot string                `json:"snapshot"`
	Logs     string                `json:"logs"`
	Context  feedbackSubmitContext `json:"context"`
}

// feedbackSubmitContext is the "App and system details" row of the form's
// preview, field for field. The computer's name is not sent: the machine ID
// identifies it.
type feedbackSubmitContext struct {
	AppVersion        string `json:"appVersion"`
	DaemonVersion     string `json:"daemonVersion"`
	ClientVersion     string `json:"clientVersion"`
	ShellVersion      string `json:"shellVersion"`
	ShellPlatform     string `json:"shellPlatform"`
	BackendID         string `json:"backendID"`
	Platform          string `json:"platform"`
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	MachineID         string `json:"machineId"`
	PaneID            string `json:"paneId"`
	CaptureKind       string `json:"captureKind"`
	SnapshotRequested *bool  `json:"snapshotRequested,omitempty"`
	SnapshotCaptured  *bool  `json:"snapshotCaptured,omitempty"`
	LogsRequested     *bool  `json:"logsRequested,omitempty"`
	LogsCaptured      *bool  `json:"logsCaptured,omitempty"`
	SubmittedAt       string `json:"submittedAt"`
}

var feedbackAllowedTypes = map[string]bool{"bug": true, "idea": true, "question": true}

var feedbackHTTPClient = &http.Client{Timeout: 20 * time.Second}

// feedbackCapture is what a report carries besides what the user typed.
type feedbackCapture struct {
	snapshot, logs                   string
	snapshotRequested, logsRequested bool
}

// feedbackGather resolves the pane text and logs for a preview or a submit:
// the previewed text when the client sends it, otherwise a fresh capture.
// Either way the result is redacted and within the size limits.
func (a *apiServer) feedbackGather(req feedbackReq) feedbackCapture {
	includePane := req.IncludeSnapshot != nil && *req.IncludeSnapshot
	includeLogs := includePane
	if req.IncludeLogs != nil {
		includeLogs = *req.IncludeLogs
	}
	c := feedbackCapture{snapshotRequested: includePane && req.PaneID != "", logsRequested: includeLogs}
	if c.snapshotRequested {
		if req.Snapshot != nil {
			c.snapshot = feedbackBoundSnapshot(*req.Snapshot)
		} else {
			c.snapshot = a.feedbackPaneSnapshot(req.PaneID)
		}
	}
	if c.logsRequested {
		logs := recentLogContext() // already redacted at capture (logring.go)
		if req.Logs != nil {
			logs = *req.Logs
		}
		c.logs = feedbackBoundLogs(logs)
	}
	return c
}

func (a *apiServer) feedbackContext(clientVersion, paneID string, c feedbackCapture) feedbackSubmitContext {
	captureKind := feedbackCaptureNone
	if c.snapshotRequested {
		captureKind = feedbackCapturePaneText
	} else if c.logsRequested {
		captureKind = feedbackCaptureLogsOnly
	}
	// The pane ID goes only with its text.
	if !c.snapshotRequested {
		paneID = ""
	}
	return feedbackSubmitContext{
		AppVersion:        a.ver,
		DaemonVersion:     a.ver,
		ClientVersion:     strings.TrimSpace(clientVersion),
		ShellVersion:      a.liveShellVersion(),
		ShellPlatform:     a.shellPlatform,
		BackendID:         a.backendID,
		Platform:          a.shellPlatform,
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		MachineID:         a.feedbackMachineID(),
		PaneID:            paneID,
		CaptureKind:       captureKind,
		SnapshotRequested: feedbackBoolPtr(c.snapshotRequested),
		SnapshotCaptured:  feedbackBoolPtr(c.snapshot != ""),
		LogsRequested:     feedbackBoolPtr(c.logsRequested),
		LogsCaptured:      feedbackBoolPtr(c.logs != ""),
	}
}

// feedbackTitle is the report's title: the one typed, else the first
// non-empty line of the body.
func feedbackTitle(title, body string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	for _, line := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func (a *apiServer) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	req.Type = strings.TrimSpace(strings.ToLower(req.Type))
	req.Body = strings.TrimSpace(req.Body)
	req.Title = feedbackTitle(req.Title, req.Body)
	if !feedbackAllowedTypes[req.Type] {
		http.Error(w, "type must be bug, idea, or question", http.StatusBadRequest)
		return
	}
	if req.Body == "" {
		http.Error(w, "description required", http.StatusBadRequest)
		return
	}

	capture := a.feedbackGather(req)
	sub := feedbackSubmission{
		Schema:   1,
		Channel:  "inapp",
		Type:     req.Type,
		Title:    redactFeedback(feedbackClamp(req.Title, feedbackMaxTitle)),
		Body:     redactFeedback(feedbackClamp(req.Body, feedbackMaxBody)),
		Snapshot: capture.snapshot,
		Logs:     capture.logs,
		Context:  a.feedbackContext(req.ClientVersion, req.PaneID, capture),
	}
	sub.Context.SubmittedAt = time.Now().UTC().Format(time.RFC3339)

	base, token := a.cloudCreds()
	if base == "" || token == "" {
		http.Error(w, "feedback needs a cloud connection — pair this machine first", http.StatusServiceUnavailable)
		return
	}

	payload, err := json.Marshal(sub)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), "POST", base+"/api/feedback", bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := feedbackHTTPClient.Do(httpReq)
	if err != nil {
		http.Error(w, "could not reach the feedback service — please try again", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))

	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		http.Error(w, "daily feedback limit reached — please try again later", http.StatusTooManyRequests)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "could not file feedback right now — please try again", http.StatusBadGateway)
		return
	}

	var out struct {
		OK     bool   `json:"ok"`
		Number int    `json:"number"`
		URL    string `json:"url"`
		// Queued: the cloud kept the report to file once GitHub accepts it.
		Queued bool `json:"queued"`
	}
	_ = json.Unmarshal(respBody, &out)
	respJSON := map[string]any{"ok": true, "number": out.Number, "url": out.URL}
	if out.Queued {
		respJSON["queued"] = true
	}
	writeJSON(w, http.StatusOK, respJSON)
}

// handleFeedbackPreview returns what a submit with the same choices would
// send, redacted: the details, the pane text and logs, and the typed title and
// body. Local only (no cloud call), so it works signed out and unpaired.
func (a *apiServer) handleFeedbackPreview(w http.ResponseWriter, r *http.Request) {
	var req feedbackReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	body := strings.TrimSpace(req.Body)
	capture := a.feedbackGather(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"title":    redactFeedback(feedbackClamp(feedbackTitle(req.Title, body), feedbackMaxTitle)),
		"body":     redactFeedback(feedbackClamp(body, feedbackMaxBody)),
		"snapshot": capture.snapshot,
		"logs":     capture.logs,
		"context":  a.feedbackContext(req.ClientVersion, req.PaneID, capture),
	})
}

// claimMachineIdentity reads this computer's ID and (redacted) name from its
// claim. The local support bundle shows both; feedback sends only the ID.
func (a *apiServer) claimMachineIdentity() (machineID, machineName string) {
	if a == nil || strings.TrimSpace(a.dataDir) == "" {
		return "", ""
	}
	c, err := LoadClaim(a.dataDir)
	if err != nil || c == nil {
		return "", ""
	}
	return strings.TrimSpace(c.MachineID), redactFeedback(strings.TrimSpace(c.MachineName))
}

func (a *apiServer) feedbackMachineID() string {
	id, _ := a.claimMachineIdentity()
	return id
}

func feedbackBoolPtr(v bool) *bool { return &v }

// feedbackPaneSnapshot returns the redacted, ANSI-stripped tail of a pane's
// ring buffer (last feedbackMaxSnapshotLines lines, capped to
// feedbackMaxSnapshotBytes). Empty if the pane is gone — a missing snapshot
// never fails the report. Mirrors the paneDebugTail accessor chain.
func (a *apiServer) feedbackPaneSnapshot(paneID string) string {
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return ""
	}
	return feedbackBoundSnapshot(stripANSI(p.buffer.Snapshot()))
}

// feedbackBoundSnapshot keeps the last feedbackMaxSnapshotLines lines,
// redacted, then at most feedbackMaxSnapshotBytes. Redacting before the byte
// cut means a cut can't split a secret past its pattern, and a second pass
// over the previewed text changes nothing.
func feedbackBoundSnapshot(s string) string {
	return feedbackTailBytes(redactFeedback(feedbackTailLines(s, feedbackMaxSnapshotLines)), feedbackMaxSnapshotBytes)
}

// feedbackBoundLogs is the log-tail twin: redacted, then the newest
// crashRecentLogBytes.
func feedbackBoundLogs(s string) string {
	return feedbackTailBytes(redactFeedback(strings.TrimRight(s, "\n")), crashRecentLogBytes)
}

// feedbackTailBytes keeps the newest max bytes of s, starting on a whole
// UTF-8 character so a re-sent preview round-trips byte for byte.
func feedbackTailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// feedbackTailLines returns the last n lines of s (trailing blank lines
// dropped first), oldest -> newest.
func feedbackTailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// feedbackClamp trims s to at most max bytes without splitting a UTF-8 rune: if
// the byte cut lands mid-rune, the incomplete trailing rune is dropped.
func feedbackClamp(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := s[:max]
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRuneInString(b); r == utf8.RuneError && size <= 1 {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// --- Reporter loop proxies (cloud feedback_reports.go) ---------------------
//
// The web client never talks to the cloud directly; these mirror the filing
// proxy above: machine Bearer auth, pass-through JSON, calm error text.

// handleFeedbackMine proxies GET /api/feedback/mine — the reporter's filed
// reports with live pipeline status.
func (a *apiServer) handleFeedbackMine(w http.ResponseWriter, r *http.Request) {
	base, token := a.cloudCreds()
	if base == "" || token == "" {
		http.Error(w, "feedback needs a cloud connection — pair this machine first", http.StatusServiceUnavailable)
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), "GET", base+"/api/feedback/mine", nil)
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := feedbackHTTPClient.Do(httpReq)
	if err != nil {
		http.Error(w, "could not reach the feedback service — please try again", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "could not load your reports right now", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleFeedbackReply proxies POST /api/feedback/reply — appends the
// reporter's follow-up to their own report's body (which unparks a
// needs-info report via the feedback repo's edited-retriage).
func (a *apiServer) handleFeedbackReply(w http.ResponseWriter, r *http.Request) {
	base, token := a.cloudCreds()
	if base == "" || token == "" {
		http.Error(w, "feedback needs a cloud connection — pair this machine first", http.StatusServiceUnavailable)
		return
	}
	// The reply is redacted here, on the device, like the report itself; the
	// cloud's pass is a backstop.
	var reply struct {
		Number int64  `json:"number"`
		Text   string `json:"text"`
	}
	if !decodeJSON(w, r, &reply, 64<<10) {
		return
	}
	reply.Text = redactFeedback(reply.Text)
	payload, err := json.Marshal(reply)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), "POST", base+"/api/feedback/reply", bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := feedbackHTTPClient.Do(httpReq)
	if err != nil {
		http.Error(w, "could not reach the feedback service — please try again", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "could not save the reply right now", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleFeedbackInbox proxies GET /api/feedback/inbox — the reporter's
// feedback-pipeline events (needs-info / fixed) with read state. Pure-DB on
// the cloud side, so the web client can poll it for the inbox chip without
// hammering GitHub.
func (a *apiServer) handleFeedbackInbox(w http.ResponseWriter, r *http.Request) {
	base, token := a.cloudCreds()
	if base == "" || token == "" {
		// Calm empty set, NOT an error: the inbox chip polls this ambiently,
		// and an unpaired machine simply has no feedback loop to show.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[]}`))
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), "GET", base+"/api/feedback/inbox", nil)
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := feedbackHTTPClient.Do(httpReq)
	if err != nil {
		http.Error(w, "could not reach the feedback service — please try again", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "could not load feedback events right now", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleFeedbackInboxRead proxies POST /api/feedback/inbox/read — stamps a
// feedback event read so the unread badge clears on every device.
func (a *apiServer) handleFeedbackInboxRead(w http.ResponseWriter, r *http.Request) {
	base, token := a.cloudCreds()
	if base == "" || token == "" {
		http.Error(w, "feedback needs a cloud connection — pair this machine first", http.StatusServiceUnavailable)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), "POST", base+"/api/feedback/inbox/read", bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := feedbackHTTPClient.Do(httpReq)
	if err != nil {
		http.Error(w, "could not reach the feedback service — please try again", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "could not mark it read right now", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleFeedbackThread proxies GET /api/feedback/thread?number=N — one
// report's conversation (filed → pipeline events → replies) for the inbox's
// thread view.
func (a *apiServer) handleFeedbackThread(w http.ResponseWriter, r *http.Request) {
	base, token := a.cloudCreds()
	if base == "" || token == "" {
		http.Error(w, "feedback needs a cloud connection — pair this machine first", http.StatusServiceUnavailable)
		return
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), "GET",
		base+"/api/feedback/thread?number="+url.QueryEscape(r.URL.Query().Get("number")), nil)
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := feedbackHTTPClient.Do(httpReq)
	if err != nil {
		http.Error(w, "could not reach the feedback service — please try again", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "could not load the report thread right now", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleFeedbackStatus tells the form up front whether feedback can be sent
// from this computer, so a user without a connected account sees how to reach
// us before typing instead of a 503 after. reason is "signed_out" (no account
// on this install) or "unpaired" (an account, but this computer is not
// connected to the cloud).
func (a *apiServer) handleFeedbackStatus(w http.ResponseWriter, _ *http.Request) {
	if base, token := a.cloudCreds(); base != "" && token != "" {
		writeJSON(w, http.StatusOK, map[string]any{"available": true})
		return
	}
	reason := "signed_out"
	if a.entitlements != nil && a.entitlements.cachedTier() != tierNone {
		reason = "unpaired"
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": reason})
}
