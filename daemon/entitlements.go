package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Entitlements gate the daemon's non-public surfaces. There are two flags:
//
//   - operator: the operator's own engineering tooling (workflow engine, task
//     owners, release train, managed agents, Aria, the Warden, the team
//     credential pool). Customers never get these, even on a team plan.
//   - team: features a customer team plan could carry (account switching,
//     higher limits). Today the cloud grants both to the same allowlist.
//
// Clouds before the split only answer "team"; a missing operator field falls
// back to the team verdict, so nothing changes for anyone on upgrade. The cloud (GET /api/entitlements, backed by
// public.is_team_user()) is the authority; this file caches its verdict so a
// team member's auto-switch keeps working through cloud outages and restarts.
//
// Verdict order:
//  1. SOLU_TEAM_FEATURES=1 / =0 in the environment (~/.solu/env) — operator
//     override and rollback.
//  2. The last verdict the cloud gave this machine (paired: machine token) or
//     the signed-in account (unpaired: the web forwards its Supabase token).
//     A later answer takes effect at once, without a restart.
//  3. Otherwise off. Nothing found on the machine grants anything: only a
//     positive team verdict from the cloud, cached or fresh, turns the team
//     features on (Benjamin, 2026-10-27: team machines only, the same rule as
//     the update channel in update_channel.go). Team machines already hold a
//     cached positive verdict.

const (
	entitlementsFile         = "entitlements.json"
	entitlementsRefreshEvery = 15 * time.Minute
	entitlementsHTTPTimeout  = 6 * time.Second
	entitlementsAccountHdr   = "X-Solu-Account-Token"
	// entitlementsTeamUpdatesMaxAge bounds how long a cached team update
	// path counts without a fresh answer (a connected install refreshes
	// every entitlementsRefreshEvery).
	entitlementsTeamUpdatesMaxAge = 7 * 24 * time.Hour
)

// teamOnlyPrefixes are the daemon routes refused to installs without the team
// entitlement: switching between the user's own saved provider logins.
var teamOnlyPrefixes = []string{
	"/api/claude/accounts",
	"/api/claude/refresh",
	"/api/claude/autoswitch",
	"/api/codex/accounts",
	"/api/codex/refresh",
	"/api/codex/autoswitch",
}

// operatorOnlyPrefixes are refused to installs without the operator
// entitlement. The team credential pool stores and relays members' provider
// logins, which provider terms don't allow for customers, so it stays
// operator-only even once team plans exist.
var operatorOnlyPrefixes = []string{
	"/api/claude/team",
	"/api/codex/team",
	"/api/muse/team",
	"/api/team/broker",
}

// entitlementRecord is the cached cloud verdict. Tier and SmartApprovals were
// added in v0.0.1200; files written by older daemons carry only Team, and
// normalizedTier derives the tier for them.
type entitlementRecord struct {
	Team bool `json:"team"`
	// Operator was added with the operator/team split. Nil (older daemons
	// and clouds) means "same as Team".
	Operator       *bool     `json:"operator,omitempty"`
	Tier           string    `json:"tier,omitempty"`
	SmartApprovals bool      `json:"smartApprovals,omitempty"`
	Source         string    `json:"source"`
	Subject        string    `json:"subject,omitempty"`
	CheckedAt      time.Time `json:"checkedAt"`
	// TeamUpdates is the team update endpoint path the paired cloud named in
	// this answer (team_updates.go), and TeamUpdatesOrigin that cloud's
	// origin. Both are kept only from a machine-token answer with a team
	// verdict, and the path counts only for the same origin
	// (cloudTeamUpdatesPath).
	TeamUpdates       string `json:"teamUpdates,omitempty"`
	TeamUpdatesOrigin string `json:"teamUpdatesOrigin,omitempty"`
}

type teamEntitlements struct {
	mu         sync.Mutex
	path       string
	record     entitlementRecord
	refreshing bool
	// answered is true once this process stored a cloud answer: a record
	// loaded from disk at boot may be stale, whatever its CheckedAt says.
	answered bool

	cloud       func() (base, token string)
	accountBase func() string
	client      *http.Client
	now         func() time.Time
	// onChange runs after a stored verdict changes (the web re-reads it).
	onChange func()
	// credsChanged, when set, returns a channel closed on the next change of
	// the cloud credentials, so run() asks the cloud as soon as a machine is
	// paired instead of on its next timer.
	credsChanged func() <-chan struct{}
}

func newTeamEntitlements(dataDir string, cloud func() (string, string)) *teamEntitlements {
	e := &teamEntitlements{
		path:  filepath.Join(dataDir, entitlementsFile),
		cloud: cloud,
		accountBase: func() string {
			if v := strings.TrimRight(strings.TrimSpace(os.Getenv("SOLU_CLOUD_URL")), "/"); v != "" {
				return v
			}
			return defaultCloudHTTPBase
		},
		client: &http.Client{Timeout: entitlementsHTTPTimeout},
		now:    time.Now,
	}
	if b, err := os.ReadFile(e.path); err == nil {
		var rec entitlementRecord
		if json.Unmarshal(b, &rec) == nil {
			e.record = rec
		}
	}
	return e
}

// envOperatorOverride reads SOLU_OPERATOR_FEATURES, falling back to
// SOLU_TEAM_FEATURES, which predates the split and still sets both.
func envOperatorOverride() (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_OPERATOR_FEATURES"))) {
	case "1", "true", "on", "yes":
		return true, true
	case "0", "false", "off", "no":
		return false, true
	}
	return envTeamOverride()
}

func envTeamOverride() (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_TEAM_FEATURES"))) {
	case "1", "true", "on", "yes":
		return true, true
	case "0", "false", "off", "no":
		return false, true
	}
	return false, false
}

const (
	tierNone    = "none"
	tierAccount = "account"
	tierTeam    = "team"
)

// normalizedTier is the record's tier. Records from older daemons (and
// answers from older clouds) have no tier: the cloud only ever answered an
// authenticated caller, so such a verdict was a team member or an account.
func (rec entitlementRecord) normalizedTier() string {
	switch rec.Tier {
	case tierNone, tierAccount, tierTeam:
		return rec.Tier
	}
	if rec.Team {
		return tierTeam
	}
	return tierAccount
}

// operatorFlag is the record's operator verdict, falling back to the team
// verdict for records written before the split.
func (rec entitlementRecord) operatorFlag() bool {
	if rec.Operator != nil {
		return *rec.Operator && rec.normalizedTier() == tierTeam
	}
	return rec.Team && rec.normalizedTier() == tierTeam
}

// tierVerdict reports the install's account tier and why. Team-only route
// gating keeps using verdict(); this adds the account tier on top.
func (e *teamEntitlements) tierVerdict() (tier string, smartApprovals bool, source string) {
	team, source := e.verdict()
	if team {
		tier = tierTeam
	} else {
		tier = tierNone
	}
	if e == nil {
		return tier, false, source
	}
	e.mu.Lock()
	rec := e.record
	e.mu.Unlock()
	if rec.CheckedAt.IsZero() {
		return tier, false, source
	}
	if !team && rec.normalizedTier() != tierNone {
		// SOLU_TEAM_FEATURES=0 or a non-team account: still an account.
		tier = tierAccount
	}
	return tier, rec.SmartApprovals && tier != tierNone, source
}

// cachedTier is the last cloud verdict's tier, ignoring env overrides ("none"
// when the cloud never answered).
func (e *teamEntitlements) cachedTier() string {
	if e == nil {
		return tierNone
	}
	e.mu.Lock()
	rec := e.record
	e.mu.Unlock()
	if rec.CheckedAt.IsZero() {
		return tierNone
	}
	return rec.normalizedTier()
}

// verdict reports whether team-only features are on and why.
func (e *teamEntitlements) verdict() (bool, string) {
	if e == nil {
		return true, "unconfigured"
	}
	if team, ok := envTeamOverride(); ok {
		return team, "env"
	}
	e.mu.Lock()
	rec := e.record
	e.mu.Unlock()
	if !rec.CheckedAt.IsZero() {
		return rec.Team && rec.normalizedTier() == tierTeam, rec.Source
	}
	return false, "none"
}

// operatorVerdict reports whether operator-only features are on and why. It
// follows the same order as verdict().
func (e *teamEntitlements) operatorVerdict() (bool, string) {
	if e == nil {
		return true, "unconfigured"
	}
	if on, ok := envOperatorOverride(); ok {
		return on, "env"
	}
	e.mu.Lock()
	rec := e.record
	e.mu.Unlock()
	if !rec.CheckedAt.IsZero() {
		return rec.operatorFlag(), rec.Source
	}
	team, source := e.verdict()
	return team, source
}

func (e *teamEntitlements) store(rec entitlementRecord) {
	e.mu.Lock()
	changed := e.record.Team != rec.Team || e.record.operatorFlag() != rec.operatorFlag() || e.record.Tier != rec.Tier || e.record.Source != rec.Source || e.record.Subject != rec.Subject ||
		e.record.TeamUpdates != rec.TeamUpdates || e.record.TeamUpdatesOrigin != rec.TeamUpdatesOrigin
	e.record = rec
	e.answered = true
	e.mu.Unlock()
	b, err := json.Marshal(rec)
	if err == nil {
		err = writeFileAtomicSynced(e.path, b, 0o600)
	}
	if err != nil {
		log.Printf("entitlements: persist failed: %v", err)
	}
	if changed {
		log.Printf("entitlements: team=%t operator=%t tier=%s source=%s teamUpdates=%t", rec.Team, rec.operatorFlag(), rec.normalizedTier(), rec.Source, rec.TeamUpdates != "")
		if e.onChange != nil {
			e.onChange()
		}
	}
}

var errEntitlementsUnsupported = errors.New("cloud has no entitlements endpoint")

// cloudVerdict is the part of the cloud's GET /api/entitlements answer the
// daemon caches.
type cloudVerdict struct {
	Team           bool   `json:"team"`
	Operator       *bool  `json:"operator"`
	Tier           string `json:"tier"`
	SmartApprovals bool   `json:"smartApprovals"`
	// TeamUpdates is the team update endpoint path, named only to a team
	// machine (cloud team_gate.go).
	TeamUpdates string `json:"teamUpdates"`
}

// fetch asks the cloud for this bearer's entitlements. A 404 means an older
// cloud; callers keep their previous verdict.
func (e *teamEntitlements) fetch(ctx context.Context, base, bearer string) (cloudVerdict, error) {
	if strings.TrimSpace(base) == "" {
		return cloudVerdict{}, errEntitlementsUnsupported // no cloud configured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/entitlements", nil)
	if err != nil {
		return cloudVerdict{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := e.client.Do(req)
	if err != nil {
		return cloudVerdict{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return cloudVerdict{}, errEntitlementsUnsupported
	default:
		return cloudVerdict{}, errors.New("entitlements: cloud status " + resp.Status)
	}
	var out cloudVerdict
	if err := json.Unmarshal(body, &out); err != nil {
		return cloudVerdict{}, err
	}
	return out, nil
}

func recordFromVerdict(v cloudVerdict, source, subject string, at time.Time) entitlementRecord {
	rec := entitlementRecord{Team: v.Team, Operator: v.Operator, Tier: v.Tier, SmartApprovals: v.SmartApprovals, Source: source, Subject: subject, CheckedAt: at}
	rec.Tier = rec.normalizedTier()
	return rec
}

// withTeamUpdates keeps the team update path a machine-token answer from
// cloudBase named, when it may ever be used: a team verdict, a valid path
// (never a URL), and a cloud the token may travel to. Anything else is
// dropped, so the record carries no path.
func (rec entitlementRecord) withTeamUpdates(path, cloudBase string) entitlementRecord {
	rec.TeamUpdates, rec.TeamUpdatesOrigin = "", ""
	path = strings.TrimSpace(path)
	origin := teamUpdatesOrigin(cloudBase)
	if rec.Source != "machine" || !rec.Team || rec.normalizedTier() != tierTeam || origin == "" || !validTeamUpdatesPath(path) {
		return rec
	}
	rec.TeamUpdates, rec.TeamUpdatesOrigin = path, origin
	return rec
}

// cloudTeamUpdatesPath is the team update endpoint path for the paired cloud
// cloudBase, or "". It counts only when that same cloud named it in a
// machine-token answer with a team verdict no older than
// entitlementsTeamUpdatesMaxAge: a path from another cloud (the machine was
// re-paired), a non-team verdict, an account answer, or an old one never
// sends the machine token anywhere.
func (e *teamEntitlements) cloudTeamUpdatesPath(cloudBase string) string {
	if e == nil {
		return ""
	}
	origin := teamUpdatesOrigin(cloudBase)
	e.mu.Lock()
	rec := e.record
	now := e.now
	e.mu.Unlock()
	if origin == "" || rec.CheckedAt.IsZero() || rec.Source != "machine" || !rec.Team || rec.normalizedTier() != tierTeam {
		return ""
	}
	if now != nil && now().Sub(rec.CheckedAt) > entitlementsTeamUpdatesMaxAge {
		return ""
	}
	if rec.TeamUpdatesOrigin != origin || !validTeamUpdatesPath(rec.TeamUpdates) {
		return ""
	}
	return strings.TrimSpace(rec.TeamUpdates)
}

// refresh updates the cached verdict. The machine identity wins when paired;
// otherwise the signed-in account's token (if the web forwarded one) is used.
func (e *teamEntitlements) refresh(ctx context.Context, accountToken string) {
	base, machineToken := "", ""
	if e.cloud != nil {
		base, machineToken = e.cloud()
	}
	if base != "" && machineToken != "" {
		v, err := e.fetch(ctx, base, machineToken)
		if err == nil {
			e.store(recordFromVerdict(v, "machine", "", e.now()).withTeamUpdates(v.TeamUpdates, base))
		} else if !errors.Is(err, errEntitlementsUnsupported) {
			log.Printf("entitlements: machine check failed: %v", err)
		}
		return
	}
	if accountToken == "" {
		return
	}
	v, err := e.fetch(ctx, e.accountBase(), accountToken)
	if err != nil {
		if !errors.Is(err, errEntitlementsUnsupported) {
			log.Printf("entitlements: account check failed: %v", err)
		}
		return
	}
	e.store(recordFromVerdict(v, "account", jwtSubject(accountToken), e.now()))
}

// stale reports whether the cached verdict should be re-checked for a caller
// whose (unverified) account subject is sub.
func (e *teamEntitlements) stale(sub string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.record.CheckedAt.IsZero() || e.now().Sub(e.record.CheckedAt) >= entitlementsRefreshEvery {
		return true
	}
	return e.record.Source == "account" && sub != "" && sub != e.record.Subject
}

func (e *teamEntitlements) refreshAsync(accountToken string) {
	e.mu.Lock()
	if e.refreshing {
		e.mu.Unlock()
		return
	}
	e.refreshing = true
	e.mu.Unlock()
	go func() {
		defer func() {
			e.mu.Lock()
			e.refreshing = false
			e.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*entitlementsHTTPTimeout)
		defer cancel()
		e.refresh(ctx, accountToken)
	}()
}

// entitlementsFirstVerdictRetry is how often run() retries until the cloud
// has answered once. Pairing can finish after boot, and team-only pollers
// wait on that first verdict; without a machine token a retry is local-only.
const entitlementsFirstVerdictRetry = 30 * time.Second

func (e *teamEntitlements) run(ctx context.Context) {
	for {
		// Taken before the refresh so a pairing that lands during it still
		// wakes the next wait.
		var credsChanged <-chan struct{}
		if e.credsChanged != nil {
			credsChanged = e.credsChanged()
		}
		e.refresh(ctx, "")
		wait := entitlementsRefreshEvery
		e.mu.Lock()
		if e.record.CheckedAt.IsZero() {
			wait = entitlementsFirstVerdictRetry
		}
		e.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-credsChanged:
			// Paired, re-paired or disconnected: ask now. At boot the cloud
			// credentials arrive after this loop starts, so without this a
			// cached verdict (and its team update path) waited a full
			// refresh interval.
			timer.Stop()
		}
	}
}

// jwtSubject reads the unverified `sub` claim. It only keys the cache; the
// cloud verifies the token itself.
func jwtSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Sub
}

func pathUnder(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func teamOnlyPath(path string) bool { return pathUnder(path, teamOnlyPrefixes) }

func operatorOnlyPath(path string) bool { return pathUnder(path, operatorOnlyPrefixes) }

// teamFeatureGate refuses team-only and operator-only routes on installs
// without the matching entitlement. Everything else passes through untouched.
func (a *apiServer) teamFeatureGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if operatorOnlyPath(r.URL.Path) {
			if on, _ := a.entitlements.operatorVerdict(); !on {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":   "team_only",
					"message": "team credentials are not available on the free beta",
				})
				return
			}
		}
		if teamOnlyPath(r.URL.Path) {
			if team, _ := a.entitlements.verdict(); !team {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":   "team_only",
					"message": "account switching and team credentials are not available on the free beta",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *apiServer) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	e := a.entitlements
	if e != nil {
		token := strings.TrimSpace(r.Header.Get(entitlementsAccountHdr))
		if e.stale(jwtSubject(token)) {
			e.mu.Lock()
			first := e.record.CheckedAt.IsZero()
			e.mu.Unlock()
			if first && token != "" {
				// First sight of this account: answer with the real verdict
				// rather than a guess the UI would immediately revise.
				ctx, cancel := context.WithTimeout(r.Context(), entitlementsHTTPTimeout)
				e.refresh(ctx, token)
				cancel()
			} else {
				e.refreshAsync(token)
			}
		}
	}
	tier, smartApprovals, source := e.tierVerdict()
	team := tier == tierTeam
	operator, _ := e.operatorVerdict()
	account := tier != tierNone
	writeJSON(w, http.StatusOK, map[string]any{
		"tier":             tier,
		"team":             team,
		"operator":         operator,
		"account":          account,
		"accountSwitching": team,
		// The credential pool is operator-only (see operatorOnlyPrefixes).
		"teamCredentials": operator,
		"viewPersonal":    a.personalViewOn(),
		"remoteAccess":    account,
		"push":            account,
		"voice":           account && voiceBuiltIn,
		"smartApprovals":  smartApprovals,
		"source":          source,
	})
}

// jevEntitlementsRef publishes the boot-time entitlements cache to the Jev
// auto-approval path, which can run before the cache exists.
var jevEntitlementsRef atomic.Pointer[teamEntitlements]

// jevCloudEntitledFromCache reports whether cloud smart approvals are allowed
// and whether that answer is known (a cloud verdict is cached).
func jevCloudEntitledFromCache() (allowed, known bool) {
	ent := jevEntitlementsRef.Load()
	if ent == nil {
		return false, false
	}
	_, smart, source := ent.tierVerdict()
	return smart, source == "machine" || source == "account"
}

// bootOperatorVerdict decides, before the cloud has been asked, whether this
// boot starts the operator-only engines (workflow tasks, managed-agent
// triggers, the Aria responder, the Muse probe). Public installs must not run
// the operator pipeline, and an operator who is offline must not lose it, so
// the order is:
//  1. SOLU_OPERATOR_FEATURES, then SOLU_TEAM_FEATURES (=1 / =0).
//  2. The last cached cloud verdict (entitlements.json).
//  3. With no verdict yet, an install that already holds workflow tasks keeps
//     the engine (an update must never strand running work).
//  4. Otherwise off.
//
// A verdict that changes later takes effect on the next daemon start.
func bootOperatorVerdict(dataDir string) (bool, string) {
	if on, ok := envOperatorOverride(); ok {
		return on, "env"
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, entitlementsFile)); err == nil {
		var rec entitlementRecord
		if json.Unmarshal(b, &rec) == nil && !rec.CheckedAt.IsZero() {
			return rec.operatorFlag(), "cached:" + rec.Source
		}
	}
	if hasExistingWorkflowTasks(dataDir) {
		return true, "existing-workflow"
	}
	return false, "none"
}

// bootTeamVerdict is the pre-split name of bootOperatorVerdict: the engines it
// gated were always operator tooling.
func bootTeamVerdict(dataDir string) (bool, string) { return bootOperatorVerdict(dataDir) }

func hasExistingWorkflowTasks(dataDir string) bool {
	entries, err := os.ReadDir(filepath.Join(dataDir, "workflow", "tasks"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}

// teamFeaturesOn is the live team verdict for runtime gates (MCP tool set,
// cloud LLM calls). A nil entitlements cache (tests, embedded servers) keeps
// the historical full behavior.
func (a *apiServer) teamFeaturesOn() bool {
	if a == nil || a.entitlements == nil {
		return true
	}
	team, _ := a.entitlements.verdict()
	return team
}

// operatorFeaturesOn is the live operator verdict for runtime gates (MCP tool
// set, orchestrator role, agent nudges). A nil cache keeps full behavior.
func (a *apiServer) operatorFeaturesOn() bool {
	if a == nil || a.entitlements == nil {
		return true
	}
	on, _ := a.entitlements.operatorVerdict()
	return on
}

// cachedTeamVerdict reads the published entitlements cache. ok is false
// before the cache exists (early boot, tests, embedded servers); callers pick
// their own default for that window.
func cachedTeamVerdict() (team, ok bool) {
	ent := jevEntitlementsRef.Load()
	if ent == nil {
		return false, false
	}
	team, _ = ent.verdict()
	return team, true
}

// cachedOperatorVerdict is cachedTeamVerdict for the operator flag.
func cachedOperatorVerdict() (on, ok bool) {
	ent := jevEntitlementsRef.Load()
	if ent == nil {
		return false, false
	}
	on, _ = ent.operatorVerdict()
	return on, true
}

// cachedOperatorVerdictFresh is cachedOperatorVerdict plus whether the
// verdict is fresh: the cloud answered during this daemon's run, or an env
// override set it. A record loaded from disk at boot (possibly stale, e.g. a
// machine restored from an old backup or a verdict another daemon wrote) is
// not fresh until the cloud answers, whatever its CheckedAt says (a clock
// correction between boots can put that in the future). The verdict and its
// freshness come from one locked snapshot.
func cachedOperatorVerdictFresh() (on, ok, fresh bool) {
	ent := jevEntitlementsRef.Load()
	if ent == nil {
		return false, false, false
	}
	if on, set := envOperatorOverride(); set {
		return on, true, true
	}
	ent.mu.Lock()
	rec, answered := ent.record, ent.answered
	ent.mu.Unlock()
	if rec.CheckedAt.IsZero() {
		on, _ = ent.verdict()
		return on, true, false
	}
	return rec.operatorFlag(), true, answered
}

// teamCloudLLMAllowed reports whether calls to the operator-only cloud LLM
// (/api/aria/complete: attention summaries) may be made. The
// cloud refuses everyone else, so asking anyway only wastes requests and
// uploads pane text for nothing. Before the cache is published (tests) the
// historical behavior holds.
func teamCloudLLMAllowed() bool {
	on, ok := cachedOperatorVerdict()
	return !ok || on
}

// teamFeaturesFromCache reports the install's team verdict for paths that
// run before the cache exists. No cache yet reads as not team: those paths
// then take the safe public route (the human answers) for a few seconds.
func teamFeaturesFromCache() bool {
	team, _ := cachedTeamVerdict()
	return team
}

// voiceBuiltIn reports that this build has voice (voice_server.go); a build
// without it never offers voice.
var voiceBuiltIn bool
