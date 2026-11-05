package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"soluterminal/grid"
)

// A cache entry is an observation of one live incarnation, never permission
// attached merely to a reusable pane ID or unchanged menu text.
type jevPaneIdentity struct {
	pane                                                                      *Pane
	pid                                                                       int
	created                                                                   time.Time
	cwd, kind, agent, provider, claude, codex, muse, harness, harnessProvider string
}

var errJevObservationChanged = errors.New("pane incarnation changed")

func captureJevPaneIdentity(p *Pane) (jevPaneIdentity, net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return jevPaneIdentityLocked(p), p.agentConn
}

func jevPaneIdentityLocked(p *Pane) jevPaneIdentity {
	return jevPaneIdentity{pane: p, pid: p.Pid, created: p.CreatedAt, cwd: p.Cwd, kind: p.Kind, agent: p.Agent, provider: p.Provider,
		claude: p.ClaudeSessionID, codex: p.CodexThreadID, muse: p.MuseSessionID, harness: p.HarnessSessionID, harnessProvider: p.HarnessProvider}
}

func (proof *jevAmbiguousProof) identityMatches(p *Pane) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return proof.identity.pid > 0 && !proof.identity.created.IsZero() && proof.connection != nil && proof.identity == jevPaneIdentityLocked(p) && proof.connection == p.agentConn &&
		p.Status == PaneRunning && !p.userClosing && !p.respawning.Load() && !p.codexLiveMigrating.Load() && !p.codexIdentityBlocked.Load()
}

func (proof *jevAmbiguousProof) processMatches(ctx context.Context) bool {
	if proof.probe == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, ariaJevTimeout)
	defer cancel()
	current, err := proof.probe(ctx, proof.identity.pid)
	return err == nil && current == proof.process
}

// The comparison and bare Enter form one transaction against the original
// manager member and captured transport, not a re-looked-up reusable pane ID.
func (a *Aria) writeJevEnter(ctx context.Context, p *Pane, proof *jevAmbiguousProof) (int, error) {
	if proof == nil || p == nil {
		return 0, errors.New("missing Jev observation")
	}
	hasCodex := false
	if proof.mode == "cautious" {
		// Capture the renderer hint before the input/grid transaction. The
		// incarnation checks inside the lease still bind the actual process.
		p.mu.Lock()
		hasCodex = p.HasCodexDescendant() || p.HasMuseDescendant()
		p.mu.Unlock()
	}
	var commits []string
	wrote := 0
	writeErr := errJevObservationChanged
	current, leaseErr := a.mgr.withCurrentPaneContext(ctx, p, func() {
		if err := lockPaneWriter(ctx, &p.agentWriteMu); err != nil {
			writeErr = err
			return
		}
		defer p.agentWriteMu.Unlock()
		if ctx.Err() != nil || !proof.validContext(ctx, p) {
			return
		}
		// agentWriteMu owns promptRec; do not reacquire it via the public
		// composer helper while holding the transaction's input lock.
		pending, recordedMs := p.promptRec.draft()
		if pending > 0 && recordedMs > 0 && time.Since(time.UnixMilli(recordedMs)) < ariaAutoDraftStaleAfter {
			writeErr = errors.New("composer has text")
			return
		}
		// readLoop holds this lock through buffer/grid updates. Keep the final
		// active-menu comparison and Enter together, without calling helpers
		// that reacquire subsMu or initialize an emulator inside the lease.
		if err := lockPaneReader(ctx, &p.subsMu); err != nil {
			writeErr = err
			return
		}
		defer p.subsMu.RUnlock()
		tail := ariaJevLockedPermissionTail(p, proof, hasCodex)
		evidence := ariaJevMenuEvidence(tail, proof.full)
		if evidence == "" || sha256.Sum256([]byte(evidence)) != proof.tailDigest || ariaJevScopeDecline(tail) != "" ||
			(proof.classified && !jevPermissionCandidateText(evidence, proof.full)) ||
			atomic.LoadInt64(&p.LastAckedChoicePromptPos) != proof.pos || atomic.LoadUint64(&p.GridChoicePromptGeneration) != proof.gridGeneration {
			return
		}
		if err := lockPaneWriter(ctx, &p.mu); err != nil {
			writeErr = err
			return
		}
		defer p.mu.Unlock()
		if !ariaAutoModePolicyMatchesLocked(ctx, p) {
			writeErr = errAutoModePolicyChanged
			return
		}
		lastInput := atomic.LoadInt64(&p.LastInputMs)
		if lastInput > p.AutoModeLastFiredMs && lastInput > 0 && time.Since(time.UnixMilli(lastInput)) < ariaAutoTypingSettle {
			writeErr = errors.New("user typing")
			return
		}
		if ctx.Err() != nil || p.AutoMode != proof.mode || proof.identity != jevPaneIdentityLocked(p) || proof.connection != p.agentConn || p.agentConn == nil || p.Status != PaneRunning || p.userClosing || p.respawning.Load() || p.codexLiveMigrating.Load() || p.codexIdentityBlocked.Load() {
			return
		}
		deadline := time.Now().Add(ariaJevTimeout)
		if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
			deadline = requestDeadline
		}
		policy := p.autoModePolicyLockedForAnswer(ctx)
		writeErr = p.writeAnswerFrameLocked(proof.connection, []byte("\r"), &policy, deadline, true)
		if writeErr == nil {
			wrote = 1
			commits = p.observeSuccessfulInputLocked([]byte("\r"), false, menuInputNone)
			// It answered an approval menu, not the input box.
			p.carryPutBackPastMenuKeyLocked(p.inputRevision.Load())
		}
	})
	if leaseErr != nil {
		return 0, leaseErr
	}
	if !current {
		return 0, errJevObservationChanged
	}
	if writeErr == nil {
		p.finishSuccessfulInput(commits)
	}
	return wrote, writeErr
}

// Caller holds subsMu. Matches the observation's source order exactly, with
// no recursive lock or lazy grid initialization during final submission.
func ariaJevLockedPermissionTail(p *Pane, proof *jevAmbiguousProof, hasCodex bool) string {
	if !proof.full {
		if proof.mode == "cautious" && p.gridEmu != nil {
			if text, ok := p.gridEmu.VisibleText(); ok && isGridChoicePromptText(text, hasCodex) {
				return cautiousCurrentDialogText(text, hasCodex)
			}
		}
		return tailOfPane(p, 30)
	}
	for _, em := range []*grid.Emulator{p.gridEmu, p.replayEmu} {
		if em != nil {
			if text, ok := snapshotVisibleText(em.Snapshot()); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	lines := strings.Split(stripANSI(p.buffer.SnapshotTail(16384)), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Trim only actual keyboard hints, never arbitrary parentheticals such as
// '(always)'. The scope guard is shared with local fallback and final Enter.
func ariaJevOnceLabel(label string) bool {
	label = normalizePromptText(label)
	if open := strings.LastIndex(label, " ("); open >= 0 && strings.HasSuffix(label, ")") {
		// Renderers may pad the key inside parentheses: "( y )".
		switch strings.TrimSpace(label[open+2 : len(label)-1]) {
		case "y", "a", "shift+tab", "enter", "return":
			label = label[:open]
		}
	}
	switch label {
	case "yes", "yes, proceed", "yes, apply it", "yes, just this once", "yes, allow once", "yes, allow this once", "allow once",
		"yes, grant these permissions for this turn", "yes, provide the requested info", "allow", "allow run the tool and continue", "allow run the tool and continue.":
		return true
	}
	return false
}

func ariaJevScopeDecline(tail string) string {
	allLines := strings.Split(tail, "\n")
	focus, offset, total := -1, 0, 0
	for i, line := range allLines {
		if ariaJevFocusedOptionRE.MatchString(line) {
			focus = i
			offset = total
		}
		total += len(line) + 1
	}
	if focus < 0 {
		return ""
	}
	row := menuOptionRow(allLines[focus])
	if row == nil || row[1] != "1" {
		return "focused option is not the first once/turn permission"
	}
	menu := parseAutoApprovalMenu(tail, offset)
	if reason := autoApprovalDenyReason(menu.header, menu.menu); reason != "" {
		return reason
	}
	lines := allLines[focus:]
	var label string
	for i, line := range lines {
		if row := menuOptionRow(line); row != nil {
			if i > 0 {
				break
			}
			label = row[2]
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		label += " " + strings.TrimSpace(line)
	}
	if label != "" && !ariaJevOnceLabel(label) {
		return "focused option is not an explicit once/turn permission"
	}
	return ""
}

// Only the bounded action and consecutive menu rows cross the provider
// boundary. Full-screen question/plan/billing denies run locally first.
func ariaJevProjectMenuLines(lines []string, start, focus int) string {
	end := focus + 1
	expected := 2
	for i := focus + 1; i < len(lines); i++ {
		if row := menuOptionRow(lines[i]); row != nil {
			if row[1] != strconv.Itoa(expected) {
				break
			}
			expected++
			end = i + 1
			if escapeOptionRE.MatchString(normalizePromptText(row[2])) {
				break
			}
			continue
		}
		if strings.TrimSpace(lines[i]) == "" {
			break // A separator closes the menu; later numbered output is not a continuation.
		}
		// A wrapped label belongs to the menu only when followed by its
		// next consecutive row, before any blank line or unrelated output.
		wrapped := false
		for j := i + 1; j < len(lines) && j <= i+8; j++ {
			if strings.TrimSpace(lines[j]) == "" {
				break
			}
			if row := menuOptionRow(lines[j]); row != nil {
				wrapped = row[1] == strconv.Itoa(expected)
				break
			}
		}
		if wrapped {
			continue
		}
		break
	}
	if expected < 3 {
		return ""
	}
	// Preserve recognized escape/confirmation footers, including short
	// wrapped footer fragments, but never arbitrary output below the menu.
	for i := end; i < len(lines); i++ {
		line := normalizePromptText(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "esc to cancel") || strings.HasPrefix(line, "press enter to confirm") || strings.HasPrefix(line, "enter to confirm") ||
			strings.HasPrefix(line, "enter to select") || line == "enter to" || line == "select" || line == "confirm" {
			end = i + 1
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}
