package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"soluterminal-shared/typesafe"
	"soluterminal/jev"
)

var ariaJevFocusedOptionRE = regexp.MustCompile(`(?m)^[\s\p{Zs}]*[❯❭›▶][\s\p{Zs}]*([0-9]{1,2})(?:\.|[\s\p{Zs}]{2,})`)

// Joining physical rows can attach a credential to an adjacent word. For
// this refusal-only check, recognize keyed prefixes even without word bounds.
var cautiousJoinedCredentialRE = regexp.MustCompile(strings.ReplaceAll(strings.Join([]string{
	reSkToken.String(), reAWSKey.String(), reGHToken.String(), reHFToken.String(),
}, "|"), `\b`, ""))

func cautiousSensitiveTail(tail string) bool {
	if redactTerminalTail(tail) != tail || opaqueSecretLike(tail) {
		return true
	}
	// Terminal rows lack soft-wrap metadata. Conservatively join nonblank
	// rows for safety checks only; keep blank separators and the exact text
	// sent to the evaluator. This also catches tokens split over many rows.
	var joined strings.Builder
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			joined.WriteByte('\n')
		} else {
			joined.WriteString(line)
		}
	}
	text := joined.String()
	return redactTerminalTail(text) != text || opaqueSecretLike(text) || cautiousJoinedCredentialRE.MatchString(text)
}

var ariaJevActionHeaderRE = regexp.MustCompile(`(?i)^(?:bash command|shell command|read file|edit file|write file|would you like to run the following command\?|would you like to make the following edits\?)$`)
var ariaJevToolActionHeaderRE = regexp.MustCompile(`(?i)^tool: [a-z][a-z0-9_.-]{0,39}$`)

const (
	ariaJevFullEnv        = "SOLU_JEV_ARIA_FULL"
	ariaJevAutoEnv        = "SOLU_JEV_ARIA_AUTO"
	ariaJevAutoMarker     = "jev_aria_auto.enabled"
	ariaJevModel          = jev.ApprovalModel
	ariaJevTimeout        = 4 * time.Second
	ariaJevMaxTailBytes   = 4096
	ariaJevDetectEnv      = "SOLU_JEV_ARIA_DETECT"
	ariaJevDetectMarker   = "jev_aria_detect.enabled"
	ariaJevDetectPanesEnv = "SOLU_JEV_ARIA_DETECT_PANES"
)

// The proof is private to one evaluation. Its full-tail digest binds the
// provider's answer to the action text as well as the Yes/No rows. The normal
// prompt fingerprint does not include most non-MCP action preambles.
type jevAmbiguousProof struct {
	dataDir        string
	mode           string
	pos            int64
	fingerprint    uint64
	tailDigest     [32]byte
	full           bool
	known          bool
	started        time.Time
	identity       jevPaneIdentity
	connection     net.Conn
	process        jevProcessIdentity
	probe          func(context.Context, int) (jevProcessIdentity, error)
	classified     bool
	gridGeneration uint64
}

type jevDetectCacheEntry struct {
	full           bool
	mode           string
	digest         [32]byte
	pos            int64
	fingerprint    uint64
	result         string // handled or fallback
	expires        time.Time
	identity       jevPaneIdentity
	connection     net.Conn
	process        jevProcessIdentity
	gridGeneration uint64
}

func (a *Aria) jevDetectCached(paneID string, proof *jevAmbiguousProof) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.jevDetectCache[paneID]
	if !ok || entry.gridGeneration != proof.gridGeneration || entry.identity != proof.identity || entry.connection != proof.connection || entry.process != proof.process || entry.full != proof.full || entry.mode != proof.mode || entry.digest != proof.tailDigest || entry.pos != proof.pos || entry.fingerprint != proof.fingerprint || time.Now().After(entry.expires) {
		delete(a.jevDetectCache, paneID)
		return "", false
	}
	return entry.result, true
}

func (a *Aria) jevDetectRemember(paneID string, proof *jevAmbiguousProof, result string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jevDetectCache == nil {
		a.jevDetectCache = map[string]jevDetectCacheEntry{}
	}
	// Each live pane needs at most one entry. Cap even if callers forget to
	// notify OnPaneExit, so short-lived panes cannot grow this indefinitely.
	if len(a.jevDetectCache) >= 512 {
		for id := range a.jevDetectCache {
			delete(a.jevDetectCache, id)
			break
		}
	}
	ttl := 10 * time.Minute
	if result == "fallback" {
		ttl = 30 * time.Second
	}
	a.jevDetectCache[paneID] = jevDetectCacheEntry{gridGeneration: proof.gridGeneration, identity: proof.identity, connection: proof.connection, process: proof.process, full: proof.full, mode: proof.mode, digest: proof.tailDigest, pos: proof.pos, fingerprint: proof.fingerprint, result: result, expires: time.Now().Add(ttl)}
}

func ariaJevActiveMenuEvidence(tail string) string {
	return ariaJevMenuEvidence(tail, false)
}

func ariaJevMenuEvidence(tail string, full bool) string {
	lines := strings.Split(tail, "\n")
	focus := -1
	for i, line := range lines {
		if ariaJevFocusedOptionRE.MatchString(line) {
			focus = i
		}
	}
	if focus < 0 {
		return ""
	}
	start := -1
	lookback := 14
	if full {
		lookback = focus // The byte bound still limits the provider payload.
	}
	bytes := 0
	for i := focus - 1; i >= 0 && i >= focus-lookback; i-- {
		line := strings.TrimSpace(lines[i])
		bytes += len(lines[i]) + 1
		if bytes > ariaJevMaxTailBytes {
			break
		}
		if isMenuOptionRow(line) {
			break
		}
		header := ariaJevActionHeaderRE.MatchString(line) || ariaJevToolActionHeaderRE.MatchString(line) || strings.HasPrefix(strings.ToLower(line), "suggestion:") || (full && ariaJevFullActionHeader(line))
		// Narrow panes wrap command/MCP headers across physical rows. Match
		// their joined text while retaining the original action in the proof.
		if full && !header && line != "" {
			joined := line
			for j := i + 1; j < focus && j <= i+2; j++ {
				if strings.TrimSpace(lines[j]) == "" {
					break
				}
				joined += " " + strings.TrimSpace(lines[j])
				if ariaJevActionHeaderRE.MatchString(joined) || ariaJevFullActionHeader(joined) {
					header = true
					break
				}
			}
		}
		if header {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	// The last visible grant row has no following Cancel row in a short
	// Codex viewport. Preserve its continuations and clipped footer so Jev
	// admission and the local fallback validate the same complete evidence.
	clipped := strings.TrimSpace(strings.Join(lines[start:], "\n"))
	if isCodexMcpClippedPermissionPromptText(clipped) {
		return clipped
	}
	return ariaJevProjectMenuLines(lines, start, focus)
}

// Keep menu footers intact: the generic log-tail helper trims short lines,
// including "Enter to select", which distinguishes questions from permissions.
// Prefer the rendered current screen over cursor-update fragments in scrollback.
func ariaJevPermissionTail(p *Pane, full bool) string {
	if !full {
		tail := tailOfPane(p, 30)
		if p.Snapshot().AutoMode == "cautious" {
			return cautiousPromptText(p, tail)
		}
		return tail
	}
	p.ensureGrid()
	if text, ok := p.gridVisibleText(); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	if text, ok := p.screenVisibleText(); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	lines := strings.Split(stripANSI(p.buffer.SnapshotTail(16384)), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (proof *jevAmbiguousProof) valid(p *Pane) bool {
	return proof.validContext(context.Background(), p)
}

func (proof *jevAmbiguousProof) validContext(ctx context.Context, p *Pane) bool {
	if ctx.Err() != nil {
		return false
	}
	if proof == nil || p == nil || !ariaJevPermissionEnabled(proof.dataDir, p.ID, proof.full) || p.Snapshot().AutoMode != proof.mode {
		return false
	}
	if !proof.identityMatches(p) || !proof.processMatches(ctx) {
		return false
	}
	tail := ariaJevPermissionTail(p, proof.full)
	evidence := ariaJevMenuEvidence(tail, proof.full)
	if ariaJevScopeDecline(tail) != "" || evidence == "" || sha256.Sum256([]byte(evidence)) != proof.tailDigest || (proof.classified && !jevPermissionCandidateText(evidence, proof.full)) {
		return false
	}
	// Capture and revalidate from the same rendered source. Claude's raw
	// stream contains partial cursor redraws, not complete screens: options
	// 1 and 4 can be the last output while all four options remain visible.
	// Comparing that fragment's hash to the rendered hash stranded valid
	// menus on every sweep. Keep the acknowledged instance and grid epoch
	// checks as well as the full action digest above and final locked write.
	_, pos, fp, ok := currentChoicePromptIdentity(p, tail)
	return ok && pos == proof.pos && fp == proof.fingerprint &&
		(!proof.full || atomic.LoadUint64(&p.GridChoicePromptGeneration) == proof.gridGeneration)
}

func (a *Aria) recordJevObservationHold(paneID, reason string) {
	a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "held", Reason: reason, Model: ariaJevModel})
	a.recordAutoEvalNoSurface(paneID)
}

func currentAutoApprovalPromptMatchesProof(p *Pane, proof *jevAmbiguousProof) bool {
	if p == nil {
		return false
	}
	if proof != nil {
		return proof.valid(p)
	}
	if ariaJevScopeDecline(ariaJevPermissionTail(p, true)) != "" {
		return false
	}
	_, ok := currentAutoApprovalPromptText(p)
	return ok
}

func ariaJevDetectEnabled(dataDir, paneID string) bool {
	if paneID == "" {
		return false
	}
	enabled := false
	switch strings.TrimSpace(os.Getenv(ariaJevDetectEnv)) {
	case "1":
		enabled = true
	case "0":
		return false
	default:
		if dataDir != "" {
			_, err := os.Stat(filepath.Join(dataDir, ariaJevDetectMarker))
			enabled = err == nil
		}
	}
	if !enabled {
		return false
	}
	allowed := strings.TrimSpace(os.Getenv(ariaJevDetectPanesEnv))
	if allowed == "" && dataDir != "" {
		b, err := os.ReadFile(filepath.Join(dataDir, ariaJevDetectMarker))
		if err == nil && len(b) <= 4096 {
			allowed = string(b)
		}
	}
	for _, id := range strings.FieldsFunc(allowed, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		if id == paneID {
			return true
		}
	}
	return false
}

// Full mode uses Jev first by default. This switch rolls back only that route;
// the separately scoped cautious/legacy pilot retains its existing policy.
func ariaJevFullEnabled() bool {
	return strings.TrimSpace(os.Getenv(ariaJevFullEnv)) != "0"
}

func ariaJevPermissionEnabled(dataDir, paneID string, full bool) bool {
	if full {
		return paneID != "" && ariaJevFullEnabled()
	}
	return ariaJevDetectEnabled(dataDir, paneID)
}

func ariaJevFullActionHeader(line string) bool {
	lower := strings.ToLower(line)
	return lower == "tool use" || lower == "fetch" || lower == "create file" ||
		(strings.HasPrefix(lower, "allow the ") && strings.Contains(lower, "mcp server")) ||
		(strings.HasPrefix(lower, "do you want to approve network access to ") && strings.HasSuffix(lower, "?"))
}

// Full mode classifies permission identity, not action risk. Only the selected
// accept-once row can be sent; unselected persistent options are not grants.
func jevPermissionCandidateText(tail string, full bool) bool {
	if !full {
		return jevAmbiguousCandidateText(tail)
	}
	return jevFullCandidateDecline(tail) == ""
}

// Stable reason codes expose local exclusions without logging terminal text.
func jevFullCandidateDecline(tail string) string {
	if tail == "" {
		return "missing_action_evidence"
	}
	if len(tail) > ariaJevMaxTailBytes {
		return "evidence_too_large"
	}
	if !ariaJevFocusedFirstOption(tail) {
		return "focused_option"
	}
	if redactTerminalTail(tail) != tail || opaqueSecretLike(tail) {
		return "sensitive_evidence"
	}
	reason := autoApprovalDeclineReason(tail, false)
	mcp := isCodexMcpPermissionPromptText(tail)
	if reason != "" && reason != "choice menu, not a permission prompt" && !mcp {
		return "nonpermission_menu"
	}
	choices := choicePromptMatches(tail)
	if len(choices) == 0 {
		return "missing_choice"
	}
	menu := parseAutoApprovalMenu(tail, choices[len(choices)-1][0])
	// Only the validated clipped MCP footer substitutes for a visible escape.
	if autoApprovalDenyReason(menu.header, menu.menu) != "" || len(menu.options) < 2 ||
		(!menuHasEscapeOption(menu) && !isCodexMcpClippedPermissionPromptText(tail)) {
		return "menu_structure"
	}
	if !ariaJevOnceLabel(menu.options[0]) || (strings.HasPrefix(menu.options[0], "allow") && !mcp) {
		return "permission_label"
	}
	// The action must belong to this menu, not a previous scrollback prompt.
	evidence := ariaJevMenuEvidence(tail, true)
	focus := ariaJevFocusedOptionRE.FindStringIndex(evidence)
	if focus == nil {
		return "missing_action_evidence"
	}
	lines := strings.Split(evidence[:focus[0]], "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if line != "" && !strings.HasSuffix(line, "?") && !strings.HasPrefix(lower, "reason:") &&
			!strings.HasPrefix(lower, "do you want") && !strings.HasPrefix(lower, "would you like") &&
			!strings.HasPrefix(line, "╌") && !strings.HasPrefix(line, "─") {
			return ""
		}
	}
	return "missing_action_payload"
}

// jevAmbiguousCandidateText admits only the approval-language miss. The
// structural deny, focused accept-once row, and explicit escape still hold.
// A model cannot reinterpret a plan, selector, billing, login, or persistent
// grant menu as a permission.
func jevAmbiguousCandidateText(tail string) bool {
	if tail == "" || len(tail) > ariaJevMaxTailBytes ||
		autoApprovalDeclineReason(tail, false) != "choice menu, not a permission prompt" ||
		!ariaJevFocusedFirstOption(tail) || !ariaJevHasActionContext(tail) ||
		redactTerminalTail(tail) != tail || opaqueSecretLike(tail) {
		return false
	}
	choices := choicePromptMatches(tail)
	if len(choices) == 0 {
		return false
	}
	menu := parseAutoApprovalMenu(tail, choices[len(choices)-1][0])
	if autoApprovalDenyReason(menu.header, menu.menu) != "" || len(menu.options) < 2 || !menuHasEscapeOption(menu) {
		return false
	}
	// Narrower than the regular classifier: avoid all session/persistent
	// grants in the model-assisted detector, including unfamiliar variants.
	if menu.options[0] != "yes" && menu.options[0] != "yes, proceed" && menu.options[0] != "yes, just this once" {
		return false
	}
	return cautiousAutoDenyReasonWithJevProof(tail, "1\n", true) == ""
}

// tryJevAmbiguousTerminal runs Jev first for full-mode permission menus,
// including known shapes. Cautious mode retains the exact-pane pilot.
func (a *Aria) tryJevAmbiguousTerminal(ctx context.Context, p *Pane, paneID, paneTitle string, started time.Time) bool {
	if p == nil || p.Snapshot().Kind == "native" {
		return false
	}
	mode := p.Snapshot().AutoMode
	if mode != "always" && mode != "cautious" {
		return false
	}
	ctx, _ = withAriaAutoModePolicy(ctx, p)
	if mode == "cautious" && a.cautiousPromptCancelled(p) {
		return true
	}
	full := mode == "always" && ariaJevFullEnabled()
	if !ariaJevPermissionEnabled(a.dataDir, paneID, full) {
		return false
	}
	tail := ariaJevPermissionTail(p, full)
	if mode == "cautious" && (cautiousSensitiveTail(tail) || cautiousDialogHasElision(tail)) {
		return false
	}
	if full {
		if reason := ariaJevScopeDecline(tail); reason != "" {
			// Do not let a shorter, status-trimmed fallback erase a question
			// footer or another explicit deny seen on the current screen.
			a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: reason, Model: "direct"})
			a.recordDirectDecline(paneID, p)
			return true
		}
	}
	evidence := ariaJevMenuEvidence(tail, full)
	if full && evidence != "" && (redactTerminalTail(evidence) != evidence || opaqueSecretLike(evidence)) {
		// This is a provider privacy boundary, not a command-risk veto in
		// Always Approve. Keep raw action text local and let the existing
		// deterministic once-only route revalidate recognized permissions,
		// including its current-menu, typing and transport guards. Unfamiliar
		// menus cannot acquire approval merely by containing sensitive text.
		if isAutoApprovalPromptText(tail, p.HasCodexDescendant() || p.HasMuseDescendant()) {
			a.ariaJevFullAudit("private_action_local_fallback", true, 0, 0, 0)
			return false
		}
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: "sensitive permission action", Model: "direct"})
		a.recordDirectDecline(paneID, p)
		return true
	}
	// In Always Approve, a permission the deterministic classifier already
	// recognizes fires whatever Jev answers: positive fires, and uncertain or
	// provider failure falls back to the same local route. Asking first only
	// added the provider round trip plus repeated process-identity probes
	// (median ~0.6s, up to 3s per approval; 2026-10-14 all 192 Jev fires were
	// known prompts). Jev decides only menus the local classifier misses.
	if full && isAutoApprovalPromptText(tail, p.HasCodexDescendant() || p.HasMuseDescendant()) {
		a.ariaJevFullAudit("known_local", true, 0, 0, 0)
		return false
	}
	if !jevPermissionCandidateText(evidence, full) {
		if full {
			a.ariaJevFullAudit("precheck_fallback", false, 0, 0, 0)
			reason := jevFullCandidateDecline(evidence)
			if a.metrics != nil {
				a.metrics.IncLabelled("solud_aria_jev_precheck_total", map[string]string{"reason": reason})
			}
			log.Printf("aria_jev_full_precheck reason=%s", reason)
		}
		return false
	}
	_, pos, fp, ok := currentChoicePromptIdentity(p, tail)
	if !ok || fp == 0 || pos == 0 {
		return false
	}
	identity, conn := captureJevPaneIdentity(p)
	proof := &jevAmbiguousProof{identity: identity, connection: conn, dataDir: a.dataDir, mode: mode, pos: pos, fingerprint: fp, tailDigest: sha256.Sum256([]byte(evidence)), full: full, known: isAutoApprovalPromptText(tail, p.HasCodexDescendant() || p.HasMuseDescendant()), started: started}
	proof.gridGeneration = atomic.LoadUint64(&p.GridChoicePromptGeneration)
	proof.probe = a.jevProcessProbe
	if proof.probe == nil {
		proof.probe = probeJevProcessIdentity
	}
	processCtx, processCancel := context.WithTimeout(ctx, ariaJevTimeout)
	process, processErr := proof.probe(processCtx, identity.pid)
	proof.process = process
	processCancel()
	if processErr != nil {
		if errors.Is(processErr, errJevProcessUnsupported) {
			// No provider request has happened, so an unsupported platform may
			// retain strictly local recognition. Never downgrade an already
			// captured identity after a call, cancellation, or stale response.
			if full {
				a.ariaJevFullAudit("process_unsupported_fallback", proof.known, 0, 0, 0)
			}
			return false
		}
		a.recordJevObservationHold(paneID, "approval process identity unavailable; retrying")
		return true
	}
	if !proof.valid(p) {
		a.recordJevObservationHold(paneID, "approval observation changed; retrying")
		return true
	}
	if strings.TrimSpace(a.dataDir) == "" {
		return a.jevRecognizedFallback(ctx, p, paneID, paneTitle, proof)
	}
	// A paused, capped, or user-held pane cannot fire. Keep those checks
	// before the provider call, including in always mode, so the detector
	// does not spend a model request for a suppressed approval.
	if allowed, reason := a.autoAllowed(); !allowed {
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "skipped_backoff", Reason: reason, Model: "direct"})
		return true
	}
	if hold, reason := autoFireUserHold(p, time.Now()); hold {
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "held", Reason: reason, Model: "direct"})
		return true
	}
	if result, ok := a.jevDetectCached(paneID, proof); ok {
		if result == "positive" {
			proof.classified = true
			return a.fireJevAmbiguousTerminal(ctx, p, paneID, paneTitle, started, proof)
		}
		if result == "terminal" {
			return true
		}
		return a.jevRecognizedFallback(ctx, p, paneID, paneTitle, proof)
	}
	client, source, configured := a.jevApprovalClient("soluterminal-aria-jev-detect/1")
	if !configured {
		if full {
			a.ariaJevFullAudit("credential_fallback", proof.known, 0, 0, 0)
		}
		a.jevDetectRemember(paneID, proof, "fallback")
		return a.jevRecognizedFallback(ctx, p, paneID, paneTitle, proof)
	}
	if client == nil {
		if full {
			a.ariaJevFullAudit("provider_fallback", proof.known, 0, 0, 0)
		}
		a.jevDetectRemember(paneID, proof, "fallback")
		return a.jevRecognizedFallback(ctx, p, paneID, paneTitle, proof)
	}
	callCtx, cancel := context.WithTimeout(ctx, ariaJevTimeout)
	callStarted := time.Now()
	questions := jev.PermissionMenuQuestions(mode == "cautious")
	if full {
		questions = jev.FullPermissionQuestions()
	}
	response, err := client.SystemOneModel(callCtx, ariaJevModel, evidence, questions)
	latency := time.Since(callStarted).Milliseconds()
	cancel()
	inputTokens := int64(-1) // A request happened; absent usage is unknown, not free.
	priced := false
	if response.InputUsageKnown() {
		inputTokens = response.Usage.InputTokens
		priced = response.Model == ariaJevModel
	}
	if ctx.Err() != nil {
		if full {
			a.ariaJevFullAudit("cancelled", proof.known, latency, inputTokens, 0, priced)
		}
		return true
	}
	if !proof.valid(p) {
		if full {
			a.ariaJevFullAudit("stale", proof.known, latency, inputTokens, 0, priced)
		}
		a.ariaJevDetectAudit("stale", latency, inputTokens, priced)
		a.recordAutoEvalNoSurface(paneID)
		return true
	}
	if err != nil || response == nil || response.Model != ariaJevModel || !response.InputUsageKnown() {
		outcome := a.jevProviderFailureOutcome(source, err)
		if full {
			a.ariaJevFullAudit(outcome, proof.known, latency, inputTokens, 0, priced)
		}
		a.ariaJevDetectAudit(outcome, latency, inputTokens, priced)
		a.jevDetectRemember(paneID, proof, "fallback")
		return a.jevRecognizedFallback(ctx, p, paneID, paneTitle, proof)
	}
	probability, _ := response.Answers["permission_menu"].NoulValue()
	permission, err := jev.InterpretPermissionMenu(response, jev.ApprovalThreshold)
	if err != nil || !permission {
		if full {
			a.ariaJevFullAudit("uncertain_fallback", proof.known, latency, response.Usage.InputTokens, probability)
		}
		a.ariaJevDetectAudit("uncertain_fallback", latency, response.Usage.InputTokens)
		a.jevDetectRemember(paneID, proof, "negative")
		return a.jevRecognizedFallback(ctx, p, paneID, paneTitle, proof)
	}
	if mode == "cautious" {
		safe, riskErr := jev.InterpretApproval(response, jev.ApprovalThreshold)
		if riskErr != nil || !safe {
			a.ariaJevDetectAudit("risk_fallback", latency, response.Usage.InputTokens)
			a.jevDetectRemember(paneID, proof, "negative")
			a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: "Jev did not verify cautious action risk", Model: ariaJevModel, LatencyMs: latency})
			a.recordDirectDecline(paneID, p)
			return true
		}
	}
	if full {
		a.ariaJevFullAudit("candidate", proof.known, latency, response.Usage.InputTokens, probability)
	}
	a.ariaJevDetectAudit("candidate", latency, response.Usage.InputTokens)
	a.jevDetectRemember(paneID, proof, "positive")
	proof.classified = true
	return a.fireJevAmbiguousTerminal(ctx, p, paneID, paneTitle, started, proof)
}

func (a *Aria) jevRecognizedFallback(ctx context.Context, p *Pane, paneID, paneTitle string, proof *jevAmbiguousProof) bool {
	if !proof.full {
		a.recordAutoEvalNoSurface(paneID)
		return true
	}
	if ctx.Err() != nil || !proof.valid(p) {
		return true
	}
	if !isAutoApprovalPromptText(ariaJevPermissionTail(p, true), p.HasCodexDescendant() || p.HasMuseDescendant()) {
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: "no verified permission or guarded local recognition", Model: "direct"})
		a.recordDirectDecline(paneID, p)
		return true
	}
	// Observation validity is NOT model permission. Unknown menus retain the
	// human gate; only locally recognized permissions use fallback.
	proof.classified = false
	a.directAutoFireWithJevProof(ctx, paneID, paneTitle, proof)
	return true
}

func (a *Aria) fireJevAmbiguousTerminal(ctx context.Context, p *Pane, paneID, paneTitle string, started time.Time, proof *jevAmbiguousProof) bool {
	if !proof.valid(p) {
		return true
	}
	mode, pos, fp := proof.mode, proof.pos, proof.fingerprint
	if mode == "always" {
		a.directAutoFireWithJevProof(ctx, paneID, paneTitle, proof)
		return true
	}
	var fired, held bool
	emitUI := a.autoEmitter(paneID, paneTitle, "automode_cautious_jev_detect", ariaAutoCountdownMs)
	emit := func(evt map[string]any) {
		switch evt["type"] {
		case "confirm_resolved":
			approved, _ := evt["approved"].(bool)
			_, failed := evt["error"]
			fired = approved && !failed
		case "auto-hold":
			held = true
		}
		emitUI(evt)
	}
	emit(map[string]any{"type": "auto-eval-start", "pane_id": paneID, "pane_title": paneTitle, "order_id": "automode_cautious_jev_detect", "instruction": "Jev classified focused permission and risk"})
	result := a.toolSendInputWithJevProof(ctx, "aria_jev_detect_approval", paneID, "1\n", pos, fp, true, proof, emit)
	emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
	if fired {
		a.jevDetectRemember(paneID, proof, "terminal")
		a.recordAutoEvalOutcome(paneID, true)
		a.recordEvalOutcome(AriaEvalOutcome{PaneID: paneID, Kind: "fired", Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds(), ToolCalls: 1, SendInput: "↵"})
	} else if held {
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "held", Reason: "user input hold", Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds()})
	} else {
		if strings.HasPrefix(result, "user rejected") {
			a.jevDetectRemember(paneID, proof, "terminal")
		}
		a.recordAutoEvalOutcome(paneID, false)
		a.recordEvalOutcome(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: truncateStr(result, 240), Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds(), ToolCalls: 1})
	}
	return true
}

// Stable cohort/outcome counters compare known permissions with permissions
// missed by the deterministic classifier. Logs contain no terminal content.
func (a *Aria) ariaJevFullAudit(outcome string, known bool, latencyMs, inputTokens int64, probability float64, priced ...bool) {
	cohort := "novel"
	if known {
		cohort = "known"
	}
	if a.metrics != nil {
		a.metrics.IncLabelled("solud_aria_jev_full_total", map[string]string{"outcome": outcome, "cohort": cohort})
	}
	log.Printf("aria_jev_full model=%s cohort=%s outcome=%s latency_ms=%d input_tokens=%d permission=%.4f estimated_cost_usd=%s", ariaJevModel, cohort, outcome, latencyMs, inputTokens, probability, ariaJevCostEstimate(inputTokens, priced))
}

func (a *Aria) ariaJevDetectAudit(outcome string, latencyMs, inputTokens int64, priced ...bool) {
	if a.metrics != nil {
		a.metrics.IncLabelled("solud_aria_jev_detect_total", map[string]string{"outcome": outcome})
	}
	log.Printf("aria_jev_detect model=%s outcome=%s latency_ms=%d input_tokens=%d estimated_cost_usd=%s", ariaJevModel, outcome, latencyMs, inputTokens, ariaJevCostEstimate(inputTokens, priced))
}

func ariaJevCostEstimate(tokens int64, priced []bool) string {
	if tokens < 0 || (len(priced) != 0 && !priced[0]) {
		return "unknown"
	}
	return fmt.Sprintf("%.8f", typesafe.Usage{InputTokens: tokens}.EstimatedCostUSD())
}

// tryJevCautiousTerminal uses Jev only to classify the risk of the already
// focused permission. It never lets a model choose an input or a target pane.
// A false return means the incumbent ARIA evaluator handles this observation.
// For team installs the fast path is opt-in, and native approvals retain
// their existing path.
func (a *Aria) tryJevCautiousTerminal(ctx context.Context, p *Pane, paneID, paneTitle, observationText string, started time.Time) bool {
	handled, _ := a.jevCautiousDecide(ctx, p, paneID, paneTitle, observationText, started, false)
	return handled
}

// jevCautiousDecide is the cautious Jev decision. required is set for
// non-team installs, where Jev is the only cautious evaluator: it runs
// without the pilot marker, and a false return means the human answers.
// fallback is the audit outcome that explains a false return.
func (a *Aria) jevCautiousDecide(ctx context.Context, p *Pane, paneID, paneTitle, observationText string, started time.Time, required bool) (handled bool, fallback string) {
	if p == nil || p.Snapshot().AutoMode != "cautious" {
		return false, "not_cautious"
	}
	ctx, policy := withAriaAutoModePolicy(ctx, p)
	// Cancellation belongs to the permission, not a provider/pilot precheck.
	// Moving focus or disabling Jev must not hand it to a second evaluator.
	if a.cautiousPromptCancelled(p) {
		return true, ""
	}
	if required {
		if ariaJevAutoKilled() {
			return false, "disabled_fallback"
		}
	} else if !ariaJevAutoEnabled(a.dataDir) {
		return false, "disabled_fallback"
	}
	fail := func(outcome string, latencyMs, tokens int64, probability float64) (bool, string) {
		a.ariaJevAudit(outcome, latencyMs, tokens, probability)
		return false, outcome
	}
	// Use the exact current menu the final tool gate checks. The deterministic gate
	// runs before any pane output leaves the machine for TypeSafe, excluding
	// obvious secret/system/network requests from this second provider.
	tail := cautiousPromptText(p, tailOfPane(p, 30))
	if tail == "" {
		return fail("empty_tail_fallback", 0, 0, 0)
	}
	if len(tail) > ariaJevMaxTailBytes {
		return fail("long_tail_fallback", 0, 0, 0)
	}
	if cautiousAutoDenyReason(tail, "1\n") != "" {
		return fail("denylist_fallback", 0, 0, 0)
	}
	if !ariaJevFocusedFirstOption(tail) || !ariaJevHasActionContext(tail) || cautiousDialogHasElision(tail) {
		return fail("precheck_fallback", 0, 0, 0)
	}
	// The cautious deny list catches explicit secret words and paths. The
	// terminal redactor catches credential-shaped values that may appear in
	// otherwise harmless scrollback. A conservative opaque-string check also
	// holds bare tokens, at the cost of falling back on some commit SHAs.
	// Skip Jev rather than send a redacted version whose missing context
	// could distort its safety decision.
	if cautiousSensitiveTail(tail) {
		return fail("sensitive_tail_fallback", 0, 0, 0)
	}
	_, promptPos, promptHash, ok := currentChoicePromptIdentity(p, tail)
	if !ok || promptHash == 0 {
		return fail("identity_fallback", 0, 0, 0)
	}
	if outcome, seen := a.jevCautiousSeen(paneID, promptPos, promptHash); seen {
		// A user's cancellation must survive later watcher observations too,
		// including the optional team fast path: never fall through to ARIA
		// and ask another model to approve this same cancelled permission.
		if outcome == "cancelled" {
			return true, ""
		}
		// One Jev answer per permission: a "not sure" stays with the human,
		// and the sweeper re-checking the same menu must not spend the
		// account's daily allowance again.
		if required {
			return false, outcome
		}
	}
	if strings.TrimSpace(a.dataDir) == "" {
		return fail("credential_fallback", 0, 0, 0)
	}
	client, source, configured := a.jevApprovalClient("soluterminal-aria-jev/1")
	if !configured {
		if reason := a.jevCloud.closedReason(time.Now()); reason != "" {
			return fail(reason, 0, 0, 0)
		}
		return fail("credential_fallback", 0, 0, 0)
	}
	if client == nil {
		return fail("provider_fallback", 0, 0, 0)
	}
	callCtx, cancel := context.WithTimeout(ctx, ariaJevTimeout)
	callStarted := time.Now()
	response, err := client.SystemOneModel(callCtx, ariaJevModel, tail, jev.ApprovalQuestions())
	latency := time.Since(callStarted).Milliseconds()
	cancel()
	var billedTokens int64
	var probability float64
	if response != nil && response.Usage.InputTokens > 0 {
		billedTokens = response.Usage.InputTokens
	}
	if response != nil {
		probability, _ = response.Answers["safe_action"].NoulValue()
	}
	if policy.policy.mode != "cautious" || !policy.matches(p) {
		a.ariaJevAudit("policy_changed_no_action", latency, billedTokens, probability)
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "held", Reason: "approval policy changed", Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds()})
		return true, ""
	}
	// A changed menu must never be evaluated from the old observation by
	// either model, regardless of whether Jev answered, abstained, or errored.
	currentTail := cautiousPromptText(p, tailOfPane(p, 30))
	if !choicePromptIdentityMatches(p, promptPos, promptHash) || !ariaJevFocusedFirstOption(currentTail) || !ariaJevHasActionContext(currentTail) || cautiousDialogHasElision(currentTail) || cautiousAutoDenyReason(currentTail, "1\n") != "" {
		a.ariaJevAudit("stale_no_action", latency, billedTokens, probability)
		a.recordAutoEvalNoSurface(paneID)
		a.recordEvalOutcome(AriaEvalOutcome{PaneID: paneID, Kind: "stale", Reason: "permission changed during Jev evaluation", Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds()})
		return true, ""
	}
	if err != nil || response == nil || response.Model != ariaJevModel || response.Usage.InputTokens < 0 {
		return fail(a.jevProviderFailureOutcome(source, err), latency, billedTokens, 0)
	}
	approved, err := jev.InterpretApproval(response, jev.ApprovalThreshold)
	if err != nil {
		a.rememberJevCautiousForPolicy(policy, promptPos, promptHash, "invalid_fallback")
		return fail("invalid_fallback", latency, response.Usage.InputTokens, probability)
	}
	if !approved {
		a.rememberJevCautiousForPolicy(policy, promptPos, promptHash, "uncertain_fallback")
		return fail("uncertain_fallback", latency, response.Usage.InputTokens, probability)
	}
	a.ariaJevAudit("candidate", latency, response.Usage.InputTokens, probability)
	var fired, held bool
	var heldReason string
	emitUI := a.autoEmitter(paneID, paneTitle, "automode_cautious_jev", ariaAutoCountdownMs)
	emit := func(evt map[string]any) {
		switch evt["type"] {
		case "confirm_resolved":
			approved, _ := evt["approved"].(bool)
			_, failed := evt["error"]
			fired = approved && !failed
		case "auto-hold":
			held = true
			heldReason, _ = evt["reason"].(string)
		}
		emitUI(evt)
	}
	emit(map[string]any{"type": "auto-eval-start", "pane_id": paneID, "pane_title": paneTitle, "order_id": "automode_cautious_jev", "instruction": "Jev classified focused permission as low risk"})
	// Fixed option 1. Existing toolSendInput reduces this to a bare Enter
	// only for a still-focused choice prompt; the classifier has already
	// proved that row is the scoped approval. The tool owns the cancellable
	// countdown, final cap/hold/identity checks, and PTY write.
	result := a.toolSendInput(ctx, "aria_jev_approval", paneID, "1\n", promptPos, promptHash, true, emit)
	emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
	if fired {
		a.ariaJevFinal("fired")
		a.recordAutoEvalOutcome(paneID, true)
		a.recordEvalOutcome(AriaEvalOutcome{PaneID: paneID, Kind: "fired", TailSnapshot: observationText, Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds(), ToolCalls: 1, SendInput: "↵"})
		return true, ""
	}
	if !policy.matches(p) {
		held, heldReason = true, "approval policy changed"
	}
	if held {
		a.ariaJevFinal("held")
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "held", Reason: heldReason, Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds()})
		return true, ""
	}
	// A failed/cancelled confirmation is terminal for this observation: do
	// not ask another model to approve a menu the user just cancelled.
	if strings.HasPrefix(result, "user rejected") {
		a.rememberJevCautiousForPolicy(policy, promptPos, promptHash, "cancelled")
		a.ariaJevFinal("cancelled")
	} else {
		a.ariaJevFinal("not_fired")
	}
	a.recordAutoEvalOutcome(paneID, false)
	a.recordEvalOutcome(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: truncateStr(result, 240), TailSnapshot: observationText, Model: ariaJevModel, LatencyMs: time.Since(started).Milliseconds(), ToolCalls: 1})
	return true, ""
}

// A stale focused option 1 in scrollback must not justify pressing Enter on
// a newer menu whose cursor is on option 2 (or any other row). The latest
// focus marker is the one the terminal will submit.
func ariaJevFocusedFirstOption(tail string) bool {
	matches := ariaJevFocusedOptionRE.FindAllStringSubmatch(tail, -1)
	return len(matches) > 0 && matches[len(matches)-1][1] == "1"
}

// Generic approval rows alone do not identify the action being granted.
// Require an action descriptor and payload in the current menu's preamble;
// if scrollback clipped the descriptor, ARIA keeps the decision. A prior
// menu's descriptor cannot authorize a newer menu after its option rows.
func ariaJevHasActionContext(tail string) bool {
	focus := ariaJevFocusedOptionRE.FindAllStringIndex(tail, -1)
	if len(focus) == 0 {
		return false
	}
	lines := strings.Split(tail[:focus[len(focus)-1][0]], "\n")
	if len(lines) > 14 {
		lines = lines[len(lines)-14:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if isMenuOptionRow(line) {
			return false
		}
		if strings.HasPrefix(strings.ToLower(line), "suggestion:") {
			return len(strings.TrimSpace(line[len("suggestion:"):])) >= 12
		}
		if ariaJevActionHeaderRE.MatchString(line) || ariaJevToolActionHeaderRE.MatchString(line) {
			for _, payload := range lines[i+1:] {
				payload = strings.TrimSpace(payload)
				if isMenuOptionRow(payload) {
					return false
				}
				if payload != "" && !strings.HasSuffix(payload, "?") && !strings.HasPrefix(strings.ToLower(payload), "do you want") && !strings.HasPrefix(strings.ToLower(payload), "would you like") && !strings.HasPrefix(payload, "╌") {
					return true
				}
			}
			return false
		}
	}
	return false
}

// ariaJevAutoKilled is the kill switch for non-team installs, where Jev is
// the cautious evaluator by default: SOLU_JEV_ARIA_AUTO=0.
func ariaJevAutoKilled() bool {
	return strings.TrimSpace(os.Getenv(ariaJevAutoEnv)) == "0"
}

func ariaJevAutoEnabled(dataDir string) bool {
	switch strings.TrimSpace(os.Getenv(ariaJevAutoEnv)) {
	case "1":
		return true
	case "0":
		return false
	}
	if dataDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dataDir, ariaJevAutoMarker))
	return err == nil
}

// ariaJevAudit deliberately records no pane text or credential. A bounded
// label set supports live rates; the log carries latency, tokens and estimated
// list-price cost for a direct comparison with incumbent ARIA usage.
func (a *Aria) ariaJevAudit(outcome string, latencyMs, inputTokens int64, probability float64) {
	if a.metrics != nil {
		a.metrics.IncLabelled("solud_aria_jev_decision_total", map[string]string{"outcome": outcome})
	}
	log.Printf("aria_jev_auto model=%s outcome=%s latency_ms=%d input_tokens=%d estimated_cost_usd=%.8f safe_action=%.4f", ariaJevModel, outcome, latencyMs, inputTokens, typesafe.Usage{InputTokens: inputTokens}.EstimatedCostUSD(), probability)
}

func (a *Aria) ariaJevFinal(outcome string) {
	if a.metrics != nil {
		a.metrics.IncLabelled("solud_aria_jev_final_total", map[string]string{"outcome": outcome})
	}
	log.Printf("aria_jev_auto_final model=%s outcome=%s", ariaJevModel, outcome)
}
