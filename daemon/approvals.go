package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// approvals.go is the native-renderer approvals pipeline (Phase 1, Component 7).
//
// A native Claude agent runs headless with --permission-prompt-tool stdio, so a
// can_use_tool permission request arrives on stdout as a control_request. The
// parser goroutine (pane_native.go) parks it into Pane.nativePendingApproval (an
// atomic.Pointer[NativeApproval]) — that lock-free pointer is what computeState
// reads at the top of its hot path to short-circuit to StateChoicePrompt, and
// it is the single source of truth + idempotency key for resolution here.
//
// Resolution writes the EXACT ground-truth control_response allow/deny frame
// (native_claude.go EncodeApproval) to the agent's stdin via WriteNoPromptRecord
// (a control answer, not a user prompt — must not pollute the prompt recorder).
// Interrupt resolves any open approval as deny FIRST, then writes the interrupt
// control_request; the agent's control_cancel_request ack clears the pointer in
// pane_native.go.
//
// Idempotency: resolveApproval CompareAndSwaps the pending pointer to nil, so the
// first call wins and any second call (lock-screen + app double-tap, or a retry)
// sees a nil/mismatched pointer and is a safe no-op. Native autonomous approvals
// route through the SAME ARIA fire-budget caps (tryReserveAutoFire*/autoRecordFire)
// as every other auto-fire path so the pause + 5/sec runaway protection still
// applies — native must NOT invent a 4th un-capped path (see nativeAutoResolve).

// nativeRendererEnabled reports whether the native-renderer mode is permitted.
// DEFAULT-ON: native mode ships available with zero config — the per-pane
// Terminal/Native toggle just works out of the box (soluterminal's whole model
// is install-and-go, no manual env/systemd tweaks). It's still opt-in PER PANE:
// terminal (Kind=="") stays the default for every pane and pays zero cost, so
// nothing changes for anyone who doesn't click Native. SOLU_NATIVE_RENDERER is
// retained only as a KILL-SWITCH: set it to "0"/"false"/"off"/"no" to disable
// the feature globally (a safety valve if a field issue ever surfaces).
func nativeRendererEnabled() bool {
	// Focus (native) mode is RETIRED by default (2026-10): no new native panes,
	// no switches into native, and existing native panes are moved back to the
	// terminal renderer once their turn settles (focus_retire.go). Set
	// SOLU_NATIVE_RENDERER=1 to bring it back while the code still exists.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_NATIVE_RENDERER"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// nativeAriaAutoEnabled reports whether ARIA auto-mode (AutoMode always /
// cautious / standing orders) is allowed to drive NATIVE-renderer pane
// approvals. Like nativeRendererEnabled this is DEFAULT-ON with a kill-switch:
// native auto-resolution is already gated three ways — the pane must be opted
// in per-pane (a non-empty AutoMode or a standing order), every fire is bounded
// by the ARIA gates (pause + 5/sec), and the global pause flag stops
// it — so it ships working out of the box. Set SOLU_ARIA_NATIVE_AUTO to
// 0/false/off/no to disable it globally (a safety valve if a field issue ever
// surfaces); ARIA then leaves native panes for the human (the pre-wiring
// behaviour).
func nativeAriaAutoEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_ARIA_NATIVE_AUTO"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// errNoPendingApproval is returned (and mapped to a no-op 200) when there is no
// open approval matching the request — already resolved, cancelled, or never
// parked. Idempotency makes a double-resolve safe, not an error to the caller.
var errNoPendingApproval = errors.New("no pending approval")

// errNativeSessionDied is returned by the switch-mode health check when a
// freshly-respawned native pane exits within the grace window — the classic
// `--resume <sessionId>` failure (the session can't be replayed on this
// machine). The switch-mode handler rolls back to the original pane.
var errNativeSessionDied = errors.New("native agent exited immediately after respawn (resume failed)")

// errPaneRespawnNil guards the switch-mode path against a spawn hook that
// returns (nil, nil) — treated as a respawn failure so we roll back rather than
// dereference a nil pane.
var errPaneRespawnNil = errors.New("respawn returned no pane")

// --- per-pane tool-approval allowlist ("always allow this tool") ---------
//
// The allowlist is a set of EXACT tool names the user has opted into
// auto-approving for THIS pane. It lives under nativeApprovalMu (the same lock
// that guards the pending-approval map) because the on-arrival decision — "is
// this tool allowlisted? then resolve immediately instead of parking" — is one
// atomic step in the approval domain. The accessors below are the ONLY writers,
// so the map is always either nil (no opt-ins) or a non-empty set.

// isToolAllowlisted reports whether tool (matched by EXACT name) is in this
// pane's allowlist. Lock-free-cheap: one map lookup under nativeApprovalMu.
func (p *Pane) isToolAllowlisted(tool string) bool {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return false
	}
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	return p.nativeAllowlist[tool]
}

// allowlistCommandPrefixes returns the pane's command-prefix rules for tool —
// entries of the form `Tool(token token ...)`, e.g. `Bash(git status)` —
// each as its whitespace-split token list. A plain tool-name entry is NOT a
// prefix rule and never appears here.
func (p *Pane) allowlistCommandPrefixes(tool string) [][]string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return nil
	}
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	var out [][]string
	pre := tool + "("
	for entry := range p.nativeAllowlist {
		if strings.HasPrefix(entry, pre) && strings.HasSuffix(entry, ")") {
			if inner := strings.TrimSpace(entry[len(pre) : len(entry)-1]); inner != "" {
				out = append(out, strings.Fields(inner))
			}
		}
	}
	return out
}

// allowlistPrefixMatch reports whether a permission request is covered by the
// pane's command-prefix rules (`Bash(git status)`-style entries). Only Bash is
// supported: the command is parsed with a real shell parser (shellArgvs) and
// EVERY command the script runs must match at least one rule — so
// `git status && rm -rf /` does not ride a `git status` rule, and neither does
// anything the parser can't fully see (expansions, command substitution,
// file redirects, parse errors). Sound by construction, not by pattern luck.
func (p *Pane) allowlistPrefixMatch(toolName string, rawInput json.RawMessage) bool {
	if strings.TrimSpace(toolName) != "Bash" {
		return false
	}
	prefixes := p.allowlistCommandPrefixes("Bash")
	if len(prefixes) == 0 {
		return false
	}
	cmd := bashCommandFromInput(rawInput)
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	argvs, sound := shellArgvs(cmd)
	if !sound {
		return false
	}
	for _, argv := range argvs {
		if !argvMatchesAnyPrefix(argv, prefixes) {
			return false
		}
	}
	return true
}

func argvMatchesAnyPrefix(argv []string, prefixes [][]string) bool {
outer:
	for _, pfx := range prefixes {
		if len(pfx) == 0 || len(argv) < len(pfx) {
			continue
		}
		for i, tok := range pfx {
			if argv[i] != tok {
				continue outer
			}
		}
		return true
	}
	return false
}

// allowlistAddTool adds tool to the pane's allowlist and returns the updated
// sorted list. Empty/whitespace tool is a no-op that still returns the current
// list (the HTTP layer validates non-empty before calling).
func (p *Pane) allowlistAddTool(tool string) []string {
	tool = strings.TrimSpace(tool)
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if tool != "" {
		if p.nativeAllowlist == nil {
			p.nativeAllowlist = make(map[string]bool)
		}
		p.nativeAllowlist[tool] = true
	}
	return sortedAllowlistLocked(p.nativeAllowlist)
}

// allowlistRemoveTool removes tool from the pane's allowlist and returns the
// updated sorted list.
func (p *Pane) allowlistRemoveTool(tool string) []string {
	tool = strings.TrimSpace(tool)
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if tool != "" && p.nativeAllowlist != nil {
		delete(p.nativeAllowlist, tool)
	}
	return sortedAllowlistLocked(p.nativeAllowlist)
}

// allowlistTools returns the pane's allowlist as a sorted slice (nil when
// empty) for snapshot/persistence/HTTP responses.
func (p *Pane) allowlistTools() []string {
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	return sortedAllowlistLocked(p.nativeAllowlist)
}

// setNativeAllowlist replaces the pane's allowlist with the given tool names
// (used on restore/resume/spawn). Empties to nil so a pane with no opt-ins
// keeps the map nil (the zero-cost fallback for terminal-like behavior).
func (p *Pane) setNativeAllowlist(tools []string) {
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if len(tools) == 0 {
		p.nativeAllowlist = nil
		return
	}
	m := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t = strings.TrimSpace(t); t != "" {
			m[t] = true
		}
	}
	if len(m) == 0 {
		p.nativeAllowlist = nil
		return
	}
	p.nativeAllowlist = m
}

// clearNativeAllowlist drops every opt-in. Called when the pane's conversation
// identity genuinely changes (a brand-NEW session), so a fresh conversation
// never inherits the prior one's auto-allow set.
func (p *Pane) clearNativeAllowlist() {
	p.nativeApprovalMu.Lock()
	p.nativeAllowlist = nil
	p.nativeApprovalMu.Unlock()
}

// sortedAllowlistLocked returns the allowlist's keys as a sorted slice (nil
// when empty). Caller holds nativeApprovalMu.
func sortedAllowlistLocked(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// maybeAutoAllowFromList checks a just-parked permission request against the
// pane's allowlist and, if the tool is opted-in, resolves it as ALLOW
// IMMEDIATELY — bypassing the ARIA fire-budget cap. This is a DELIBERATE,
// explicit user opt-in ("always allow this tool"), not an autonomous ARIA
// heuristic, so it must NOT be throttled by the pause + 5/sec runaway gates
// (those exist to bound ARIA's model-driven decisions; the user pressing
// "always allow" is ground truth). It still resolves by the EXACT RequestID via
// resolveApproval (never the empty-id arbitrary pick), and it logs every
// auto-allow so the daemon log shows the truth.
//
// Returns true if it consumed (auto-allowed) the request — in which case the
// caller leaves NO pending approval card. Returns false if the tool is not
// allowlisted (the request stays parked for the user) OR the resolve write
// failed (the request also stays parked so the user can still answer it; the
// agent is NOT left hung).
//
// Cautious-mode exception: when the pane's ARIA auto-mode is "cautious", an
// allowlisted tool whose ARGUMENTS trip the deterministic deny needles stays
// parked. Without this, "always allow Bash" + cautious mode silently behaved
// like "always approve" for every Bash command (rm -rf included) — the
// allowlist matches tool NAME only and never inspected arguments. In off/always
// modes the opt-in keeps its original absolute meaning; cautious is precisely
// the user saying "apply judgment", so the red-flag backstop applies to
// opted-in tools too.
//
// CRITICAL ordering: the caller invokes this AFTER setNativePending and OUTSIDE
// nativeMu (it runs in emitNativeEvent's post-fanout switch, the same place the
// existing approval bookkeeping runs). resolveApproval → WriteNoPromptRecord
// writes the child's stdin and takes nativeApprovalMu (via takeNativePending)
// and agentWriteMu — NEVER nativeMu — so there is no deadlock with the parser
// goroutine's fanout lock.
// allowlistAutoAllows reports WHETHER an allowlist rule covers this request and
// whether the cautious backstop vetoes it — the decision maybeAutoAllowFromList
// acts on, with none of the acting. PURE: no resolve, no stdin write, no state
// change, so it is safe to call on the emit path before the event fans out.
//
// `how` is "" when no rule matches; otherwise "tool" / "prefix". `denyReason` is
// non-empty only when a rule DID match but cautious mode vetoed it. Auto-allow
// happens iff how != "" && denyReason == "".
//
// This exists so the client never has to re-derive the rules. A whole-tool rule
// ("Bash") is a string compare the client could do, but an argument-scoped rule
// ("Bash(git status)") requires parsing shell argv the way allowlistPrefixMatch
// does — and a client that guessed WRONG would either flash a card the daemon
// then tears down, or (worse, if it guessed more permissively than the daemon)
// hide a card for a request nobody approved, hanging the agent on a question the
// user can never see. The daemon already knows; it says so on the event.
func (p *Pane) allowlistAutoAllows(toolName string, rawInput json.RawMessage) (how, denyReason string) {
	if p.Snapshot().NeverAutoAnswer {
		return "", "automatic answers locked by user"
	}
	// Two rule shapes: a whole-tool opt-in ("Bash") or an argument-scoped
	// prefix rule ("Bash(git status)") that only covers commands it can fully
	// see (see allowlistPrefixMatch).
	switch {
	case p.isToolAllowlisted(toolName):
		how = "tool"
	case p.allowlistPrefixMatch(toolName, rawInput):
		how = "prefix"
	default:
		return "", ""
	}
	if p.Snapshot().AutoMode == "cautious" {
		if reason := nativeCautiousAutoDenyReason(&NativeApproval{ToolName: toolName, RawInput: rawInput}); reason != "" {
			return how, reason
		}
	}
	return how, ""
}

// maybeAutoAllowFromList resolves a request the emit path ALREADY decided to
// auto-allow — `how` is the non-empty verdict from allowlistAutoAllows, carried
// over rather than recomputed.
//
// Taking the decision as an argument is the point: the verdict is also stamped
// onto the outgoing event as WouldAutoAllow, and the client hides the approval
// card on it. Re-deriving here would make the stamp and the resolve two
// INDEPENDENT reads of a mutable allowlist — a rule removed in the microseconds
// between them (the user un-ticking a tool mid-turn) would stamp "hidden" and
// then decline to resolve, leaving the request parked behind a card that never
// renders and an agent hung on a question nobody can see. One decision, used
// twice, cannot disagree with itself.
func (p *Pane) maybeAutoAllowFromList(reqID, toolName, how string) bool {
	if reqID == "" || how == "" {
		return false
	}
	pending, err := p.resolveAutomaticApproval(reqID)
	if err != nil || pending == nil {
		// Resolve failed (e.g. write error) or the approval was already
		// taken by a racing manual/ARIA resolve. Either way DON'T claim it —
		// if it's still parked the user can answer it; if it's gone it's
		// already handled. Log a write failure so it isn't silent.
		if err != nil && !errors.Is(err, errNoPendingApproval) {
			log.Printf("approvals: allowlist auto-allow write FAILED (pane=%s tool=%s reqID=%s): %v",
				p.ID, toolName, reqID, err)
		}
		return false
	}
	log.Printf("approvals: allowlist auto-allowed tool=%s via %s rule (pane=%s reqID=%s) — user opt-in, ARIA cap bypassed",
		toolName, how, p.ID, reqID)
	return true
}

// setNativePending parks a newly-arrived permission request, keyed by RequestID
// so concurrent requests (parallel tool calls) coexist instead of overwriting.
func (p *Pane) setNativePending(ap *NativeApproval) {
	if ap == nil || ap.RequestID == "" {
		return
	}
	p.nativeApprovalMu.Lock()
	if p.nativePendingApprovals == nil {
		p.nativePendingApprovals = make(map[string]*NativeApproval)
	}
	_, existed := p.nativePendingApprovals[ap.RequestID]
	p.nativePendingApprovals[ap.RequestID] = ap
	p.nativePendingCount.Store(int32(len(p.nativePendingApprovals)))
	p.nativeApprovalMu.Unlock()
	// Advance the choice-prompt position on each genuinely-new approval so the
	// aria-watcher's "a new menu appeared while still in choice-prompt" re-arm
	// (and the 15s awaiting-approval notification) fire for back-to-back native
	// approvals. computeState short-circuits native panes before the terminal
	// branch that normally maintains this counter, so without this a second
	// approval arriving while the first is still open would share the prior
	// position and be treated as "same menu, already handled." Native panes
	// never run the terminal grid identity logic, so repurposing the field as a
	// monotonic approval counter here is safe.
	if !existed {
		atomic.AddInt64(&p.LastAckedChoicePromptPos, 1)
	}
	// Announce the state flip (idle/busy → choice-prompt) immediately instead of
	// waiting for the 2s activity ticker — the phone's "Needs you" list refetches
	// on this event. Outside nativeApprovalMu: the edge callback Snapshots the
	// pane, and Snapshot's pinned lock order takes nativeApprovalMu first.
	p.publishTurnEdge()
}

// takeNativePending removes and returns the approval matching reqID. Returns
// nil when nothing matches (the idempotency key: a second resolve of the same
// reqID gets nil → errNoPendingApproval).
//
// An EMPTY reqID is a logged no-op (returns nil) — NEVER an arbitrary pick.
// With parallel tool calls there can be several pending approvals at once; an
// arbitrary pick let an ARIA decision made about ONE specific tool+args be
// applied to a DIFFERENT pending approval (approve a safe Read → allow a
// dangerous Bash). Resolution MUST always carry the concrete RequestID of the
// exact approval that was evaluated.
func (p *Pane) takeNativePending(reqID string) *NativeApproval {
	if reqID == "" {
		// Refuse the ambiguous "resolve whatever's pending" case: under
		// parallel tool calls it could resolve the wrong tool. Callers must
		// thread the RequestID of the approval they actually evaluated.
		log.Printf("approvals: takeNativePending called with empty reqID (pane=%s) — refusing arbitrary pick", p.ID)
		return nil
	}
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if len(p.nativePendingApprovals) == 0 {
		return nil
	}
	var ap *NativeApproval
	if v, ok := p.nativePendingApprovals[reqID]; ok {
		ap = v
		delete(p.nativePendingApprovals, reqID)
	}
	p.nativePendingCount.Store(int32(len(p.nativePendingApprovals)))
	return ap
}

// takeNativePendingByToolUse removes and returns the open approval whose
// ToolUseID matches callID (nil when nothing matches). Used by the tool_result
// reconciliation in emitNativeEvent: a tool_result arriving for a tool_use
// proves its permission request was already answered — by this daemon, a
// predecessor daemon whose answer we're re-reading from the agent backlog, a
// concurrent client, or ARIA — so any still-parked entry for it is a ghost.
// Unlike takeNativePending (keyed by RequestID, the resolution path), this is
// keyed by the tool_use id and only CLEARS state; it never feeds a
// control_response, so there is no wrong-tool resolution hazard.
func (p *Pane) takeNativePendingByToolUse(callID string) *NativeApproval {
	if callID == "" {
		return nil
	}
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if len(p.nativePendingApprovals) == 0 {
		return nil
	}
	for reqID, v := range p.nativePendingApprovals {
		if v.ToolUseID == callID {
			delete(p.nativePendingApprovals, reqID)
			p.nativePendingCount.Store(int32(len(p.nativePendingApprovals)))
			return v
		}
	}
	return nil
}

// drainNativePending removes and returns ALL open approvals (turn_end / error /
// control_cancel / interrupt / teardown clear every entry at once).
func (p *Pane) drainNativePending() []*NativeApproval {
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if len(p.nativePendingApprovals) == 0 {
		return nil
	}
	out := make([]*NativeApproval, 0, len(p.nativePendingApprovals))
	for _, v := range p.nativePendingApprovals {
		out = append(out, v)
	}
	p.nativePendingApprovals = make(map[string]*NativeApproval)
	p.nativePendingCount.Store(0)
	return out
}

// hasNativePending is the lock-free hot-path check computeState reads.
func (p *Pane) hasNativePending() bool { return p.nativePendingCount.Load() > 0 }

// peekNativePending returns the open approval for reqID without removing it
// (empty reqID returns an arbitrary open one). For INSPECTION/TESTS ONLY — it
// is read-only so an arbitrary pick is harmless here, but its result must NEVER
// feed a resolution: resolution goes through takeNativePending keyed by a
// concrete RequestID (an arbitrary pick there could allow the wrong tool).
func (p *Pane) peekNativePending(reqID string) *NativeApproval {
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if reqID == "" {
		for _, v := range p.nativePendingApprovals {
			return v
		}
		return nil
	}
	return p.nativePendingApprovals[reqID]
}

// listNativePending returns a snapshot of every currently-open native approval,
// sorted by RequestID for deterministic ordering. Read-only: building an ARIA
// observation, or deciding which reqIDs to auto-resolve. The returned slice is
// a fresh copy, but the *NativeApproval pointers are the live entries and must
// NOT be mutated. Resolution MUST still go through resolveApproval keyed by a
// concrete RequestID — NEVER act on an element of this slice without carrying
// its exact reqID (the parallel-tool-call wrong-tool hazard; see
// takeNativePending). Empty when nothing is pending.
func (p *Pane) listNativePending() []*NativeApproval {
	p.nativeApprovalMu.Lock()
	defer p.nativeApprovalMu.Unlock()
	if len(p.nativePendingApprovals) == 0 {
		return nil
	}
	out := make([]*NativeApproval, 0, len(p.nativePendingApprovals))
	for _, v := range p.nativePendingApprovals {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

// resolveApproval answers ONE of the pane's open native permission requests.
//
// It is idempotent by RequestID: takeNativePending atomically removes the
// matching entry, so only the first caller gets it and writes the
// control_response; a concurrent/second resolve observes nothing and returns
// errNoPendingApproval (the HTTP layer maps that to a no-op 200, since a
// lock-screen + in-app double-tap is an expected, safe duplicate).
//
// reqID MUST identify the exact approval to resolve. It must match an open
// approval's RequestID; an empty or unknown reqID is a no-op (returns
// errNoPendingApproval). The previous "empty == resolve whatever's pending"
// behaviour was a safety hole: with parallel tool calls there can be SEVERAL
// open approvals, and an arbitrary pick could apply an ARIA decision made about
// one tool to a different (possibly dangerous) one. Each approval is resolved
// independently by its own reqID — including the ARIA auto-fire path.
//
// On allow, updatedInput echoes request.input verbatim (RawInput). The frame
// goes out via WriteNoPromptRecord so it never reaches the prompt recorder. The
// claimed approval is returned so the caller can bridge into ARIA's confirm
// registry (ConfirmID) at the apiServer layer where Aria is reachable.
func (p *Pane) resolveApproval(reqID string, allow bool, denyReason string) (*NativeApproval, error) {
	return p.resolveApprovalWithPolicy(reqID, allow, denyReason, nil)
}

func (p *Pane) resolveAutomaticApproval(reqID string) (*NativeApproval, error) {
	policy := p.autoModePolicy()
	return p.resolveAutomaticApprovalForPolicy(reqID, policy)
}

func (p *Pane) resolveAutomaticApprovalForPolicy(reqID string, policy paneAutoModePolicy) (*NativeApproval, error) {
	return p.resolveApprovalWithPolicy(reqID, true, "", &policy)
}

func (p *Pane) resolveApprovalWithPolicy(reqID string, allow bool, denyReason string, policy *paneAutoModePolicy) (*NativeApproval, error) {
	if policy != nil && !policy.matches(p) {
		return nil, errAutoModePolicyChanged
	}
	pending := p.takeNativePending(reqID)
	if pending == nil {
		// Diagnostic: a resolve that finds NO live pending entry. Expected for a
		// benign double-tap, but ALSO the signature of a "dead question" — a click
		// on a replayed approval card whose request was already resolved. The
		// historyBackfill filter (dropResolvedApprovalCards) should prevent the
		// latter; if this still fires from a real click, capture the wire here.
		log.Printf("[native-turn] pane=%s approval_resolve_noop reqId=%s allow=%v — no live pending (double-tap or stale replay card)", p.ID, reqID, allow)
		return nil, errNoPendingApproval
	}
	adapter, ok := p.NativeAdapterRef()
	if !ok {
		return nil, errors.New("pane is not native")
	}
	frame := adapter.EncodeApproval(pending.RequestID, pending.ToolUseID, allow, pending.RawInput, denyReason)
	if len(frame) == 0 {
		p.setNativePending(pending)
		return nil, errors.New("native agent cannot correlate this approval request")
	}
	write := p.WriteNoPromptRecord
	if policy != nil {
		write = func(b []byte) (int, error) {
			return p.writeWithAutoModePolicy(b, false, *policy, time.Now().Add(ariaJevTimeout))
		}
	}
	if _, err := write(frame); err != nil {
		p.setNativePending(pending)
		return nil, err
	}
	if committer, ok := adapter.(interface{ CommitApproval(string) }); ok {
		committer.CommitApproval(pending.RequestID)
	}
	// Diagnostic: the user's own allow/deny (an AskUserQuestion answer is a deny
	// carrying the chosen option as the reason). Pairs with the "[native-turn]"
	// set=true/set=false edges so a repro shows whether the post-answer turn ever
	// settled.
	action := "allow"
	if !allow {
		action = "deny"
	}
	log.Printf("[native-turn] pane=%s approval_resolved callId=%s reqId=%s tool=%s action=%s reasonLen=%d",
		p.ID, pending.ToolUseID, pending.RequestID, pending.ToolName, action, len(denyReason))
	// Emit a durable, request-correlated resolution edge on the SAME native
	// stream as permission_request. Focus can now dismiss the exact card as soon
	// as the control response is written, instead of waiting for a long-running
	// tool's eventual tool_result. Keeping this in the ring also heals reconnects
	// and warm-unmounted panes; tool_result remains a client fallback for older
	// daemons.
	p.emitNativeEvent(NativeEvent{
		Type:      EvPermissionResolved,
		RequestID: pending.RequestID,
		CallID:    pending.ToolUseID,
		ToolName:  pending.ToolName,
	})
	// Announce the cleared choice-prompt immediately. Resolution used to publish
	// nothing — the clear rode the 2s activity ticker (whose <1s min-hold can
	// drop it entirely), so remote clients kept showing "needs approval" after
	// ARIA/desktop/another phone had already answered.
	p.publishTurnEdge()
	return pending, nil
}

// interruptNative sends an interrupt to the native agent. Per the plan §6 rule,
// any OPEN permission request is resolved as deny FIRST (so the tool doesn't run
// and computeState stops reporting StateChoicePrompt), then the interrupt frame
// is written. The agent acks with control_cancel_request, which pane_native.go
// already clears the pending pointer for (belt-and-suspenders with the deny here).
func (p *Pane) interruptNative() (denied []string, err error) {
	adapter, ok := p.NativeAdapterRef()
	if !ok {
		return nil, errors.New("pane is not native")
	}
	// Deny EVERY open approval first (parallel tool calls can leave several),
	// then interrupt the turn. A single drain so none is left orphaned.
	var approvalWriteErr error
	for _, ap := range p.drainNativePending() {
		frame := adapter.EncodeApproval(ap.RequestID, ap.ToolUseID, false, ap.RawInput, "interrupted")
		if len(frame) == 0 {
			p.setNativePending(ap)
			continue
		}
		if _, err := p.WriteNoPromptRecord(frame); err != nil {
			p.setNativePending(ap)
			if approvalWriteErr == nil {
				approvalWriteErr = err
			}
			continue
		}
		if committer, ok := adapter.(interface{ CommitApproval(string) }); ok {
			committer.CommitApproval(ap.RequestID)
		}
		denied = append(denied, ap.RequestID)
		p.emitNativeEvent(NativeEvent{
			Type: EvPermissionResolved, RequestID: ap.RequestID,
			CallID: ap.ToolUseID, ToolName: ap.ToolName,
		})
	}
	frame := adapter.EncodeInterrupt()
	if len(frame) == 0 {
		if p.IsAgentWorking() {
			return denied, errors.New("native agent cannot correlate the active turn")
		}
		return denied, approvalWriteErr
	}
	if _, err := p.WriteNoPromptRecord(frame); err != nil {
		return denied, err
	}
	return denied, approvalWriteErr
}

// nativeAutoResolve is a self-contained autonomous (ARIA) approval helper for a
// native pane that bundles reserve→write→record into one call.
//
// NOTE: it is NOT on the wired auto-mode path. The live native auto-mode flow
// (aria_native_auto.go: directAutoFireNative and toolResolvePermission) instead
// mirrors the terminal path — it reserves in the fire-path and records in the
// autoEmitter's confirm_resolved branch — so the budget bookkeeping stays
// identical to send_input. Do NOT route those callers through this helper as
// well: the autoEmitter would record a second time and double-count the fire
// against the burst cap. This helper is retained (and tested) as the canonical
// reserve→write→record reference; if you wire it in, drop the autoEmitter
// confirm_resolved record for that path.
//
// It MUST route through the identical fire-budget caps as every other auto-fire
// path: reserve a slot BEFORE writing the allow frame, record the fire only
// AFTER the write lands, and release the reservation on failure. This keeps the
// pause + 5/sec runaway protection applying to native panes — native does not
// get a 4th un-capped path.
//
// reqID is the RequestID of the EXACT approval ARIA evaluated — it is threaded
// through to resolveApproval so ARIA resolves THAT approval and never an
// arbitrary one. Under parallel tool calls (Claude fires Read/Grep/Bash at
// once) several approvals can be pending simultaneously; ARIA made its
// allow/deny decision by evaluating one specific tool+args, so it must resolve
// that specific RequestID. An empty reqID is rejected (returns false) before
// any budget slot is reserved — the caller has no concrete approval to act on.
//
// confirmID is the ARIA confirm registry key (when the approval was bridged via
// awaitConfirm) OR a synthetic key for the no-registry directAutoFire-equivalent;
// it is what the caps are keyed on. Returns true when the approval was both
// reserved and written.
func (a *Aria) nativeAutoResolve(p *Pane, reqID, confirmID string, useRegistry bool) bool {
	if a == nil || p == nil {
		return false
	}
	if reqID == "" {
		// No concrete approval to resolve — refuse rather than pick one at
		// random (the parallel-tool-call safety hole). Don't touch the budget.
		log.Printf("approvals: nativeAutoResolve refused — empty reqID (pane=%s confirmID=%s)", p.ID, confirmID)
		return false
	}
	var reserved bool
	if useRegistry {
		reserved = a.tryReserveAutoFireForConfirm(confirmID)
	} else {
		reserved = a.tryReserveAutoFireSlot(confirmID)
	}
	if !reserved {
		return false
	}
	pending, err := p.resolveAutomaticApproval(reqID)
	if err != nil || pending == nil {
		// Write failed or nothing pending — give the budget slot back.
		a.releaseAutoFireReservation(confirmID)
		return false
	}
	// The allow frame landed: log the fire exactly once and
	// mark the pane's auto-mode fired (mirrors the confirm_resolved branch).
	a.autoRecordFire(confirmID)
	p.MarkAutoModeFired()
	return true
}

// resolveApprovalReq is the body for POST /api/panes/{id}/approvals/{reqId}/resolve.
type resolveApprovalReq struct {
	Allow      bool   `json:"allow"`
	DenyReason string `json:"denyReason,omitempty"`
}

// handlePaneApprovalResolve answers a native pane's pending permission request
// from the lock-screen / app. Path: POST /api/panes/{id}/approvals/{reqId}/resolve.
//
// Idempotent: a resolve for an already-resolved (or unknown) reqId returns 200
// (not 404), because a lock-screen + in-app double-tap is an expected, safe
// duplicate — unlike handleAriaConfirm which 404s on unknown confirmID.
func (a *apiServer) handlePaneApprovalResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reqID := r.PathValue("reqId")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	if p.Snapshot().Kind != "native" {
		http.Error(w, "pane is not native", 409)
		return
	}
	var req resolveApprovalReq
	// Body is optional for a bare deny; tolerate empty/missing JSON.
	if body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16)); len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), 400)
			return
		}
	}
	// Allowing or denying both answer the pane's permission request: the
	// person, or an agent under the prompt rules (requirePromptAnswer).
	action := "deny a pane's permission request"
	if req.Allow {
		action = "allow a pane's permission request"
	}
	note, ok := a.admitPromptAnswer(w, r, p, action)
	if !ok {
		return
	}
	// A delegated API caller is still a machine answer. Only the person's
	// native approval card may use the manual writer on a locked pane.
	var policy *paneAutoModePolicy
	if a.identifyCaller(r).Kind != callerHuman {
		captured := p.autoModePolicy()
		policy = &captured
	}
	pending, err := p.resolveApprovalWithPolicy(reqID, req.Allow, req.DenyReason, policy)
	if err != nil {
		if errors.Is(err, errAutoModePolicyChanged) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, errNoPendingApproval) {
			// No-op: already resolved or never pending. Safe duplicate.
			writeJSON(w, 200, map[string]any{"resolved": false, "reason": "no pending approval"})
			return
		}
		http.Error(w, "resolve failed: "+err.Error(), 500)
		return
	}
	if pending != nil {
		note(approvalPromptID(pending.RequestID))
	}
	// If this approval is bridged into ARIA's confirm registry, resolve that
	// entry too so the in-app ConfirmCard path and the lock-screen path
	// converge on signalConfirm (idempotent — false on already-gone).
	if a.aria != nil && pending != nil && pending.ConfirmID != "" {
		a.aria.signalConfirm(pending.ConfirmID, req.Allow, false)
	}
	writeJSON(w, 200, map[string]any{"resolved": true, "allow": req.Allow})
}

// handlePaneNativeInterrupt sends an interrupt to a native pane's agent,
// resolving any open approval as deny first. Path:
// POST /api/panes/{id}/native/interrupt.
func (a *apiServer) handlePaneNativeInterrupt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	if p.Snapshot().Kind != "native" {
		http.Error(w, "pane is not native", 409)
		return
	}
	// Interrupting denies every pending approval: that answers the pane's
	// permission request, gated exactly like the REST deny.
	note := func(string) {}
	if p.peekNativePending("") != nil {
		var ok bool
		if note, ok = a.admitPromptAnswer(w, r, p, "deny a pane's permission request"); !ok {
			return
		}
	}
	denied, err := p.interruptNative()
	if len(denied) > 0 {
		note(approvalsPromptID(denied))
	}
	if err != nil {
		http.Error(w, "interrupt failed: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"interrupted": true})
}
