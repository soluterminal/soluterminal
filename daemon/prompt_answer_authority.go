package main

import (
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Who may answer a permission prompt in a pane, or act on prompts for the
// person (ST-10, criteria-1237-st10.md with amendment A4).
//
// The person always may. An agent may only under the person's
// Agent-permissions grant, and then only from a pane the daemon traced to the
// calling process: over the MCP bridge (the socket's peer credentials) or a
// traced HTTP connection. A pane id, handle, SOLU_PANE_ID or pane header the
// caller supplies is a claim and never authority, on every platform. Workers,
// managed-agent sessions and requests forwarded for a remote device never
// qualify. No pane, traced or not, answers a prompt shown in its own pane:
// an agent approving its own permission request defeats the prompt.
//
// The same decision covers every route that acts on prompts for the person:
// answer_prompt, send_input keys at a prompt, the pane socket, put-back and
// schedule writes, resolving or interrupting a native approval, turning on
// auto-mode, adding to the native allowlist, and approving an Aria
// confirmation.
//
// Codex panes reach the daemon over HTTP. Where that connection can't be
// traced, including WSL's default networking, they can't answer other panes'
// prompts until the Codex bridge ships (PR2b-Codex).

const (
	promptAnswerGrantOff  = "Agents can't answer prompts on their own. The person can answer it in the SoluTerminal app, or turn on Settings → Privacy & security → Agent permissions to let agents in panes do it."
	promptAnswerUntraced  = "Only an agent traced to its own pane can answer prompts: turn on agent tracing (MCP bridge), or answer it in the app. Codex panes can't answer other panes' prompts until Codex has the MCP bridge."
	promptAnswerOwnPrompt = "A pane can't answer a prompt shown in its own pane; the person answers it in the app."
	promptAnswerWorker    = "Workers and managed agents never answer prompts; the person answers it in the app."
)

// mcpCallMeta is how an MCP call reached the daemon, for the prompt rules.
type mcpCallMeta struct {
	id callerIdentity
	// transport: "unix" (the MCP bridge), "tcp" (HTTP) or "rest".
	transport string
}

// mcpMetaFromCallerPane is the meta of a call known only by its caller pane
// string (tests and internal callers): a traced pane, or an untraced caller.
func mcpMetaFromCallerPane(caller string) mcpCallMeta {
	if c := splitMCPCaller(caller); c.ID != "" {
		return mcpCallMeta{id: callerIdentity{Kind: callerPane, PaneID: c.ID}, transport: "tcp"}
	}
	return mcpCallMeta{id: callerIdentity{Kind: callerUnverified}, transport: "tcp"}
}

// promptAnswerVerdict is the decision on one prompt answer.
type promptAnswerVerdict struct {
	allowed bool
	refusal string
	// grantOff, worker: refusals the person's own wording covers
	// (refuseCaller) on routes that name their action.
	grantOff, worker bool
	// pane is the answering, traced pane.
	pane string
}

// decidePromptAnswer applies the rules to an agent caller (never the person)
// acting on a prompt in target (nil when there is no such pane, or the act
// names none).
func (a *apiServer) decidePromptAnswer(m mcpCallMeta, scoped bool, target *Pane) promptAnswerVerdict {
	if scoped || m.id.Kind == callerWorker {
		return promptAnswerVerdict{refusal: promptAnswerWorker, worker: true}
	}
	if !a.agentAuthority.active(time.Now()) {
		return promptAnswerVerdict{refusal: promptAnswerGrantOff, grantOff: true}
	}
	if m.id.Kind != callerPane || m.id.PaneID == "" || m.id.FromEnv || m.id.Forwarded || m.id.Relay {
		return promptAnswerVerdict{refusal: promptAnswerUntraced}
	}
	if target != nil && target.ID == m.id.PaneID {
		return promptAnswerVerdict{refusal: promptAnswerOwnPrompt}
	}
	return promptAnswerVerdict{allowed: true, pane: m.id.PaneID}
}

// promptRevID names a terminal pane's prompt by its input revision, which an
// answer's write moves on: read it before the write.
func promptRevID(p *Pane) string {
	return fmt.Sprintf("rev:%d", p.inputRevision.Load())
}

// approvalPromptID names a native pane's approval by its concrete request id.
func approvalPromptID(reqID string) string {
	return "approval:" + reqID
}

// approvalsPromptID names every approval an act answered at once.
func approvalsPromptID(reqIDs []string) string {
	ids := slices.Sorted(slices.Values(reqIDs))
	return approvalPromptID(strings.Join(ids, ","))
}

// logPromptAct is the audit line of an agent's act on a prompt under the
// grant: the caller kind, transport, answering pane, target pane, the prompt
// ("" when the act names none) and the act. prompt can hold a caller's text
// (a route's request id), so it is quoted and stays on one line.
func logPromptAct(kind callerKind, transport, pane, target, prompt, action string) {
	log.Printf("authority: prompt answer by agent caller=%s transport=%s pane=%s (traced) target=%s prompt=%q action=%q",
		kind, transport, pane, target, prompt, action)
}

// logPromptAnswer records an allowed act on the prompt named by prompt.
func logPromptAnswer(v promptAnswerVerdict, m mcpCallMeta, target *Pane, prompt, action string) {
	targetID := ""
	if target != nil {
		targetID = target.ID
	}
	logPromptAct(m.id.Kind, m.transport, v.pane, targetID, prompt, action)
}

// toolSucceeded: a tool result that isn't an error.
func toolSucceeded(res any) bool {
	m, ok := res.(map[string]any)
	return !ok || m["isError"] != true
}

// requirePromptAnswer gates a REST act that takes effect as soon as it is
// admitted; prompt names what it answers, for the audit line.
func (a *apiServer) requirePromptAnswer(w http.ResponseWriter, r *http.Request, p *Pane, action, prompt string) bool {
	note, ok := a.admitPromptAnswer(w, r, p, action)
	if ok {
		note(prompt)
	}
	return ok
}

// admitPromptAnswer gates a REST act on a prompt in p (nil when the act names
// no pane): the person, or an agent under the same rules as MCP answer_prompt.
// note records an agent's act: call it once the act has happened, with the
// prompt it actually answered, so a no-op or failure leaves no audit line.
func (a *apiServer) admitPromptAnswer(w http.ResponseWriter, r *http.Request, p *Pane, action string) (note func(prompt string), ok bool) {
	id := a.identifyCaller(r)
	if id.Kind == callerHuman {
		// The person, as requireHumanOrDelegated admitted them (a remote
		// device's view-only limits are enforced where it writes).
		a.logPersonAct(id, action)
		return func(string) {}, true
	}
	if id.Kind != callerPane && id.Kind != callerWorker {
		// Not an agent: the person's own refusal, with its device_required
		// code and guidance for an unapproved device.
		a.refuseCaller(w, id, action, true)
		return nil, false
	}
	m := mcpCallMeta{id: id, transport: "rest"}
	v := a.decidePromptAnswer(m, false, p)
	if !v.allowed {
		if v.grantOff || v.worker {
			a.refuseCaller(w, id, action, true)
			return nil, false
		}
		target := ""
		if p != nil {
			target = p.ID
		}
		log.Printf("authority: refused %s caller=%s pane=%s pid=%d target=%s", action, id.Kind, id.PaneID, id.PID, target)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "human_required", "caller": string(id.Kind), "message": "Can't " + action + ": " + v.refusal})
		return nil, false
	}
	return func(prompt string) { logPromptAnswer(v, m, p, prompt, action) }, true
}
