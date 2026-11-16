package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// terminal_one_tap.go: phone one-tap Approve for a TERMINAL Claude Code pane's
// permission menu (ST-265; design agreed with @secure on 14 Nov).
//
// A terminal pane has no structured approval channel: the daemon sees the
// rendered menu and nothing else. So one-tap is offered only when every one
// of these holds, and the push otherwise carries no promptId (the phone
// shows Review):
//
//  1. The pane's agent is Claude Code (no Codex, Muse or harness descendant),
//     and the live state is a choice prompt read from the rendered grid
//     (never the raw tail, which keeps an answered menu).
//  2. The grid's dialog is a "Bash command … Do you want to proceed?" menu as
//     captured on Claude Code 2.1.322. The extractor also recognises file
//     create/edit dialogs, but those go to Review: they show the path, not the
//     content (the phone's glanceable rule says the same). The strict
//     extractor takes only the dialog box; its focus marker must be on option
//     1, "Yes" (allow once); the command must be one line, untruncated.
//  3. Widening is judged on the REAL tool input, not the screen (a
//     dangerouslyDisableSandbox call renders exactly like a plain Bash
//     prompt): exactly one PreToolUse hook record, posted by the pane's own
//     top-level claude process, must match the dialog's command or path, and
//     approvalWidening must find nothing in it or in the visible command.
//
// Every Codex approval goes to Review: each one is a request to leave the
// sandbox, and its Reason line is model text.
//
// The phone verifies the same SOLU-APPROVAL-V1 signature as native approvals
// (approval_signature.go), with promptId in the requestId slot, and answers
// through handlePaneChoicePromptResolve, which re-checks all of the above
// against the live screen before writing.

// --- PreToolUse hook records -------------------------------------------------

// termPreTool is one PreToolUse hook call for a terminal pane: the tool and
// its full input as Claude Code sent it, before the permission menu painted.
type termPreTool struct {
	ToolUseID string
	Tool      string
	Input     json.RawMessage
	At        time.Time
	// PID is the claude process that sent it; it must still be the pane's
	// top-level claude when the record is used.
	PID int
}

const (
	termPreToolMax      = 32
	termPreToolTTL      = 30 * time.Minute
	termPreToolInputMax = 64 << 10
	// termSummaryMax bounds the signed summary: it rides FCM's 4 KB data
	// payload twice (body and data).
	termSummaryMax = 200
)

type termPreToolSet struct {
	mu      sync.Mutex
	records []termPreTool
}

var termPreTools sync.Map // pane id -> *termPreToolSet

func termPreToolsFor(paneID string) *termPreToolSet {
	v, _ := termPreTools.LoadOrStore(paneID, &termPreToolSet{})
	return v.(*termPreToolSet)
}

// recordTermPreTool keeps a PreToolUse record for paneID. The caller has
// already checked that the hook came from the pane's own claude process.
func recordTermPreTool(paneID string, rec termPreTool) {
	s := termPreToolsFor(paneID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	if len(rec.Input) > termPreToolInputMax {
		rec.Input = nil // too large to keep: can never match, so Review
	}
	// A retry of the very same call (same tool, byte-identical input)
	// replaces the older record: the two can't disagree about widening, and
	// a denied call (no PostToolUse) would otherwise make every retry
	// ambiguous for the record TTL.
	out := s.records[:0]
	for _, r := range s.records {
		if (rec.ToolUseID != "" && r.ToolUseID == rec.ToolUseID) || (r.Tool == rec.Tool && rec.Input != nil && string(r.Input) == string(rec.Input)) {
			continue
		}
		out = append(out, r)
	}
	s.records = out
	if len(s.records) >= termPreToolMax {
		s.records = append(s.records[:0], s.records[1:]...)
	}
	s.records = append(s.records, rec)
}

// forgetTermPreTool drops the record a PostToolUse answers.
func forgetTermPreTool(paneID, toolUseID string) {
	if toolUseID == "" {
		return
	}
	v, ok := termPreTools.Load(paneID)
	if !ok {
		return
	}
	s := v.(*termPreToolSet)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.records[:0]
	for _, r := range s.records {
		if r.ToolUseID != toolUseID {
			out = append(out, r)
		}
	}
	s.records = out
}

func (s *termPreToolSet) pruneLocked(now time.Time) {
	out := s.records[:0]
	for _, r := range s.records {
		if now.Sub(r.At) < termPreToolTTL {
			out = append(out, r)
		}
	}
	s.records = out
}

func termPreToolSnapshot(paneID string) []termPreTool {
	v, ok := termPreTools.Load(paneID)
	if !ok {
		return nil
	}
	s := v.(*termPreToolSet)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	return append([]termPreTool(nil), s.records...)
}

// Process lookups, swappable in tests.
var (
	termProcComm   = readComm
	termProcParent = procParent
)

// isPaneClaudeMainProcess reports whether pid is the pane's own top-level
// claude process: a "claude" process that is the pane's process or descends
// from it with no other "claude" between them. A tool's child (an agent's
// curl, a nested `claude -p`, a process renamed to claude) always has the real
// claude above it and is refused, so it can't post a fake record (@secure).
func isPaneClaudeMainProcess(panePID, pid int) bool {
	if panePID <= 0 || pid <= 0 || termProcComm(pid) != "claude" {
		return false
	}
	for cur, hops := pid, 0; hops < 64; hops++ {
		if cur == panePID {
			return true
		}
		parent := termProcParent(cur)
		if parent <= 1 || parent == cur {
			return false
		}
		if termProcComm(parent) == "claude" {
			// Another claude above pid: pid is a descendant of the agent,
			// not the agent. (The pane's own process may itself be claude,
			// in which case pid == panePID matched above.)
			return false
		}
		cur = parent
	}
	return false
}

// --- Dialog extraction -------------------------------------------------------

// termApprovalView is a rendered Claude Code permission dialog the extractor
// recognises. Only Kind "bash" is answerable by one-tap; write and edit are
// recognised so they can be declined precisely.
type termApprovalView struct {
	Kind    string // "bash", "write" or "edit"
	Command string // bash: the single command line
	Path    string // write/edit: the path the dialog names
	Summary string // the dialog box text, exactly as signed and shown
}

var (
	termTopRule     = regexp.MustCompile(`^─{20,}$`)
	termDotRule     = regexp.MustCompile(`^╌{20,}$`)
	termQProceed    = regexp.MustCompile(`^ Do you want to proceed\?$`)
	termQCreate     = regexp.MustCompile(`^ Do you want to create (.+)\?$`)
	termQEdit       = regexp.MustCompile(`^ Do you want to make this edit to (.+)\?$`)
	termFocusedYes  = regexp.MustCompile(`^ ❯ 1\. Yes$`)
	termOptionLine  = regexp.MustCompile(`^   ?(❯ )?[0-9]+\. `)
	termFooter      = regexp.MustCompile(`^ Esc to cancel · Tab to amend$`)
	termOtherFocus  = regexp.MustCompile(`❯ [2-9]\.`)
	termTruncations = []string{"…", "..."}
)

// extractClaudeApprovalDialog finds the one permission dialog on a rendered
// grid and checks it is a kind one-tap may answer. Any doubt returns false.
func extractClaudeApprovalDialog(grid string) (termApprovalView, string) {
	lines := strings.Split(grid, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	// The question line: exactly one on screen.
	q, kind, named := -1, "", ""
	for i, l := range lines {
		var k, n string
		switch {
		case termQProceed.MatchString(l):
			k = "bash"
		case termQCreate.MatchString(l):
			k, n = "write", termQCreate.FindStringSubmatch(l)[1]
		case termQEdit.MatchString(l):
			k, n = "edit", termQEdit.FindStringSubmatch(l)[1]
		default:
			continue
		}
		if q >= 0 {
			return termApprovalView{}, "more than one permission dialog on screen"
		}
		q, kind, named = i, k, n
	}
	if q < 0 {
		return termApprovalView{}, "no recognised permission dialog"
	}
	// @secure: the focus marker must be on option 1, "Yes" (allow once).
	if q+1 >= len(lines) || !termFocusedYes.MatchString(lines[q+1]) {
		return termApprovalView{}, "focus is not on option 1 (Yes)"
	}
	// Options run to the footer; no other option may hold the focus.
	end := -1
	for i := q + 2; i < len(lines); i++ {
		l := lines[i]
		if termFooter.MatchString(l) {
			end = i
			break
		}
		if termOtherFocus.MatchString(l) {
			return termApprovalView{}, "focus is not on option 1 (Yes)"
		}
		if l != "" && !termOptionLine.MatchString(l) && !strings.HasPrefix(l, "      ") {
			return termApprovalView{}, "unrecognised dialog options"
		}
	}
	if end < 0 {
		return termApprovalView{}, "dialog footer not on screen"
	}
	// The box starts after the last full-width rule above the question.
	top := -1
	for i := q - 1; i >= 0; i-- {
		if termTopRule.MatchString(lines[i]) {
			top = i
			break
		}
	}
	if top < 0 {
		return termApprovalView{}, "dialog top not on screen"
	}
	box := lines[top+1 : end+1]
	// Header: the first non-empty line of the box.
	h := 0
	for h < len(box) && box[h] == "" {
		h++
	}
	if h >= len(box) {
		return termApprovalView{}, "empty dialog"
	}
	header := strings.TrimSpace(box[h])
	// The dotted rules frame the command or the file content.
	var dots []int
	for i, l := range box {
		if termDotRule.MatchString(l) {
			dots = append(dots, i)
		}
	}
	v := termApprovalView{Kind: kind}
	switch kind {
	case "bash":
		if header != "Bash command" {
			return termApprovalView{}, "not a Bash command dialog"
		}
		if len(dots) != 2 {
			return termApprovalView{}, "command block not framed"
		}
		var cmd []string
		for _, l := range box[dots[0]+1 : dots[1]] {
			if strings.TrimSpace(l) != "" {
				cmd = append(cmd, strings.TrimSpace(l))
			}
		}
		// @secure: a command must be one line and not cut.
		if len(cmd) != 1 {
			return termApprovalView{}, "command is not a single line"
		}
		v.Command = cmd[0]
	case "write", "edit":
		want := map[string]string{"write": "Create file", "edit": "Edit file"}[kind]
		if header != want {
			return termApprovalView{}, "not a " + want + " dialog"
		}
		if h+1 >= len(box) || strings.TrimSpace(box[h+1]) == "" {
			return termApprovalView{}, "no file path"
		}
		v.Path = strings.TrimSpace(box[h+1])
		if base := v.Path[strings.LastIndex(v.Path, "/")+1:]; base != named {
			return termApprovalView{}, "dialog path and question disagree"
		}
	}
	for _, t := range termTruncations {
		if strings.Contains(v.Command, t) || strings.Contains(v.Path, t) {
			return termApprovalView{}, "command or path is truncated"
		}
	}
	// Control, bidi-override and zero-width characters can make what the
	// phone displays differ from what runs.
	for _, r := range v.Command + v.Path {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return termApprovalView{}, "command or path has invisible or control characters"
		}
	}
	// The summary the phone shows, hashes and signs: lines of the dialog
	// itself, without the model-written description, the option list (one-tap
	// only ever answers option 1, Yes) or file content.
	subject := v.Command
	if subject == "" {
		subject = v.Path
	}
	v.Summary = header + "\n" + subject + "\n" + strings.TrimSpace(lines[q])
	if len(v.Summary) > termSummaryMax {
		return termApprovalView{}, "dialog too long for a notification"
	}
	return v, ""
}

// --- Matching the hook record --------------------------------------------------

// matchTermPreTool returns the single hook record that is the dialog's tool
// call, or a reason.
func matchTermPreTool(v termApprovalView, records []termPreTool) (termPreTool, string) {
	var found []termPreTool
	for _, r := range records {
		var in map[string]any
		if json.Unmarshal(r.Input, &in) != nil {
			continue
		}
		switch v.Kind {
		case "bash":
			cmd, _ := in["command"].(string)
			if r.Tool == "Bash" && strings.TrimSpace(cmd) == v.Command {
				found = append(found, r)
			}
		case "write", "edit":
			tools := map[string]bool{"write": r.Tool == "Write", "edit": r.Tool == "Edit" || r.Tool == "MultiEdit"}
			fp, _ := in["file_path"].(string)
			fp = strings.ReplaceAll(fp, `\`, "/")
			if tools[v.Kind] && fp != "" && (fp == v.Path || strings.HasSuffix(fp, "/"+v.Path)) {
				found = append(found, r)
			}
		}
	}
	switch len(found) {
	case 0:
		return termPreTool{}, "no tool-call record from the pane's claude matches the dialog"
	case 1:
	default:
		return termPreTool{}, "more than one matching tool-call record"
	}
	// The dialog is for the newest call: an older record that happens to
	// match (a stale, denied call) never stands in for it.
	if newest := records[len(records)-1]; newest.ToolUseID != found[0].ToolUseID || !newest.At.Equal(found[0].At) {
		return termPreTool{}, "the matching record is not the newest tool call"
	}
	// @secure: a command with a newline goes to Review.
	if v.Kind == "bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(found[0].Input, &in)
		if strings.ContainsAny(in.Command, "\r\n") {
			return termPreTool{}, "command spans more than one line"
		}
	}
	return found[0], ""
}

// --- Live evaluation -----------------------------------------------------------

// termOneTap is a terminal permission menu one-tap may answer, as seen now.
type termOneTap struct {
	PromptID string
	Tool     string
	Summary  string
	Widens   bool
	pos      int64
	fp       uint64
}

// termPaneIsClaudeOnly reports a terminal pane whose agent is Claude Code and
// nothing else. Swappable in tests.
var termPaneIsClaudeOnly = func(p *Pane) bool {
	snap := snapshotPaneProcesses(p)
	// The scan covers descendants only; a pane may run claude directly.
	hasClaude := snap.hasClaude || (p.Pid > 0 && termProcComm(p.Pid) == "claude")
	return hasClaude && !snap.hasCodex && !snap.hasMuse && !snap.hasPi && !snap.hasDeepseek
}

// evaluateTerminalOneTap checks the pane's live screen against every
// condition above. reason is set when one-tap must not be offered.
func evaluateTerminalOneTap(p *Pane) (t termOneTap, reason string) {
	if p == nil {
		return t, "no pane"
	}
	if p.Snapshot().Kind == "native" {
		return t, "not a terminal pane"
	}
	// The cheap record check first: a pane with no hook records (every Codex
	// or plain-shell pane) can't qualify, so it pays for neither the process
	// walk nor a grid emulator.
	records := termPreToolSnapshot(p.ID)
	if len(records) == 0 {
		return t, termReasonNoRecords
	}
	if !termPaneIsClaudeOnly(p) {
		return t, "agent is not Claude Code alone"
	}
	// The grid first, so the prompt identity is read from the rendered screen.
	p.ensureGrid()
	_, pos, fp, ok := currentChoicePromptIdentity(p, "")
	if !ok || fp == 0 {
		return t, "no live choice prompt"
	}
	grid, visible := p.gridVisibleText()
	if !visible || !isGridChoicePromptText(grid, false) || approvalPromptFingerprint(grid, false) != fp {
		return t, "choice prompt is not on the rendered screen"
	}
	if !isAutoApprovalPromptText(grid, false) {
		return t, "not an approval prompt"
	}
	view, why := extractClaudeApprovalDialog(grid)
	if why != "" {
		return t, why
	}
	// One-tap answers Bash commands only, matching the phone's glanceable
	// rule: a file create/edit dialog shows the path, not the content, so it
	// goes to Review (defence in depth; the phone also refuses it).
	if view.Kind != "bash" {
		return t, "one-tap answers Bash commands only"
	}
	rec, why := matchTermPreTool(view, records)
	if why != "" {
		return t, why
	}
	// The record must belong to THIS prompt: sent by the claude that is the
	// pane's agent now (not a predecessor that exited), and after the
	// person's last input to the pane (any answer to an earlier prompt, at
	// the desktop or by a tap, makes older records stale).
	if !isPaneClaudeMainProcess(p.Pid, rec.PID) {
		return t, "the tool-call record's claude is no longer the pane's agent"
	}
	if last := atomic.LoadInt64(&p.LastInputMs); last > 0 && rec.At.UnixMilli() <= last {
		return t, "the tool-call record predates the last input to the pane"
	}
	t = termOneTap{
		PromptID: fmt.Sprintf("term:%d:%016x", pos, fp),
		Tool:     rec.Tool,
		Summary:  view.Summary,
		pos:      pos,
		fp:       fp,
	}
	if approvalWidening(rec.Tool, rec.Input) != "" || (view.Command != "" && commandWidening(view.Command) != "") {
		t.Widens = true
	}
	return t, ""
}

// --- Signing -------------------------------------------------------------------

// termSigned caches the signature for a pane's current prompt, so every push
// and the resolve see the same issuedAt and digest.
type termSignedEntry struct {
	promptID string
	signed   *signedApproval
}

var termSignCache sync.Map // pane id -> termSignedEntry

var termSignMu sync.Mutex

func signTerminalOneTap(paneID string, t termOneTap) *signedApproval {
	// One signature per prompt even when two pushes race.
	termSignMu.Lock()
	defer termSignMu.Unlock()
	if v, ok := termSignCache.Load(paneID); ok {
		e := v.(termSignedEntry)
		if e.promptID == t.PromptID && e.signed != nil && e.signed.tool == t.Tool && e.signed.summary == t.Summary && e.signed.widens == t.Widens {
			if s := activeApprovalSigner.Load(); s != nil && s.currentMachineID() == e.signed.machineID {
				return e.signed
			}
		}
	}
	signed := signApproval(paneID, t.PromptID, t.Tool, t.Summary, t.Widens)
	if signed == nil {
		return nil
	}
	termSignCache.Store(paneID, termSignedEntry{promptID: t.PromptID, signed: signed})
	return signed
}

// liveTerminalDigest is the digest held for the pane's prompt promptID.
func liveTerminalDigest(paneID, promptID string) string {
	if s := liveTerminalSigned(paneID, promptID); s != nil {
		return s.digest
	}
	return ""
}

func liveTerminalSigned(paneID, promptID string) *signedApproval {
	v, ok := termSignCache.Load(paneID)
	if !ok {
		return nil
	}
	e := v.(termSignedEntry)
	if e.promptID != promptID {
		return nil
	}
	return e.signed
}

// forgetTerminalOneTapPane drops a closed pane's records and signature.
func forgetTerminalOneTapPane(paneID string) {
	termPreTools.Delete(paneID)
	termSignCache.Delete(paneID)
	termAnswered.Delete(paneID)
	termResolveLocks.Delete(paneID)
}

// termAnswered remembers the last prompt a one-tap answered per pane, and
// termResolveLocks serialises resolves, so a lock-screen tap and an in-app
// tap can't both write.
var (
	termAnswered     sync.Map // pane id -> prompt id
	termResolveLocks sync.Map // pane id -> *sync.Mutex
)

func termResolveLock(paneID string) *sync.Mutex {
	v, _ := termResolveLocks.LoadOrStore(paneID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// termReasonNoRecords is the decline for a pane with no hook records: every
// Codex, plain-shell or hookless pane at a menu. It is not logged.
const termReasonNoRecords = "no matching tool-call record from the pane's claude"

func logTermOneTapDecline(paneID, reason string) {
	if reason == termReasonNoRecords {
		return
	}
	log.Printf("terminal one-tap: pane=%s offered=false reason=%q", paneID, reason)
}

// recordTermPreToolFromHook keeps a PreToolUse record for terminal one-tap,
// but only when the request came from the pane's own top-level claude
// process (@secure, ST-265). Anything else is ignored, which leaves the
// record missing and the menu on Review.
func (a *apiServer) recordTermPreToolFromHook(r *http.Request, paneID string, payload hookPayload) {
	if a.mgr == nil {
		return
	}
	if payload.ToolName == "" || payload.ToolInput == nil {
		// A tool call we can't keep a record of: like any other unkept call,
		// no older record may stand in for it.
		termPreTools.Delete(paneID)
		return
	}
	p, ok := a.mgr.Get(paneID)
	if !ok || p.Snapshot().Kind == "native" {
		return
	}
	pid, local := lookupPeerPID(r)
	if !local || !isPaneClaudeMainProcess(p.Pid, pid) {
		// A tool call this pane made that can't be attributed (a PID lookup
		// miss, as on WSL's routed loopback, or a forged sender) poisons the
		// pane's records: an older record must never stand in for it.
		log.Printf("terminal one-tap: pane=%s PreToolUse from pid %d is not the pane's claude; clearing its records", paneID, pid)
		termPreTools.Delete(paneID)
		return
	}
	input, err := json.Marshal(payload.ToolInput)
	if err != nil {
		termPreTools.Delete(paneID)
		return
	}
	recordTermPreTool(paneID, termPreTool{ToolUseID: payload.ToolUseID, Tool: payload.ToolName, Input: input, At: time.Now(), PID: pid})
}

// forgetTermPreToolFromHook drops the record a PostToolUse answers, under
// the same sender check: otherwise an agent's child could delete the live
// record and leave a stale one to match.
func (a *apiServer) forgetTermPreToolFromHook(r *http.Request, paneID string, payload hookPayload) {
	if a.mgr == nil || payload.ToolUseID == "" {
		return
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return
	}
	if pid, local := lookupPeerPID(r); !local || !isPaneClaudeMainProcess(p.Pid, pid) {
		log.Printf("terminal one-tap: pane=%s ignored a PostToolUse from pid %d (not the pane's claude)", paneID, pid)
		return
	}
	forgetTermPreTool(paneID, payload.ToolUseID)
}
