package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Scoped MCP capability tokens.
//
// The /mcp endpoint normally grants the FULL tool surface to any authed
// caller (tokenless loopback, or the daemon token). That is right for
// interactive panes — a human is in the loop and Claude Code's own
// permission prompts gate each call — and wrong for daemon-spawned
// judgment runs (the Warden): a headless child's tool budget must be
// enforced by the daemon, not by prompt text, or drift and prompt
// injection walk straight through it.
//
// A scope is minted in-process only (never over HTTP), handed to the
// child via the Authorization header in its --mcp-config entry, and
// enforced in mcpDispatch: tools/list is filtered to the scope and
// tools/call refuses anything outside it. Tokens are random, TTL-bound,
// and revoked when the run ends. Presenting a scoped token RESTRICTS the
// caller even on loopback — the scope lookup runs before the loopback
// allowance, which is the whole point: the spawned child is loopback too.
type mcpScope struct {
	// Identity names the caller class ("warden") — stamped as provenance
	// on artifacts the caller creates (pane-flag owner) so guards like
	// "only clear your own flags" have something to check.
	Identity string
	// A Warden triage capability is bound to exactly one pane and attention
	// episode. Other Warden runs leave these empty and cannot write flags.
	WardenPaneID  string
	WardenEpisode string
	// Tools is the exact allowed tool-name set. Nothing outside it is
	// visible (tools/list) or callable (tools/call).
	Tools map[string]bool
	// Expires ends the scope's life even if Revoke is never called —
	// a crashed run must not leave a live capability behind.
	Expires time.Time

	// ---------------------------------------------------------------------
	// Managed-agent fields (docs/DESIGN-agents-surface.md §3.2). Every one is
	// ADDITIVE and optional: a scope minted by the Warden leaves them zero,
	// which is why Class=="" reads as "warden/legacy" everywhere below and the
	// Warden's behavior is byte-identical to before this slice.
	// ---------------------------------------------------------------------

	// Class is "" (warden/legacy) or "agent". It is the switch that merges
	// agentMcpTools() into tools/list and turns on the dispatch hard-deny; no
	// agent_* tool is ever visible or callable without it.
	Class string
	// AgentID/RunID are the provenance stamped on every engine event and audit
	// row an agent-class call produces (§3.5).
	AgentID string
	RunID   string
	// TemplateID identifies the policy ceiling that minted this scope. PaneRole
	// is set only for interactive role scopes; together they distinguish the
	// proof-gated Orchestrator recovery contract from scheduled managed agents.
	TemplateID string
	PaneRole   string
	// RepoScope is the agent's absolute repository path. "" means no repo-bound
	// tool may run at all — every grantable tool compares its target task's
	// repository against this and refuses outside it (§3.4 step 2).
	RepoScope  string
	RepoScopes []string
	// Grants is the agent's grant-id set, copied from the SAME array the card
	// renders (§2.4). Tools is derived from it at mint time; dispatch
	// re-derives from the registry rather than trusting Tools (§3.4).
	Grants map[string]bool
	// Caps are the per-run/per-task blast bounds (§3.6).
	Caps struct {
		FileTasks                int // per run
		ResumesPerTask           int
		RetriesPerRun            int
		RetiresPerRun            int
		ResolvesPerRun           int
		ArchivesPerRun           int
		LinkRefilesPerRun        int
		ProposalsPerRun          int
		DependencyReleasesPerRun int
	}

	// callMu guards the mutable per-run counters below. A run is one child
	// process, but nothing stops it issuing concurrent tool calls, and a cap
	// that can be raced is not a cap.
	callMu        sync.Mutex
	usedFileTasks int
	usedRetries   int
	// retryCapOwner lets a recovery-only view share the cached pane/repository
	// counter without exposing that cached scope's read or bookkeeping tools.
	retryCapOwner          *mcpScope
	usedRetires            int
	usedResolves           int
	usedArchives           int
	usedLinkRefiles        int
	usedProposals          int
	usedDependencyReleases int
	inspected              map[string]bool
}

// takeRetry reserves one fresh-successor attempt. Zero fails closed, and the
// same mutex used by every other per-run cap prevents concurrent calls from
// exceeding the pane-run allowance.
func (sc *mcpScope) takeRetry() bool {
	if sc == nil {
		return false
	}
	owner := sc
	if sc.retryCapOwner != nil {
		owner = sc.retryCapOwner
	}
	owner.callMu.Lock()
	defer owner.callMu.Unlock()
	if owner.Caps.RetriesPerRun <= 0 || owner.usedRetries >= owner.Caps.RetriesPerRun {
		return false
	}
	owner.usedRetries++
	return true
}

func (sc *mcpScope) allows(tool string) bool {
	return sc != nil && sc.Tools[tool]
}

// requireGrant is the first thing every agent tool handler does (§3.4 step 1).
// A non-agent scope can never hold a grant — the error names the grant so the
// model stops retrying and the denial row reads as a sentence.
func (sc *mcpScope) requireGrant(id string) error {
	if sc == nil || sc.Class != mcpScopeClassAgent {
		return errors.New("grant " + id + " requires a managed-agent capability token")
	}
	if !sc.Grants[id] {
		return errors.New("this agent does not hold the " + id + " grant")
	}
	return nil
}

// grantIDs returns the scope's grants as a slice, for re-deriving the granted
// tool set from the registry at call time.
func (sc *mcpScope) grantIDs() []string {
	if sc == nil {
		return nil
	}
	out := make([]string, 0, len(sc.Grants))
	for id, on := range sc.Grants {
		if on {
			out = append(out, id)
		}
	}
	return out
}

// takeFileTaskWithOptionalLinkRefile reserves all allowances a filing needs in
// one critical section. Validation failures after the engine call still spend
// the reservation, but a repo-scope or either-cap denial spends neither cap.
func (sc *mcpScope) takeFileTaskWithOptionalLinkRefile(linkRefile bool) (fileAllowed, linkAllowed bool) {
	if sc == nil {
		return false, !linkRefile
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.usedFileTasks >= sc.Caps.FileTasks {
		return false, true
	}
	if linkRefile && (sc.Caps.LinkRefilesPerRun <= 0 || sc.usedLinkRefiles >= sc.Caps.LinkRefilesPerRun) {
		return true, false
	}
	sc.usedFileTasks++
	if linkRefile {
		sc.usedLinkRefiles++
	}
	return true, true
}

// takeRetire spends one automatic-retirement allowance. A missing/zero cap
// always refuses, so a newly-added grant cannot accidentally ship unlimited.
func (sc *mcpScope) takeRetire() bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.Caps.RetiresPerRun <= 0 || sc.usedRetires >= sc.Caps.RetiresPerRun {
		return false
	}
	sc.usedRetires++
	return true
}

func (sc *mcpScope) takeDependencyRelease() bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.Caps.DependencyReleasesPerRun <= 0 || sc.usedDependencyReleases >= sc.Caps.DependencyReleasesPerRun {
		return false
	}
	sc.usedDependencyReleases++
	return true
}

// takeResolveEvidence spends one evidence-resolution allowance. Like every
// closure cap, zero means no authority rather than unlimited authority.
func (sc *mcpScope) takeResolveEvidence() bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.Caps.ResolvesPerRun <= 0 || sc.usedResolves >= sc.Caps.ResolvesPerRun {
		return false
	}
	sc.usedResolves++
	return true
}

// takeArchive spends one superseded-archive allowance. A missing cap always
// refuses, matching takeRetire's fail-closed contract.
func (sc *mcpScope) takeArchive() bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.Caps.ArchivesPerRun <= 0 || sc.usedArchives >= sc.Caps.ArchivesPerRun {
		return false
	}
	sc.usedArchives++
	return true
}

func (sc *mcpScope) takePropose() bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.Caps.ProposalsPerRun <= 0 || sc.usedProposals >= sc.Caps.ProposalsPerRun {
		return false
	}
	sc.usedProposals++
	return true
}

// releasePropose returns a slot spent by takePropose when the proposal was
// refused before anything attached.
func (sc *mcpScope) releasePropose() {
	if sc == nil {
		return
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.usedProposals > 0 {
		sc.usedProposals--
	}
}

func (sc *mcpScope) takeLinkRefile() bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.Caps.LinkRefilesPerRun <= 0 || sc.usedLinkRefiles >= sc.Caps.LinkRefilesPerRun {
		return false
	}
	sc.usedLinkRefiles++
	return true
}

func (sc *mcpScope) releaseLinkRefile() {
	if sc == nil {
		return
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.usedLinkRefiles > 0 {
		sc.usedLinkRefiles--
	}
}

func (sc *mcpScope) recordInspected(taskID string) {
	if sc == nil || taskID == "" {
		return
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	if sc.inspected == nil {
		sc.inspected = map[string]bool{}
	}
	sc.inspected[taskID] = true
}

func (sc *mcpScope) wasInspected(taskID string) bool {
	if sc == nil {
		return false
	}
	sc.callMu.Lock()
	defer sc.callMu.Unlock()
	return sc.inspected[taskID]
}

// mcpScopeClassAgent is the one non-empty Class this daemon mints.
const mcpScopeClassAgent = "agent"

type mcpScopeStore struct {
	mu      sync.Mutex
	byToken map[string]*mcpScope
}

func newMcpScopeStore() *mcpScopeStore {
	return &mcpScopeStore{byToken: map[string]*mcpScope{}}
}

// Mint creates a legacy (Class=="") scope and returns its bearer token. The
// token is the only handle — treat it like a credential (0600 files, never
// logged). This is the Warden's path and it stays exactly three fields wide.
func (s *mcpScopeStore) Mint(identity string, tools []string, ttl time.Duration) string {
	set := make(map[string]bool, len(tools))
	for _, t := range tools {
		set[t] = true
	}
	return s.MintScoped(&mcpScope{Identity: identity, Tools: set}, ttl)
}

// MintScoped stores a fully-formed scope (the managed-agent path, §3.2) and
// returns its bearer token. Expires is stamped here so no caller can mint a
// capability that outlives its run by forgetting to set it.
func (s *mcpScopeStore) MintScoped(sc *mcpScope, ttl time.Duration) string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing means the process is in a state where no
		// credential should be minted at all.
		panic("mcp scope: crypto/rand unavailable: " + err.Error())
	}
	token := hex.EncodeToString(raw[:])
	if sc == nil {
		sc = &mcpScope{}
	}
	sc.Expires = time.Now().Add(ttl)
	s.mu.Lock()
	s.byToken[token] = sc
	s.mu.Unlock()
	return token
}

// Lookup resolves a bearer value to its scope. Expired entries are pruned
// on sight. Nil-receiver-safe so call sites don't need a wiring check.
func (s *mcpScopeStore) Lookup(token string) (*mcpScope, bool) {
	if s == nil || token == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.byToken[token]
	if !ok {
		return nil, false
	}
	if time.Now().After(sc.Expires) {
		delete(s.byToken, token)
		return nil, false
	}
	return sc, true
}

// Revoke ends a scope immediately (normal end-of-run path).
func (s *mcpScopeStore) Revoke(token string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.byToken, token)
	s.mu.Unlock()
}

// contains revalidates the original capability, including revocation, before a queued spawn.
func (s *mcpScopeStore) contains(scope *mcpScope) bool {
	if s == nil || scope == nil || !time.Now().Before(scope.Expires) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range s.byToken {
		if sc == scope {
			return true
		}
	}
	return false
}
