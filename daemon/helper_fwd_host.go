package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	hp "soluterminal/helperproto"
)

// Host side of M3 `fwd` (ST-253, helper-protocol-v1 "Proposed for M3 fwd",
// @secure ruling 8.6a). A helper-opened `fwd` stream carries one HTTP request
// from a process in a WSL pane, attested by the helper (helper_attest.go).
//
// The host accepts it only when every one of these holds, and otherwise
// closes the stream with nothing dispatched:
//   - fwd is enabled in this build (helperFwdEnabled; off until the PR that
//     turns it on carries @secure's marker, 8.6a.5);
//   - the session is the host's own child session and its hello passed;
//   - attest.agent is an agent THIS session spawned or listed, and it backs
//     the pane named in the stream's first line, which is a live pane;
//   - the per-session (32 open) and per-agent (50/s) limits allow it;
//   - the request is POST /mcp (body up to 4 MiB) or POST /api/hooks/<name>
//     (up to 1 MiB), headers up to 16 KiB, with a declared length.
//
// The request is then rebuilt from scratch, never copied: one Content-Type
// header, Host set to the loopback name, a RemoteAddr that is no IP address
// (so any handler that requires loopback refuses it), and a context key that
// classifyCaller reads before anything else. No header the caller wrote can
// reach a handler or change its classification. It is dispatched in process
// to the API handler, never through a listener.

// helperFwdEnabled is the build's default for fwd. Turning it on is its own
// PR with @secure's marker (8.6a.5).
const helperFwdEnabled = false

const helperFwdRemoteAddr = "helper-fwd"

// helperFwdCaller is what the fwd path vouches for on a dispatched request.
type helperFwdCaller struct {
	PaneID  string
	AgentID string
	Distro  string
}

type helperFwdCallerKey struct{}

// helperFwdCallerOf returns the fwd caller a request was dispatched for. Only
// helperFwdHost.dispatch sets the key; no header or transport can.
func helperFwdCallerOf(r *http.Request) (helperFwdCaller, bool) {
	c, ok := r.Context().Value(helperFwdCallerKey{}).(helperFwdCaller)
	return c, ok && c.PaneID != ""
}

// helperFwdHost handles fwd streams for one distro's helper session.
type helperFwdHost struct {
	// enabled is helperFwdEnabled, copied at construction: there is no
	// runtime switch (8.6a.5). Only tests build a host with it set.
	enabled bool
	distro  string
	handler http.Handler // the API handler (apiServer.apiHandler(mux))
	// backedBy reports whether paneID is a live pane whose CURRENT backing
	// agent on this helper is agentID. A pane replaced under the same id
	// (respawn, resume) has a new agent, so the old one no longer acts for
	// it. Supplied by the pane backend that owns helper-backed panes.
	backedBy func(paneID, agentID string) bool
	now      func() time.Time
	logf     func(string, ...any)

	// admitWait bounds how long a stream waits for its agent and pane to be
	// registered: the helper starts the CLI before the host has the spawn
	// reply and has published the pane, so a first MCP initialize or
	// SessionStart hook can arrive a moment early.
	admitWait time.Duration

	slots  chan struct{}
	rateMu sync.Mutex
	rate   map[string]*helperFwdWindow
}

func newHelperFwdHost(distro string, handler http.Handler, backedBy func(paneID, agentID string) bool) *helperFwdHost {
	return &helperFwdHost{
		enabled:   helperFwdEnabled,
		distro:    distro,
		handler:   handler,
		backedBy:  backedBy,
		now:       time.Now,
		logf:      log.Printf,
		admitWait: helperFwdAdmitWait,
		slots:     make(chan struct{}, helperFwdMaxOpen),
		rate:      map[string]*helperFwdWindow{},
	}
}

// streamHandlers is what the host session registers: nothing while fwd is
// off, so a helper-opened fwd stream stays a protocol break (ruling 8.6).
func (h *helperFwdHost) streamHandlers() map[string]hp.HostStreamHandler {
	if !h.enabled {
		return nil
	}
	return map[string]hp.HostStreamHandler{hp.KindFwd: h.handle}
}

func (h *helperFwdHost) allow(agentID string) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	now := h.now()
	w := h.rate[agentID]
	if w == nil || now.Sub(w.start) >= time.Second || now.Before(w.start) {
		for id, old := range h.rate {
			if now.Sub(old.start) >= time.Second {
				delete(h.rate, id)
			}
		}
		w = &helperFwdWindow{start: now}
		h.rate[agentID] = w
	}
	if w.n >= helperFwdPerAgentRate {
		return false
	}
	w.n++
	return true
}

// handle is the KindFwd stream handler. It always closes c, which also
// releases the session's stream slot.
func (h *helperFwdHost) handle(s *hp.HostSession, sh hp.StreamHeader, c net.Conn) {
	defer c.Close()
	caller, status, why := h.admitWaiting(s, sh)
	if status != 0 {
		h.logf("fwd: %s refused pane=%q: %s", h.distro, sh.Pane, why)
		h.respond(c, status, []byte(why))
		return
	}
	select {
	case h.slots <- struct{}{}:
	default:
		h.respond(c, http.StatusTooManyRequests, []byte("busy"))
		return
	}
	defer func() { <-h.slots }()
	if !h.allow(caller.AgentID) {
		h.respond(c, http.StatusTooManyRequests, []byte("busy"))
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(helperFwdHookTimeout))
	path, body, status, why := readHelperFwdRequest(c)
	if status != 0 {
		h.logf("fwd: %s pane=%s request refused: %s", h.distro, caller.PaneID, why)
		h.respond(c, status, []byte(why))
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	// The pane may have been reaped or replaced while the request was read:
	// admit again, and dispatch only for the same pane and agent.
	if again, status, why := h.admit(s, sh); status != 0 || again != caller {
		if why == "" {
			why = "pane changed"
		}
		h.logf("fwd: %s pane=%s refused at dispatch: %s", h.distro, caller.PaneID, why)
		h.respond(c, http.StatusForbidden, []byte(why))
		return
	}
	timeout := helperFwdHookTimeout
	if path == helperFwdMCPPath {
		timeout = helperFwdMCPTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The helper closing the stream (its caller left) ends the call.
	go func() {
		var b [1]byte
		if _, err := c.Read(b[:]); err != nil {
			cancel()
		}
	}()
	code, out := h.dispatch(ctx, caller, path, body)
	_ = c.SetWriteDeadline(time.Now().Add(mcpSocketWriteTimeout))
	h.respond(c, code, out)
}

// Refusals that a stream arriving just ahead of its pane's registration
// would see; admitWaiting retries only these.
const (
	helperFwdNotYetAgent = "agent does not back this pane on this session"
	helperFwdNotYetPane  = "pane is not live or this agent no longer backs it"
	// With the helper's helperAttestRegisterWait (1.5 s) this stays inside
	// hook-forward's 4 s timeout, itself inside Claude's 5 s hook timeout.
	helperFwdAdmitWait = 2 * time.Second
)

// admitWaiting is admit, retried for up to admitWait while the only problem
// is that the agent or pane isn't registered yet. Anything else (fwd off, no
// hello, a bad stream or attestation) is refused at once.
func (h *helperFwdHost) admitWaiting(s *hp.HostSession, sh hp.StreamHeader) (helperFwdCaller, int, string) {
	deadline := time.Now().Add(h.admitWait)
	for {
		caller, status, why := h.admit(s, sh)
		if status == 0 || (why != helperFwdNotYetAgent && why != helperFwdNotYetPane) || !time.Now().Before(deadline) {
			return caller, status, why
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// admit applies the session and attestation checks.
func (h *helperFwdHost) admit(s *hp.HostSession, sh hp.StreamHeader) (helperFwdCaller, int, string) {
	if !h.enabled {
		return helperFwdCaller{}, http.StatusForbidden, "fwd is not enabled"
	}
	if s == nil || !s.HelloPassed() {
		return helperFwdCaller{}, http.StatusForbidden, "session hello has not passed"
	}
	if sh.S != hp.KindFwd || sh.Kind != "http" {
		return helperFwdCaller{}, http.StatusBadRequest, "not an http fwd stream"
	}
	if !hp.ValidPaneID(sh.Pane) {
		return helperFwdCaller{}, http.StatusBadRequest, "invalid pane id"
	}
	var att hp.FwdAttest
	dec := json.NewDecoder(bytes.NewReader(sh.Attest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&att); err != nil || !hp.ValidAgentID(att.Agent) || att.PID <= 0 || att.Start == 0 {
		return helperFwdCaller{}, http.StatusBadRequest, "invalid attestation"
	}
	paneID, ok := s.PaneForAgent(att.Agent)
	if !ok {
		return helperFwdCaller{}, http.StatusForbidden, helperFwdNotYetAgent
	}
	if paneID != sh.Pane {
		// A definite mismatch, not a registration still on its way.
		return helperFwdCaller{}, http.StatusForbidden, "agent backs another pane"
	}
	if h.backedBy == nil || !h.backedBy(paneID, att.Agent) {
		return helperFwdCaller{}, http.StatusForbidden, helperFwdNotYetPane
	}
	return helperFwdCaller{PaneID: paneID, AgentID: att.Agent, Distro: h.distro}, 0, ""
}

// readHelperFwdRequest reads one request from the stream within the limits.
// The header block is read raw and measured before anything parses it.
func readHelperFwdRequest(c io.Reader) (path string, body []byte, status int, why string) {
	br := bufio.NewReaderSize(c, 4096)
	var head bytes.Buffer
	// continued: the last piece filled the buffer, so this one is the rest of
	// the same line and can never be the blank line that ends the headers.
	continued := false
	for {
		piece, err := br.ReadSlice('\n')
		if head.Len()+len(piece) > helperFwdMaxHookHeader {
			return "", nil, http.StatusRequestHeaderFieldsTooLarge, "headers too large"
		}
		head.Write(piece)
		if err == bufio.ErrBufferFull {
			continued = true
			continue
		}
		if err != nil {
			return "", nil, http.StatusBadRequest, "bad request"
		}
		if !continued && (string(piece) == "\r\n" || string(piece) == "\n") {
			break
		}
		continued = false
	}
	req, err := http.ReadRequest(bufio.NewReader(io.MultiReader(&head, io.LimitReader(br, int64(helperFwdMaxMCPRequest)+1))))
	if err != nil {
		return "", nil, http.StatusBadRequest, "bad request"
	}
	defer req.Body.Close()
	if req.Method != http.MethodPost || req.URL.RawQuery != "" || !helperFwdPathOK(req.URL.Path) || req.URL.Path != req.RequestURI {
		return "", nil, http.StatusNotFound, "path not forwarded"
	}
	if len(req.TransferEncoding) != 0 || req.ContentLength < 0 {
		return "", nil, http.StatusLengthRequired, "length required"
	}
	if req.ContentLength > int64(helperFwdMaxBody(req.URL.Path)) {
		return "", nil, http.StatusRequestEntityTooLarge, "request too large"
	}
	body, err = io.ReadAll(io.LimitReader(req.Body, req.ContentLength+1))
	if err != nil || int64(len(body)) != req.ContentLength {
		return "", nil, http.StatusBadRequest, "short body"
	}
	return req.URL.Path, body, 0, ""
}

// dispatch rebuilds the request and runs it through the API handler in
// process. Nothing from the caller's request but its path and body is used.
func (h *helperFwdHost) dispatch(ctx context.Context, caller helperFwdCaller, path string, body []byte) (int, []byte) {
	ctx = context.WithValue(ctx, helperFwdCallerKey{}, caller)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1"+path, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header = http.Header{"Content-Type": {"application/json"}}
	req.Host = "127.0.0.1"
	req.RemoteAddr = helperFwdRemoteAddr
	req.RequestURI = path
	rec := newHelperFwdRecorder(helperFwdMaxResponse)
	if !h.serveRecovered(rec, req) {
		return http.StatusInternalServerError, []byte("internal error")
	}
	if rec.overflow {
		return http.StatusInsufficientStorage, []byte("response too large")
	}
	return rec.status(), rec.buf.Bytes()
}

// serveRecovered runs the handler, turning a panic into a failed request as
// net/http would: this runs on a helper session goroutine, where an
// unrecovered panic would end the daemon.
func (h *helperFwdHost) serveRecovered(w http.ResponseWriter, r *http.Request) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				ok = false
				return
			}
			h.logf("fwd: %s panic serving %s: %v\n%s", h.distro, r.URL.Path, v, debug.Stack())
			ok = false
		}
	}()
	h.handler.ServeHTTP(w, r)
	return true
}

// respond writes one HTTP/1.1 response with a JSON or plain body.
func (h *helperFwdHost) respond(c net.Conn, status int, body []byte) {
	ct := "text/plain; charset=utf-8"
	if status/100 == 2 {
		ct = "application/json"
	}
	resp := &http.Response{
		StatusCode: status, ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {ct}, "Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	if err := resp.Write(c); err != nil {
		h.logf("fwd: %s write response: %v", h.distro, err)
	}
}

// helperFwdRecorder is a bounded in-memory ResponseWriter.
type helperFwdRecorder struct {
	hdr      http.Header
	code     int
	buf      bytes.Buffer
	max      int
	overflow bool
}

func newHelperFwdRecorder(max int) *helperFwdRecorder {
	return &helperFwdRecorder{hdr: http.Header{}, max: max}
}

func (r *helperFwdRecorder) Header() http.Header { return r.hdr }

func (r *helperFwdRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *helperFwdRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	if r.buf.Len()+len(b) > r.max {
		r.overflow = true
		return 0, fmt.Errorf("fwd response larger than %d bytes", r.max)
	}
	return r.buf.Write(b)
}

func (r *helperFwdRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// helperFwdKernelSupported reports whether a distro kernel (hello's
// `kernel`, `uname -r`) has SO_PEERPIDFD (Linux 6.5+), without which the
// helper attests nothing (#1330). @secure: report it and show the notice.
func helperFwdKernelSupported(kernel string) bool {
	major, minor, ok := parseKernelVersion(kernel)
	return ok && (major > 6 || major == 6 && minor >= 5)
}

// observeHello records what the helper's hello said about its kernel. Called
// by the host's helper supervisor after each hello (W2); it returns the
// notice to show when attestation can't work in that distro, else "".
func (h *helperFwdHost) observeHello(res hp.HelloResult) string {
	if helperFwdKernelSupported(res.Kernel) {
		return ""
	}
	h.logf("fwd: %s kernel %q lacks SO_PEERPIDFD (Linux 6.5+): WSL panes there stay unverified callers. %s", h.distro, res.Kernel, helperFwdKernelNotice)
	return helperFwdKernelNotice
}

// helperFwdKernelNotice is the plain notice for a distro whose kernel is
// too old for verified WSL panes (@secure on #1330).
const helperFwdKernelNotice = "Update WSL (wsl --update) for verified WSL panes"

func parseKernelVersion(s string) (major, minor int, ok bool) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minorDigits := parts[1]
	for i, r := range minorDigits {
		if r < '0' || r > '9' {
			minorDigits = minorDigits[:i]
			break
		}
	}
	minor, err2 := strconv.Atoi(minorDigits)
	return major, minor, err1 == nil && err2 == nil
}
