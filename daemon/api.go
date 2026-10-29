package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type apiServer struct {
	// ext holds optional features' per-server state (extSlot, extensions.go).
	ext extState
	// The provider logins this daemon reports on: the provider CLI's own
	// login (provider_login.go), or a credential manager that replaces it.
	claudeLogin claudeLoginState
	codexLogin  codexLoginState
	museLogin   museLoginState
	// resources are the shared resource settings (caps, disk/memory floors)
	// when a settings store is built in; nil means the defaults.
	resources           resourceSettingsReader
	storage             *hostStorageService
	originAliases       *paneOriginAliases
	codexReconnects     codexReconnectArms
	ownerContinuationMu sync.Mutex
	retirementMu        sync.Mutex
	shinoqueTokens      *shinoqueTokenCache
	lineupMu            sync.Mutex
	configurationMu     sync.Mutex
	// Test-only external storage fault; it cannot bypass the durable writer or
	// authorize a change. Production always uses the fsynced audit below.
	harnessMu     sync.Mutex
	harnessStores map[string]*harnessCredStore
	// Background runtime installs for native harnesses. Guarded by
	// harnessInstallMu; a provider appears in harnessInstalling while its
	// installer runs and in harnessInstallErrs when it last failed.
	harnessInstallMu   sync.Mutex
	harnessInstalling  map[string]bool
	harnessInstallErrs map[string]string
	// harnessInstallTried bounds automatic provisioning to one attempt per
	// provider per daemon process, so a persistent failure cannot loop npm.
	harnessInstallTried map[string]bool
	// harnessAutoInstall enables background runtime provisioning. The real
	// daemon turns it on; tests leave it off so a status GET never spawns npm.
	harnessAutoInstall bool
	mgr                *Manager
	// conditionalSubmitProbe is a test-only pure process probe. Nil in production.
	conditionalSubmitProbe func(context.Context, int) (conditionalSubmitProcess, error)
	// Test-only paste-gap seam; nil uses the cancellable provider timing.
	conditionalSubmitEnterWait func(context.Context, time.Duration) error
	ws                         *WorkspaceStore
	ports                      *PortRegistry
	panes                      paneStore
	// closed remembers recently closed panes for reopen (closed_panes.go).
	closed *closedPaneStore
	uiBus  *uiBus
	// In-memory ring of files agents recently created/edited (from the
	// Edit/Write hooks). Backs GET /api/recent-files. See recentfiles.go.
	recentFiles *recentFilesRing
	scheds      *ScheduleStore
	updater     *Updater
	// fleet inventories this Manager's daemon/pane agent binaries for the
	// debug endpoint only. Nil on unsupported/degraded test servers.
	fleet *fleetBinaryInspector
	aria  *Aria
	// mcpScopes holds daemon-minted scoped capability tokens for /mcp —
	// per-run restricted tool budgets for spawned judgment children (the
	// Warden). Nil-safe (Lookup on nil = not found), so tests and partial
	// wiring degrade to "no scoped callers" rather than panicking.
	mcpScopes *mcpScopeStore
	// pane session. The cache is in-process only and is re-minted after an
	// Authority-card edit; see orchestratorPaneScope.
	orchestratorScopesMu sync.Mutex
	// wardenSettings + warden back the opt-in fleet caretaker (DEFAULT OFF).
	// Both nil in tests that don't exercise the warden surface.
	wardenSettings *wardenSettingsStore
	warden         *Warden
	// relayPaneDirsForTest is TEST-ONLY (nil in production): extra directories
	// treated like live pane cwds when building the relay file roots
	// (files_relay_roots.go), so handler tests need no live Manager.
	relayPaneDirsForTest []string
	// readiness-transition regression complete boot after the initial scoped
	// argument pass but before the canonical MCP boot gate.
	// jevBridge is an optional, default-off advisory endpoint with its own
	// route-scoped credential. It is deliberately outside authGate's token
	// namespace and has no workflow mutation adapter.
	jevBridge    *jevBridge
	modelGateway *modelGateway
	// nudger is the origin-pane delegation nudger (delegation_nudge.go). The
	// hook handler tells it when a pane's Claude session rotates, so a fresh
	// session can be handed a digest of the work that pane already delegated.
	// Nil in tests: every method is nil-receiver safe.
	// attention is the per-pane "who needs to act" engine (attention.go).
	// Poked after mutations that change the classification inputs (the
	// auto-mode toggle) so the signal flips immediately instead of on the
	// next 2s tick. Nil in tests.
	attention *attentionEngine
	// pusher is set/swapped dynamically by the relay supervisor's onCreds
	// path (see cloudMu). Read it via getPusher(), never directly.
	pusher  *Pusher
	metrics *Metrics
	system  *systemSampler
	// claudeStat caches the Claude CLI probe (installed/version/authed) for GET
	// /api/health. Probed at boot, on a slow ticker, after a guided install, and
	// on explicit re-check — NEVER per health request (that path is a 10s
	// heartbeat on a multiplexed pipe; forking a login shell there would add
	// latency to the reachability signal). Guarded by claudeStatMu. See
	// claude_bootstrap.go.
	claudeStatMu sync.Mutex
	claudeStat   claudeStatus
	// claudeProbeMu coalesces concurrent probes (boot, ticker, on-demand
	// rechecks) onto ONE login shell via TryLock — a looping UI hammering
	// re-check must not fan out 8s shells. See refreshClaudeStatus.
	claudeProbeMu sync.Mutex
	// Absence hysteresis (guarded by claudeStatMu): a CONFIRMED install may
	// flip to "not installed" only after claudeAbsentConfirmProbes consecutive
	// definitive absents spanning claudeAbsentConfirmWindow. A Claude self-update
	// (npm reinstall) leaves the binary genuinely absent from disk for a window
	// of seconds — indistinguishable from an uninstall by any single probe — so
	// durability over time is the only sound discriminator. See confirmClaudeAbsent.
	claudeAbsentStreak  int
	claudeFirstAbsentAt time.Time
	// detectClaude / registerIntegrationsFn / nowFn are test seams for
	// refreshClaudeStatus (the real probe forks a login shell; the real
	// registration rewrites the user's Claude config). Production leaves them
	// nil → detectClaudeCLI / registerClaudeIntegrations / time.Now.
	detectClaude           func() (claudeProbeOutcome, string, bool)
	registerIntegrationsFn func()
	nowFn                  func() time.Time
	// Muse Code CLI bootstrap status (muse_bootstrap.go) — the parallel of
	// the Claude fields above, minus integration registration (Muse has no
	// MCP/hook surface to re-register). Guarded by museStatMu; probes
	// coalesced by museProbeMu; absence damped by museAbsentStreak /
	// museFirstAbsentAt. nowFn is shared with the Claude hysteresis.
	museStatMu        sync.Mutex
	museStat          museStatus
	museProbeMu       sync.Mutex
	museAbsentStreak  int
	museFirstAbsentAt time.Time
	// detectMuse is the test seam for refreshMuseStatus (the real probe
	// forks a login shell). Production leaves it nil → detectMuseCLI.
	detectMuse func() (museProbeOutcome, string, bool)
	// Codex CLI bootstrap status (codex_bootstrap.go) — the same shape as the
	// Muse fields above. detectCodex is the test seam (the real probe forks a
	// login shell); its argument asks for a `codex login status` check.
	codexStatMu        sync.Mutex
	codexStat          codexCLIStatus
	codexProbeMu       sync.Mutex
	codexAbsentStreak  int
	codexFirstAbsentAt time.Time
	detectCodex        func(checkLogin bool) codexProbeResult
	// orchestratorHostProbe is the deterministic test seam for provider
	// availability. The small cache avoids repeatedly invoking login shells.
	orchestratorHostProbe   func(agent string) orchestratorHostAvailability
	orchestratorHostCacheMu sync.Mutex
	orchestratorHostCache   map[string]cachedOrchestratorHostAvailability
	// claudeRegMu serializes (re-)registration of the MCP server + hooks into
	// the user's Claude config so overlapping probes can't race the shared
	// config tmp files (~/.claude.json.solu-tmp etc.). See
	// registerClaudeIntegrations.
	claudeRegMu sync.Mutex

	// entitlements gates team-only credential features (entitlements.go).
	entitlements *teamEntitlements
	checkin      *installCheckin
	privacy      *privacySettings
	naming       *paneNaming
	// agentAuthority is the person's grant letting agents in panes approve
	// for them (caller_authority.go).
	agentAuthority *agentAuthority
	// trustedDevices decides which remote devices may use the relay
	// (trusted_devices.go).
	trustedDevices *trustedDevices
	// privateRelayInner serves requests that arrive inside an approved
	// device's private relay channel (private_relay.go).
	privateRelayInner http.Handler
	view              *personalView

	// Pane recovery preference for host reboot / WSL shutdown restores.
	// Controls whether resumable Claude panes are suspended for user action or
	// background-respawned with `claude --resume` on daemon startup.
	recoverySettings *recoverySettingsStore
	// Per-machine native (Focus-renderer) toggles, e.g. the prompt-suggestions
	// opt-in. Read fresh at every native (re)spawn (Manager.promptSuggestionsEnabled).
	nativeSettings *nativeSettingsStore
	// Per-install self-update preference (off | when-idle). The client reads it
	// to decide whether to auto-apply the daemon swap once safe; the daemon just
	// persists it (headless auto-apply is a future v2).
	updateSettings *updateSettingsStore
	rollback       *updateRollbackStore

	startT        time.Time
	ver           string
	addr          string
	backendID     string
	shellVersion  string
	shellPlatform string
	// Where /api/update/apply-shell drops the downloaded installer.
	// Same as main.go's --data flag; mirrored here so updateApplyShell
	// has a stable per-user path to write into.
	dataDir string
	// Adopt-mode preview registry. Nil in older focused tests and when boot
	// loading failed; every route still remains hidden unless SOLU_PREVIEWS=1.
	previews *previewManager

	// cloudMu guards every live cloud-creds field below — cloudBase,
	// cloudToken, ccCloudBase, ccCloudToken, and the pusher pointer. They
	// used to be written exactly once at boot, but the relay supervisor now
	// writes them DYNAMICALLY: at boot if claim.json exists, the instant a
	// post-boot pairing appears (onCreds, on the supervisor goroutine), and
	// again on every token rotation (the rotation callback goroutine). The
	// HTTP handlers read them concurrently (voice, cc-usage, ack, status),
	// so without this lock the pairing/rotation write races every request.
	// ALWAYS go through setCloudCreds / rotateCloudToken (writers) and the
	// cloud*()/getPusher() accessors (readers) — never touch the fields raw.
	cloudMu sync.RWMutex
	// Closed on credential changes so dormant team watchers wake on pairing.
	cloudCredsChanged chan struct{}

	// Optional cloud-proxy fields for /api/transcribe. Set by main.go
	// when --relay is on; empty otherwise. If set, voice requests are
	// forwarded to <cloudBase>/api/transcribe using cloudToken so the
	// user doesn't need their own GROQ_API_KEY. When empty, falls back
	// to local Groq via ~/.solu/env. Guarded by cloudMu.
	cloudBase           string
	cloudToken          string
	harnessPairingScope string // stable across token rotation; guarded by cloudMu

	// Auth gate, shared with the HTTP middleware. Set in main.go after
	// the token is loaded. Used by the /mcp handler to accept token-authed
	// non-loopback callers (e.g. the Aria hub on the Windows host reaching
	// this WSL daemon) in addition to loopback ones.
	gate *authGate

	// Preview channel (follow-main builds on the owner's PC); nil in tests.
	preview *previewChannel

	// Relay supervisor — the SOLE owner of the cloud relay tunnel lifecycle.
	// Backs GET /api/cloud/status (the desktop UI's local heartbeat) and lets
	// voice/push/ARIA consult LIVE cloud state instead of the boot-time relay
	// flag. nil on a local-only daemon (no --relay) and in tests.
	cloudSup *relaySupervisor

	// Pairing manager — drives server-driven (web-initiated) machine pairing
	// (POST /api/cloud/pair/start + /cancel). It writes claim.json on success;
	// the supervisor above observes that and dials. Decoupled by design so the
	// exactly-one-tunnel invariant is preserved. nil on a local-only daemon and
	// in tests. Its own internal state is mutex-guarded (see pairing.go).
	pairing *pairingManager

	// onCloudDisconnect stops every cloud-bound reporter at once when the user
	// disconnects, given the token being dropped. Set by main; nil in tests.
	onCloudDisconnect func(token string)
	// cloudRevoker removes this computer from its account in the cloud after
	// Disconnect, retrying while the cloud is unreachable. nil in tests.
	cloudRevoker *cloudRevoker

	// In-app vs out-app session ledger for Claude Code usage capture. Fed
	// by the hook handler (every hook event records sessionId → in_app) and
	// read/flushed by the transcript scanner. nil in tests that construct
	// apiServer without one — the hook handler guards for that.
	ccLedger *inAppLedger

	// Cloud base + machine token for the read-side Claude Code usage proxy
	// (GET /api/cc-usage/summary). Set in main.go from the SAME cloudBase /
	// relay token the cc-usage scanner POSTs with. Empty in local-only dev,
	// in which case handleCCUsageSummary returns the contract's empty shape.
	ccCloudBase  string
	ccCloudToken string

	// Test hooks for spawn/rollback paths. Production leaves these nil.
	spawnPane func(SpawnOpts) (*Pane, error)
	killPane  func(string) error
}

func (a *apiServer) spawnPaneForRequest(opts SpawnOpts) (*Pane, error) {
	var p *Pane
	var err error
	if a.spawnPane != nil {
		p, err = a.spawnPane(opts)
	} else {
		p, err = a.mgr.Spawn(opts)
	}
	// The check-in's agentPaneDay yes/no (install_checkin_activity.go). Boot
	// recovery resumes panes through the Manager directly, so it never counts.
	if err == nil && p != nil && checkinAgentPane(p.Snapshot()) {
		a.checkin.noteAgentPane()
	}
	return p, err
}

func (a *apiServer) killPaneForRollback(id string) error {
	if a.killPane != nil {
		return a.killPane(id)
	}
	return a.mgr.Kill(id)
}

func (a *apiServer) rollbackSpawnedPane(p *Pane) {
	if p == nil {
		return
	}
	// Drop manager membership before durable removal so an in-flight flag
	// transaction cannot recreate the record after Remove returns.
	_ = a.killPaneForRollback(p.ID)
	if a.panes != nil {
		_ = a.panes.Remove(p.ID)
	}
	if a.ws != nil {
		_ = a.ws.RemovePane(p.ID)
	}
}

// --- live cloud-creds accessors (guarded by cloudMu) -------------------------
//
// The relay supervisor writes these dynamically (boot / post-boot pairing /
// rotation) while HTTP handlers read them concurrently. Everything below funnels
// through cloudMu so the writes never race the reads. See the cloudMu comment on
// the struct for the full rationale.

// setCloudCreds installs a fresh cloud base + token + pusher in one atomic
// swap. Called from the supervisor's onCreds goroutine the first time a usable
// machine token is observed. pusher may be nil.
func (a *apiServer) setCloudCreds(cloudBase, token string, pusher *Pusher) {
	a.cloudMu.Lock()
	a.harnessPairingScope = harnessScopeForPairing(a.dataDir, cloudBase, token)
	changed := a.cloudBase != cloudBase || a.cloudToken != token
	a.cloudBase = cloudBase
	a.cloudToken = token
	a.ccCloudBase = cloudBase
	a.ccCloudToken = token
	a.pusher = pusher
	if changed {
		a.notifyCloudCredsChangedLocked()
	}
	a.cloudMu.Unlock()
}

// clearCloudCreds zeroes the cloud base + token + pusher — the inverse of
// setCloudCreds. Called when the machine is disconnected (claim.json deleted) so
// the HTTP handlers (voice / cc-usage / ack / status) stop reaching a cloud this
// machine no longer belongs to. Safe to call when nothing was wired.
func (a *apiServer) clearCloudCreds() {
	a.cloudMu.Lock()
	a.cloudBase = ""
	a.cloudToken = ""
	a.harnessPairingScope = ""
	a.ccCloudBase = ""
	a.ccCloudToken = ""
	a.pusher = nil
	a.notifyCloudCredsChangedLocked()
	a.cloudMu.Unlock()
}

// rotateCloudToken swaps only the token across every cloud-creds field (the base
// is unchanged on a rotation). Called from the token-rotation callback goroutine.
func (a *apiServer) rotateCloudToken(token string) {
	a.cloudMu.Lock()
	// The rotation poller re-delivers the same token while a confirm keeps
	// failing; waking every team watcher for it would reconnect them all.
	changed := a.cloudToken != token
	a.cloudToken = token
	a.ccCloudToken = token
	if changed {
		a.notifyCloudCredsChangedLocked()
	}
	a.cloudMu.Unlock()
}

// cloudCreds returns a consistent (base, token) snapshot for the voice proxy.
func (a *apiServer) cloudCreds() (base, token string) {
	a.cloudMu.RLock()
	defer a.cloudMu.RUnlock()
	return a.cloudBase, a.cloudToken
}

// ccCloudCreds returns a consistent (base, token) snapshot for the cc-usage
// read proxy.
func (a *apiServer) ccCloudCreds() (base, token string) {
	a.cloudMu.RLock()
	defer a.cloudMu.RUnlock()
	return a.ccCloudBase, a.ccCloudToken
}

// cloudCredsChangedCh returns a channel closed on the next change of the
// cloud credentials (pairing, rotation, Disconnect).
func (a *apiServer) cloudCredsChangedCh() <-chan struct{} {
	a.cloudMu.Lock()
	defer a.cloudMu.Unlock()
	if a.cloudCredsChanged == nil {
		a.cloudCredsChanged = make(chan struct{})
	}
	return a.cloudCredsChanged
}

// notifyCloudCredsChangedLocked wakes everything waiting on a.cloudCredsChanged
// (pairing, rotation, Disconnect). Callers hold cloudMu.
func (a *apiServer) notifyCloudCredsChangedLocked() {
	if a.cloudCredsChanged != nil {
		close(a.cloudCredsChanged)
	}
	a.cloudCredsChanged = make(chan struct{})
}

// getPusher returns the live pusher (nil before pairing). The pointer itself is
// swapped on (re)pairing, so callers must read it through here.
func (a *apiServer) getPusher() *Pusher {
	a.cloudMu.RLock()
	defer a.cloudMu.RUnlock()
	return a.pusher
}

func (a *apiServer) routes(mux *http.ServeMux) {

	a.registerHarnessRoutes(mux)
	// Optional features add their own routes (extensions.go).
	a.registerExtensionRoutes(mux)
	for _, pattern := range []string{
		"GET /api/key-providers/{provider}", "PUT /api/key-providers/{provider}/accounts",
		"DELETE /api/key-providers/{provider}/accounts/{name}",
		"POST /api/key-providers/{provider}/select", "POST /api/key-providers/{provider}/check",
	} {
		mux.HandleFunc(pattern, a.apiKeyAccounts)
	}
	mux.HandleFunc("/api/model-gateway/settings", func(w http.ResponseWriter, r *http.Request) {
		if a.modelGateway == nil {
			http.Error(w, "gateway unavailable", 503)
			return
		}
		a.modelGateway.status(w, r)
	})
	mux.HandleFunc(modelGatewayPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		if a.modelGateway == nil {
			http.Error(w, "gateway unavailable", 503)
			return
		}
		a.modelGateway.ServeHTTP(w, r)
	})
	// Register without a method pattern so the bridge itself can return the
	// stable JSON 405 envelope and audit unsupported methods exactly once.
	mux.HandleFunc(jevBridgePath, func(w http.ResponseWriter, r *http.Request) {
		if a.jevBridge == nil {
			writeJevBridgeAPIError(w, http.StatusServiceUnavailable, "jev_bridge_unavailable", "Jev decision bridge is unavailable")
			return
		}
		a.jevBridge.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/health", a.health)

	mux.HandleFunc("GET /api/system/status", a.systemStatus)
	mux.HandleFunc("GET /api/system/activity", a.systemActivity)
	mux.HandleFunc("GET /api/system/host-storage", a.hostStorageStatus)
	mux.HandleFunc("POST /api/system/host-storage/refresh", a.hostStorageRefresh)
	mux.HandleFunc("POST /api/system/host-storage/rotate-crash-dumps", a.rotateHostCrashDumps)
	// Short aliases keep the operator-facing endpoint named the same way as
	// the host-storage ledger and CLI while preserving the existing System API.
	mux.HandleFunc("GET /api/host-storage", a.hostStorageStatus)
	mux.HandleFunc("POST /api/host-storage/refresh", a.hostStorageRefresh)
	// Operational telemetry. Loopback-only — see metrics.go.
	mux.HandleFunc("GET /metrics", metricsHandler(a.metrics, a.mgr))
	mux.HandleFunc("GET /api/panes", a.listPanes)
	mux.HandleFunc("POST /api/panes", a.createPane)
	mux.HandleFunc("POST /api/panes/migrate-clean-all", a.migrateCleanAll)
	mux.HandleFunc("DELETE /api/panes/{id}", a.deletePane)
	mux.HandleFunc("POST /api/panes/{id}/resume", a.handleResumePane)
	mux.HandleFunc("/api/recovery/settings", a.handleRecoverySettings)
	mux.HandleFunc("/api/native/settings", a.handleNativeSettings)
	mux.HandleFunc("/api/update/settings", a.handleUpdateSettings)
	mux.HandleFunc("/api/warden/settings", a.handleWardenSettings)

	mux.HandleFunc("GET /api/warden/status", a.handleWardenStatus)
	mux.HandleFunc("POST /api/warden/run-now", a.handleWardenRunNow)
	mux.HandleFunc("GET /api/warden/runs/{name}", a.handleWardenRunLog)
	mux.HandleFunc("POST /api/panes/{id}/resize", a.resizePane)
	mux.HandleFunc("POST /api/panes/{id}/redraw", a.redrawPane)
	mux.HandleFunc("POST /api/panes/{id}/rename", a.renamePane)
	mux.HandleFunc("POST /api/panes/{id}/title", a.setPaneTitle)
	mux.HandleFunc("POST /api/panes/{id}/title/generate", a.generatePaneTitle)
	mux.HandleFunc("POST /api/panes/{id}/identity", a.setPaneIdentity)
	mux.HandleFunc("POST /api/panes/{id}/name/suggest", a.handleSuggestPaneName)
	mux.HandleFunc("POST /api/panes/{id}/auto-mode", a.setPaneAutoMode)
	mux.HandleFunc("POST /api/panes/{id}/announce-delegation", a.setPaneAnnounceDelegation)
	mux.HandleFunc("POST /api/panes/{id}/composer-state", a.setPaneComposerState)
	mux.HandleFunc("POST /api/panes/{id}/flag", a.setPaneFlag)
	mux.HandleFunc("POST /api/panes/{id}/migrate-clean", a.migratePaneClean)
	mux.HandleFunc("POST /api/panes/{id}/ack", a.ackPane)
	mux.HandleFunc("/api/panes/{id}/io", handleWS(a.mgr, a.promptInputGate))

	mux.HandleFunc("GET /api/workspaces", a.listWorkspaces)
	mux.HandleFunc("POST /api/workspaces", a.putWorkspace)
	mux.HandleFunc("POST /api/workspaces/{name}/move-pane", a.moveWorkspacePane)
	mux.HandleFunc("DELETE /api/workspaces/{name}", a.deleteWorkspace)
	mux.HandleFunc("POST /api/workspaces/{name}/rename", a.renameWorkspace)
	mux.HandleFunc("POST /api/workspaces/{name}/context-pack", a.setWorkspaceContextPack)

	mux.HandleFunc("GET /api/ports", a.listPorts)
	mux.HandleFunc("POST /api/ports/probe", a.probePort)
	mux.HandleFunc("POST /api/previews", a.createPreview)
	mux.HandleFunc("GET /api/previews", a.listPreviews)
	mux.HandleFunc("DELETE /api/previews/{previewId}", a.stopPreview)
	mux.HandleFunc("POST /api/previews/{previewId}/link", a.previewLink)
	mux.HandleFunc("POST /api/previews/{previewId}/rotate-link", a.previewLink)
	mux.HandleFunc("POST /api/previews/{previewId}/relaunch", a.relaunchPreview)
	mux.HandleFunc("/api/preview-serve/{previewId}/{path...}", a.servePreview)

	mux.HandleFunc("GET /api/dirs", a.listDirs)
	mux.HandleFunc("GET /api/panes/{id}/cwd", a.paneLiveCwd)
	mux.HandleFunc("GET /api/sessions/recent", a.recentSessions)
	mux.HandleFunc("GET /api/panes/closed", a.listClosedPanes)
	mux.HandleFunc("POST /api/panes/closed/{id}/reopen", a.reopenClosedPane)
	mux.HandleFunc("GET /api/panes/{id}/turns", a.paneTurns)
	mux.HandleFunc("GET /api/panes/{id}/turns/{n}/diff", a.paneTurnDiff)
	mux.HandleFunc("GET /api/panes/{id}/debug-tail", a.paneDebugTail)
	mux.HandleFunc("GET /api/panes/{id}/debug-grid", a.paneDebugGrid)
	mux.HandleFunc("GET /api/debug/ownership", a.ownershipDebug)
	// Binary/agent-protocol inventory of this daemon and its pane agents.
	// Debug only: walks /proc, so it must never back a poll.
	mux.HandleFunc("GET /api/debug/fleet", a.getDebugFleet)
	mux.HandleFunc("GET /api/panes/{id}/conversation", a.paneConversation)
	// Native-renderer pane surface (Component 6). SSE stream + ring backfill +
	// user-turn input. Default-off: all three 409 when the pane isn't native.
	// Go 1.22 ServeMux: the longer literal /native/history wins over /native for
	// its path, and method+pattern specificity disambiguates — no conflict.
	mux.HandleFunc("GET /api/panes/{id}/native", a.paneNativeStream)
	// WebSocket transport for the native event stream (native_ws.go). Same
	// payload/ordering as the SSE handler above, but on the WS pool so visible
	// native panes don't exhaust the browser's ~6 HTTP/1.1 connections-per-host
	// on the Local desktop. The SSE route is KEPT this release for rollout
	// safety (old bundles + a one-line client revert path).
	mux.HandleFunc("GET /api/panes/{id}/native/ws", handleNativeWS(a.mgr))
	mux.HandleFunc("GET /api/panes/{id}/native/history", a.paneNativeHistory)
	mux.HandleFunc("POST /api/panes/{id}/native/input", a.paneNativeInput)
	// Per-pane tool-approval allowlist ("always allow this tool"). GET lists the
	// current set; POST {tool,action} adds/removes by EXACT tool name. An
	// allowlisted tool's can_use_tool is auto-allowed on arrival (no pending
	// card), bypassing the ARIA fire-budget cap (deliberate user opt-in).
	mux.HandleFunc("GET /api/panes/{id}/native/allowlist", a.paneNativeAllowlistGet)
	mux.HandleFunc("POST /api/panes/{id}/native/allowlist", a.paneNativeAllowlistPost)
	// Native approvals: answer a parked can_use_tool permission request
	// (lock-screen / app). Idempotent — a double-tap returns 200, not 404.
	mux.HandleFunc("POST /api/panes/{id}/approvals/{reqId}/resolve", a.handlePaneApprovalResolve)
	mux.HandleFunc("POST /api/panes/{id}/native/interrupt", a.handlePaneNativeInterrupt)
	// Flip a pane between terminal and native rendering: kill + respawn-with-
	// resume, rolling back to the original pane if the new one fails to come up
	// (never leaves a dead Kind=="native" session). Gated by SOLU_NATIVE_RENDERER
	// for the "native" target.
	mux.HandleFunc("POST /api/panes/{id}/switch-mode", a.handleSwitchMode)
	// Restart a native pane's headless agent in place (same id, resume same
	// Claude session or Codex thread) to reclaim the long-lived process's heap. Native-only,
	// settle-gated, shares the respawn single-flight. Gated by SOLU_PANE_REFRESH.
	mux.HandleFunc("POST /api/panes/{id}/refresh", a.handleRefresh)
	// Restart this pane's agent in place on the installed binary, resuming its
	// conversation; ?fresh=1 starts a brand-new one. Any pane kind, settle-gated,
	// shares the respawn single-flight. Also the action behind the per-pane
	// "older build" dot (paneDTO.agentStale).
	mux.HandleFunc("POST /api/panes/{id}/restart", a.handleRestartPane)
	mux.HandleFunc("POST /api/panes/{id}/effort", a.handleSetEffort)
	mux.HandleFunc("POST /api/panes/{id}/model", a.handleSetModel)
	mux.HandleFunc("POST /api/panes/{id}/role", a.handleSetRole)
	mux.HandleFunc("POST /api/panes/{id}/context-pack", a.handleSetContextPack)
	mux.HandleFunc("GET /api/orchestrator-hosts", a.handleOrchestratorHosts)
	mux.HandleFunc("GET /api/panes/search", a.searchPanes)
	mux.HandleFunc("GET /api/activity", a.activity)
	mux.HandleFunc("POST /api/hooks/tool-call", a.hookToolCall)
	mux.HandleFunc("GET /api/panes/{id}/export", a.exportPane)
	mux.HandleFunc("GET /api/support/bundle", a.handleSupportBundle)

	mux.HandleFunc("GET /api/file", a.readFile)
	mux.HandleFunc("POST /api/file/line", a.writeFileLine)

	mux.HandleFunc("GET /api/dir/list", a.listDirContents)
	mux.HandleFunc("GET /api/files/search", a.searchFiles)
	mux.HandleFunc("GET /api/git/diff", a.gitDiff)
	mux.HandleFunc("GET /api/recent-files", a.handleRecentFiles)

	// In-app feedback (bug / idea / question). Gathers + redacts context
	// locally, then forwards to the cloud which files a GitHub issue. See
	// daemon/feedback.go.
	mux.HandleFunc("POST /api/feedback", a.handleFeedback)
	mux.HandleFunc("GET /api/feedback/mine", a.handleFeedbackMine)
	mux.HandleFunc("GET /api/feedback/status", a.handleFeedbackStatus)
	mux.HandleFunc("POST /api/feedback/reply", a.handleFeedbackReply)
	mux.HandleFunc("GET /api/feedback/inbox", a.handleFeedbackInbox)
	mux.HandleFunc("POST /api/feedback/inbox/read", a.handleFeedbackInboxRead)
	mux.HandleFunc("GET /api/feedback/thread", a.handleFeedbackThread)

	// ARIA (autonomous auto-approver). Loopback-only regression-test
	// surface: given a synthetic pane tail + instruction, returns the
	// autoresponder's would-be decision without executing any tools.
	// See aria_dryrun.go / aria_eval.go.
	mux.HandleFunc("POST /api/aria/dryrun", a.handleAriaDryRun)
	mux.HandleFunc("POST /api/aria/evals", a.handleAriaEvals)
	// User cancel/approve for a pending auto-approve countdown.
	// Body: {"id":"cf_<hex>","approve":<bool>}.
	mux.HandleFunc("POST /api/aria/confirm", a.handleAriaConfirm)
	// Autoresponder kill switch + status. Pause halts autonomous
	// evaluations immediately; status reports current paused state
	// plus remaining auto-fire budget for the current rolling hour.
	mux.HandleFunc("POST /api/aria/pause", a.handleAriaPause)
	mux.HandleFunc("POST /api/aria/cap/reset", a.handleAriaResetCap)
	mux.HandleFunc("GET /api/aria/status", a.handleAriaStatus)
	mux.HandleFunc("GET /api/smart-approvals", a.handleSmartApprovalsStatus)
	mux.HandleFunc("GET /api/aria/auto-pending", a.handleAriaAutoPending)
	// Read-side Claude Code per-account usage. Proxies the cloud's
	// GET /api/cc-usage/summary using the daemon's machine token; the
	// browser never sees the token. See ccusage_api.go.
	mux.HandleFunc("GET /api/cc-usage/summary", a.handleCCUsageSummary)
	// Team usage rollup (Phase 3). Same machine-token proxy; the cloud does
	// team resolution + owner-only per-member redaction. See ccusage_api.go.
	mux.HandleFunc("GET /api/cc-usage/team-summary", a.handleCCUsageTeamSummary)
	// Phase 3.1 owner-authorized per-member detail. Same proxy; the cloud does
	// the owner authorization (403 if the caller may not view the target).
	mux.HandleFunc("GET /api/cc-usage/member-summary", a.handleCCUsageMemberSummary)
	// GitHub App "Connect GitHub" flow — machine-token proxies to the cloud
	// (github_app_proxy.go). The web UI only ever talks to the daemon; the
	// cloud resolves the machine token to the owning user.
	mux.HandleFunc("GET /api/github/app/status", a.handleGithubAppStatus)
	mux.HandleFunc("GET /api/github/app/install", a.handleGithubAppInstallURL)
	mux.HandleFunc("POST /api/github/app/claim", a.handleGithubAppClaim)
	mux.HandleFunc("DELETE /api/github/app", a.handleGithubAppDisconnect)
	// Babysitter write path (phase 3): loopback agent → machine token →
	// cloud posts the review as soluterminal[bot].
	mux.HandleFunc("POST /api/github/app/pr-review", a.handleGithubAppPRReview)
	// Per-pane ring buffer of the last few autonomous-eval outcomes
	// (newest first). Lets the panel show WHY ARIA declined / errored
	// instead of just a silent "backed off" badge.
	mux.HandleFunc("GET /api/aria/panes/{id}/evals", a.handleAriaPaneEvals)
	// Cross-pane decision feed: the most recent auto-approval outcomes
	// (fired / declined / skipped + reason) across ALL panes, newest
	// first. The single place to answer "auto-approval is on but this
	// prompt didn't go through" without knowing a pane id.
	mux.HandleFunc("GET /api/aria/recent", a.handleAriaRecent)
	// Per-machine ARIA model picks (chat + sweeper). The cloud holds
	// API keys for every supported provider (Anthropic, DeepSeek), so
	// the UI never collects keys. GET returns the current picks +
	// effective fallbacks; PUT merges a partial update. Stored in

	// MCP (Model Context Protocol) — exposes `preview_file` etc. to
	// Claude Code. Loopback-only; enforced inside the handler since the
	// cloud relay bypasses authGate.
	mux.HandleFunc("/mcp", a.mcp)

	// What MCP servers Claude Code would see for a given cwd. Read-only;
	// merges ~/.claude.json (user + per-project) with .mcp.json walks.
	mux.HandleFunc("GET /api/mcp/servers", a.listMcpServers)

	// SSE event bus for daemon → web-UI messages (e.g. "Claude asked to
	// preview this file"). Auth-gated like the rest of /api/* already.
	mux.HandleFunc("GET /api/ui/events", a.uiEvents)

	// Scheduled prompt injection + built-in agent runs (`Schedules` feature).
	mux.HandleFunc("GET /api/schedules", a.listSchedules)
	mux.HandleFunc("GET /api/schedules/preview", a.previewSchedule)
	mux.HandleFunc("GET /api/schedules/{id}", a.getSchedule)
	mux.HandleFunc("POST /api/schedules", a.createSchedule)
	mux.HandleFunc("PUT /api/schedules/{id}", a.updateSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", a.deleteSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/enable", a.enableSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/disable", a.disableSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/fire", a.fireSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/snooze", a.snoozeSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/read", a.markScheduleRead)
	mux.HandleFunc("GET /api/schedules/{id}/runlog", a.scheduleRunLog)

	// Todos was removed; every old route answers 410 Gone for one release
	// (daemon/todos_removed.go).
	mux.HandleFunc("/api/todos", a.todosRemoved)
	mux.HandleFunc("/api/todos/", a.todosRemoved)

	// Inject raw bytes into a pane's PTY. Powers "Fire now" in the
	// Schedules UI and is a general-purpose primitive for any future
	// automation. Same auth as the rest of /api/panes/*.
	mux.HandleFunc("POST /api/panes/{id}/inject", a.injectPane)
	mux.HandleFunc("POST /api/panes/{id}/unsent-message", a.actOnUnsentMessage)
	mux.HandleFunc("POST /api/panes/{id}/review", a.reportPaneReview)
	mux.HandleFunc("POST /api/panes/{id}/review/followup", a.reportPaneReviewFollowup)
	mux.HandleFunc("DELETE /api/panes/{id}/review", a.clearPaneReview)
	mux.HandleFunc("POST /api/pane-origins/remap", a.remapPaneOrigins)
	mux.HandleFunc("POST /api/panes/{id}/submit-message", a.submitPaneMessage)
	a.registerConditionalSubmitRoutes(mux)

	// Image paste — browser uploads raw image bytes, daemon writes them
	// to ~/.soluterminal/pastes/<name> and returns the absolute path so
	// the web layer can send `@<path>` into the Claude Code prompt.
	mux.HandleFunc("POST /api/panes/{id}/paste-image", a.pasteImage)
	// Sibling of paste-image for arbitrary (non-image) files: the extension
	// comes from the X-Filename header (allowlisted), the body is the raw file,
	// and Claude reads it from the returned `@<path>` via its Read tool.
	mux.HandleFunc("POST /api/panes/{id}/paste-file", a.pasteFile)

	// Reverse-proxy so cloud-connected browsers can reach localhost
	// dev servers running on the daemon host. Allowlisted to ports
	// the PortRegistry currently sees. Any HTTP method (including WS
	// upgrades) — hence the pattern without a verb prefix.
	mux.HandleFunc("/api/proxy/", a.proxyPort)

	// Self-update — reports the current version + cached manifest
	// (status), forces a fresh manifest fetch (check), and downloads +
	// swaps the binary then exits with ExitCodeRestartForUpdate (apply).
	// See updater.go for the full mechanism.
	mux.HandleFunc("GET /api/update/status", a.updateStatus)
	mux.HandleFunc("POST /api/update/check", a.updateCheck)
	mux.HandleFunc("POST /api/update/apply", a.updateApply)
	mux.HandleFunc("POST /api/update/apply-local", a.updateApplyLocal)
	mux.HandleFunc("GET /api/update/preview", a.previewStatus)
	mux.HandleFunc("POST /api/update/preview/install", a.previewInstall)
	mux.HandleFunc("GET /api/update/rollback", a.updateRollbackTargets)
	mux.HandleFunc("POST /api/update/rollback", a.updateRollback)
	mux.HandleFunc("DELETE /api/update/rollback/hold", a.updateHoldRelease)
	// Tauri-shell installer update (NSIS/MSI/DMG). Downloads the
	// installer and launches it; the installer takes over closing the
	// running app + replacing it. See updater.ApplyShell.
	mux.HandleFunc("POST /api/update/apply-shell", a.updateApplyShell)
	mux.HandleFunc("POST /api/update/shell/reveal", a.updateShellReveal)

	// Codex live terminal authentication (codex_live_auth.go).
	mux.HandleFunc("GET /api/codex/live-auth", a.getCodexLiveAuth)
	mux.HandleFunc("PUT /api/codex/live-auth", a.putCodexLiveAuth)
	mux.HandleFunc("POST /api/codex/live-auth/refresh", a.refreshCodexLiveAuth)
	mux.HandleFunc("POST /api/panes/{id}/codex-live-auth", a.migrateCodexLiveAuth)

	// Client-visible Supabase creds. Returning this flips the frontend
	// into the auth-gated flow (with isLocal=true skipping the
	// cloud-specific machine picker). 404 when no creds are configured
	// — preserves the no-auth local-passthrough behavior for unconfigured
	// dev builds. See config.go.
	mux.HandleFunc("GET /api/config", a.handleConfig)
	// Which team-only features this install may show (entitlements.go).
	mux.HandleFunc("GET /api/entitlements", a.handleEntitlements)
	// Account-scoped cloud resources for the desktop app (account_proxy.go).
	mux.HandleFunc("GET /api/user/prefs", a.handleUserPrefsProxy)
	mux.HandleFunc("PUT /api/user/prefs", a.handleUserPrefsProxy)
	mux.HandleFunc("GET /api/user/marketing-consent", a.handleMarketingConsentProxy)
	mux.HandleFunc("PUT /api/user/marketing-consent", a.handleMarketingConsentProxy)
	mux.HandleFunc("POST /api/user/marketing-consent", a.handleMarketingConsentProxy)
	// Email-first sign-in lookup, forwarded to the cloud (account_proxy.go).
	mux.HandleFunc("POST /api/auth/method", a.handleAuthMethodProxy)
	// Anonymous daily install check-in opt-out (install_checkin.go). PUT is
	// local-only.
	mux.HandleFunc("GET /api/settings/checkin", a.handleGetCheckinSettings)
	mux.HandleFunc("PUT /api/settings/checkin", a.handlePutCheckinSettings)
	// Usage-statistics and crash-report choices (privacy_settings.go). PUT is
	// local-only.
	mux.HandleFunc("GET /api/view", a.handleGetView)
	mux.HandleFunc("PUT /api/view", a.handlePutView)
	mux.HandleFunc("GET /api/settings/privacy", a.handleGetPrivacySettings)
	mux.HandleFunc("GET /api/settings/pane-naming", a.handleGetPaneNaming)
	mux.HandleFunc("PUT /api/settings/pane-naming", a.handlePutPaneNaming)
	mux.HandleFunc("PUT /api/settings/privacy", a.handlePutPrivacySettings)
	// Agent permissions (caller_authority.go): only a person at the computer
	// can turn them on.
	mux.HandleFunc("GET /api/settings/agent-authority", a.handleGetAgentAuthority)
	mux.HandleFunc("PUT /api/settings/agent-authority", a.handlePutAgentAuthority)
	// Remote devices (trusted_devices.go): enrolment and status are reachable
	// by unapproved devices over the relay; approval and the policy are local.
	mux.HandleFunc("GET /api/remote-devices", a.handleListRemoteDevices)
	mux.HandleFunc("POST /api/remote-devices/enroll", a.handleEnrollRemoteDevice)
	mux.HandleFunc("GET /api/remote-devices/{id}/status", a.handleRemoteDeviceStatus)
	mux.HandleFunc("POST /api/remote-devices/{id}/approve", a.handleApproveRemoteDevice)
	mux.HandleFunc("DELETE /api/remote-devices/{id}", a.handleRemoveRemoteDevice)
	mux.HandleFunc("PUT /api/remote-devices/policy", a.handlePutRemoteAccessPolicy)
	mux.HandleFunc("GET "+privateRelayPath, a.handlePrivateRelay)
	// Opt-in for the machine-wide Claude Code / Codex registration
	// (agent_integration_api.go). Changes are local-only.
	mux.HandleFunc("GET /api/settings/agent-integration", a.handleGetAgentIntegration)
	mux.HandleFunc("POST /api/settings/agent-integration", a.handleSetAgentIntegration)
	mux.HandleFunc("POST /api/settings/agent-integration/notice-ack", a.handleAckAgentIntegrationNotice)

	// Runtime-tunable PTY enter-gap (the ms wait between writing the body
	// and the trailing \r — see pty_submit.go). The web client fetches this
	// to mirror any SOLU_ENTER_GAP_* env override without a rebuild. Served
	// from a dedicated endpoint (not /api/config) because /api/config 404s
	// in local-passthrough/dev mode, but the gap must be available there
	// too. Defaults (400/100) when no env is set — additive, no behavior
	// change. See config_enter_gap.go semantics in pty_submit.go.
	mux.HandleFunc("GET /api/enter-gap", a.handleEnterGap)

	// Live cloud/relay state — the desktop UI's local heartbeat. Reports
	// whether cloud is configured, whether usable creds exist, and the live
	// relay connection state (online/connecting/offline/unconfigured), plus
	// invalid-creds / corrupt-claim flags AND any in-flight pairing progress.
	// Drives the post-pair "Paired — connecting…" copy without any restart.
	// See relay_supervisor.go + pairing.go.
	mux.HandleFunc("GET /api/cloud/status", a.handleCloudStatus)

	// Server-driven machine pairing — the embedded web UI pairs with the cloud
	// without a native OS dialog or a `solud --claim` subprocess. start mints a
	// code (via the cloud's /api/claim/start) and begins a background poll that
	// writes claim.json on accept; cancel stops a pending pair. Both are
	// LOCAL-ONLY: a relay-forwarded request is rejected (you pair from the
	// machine itself, never remotely). See pairing.go.
	mux.HandleFunc("POST /api/cloud/pair/start", a.handleCloudPairStart)
	mux.HandleFunc("POST /api/setup/claude/recheck", a.handleClaudeRecheck)
	mux.HandleFunc("GET /api/setup/claude/install-command", a.handleClaudeInstallCommand)
	mux.HandleFunc("POST /api/setup/muse/recheck", a.handleMuseRecheck)
	mux.HandleFunc("GET /api/setup/muse/install-command", a.handleMuseInstallCommand)
	mux.HandleFunc("POST /api/setup/codex/recheck", a.handleCodexRecheck)
	mux.HandleFunc("GET /api/setup/codex/install-command", a.handleCodexInstallCommand)
	mux.HandleFunc("POST /api/cloud/pair/cancel", a.handleCloudPairCancel)
	// Connect with the signed-in account (pairing_account.go): the web calls
	// it after sign-in so account features work without the pairing code.
	mux.HandleFunc("POST /api/cloud/pair/account", a.handleCloudPairAccount)
	// Forgets this machine's cloud connection (deletes claim.json + kicks the
	// supervisor back to "not set up"). LOCAL-ONLY. The escape hatch for a
	// machine whose cloud record was deleted server-side (token permanently
	// rejected → "sign-in needed" forever) or a user who no longer wants cloud.
	mux.HandleFunc("POST /api/cloud/disconnect", a.handleCloudDisconnect)
}

// cloudStatusResponse is the wire shape of GET /api/cloud/status: the
// supervisor's live cloudStatus (flattened — its fields appear at the top
// level) plus an in-flight `pairing` object. Embedding cloudStatus preserves
// every existing field/JSON tag verbatim; only `pairing` is added.
type cloudStatusResponse struct {
	cloudStatus
	Pairing pairingSnapshot `json:"pairing"`
}

// handleCloudStatus returns the relay supervisor's live state plus any in-flight
// pairing progress as JSON. On a local-only daemon (no --relay, so no
// supervisor) it reports the "unconfigured" shape so the UI renders "Local only"
// rather than erroring; pairing is then always inactive.
func (a *apiServer) handleCloudStatus(w http.ResponseWriter, _ *http.Request) {
	resp := cloudStatusResponse{
		cloudStatus: cloudStatus{Configured: false, RelayState: relayStateUnconfigured},
	}
	if a.cloudSup != nil {
		resp.cloudStatus = a.cloudSup.status()
	}
	if a.pairing != nil {
		resp.Pairing = a.pairing.snapshot()
	}
	writeJSON(w, 200, resp)
}

// pairRequestOverRelay reports whether a /api/cloud/pair/* request arrived over
// the internet-exposed cloud relay rather than the local, auth-gated server.
// The cloud proxy stamps X-Forwarded-Prefix=/m/<machineId> on every relayed
// request and OVERWRITES any client-supplied value, so a remote caller cannot
// forge "I'm local". Pairing is a machine-local operation by design — the user
// pairs from the machine itself — so we refuse it over the relay. (Same gate
// the secret-file endpoints use; see files.go fileAccessOverRelay.)
func pairRequestOverRelay(r *http.Request) bool {
	return requestViaRelay(r)
}

// handleCloudPairStart begins (or resumes) server-driven pairing. LOCAL-ONLY:
// a relay-forwarded request is rejected 403. Returns 200 {code, expiresAt}.
// Idempotent — a second start while a pair is pending returns the SAME code.
func (a *apiServer) handleCloudPairStart(w http.ResponseWriter, r *http.Request) {
	if pairRequestOverRelay(r) {
		http.Error(w, "pairing is local-only", http.StatusForbidden)
		return
	}
	if a.pairing == nil {
		http.Error(w, "cloud is not configured (no --relay); cannot pair", http.StatusBadRequest)
		return
	}
	// Pairing by hand is a fresh choice to connect; an earlier explicit
	// disconnect no longer applies.
	setUserDisconnected(a.dataDir, false)
	res, err := a.pairing.start(r.Context())
	if err != nil {
		if err == errPairUnconfigured {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Cloud unreachable / claim-start failure → 502 (upstream problem).
		http.Error(w, "could not start pairing: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, 200, res)
}

// handleCloudPairCancel cancels any pending pair. LOCAL-ONLY: a relay-forwarded
// request is rejected 403. Always 200 (idempotent — cancelling nothing is fine).
func (a *apiServer) handleCloudPairCancel(w http.ResponseWriter, r *http.Request) {
	if pairRequestOverRelay(r) {
		http.Error(w, "pairing is local-only", http.StatusForbidden)
		return
	}
	if a.pairing != nil {
		a.pairing.cancel()
	}
	w.WriteHeader(http.StatusOK)
}

// handleCloudDisconnect forgets this machine's cloud connection and removes it
// from the account: it cancels any pending pairing, queues the cloud revoke,
// deletes claim.json, stops every cloud-bound reporter before it returns, and
// kicks the relay supervisor so it tears the tunnel down and reverts to the
// "not set up" state. LOCAL-ONLY: a relay-forwarded request is rejected 403
// (you can't disconnect a machine remotely through its own tunnel — that would
// sever the very link the request arrived on). Always 200 on success
// (idempotent — disconnecting an already-disconnected machine is fine).
//
// Before the revoke, the cloud kept the machine and its token after
// Disconnect, so crash and update reports were still accepted and a copied
// claim.json still worked. The revoke is queued on disk with a revoke-only key
// (cloud_revoke.go), so it is retried when the cloud is unreachable while the
// token itself is gone at once.
//
// Disconnect is also the escape hatch for a daemon whose cloud machine record
// was deleted server-side: it parks on CredsInvalid (token permanently
// rejected → the UI shows "sign-in needed" forever) until the stale claim is
// cleared. The WSL claim.json also survives a Windows desktop reinstall, so a
// fresh install inherits the dead connection — this is the user-facing clear.
func (a *apiServer) handleCloudDisconnect(w http.ResponseWriter, r *http.Request) {
	if pairRequestOverRelay(r) {
		http.Error(w, "disconnect is local-only", http.StatusForbidden)
		return
	}
	if a.pairing != nil {
		a.pairing.cancel()
	}
	claim, _ := loadClaimSafe(a.dataDir)
	token := ""
	if claim != nil {
		token = claim.Token
		// Queue the revoke before deleting the token: if this fails we keep
		// the claim rather than lose the only way to revoke it.
		fallbackRelay := ""
		if a.pairing != nil {
			fallbackRelay = a.pairing.relayURL
		}
		revoke := cloudRevokeFromClaim(claim, fallbackRelay, time.Now())
		if err := a.queueCloudRevoke(revoke); err != nil {
			http.Error(w, "could not queue the cloud revoke: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := RemoveClaim(a.dataDir); err != nil {
		http.Error(w, "could not clear cloud credentials: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Remember the choice so signing in again doesn't reconnect on its own.
	setUserDisconnected(a.dataDir, true)
	// Stop sending now, not on the supervisor's next reconcile.
	if a.onCloudDisconnect != nil {
		a.onCloudDisconnect(token)
	}
	// Kick the supervisor to reconcile from the now-absent claim.json: it tears
	// any tunnel down and clears HasCreds/CredsInvalid → status reverts to
	// "not set up". nil on a local-only daemon (no --relay) — nothing to kick.
	if a.cloudSup != nil {
		a.cloudSup.signalTokenChanged()
	}
	w.WriteHeader(http.StatusOK)
}

// handleEnterGap returns the resolved (env-overridden, else default) PTY
// enter-gap timings in integer milliseconds. claudeMs is the Claude/raw gap
// (default 400), defaultMs the bracketed-paste gap (default 100). The web
// client (web/src/ptySubmit.ts) applies these at runtime, falling back to its
// own compiled-in 400/100 if this call hasn't returned yet — so a submit
// before this loads is always safe.
func (a *apiServer) handleEnterGap(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"claudeMs":  EnterGapClaudeMS(),
		"defaultMs": EnterGapDefaultMS(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// liveHealthIdentity is the small, authoritative subset the desktop shell
// needs before deciding whether an existing daemon may be reused. Keep the
// normal health response sourced from this same map so the startup contract
// cannot drift from the web-facing response.
func (a *apiServer) liveHealthIdentity() map[string]any {
	body := map[string]any{
		"status":    "ok",
		"version":   a.ver,
		"pid":       os.Getpid(),
		"backendID": a.backendID,
	}
	// A rollback hold naming the bundled version makes an older daemon
	// intentional rather than stale to the Windows shell.
	if a.updater != nil {
		if held := a.updater.Status().HoldVersion; held != "" {
			body["updateHoldVersion"] = held
		}
	}
	return body
}

func (a *apiServer) health(w http.ResponseWriter, r *http.Request) {
	body := a.liveHealthIdentity()
	// This exact query is the authenticated desktop startup probe. Return
	// before pane enumeration, system/readiness decoration, or fleet
	// inventory; older daemons safely ignore it and return the full payload.
	if r.URL.RawQuery == "identity=1" {
		writeJSON(w, 200, body)
		return
	}
	// A capability bit only. Never expose the token or its path.
	body["shinoqueRelay"] = a.shinoqueTokens != nil && a.shinoqueTokens.read() != ""
	body["build"] = buildProvenance()

	exe, _ := os.Executable()
	cs := a.currentClaudeStatus()
	web := CurrentWebProvenance()
	paneCount := 0
	if a.mgr != nil {
		paneCount = len(a.mgr.List())
	}
	body["uptimeSec"] = int(time.Since(a.startT).Seconds())
	body["panes"] = paneCount
	body["addr"] = a.addr
	body["shellVersion"] = a.liveShellVersion()
	body["shellPlatform"] = a.shellPlatform
	body["binaryPath"] = exe
	body["startedAt"] = a.startT.Format(time.RFC3339Nano)
	body["webVersion"] = web.WebVersion
	body["webSourceSha"] = web.SourceSHA
	// Claude Code CLI bootstrap status (claude_bootstrap.go). claudeChecked
	// is false until the first probe completes (UI shows "checking…" rather
	// than a premature "not installed").
	body["claudeChecked"] = cs.checked
	body["claudeInstalled"] = cs.installed
	body["claudeVersion"] = cs.version
	body["claudeAuthed"] = cs.authed
	// Named capabilities of this build (daemon_features.go) — clients gate
	// features on these instead of comparing versions.
	body["features"] = daemonFeatures
	// Focus (native) renderer availability: clients hide every Focus entry
	// point when false (the default since Focus was retired).
	body["focusEnabled"] = nativeRendererEnabled()
	body["workflowEngineAvailable"] = false
	body["workflowBoot"] = map[string]any{"state": "retired", "ready": true}
	ms := a.currentMuseStatus()
	body["museChecked"] = ms.checked
	body["museInstalled"] = ms.installed
	body["museVersion"] = ms.version
	body["museAuthed"] = ms.authed
	xs := a.currentCodexStatus()
	body["codexChecked"] = xs.checked
	body["codexInstalled"] = xs.installed
	body["codexVersion"] = xs.version
	body["codexAuthed"] = xs.authed
	body["paneEnvironment"] = paneEnvironment()
	for _, hook := range healthHooks {
		hook(a, body)
	}
	if system := a.systemHealthSummary(); system != nil {
		body["system"] = system
	}
	writeJSON(w, 200, body)
}

func (a *apiServer) workflowBoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"state": "retired", "ready": true})
}

// liveShellVersion reports the currently-installed Tauri shell rather than the
// --shell-version launch flag this daemon was spawned with, which goes stale
// when the desktop app is upgraded under a long-lived (flag-forwarding) daemon.
// See Updater.CurrentShellVersion. Falls back to the flag for a standalone
// daemon with no updater.
func (a *apiServer) liveShellVersion() string {
	if a.updater != nil {
		if v := a.updater.CurrentShellVersion(); v != "" {
			return v
		}
	}
	return a.shellVersion
}

// paneDTO is the JSON shape returned by /api/panes — embeds a locked
// snapshot of the pane's fields plus computed process/UI state flags.
type paneDTO struct {
	PaneSnapshot
	CodexAuth *codexPaneAuthInfo `json:"codexAuth,omitempty"`
	State     PaneRuntimeState   `json:"state"`
	// AnnounceDelegation is the EFFECTIVE delegation-announcement setting —
	// the explicit override resolved against the role default. Clients render
	// this; the raw override rides the embedded snapshot as
	// announceDelegationEvents.
	AnnounceDelegation bool `json:"announceDelegation,omitempty"`
	HasClaude          bool `json:"hasClaude"`
	HasCodex           bool `json:"hasCodex"`
	HasMuse            bool `json:"hasMuse,omitempty"`
	HasPi              bool `json:"hasPi,omitempty"`
	HasDeepseek        bool `json:"hasDeepseek,omitempty"`
	// Attention is the daemon-computed "who needs to act" signal ("none" |
	// "auto-pending" | "needs-user") every alert channel keys on — see
	// attention.go. Omitted (undefined client-side) only in the brief window
	// before the engine's first evaluation of the pane; clients treat absent
	// as "unknown → fall back to state-based alerting" so nothing is ever
	// silently swallowed. AttentionReason carries the short human explanation
	// when needs-user has one (never-auto tool, decline, escalation).
	Attention       string `json:"attention,omitempty"`
	AttentionReason string `json:"attentionReason,omitempty"`
	// LLM-graded attention item for the CURRENT needs-user prompt (see
	// aria_attention_summary.go). Empty until the async summary lands;
	// clients fall back to their heuristic tail preview.
	AttentionSummary string `json:"attentionSummary,omitempty"`
	AttentionRisk    string `json:"attentionRisk,omitempty"`
	AttentionAction  string `json:"attentionAction,omitempty"`
	// LiveCwd is the current working directory of the shell process
	// inside the pane — reflects the user's `cd`s since spawn, unlike
	// Cwd (spawn-time). Fetched every /api/panes poll via a cheap
	// readlink on /proc/<shellpid>/cwd. Empty string if the descendant
	// walk can't find a running shell (rare — dying pane).
	LiveCwd string `json:"liveCwd,omitempty"`
	// AriaBackoffStreak is the count of consecutive no-action ARIA
	// auto-evals on this pane. Once it reaches the suspend threshold
	// the recovery sweeper stops re-firing for this menu position
	// (re-arms when a new menu appears). 0 = healthy, >=3 = backed off.
	AriaBackoffStreak int `json:"ariaBackoffStreak,omitempty"`
	// Degraded is true when the pane's running `claude` inherited a dirty
	// CLAUDE_CODE child-session env and will never write its own transcript,
	// so the conversation/Activity view is permanently empty until the pane
	// is restarted/migrated. Computed live from /proc; omitted when detection
	// is unavailable (non-Linux, no claude descendant, environ unreadable) so
	// healthy and undetectable panes look identical on the wire.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degradedReason,omitempty"`
	// SwitchNotice is set when the most recent Terminal↔Native mode switch
	// INTERRUPTED an in-flight turn, so the client can show a "previous response
	// interrupted — Continue" marker. null/omitted when there's no outstanding
	// notice. Cleared on the next user turn (or explicit ack). Shape:
	// {"interrupted":true,"fromKind":"native"|"terminal","at":<unixMillis>}.
	SwitchNotice *switchNotice `json:"switchNotice,omitempty"`
	// PendingSwitch is set when a DEFERRED Terminal↔Native switch is armed but
	// not yet fired (waiting for the agent's current turn to settle). null/omitted
	// when none is armed. Shape: {"target":"native"|"terminal","at":<unixMillis>}.
	// target is the destination kind, with the empty terminal kind normalized to
	// "terminal" (mirroring switchNotice.fromKind).
	PendingSwitch *pendingSwitchDTO `json:"pendingSwitch,omitempty"`
	// DelegatedPending is the durable delegation queue summarized for the pane
	// header. It is omitted while no updates are buffered.
	DelegatedPending *delegatedPendingDTO `json:"delegatedPending,omitempty"`
	// AgentMidTurn is the prompt-aware "switching the renderer NOW would interrupt
	// live agent work" signal the client's mode-switch confirm gates on. Unlike
	// the coarse `state` it does NOT misread a terminal Claude pane's ~3s
	// post-turn recency tail (final redraws / cursor blink while the prompt box is
	// already shown) as "working", so a just-settled pane switches one-click
	// instead of wrongly offering the "switch when done" confirm. ALWAYS emitted
	// (no omitempty): a literal false is meaningful — the client distinguishes it
	// from an absent field (older daemon → falls back to the recency states).
	AgentMidTurn bool `json:"agentMidTurn"`
	// AgentRssMB is the last-sampled resident memory (MB) of the pane's selected
	// Claude or Codex process — the heap "Restart agent" reclaims. Updated by the
	// slow RSS sampler (~60s), so this is cached, never a per-request /proc read.
	AgentRssMB int `json:"agentRssMB,omitempty"`
	// AgentVersion is the build the pane's `solud agent` process reported.
	// AgentStale is true only when that agent speaks an older
	// agentProtoVersion than this daemon — i.e. restarting the pane would
	// actually change relay behavior. A merely different binary is not stale.
	AgentVersion string `json:"agentVersion,omitempty"`
	AgentStale   bool   `json:"agentStale,omitempty"`
	// ClaudeRssMB is a compatibility alias for clients loaded before AgentRssMB
	// shipped. Emit both during the rolling update; new clients prefer AgentRssMB.
	ClaudeRssMB int `json:"claudeRssMB,omitempty"`
	// AITitle is the last-known Claude-generated conversation title (the same
	// ai-title the Activity header shows), cached on the pane and warmed OFF the
	// request path (conversation endpoint + activity watcher), so the bulk list
	// carries it without a per-pane transcript scan here. Empty until the pane's
	// transcript has been read at least once (opened, or a completed turn) — the
	// client's list subtitle prefers titleOverride, then this, then last-prompt.
	AITitle string `json:"aiTitle,omitempty"`
	// PendingApproval surfaces the pane's SINGLE open native tool-permission so a
	// client can render Approve/Deny instantly (no /native/history fetch) and a
	// push can carry the requestId for a one-tap resolve. Set only for a native
	// pane with exactly one open approval — resolution needs a concrete requestId
	// and an arbitrary pick among parallel approvals could answer the wrong tool.
	PendingApproval *approvalHintDTO `json:"pendingApproval,omitempty"`
	// ResumeReady gates the header "resume" badge: true only when this pane's
	// captured claudeSessionId has a transcript actually on disk. The id is pinned
	// at SessionStart — BEFORE the transcript is written (a brand-new claude writes
	// nothing until its first message) — so claudeSessionId alone is premature for
	// the badge. Derived live via a single os.Stat (NOT a transcript walk); omitted
	// (→ undefined client-side) when not ready.
	ResumeReady bool `json:"resumeReady,omitempty"`
	// IdentityBlocked means a foreground-provider/thread probe is ambiguous or
	// temporarily unverifiable. Both providers' durable IDs remain stored for
	// recovery but are hidden from this DTO, and renderer switching is refused.
	IdentityBlocked bool `json:"identityBlocked,omitempty"`
	// NativeEpoch is the daemon's per-process id (daemonEpoch). It changes only when
	// the daemon restarts — exactly when every native pane's event ring is rebuilt
	// from seq 1 by AttachExisting while the surviving agent keeps its pid. A native
	// client resets + re-seeds its fold cache on an epoch change (the same reset a
	// pid respawn triggers), so a mid-turn daemon swap reattaches LOSSLESSLY instead
	// of jumbling the re-seq'd replay against the surviving client cursor. omitempty:
	// an older daemon omits it → the client sees undefined and never false-resets (it
	// resets only on a defined→defined change).
	NativeEpoch uint64 `json:"nativeEpoch,omitempty"`
	// WorkflowStatus surfaces a background Claude Code Workflow still running after
	// the launching turn ended — the Workflow tool returns immediately + notifies on
	// completion, so nativeInTurn settles to idle while subagents keep churning and
	// the pane would otherwise look done. Present only while the pane's
	// nativeWorkflowActive latch is set (Workflow tool_call seen, not yet cleared by
	// the next turn). The client renders a "workflow · N/M agents · active Xs ago"
	// footer line and infers stalled-vs-working from lastActivityMs. omitted (nil)
	// when no workflow is running, so it costs absent panes nothing. See
	// pane_provider_status.go.
	WorkflowStatus *workflowStatus `json:"workflowStatus,omitempty"`
	// Turn summarizes terminal-mode agent turns (turns.go): whether one is
	// running and since when, plus the last completed turn's change stats.
	Turn *paneTurnDTO `json:"turn,omitempty"`
}

// approvalHintDTO is the wire shape of a pane's single open native approval.
type approvalHintDTO struct {
	RequestID string `json:"requestId"`
	ToolName  string `json:"toolName"`
	Summary   string `json:"summary,omitempty"`
}

// pendingSwitchDTO is the wire shape of an armed deferred mode switch. Target is
// normalized ("" → "terminal") so the client never sees the empty kind. At is
// the arm time in unix millis.
type pendingSwitchDTO struct {
	Target string `json:"target"`
	At     int64  `json:"at"`
}

type delegatedPendingDTO struct {
	Count    int   `json:"count"`
	NeedsYou int   `json:"needsYou"`
	OldestAt int64 `json:"oldestAt"`
}

type paneProcessSnapshot struct {
	hasClaude   bool
	hasCodex    bool
	hasMuse     bool
	hasPi       bool
	hasDeepseek bool
	liveCwd     string
	claudePid   int
	degraded    paneDegradedResult
}

// codexIdentityProbe separates verified thread absence/continuity from an
// empty identity caused by failed or ambiguous process inspection.
type codexIdentityProbe struct {
	threadID       string
	resumeThreadID string
	active         bool
	fresh          bool
	blocked        bool
}

func snapshotPaneProcesses(p *Pane) paneProcessSnapshot {
	return snapshotPaneProcessesFromTree(p, snapshotProcessTree())
}

// sweepCodexIdentities runs the same per-pane process scan the /api/panes poll
// performs (snapshotPaneProcessesFromTree → discoverCodexIdentity → adopt*),
// so Codex thread adoption does not DEPEND on a client polling. The poll only
// fires while a window is open; a Codex TUI running while every client is
// closed would otherwise never bind its thread id to the pane — and after a
// teardown that pane would be dropped at boot as an unidentified shell,
// orphaning a perfectly resumable rollout. One process-tree snapshot per
// sweep, shared across panes; adoption persists through onPromptsCommit
// exactly as on the poll path, and the adopt* setters are mutex/atomic-safe
// against a concurrent poll.
func sweepCodexIdentities(mgr *Manager) {
	tree := snapshotProcessTree()
	for _, p := range mgr.List() {
		snap := p.Snapshot()
		// Native panes learn identity over their protocol. Exited panes have
		// no processes to scan, but their failed durable writes still need retry.
		if snap.Status == PaneRunning && snap.Kind != "native" {
			snapshotPaneProcessesFromTree(p, tree)
		}
		// Retry durable identity writes even when discovery sees the same ID.
		// PaneStore.Put skips unchanged snapshots, avoiding repeated fsyncs.
		if mgr.onPromptsCommit != nil {
			mgr.onPromptsCommit(p)
		}
	}
}

func snapshotPaneProcessesFromTree(p *Pane, tree processTreeSnapshot) paneProcessSnapshot {
	if p.Pid <= 0 {
		return paneProcessSnapshot{}
	}
	pids := tree.descendantPids(p.Pid)
	snap := paneProcessSnapshot{
		liveCwd: liveCwdOfPids(p.Pid, pids),
	}
	codexPids := make([]int, 0, 1)
	claudePids := make([]int, 0, 1)
	musePids := make([]int, 0, 1)
	for _, pid := range pids {
		if pid == p.Pid {
			continue
		}
		comm := readComm(pid)
		switch comm {
		case "claude":
			if !snap.hasClaude {
				snap.claudePid = pid // first claude descendant; used for degraded detection
			}
			snap.hasClaude = true
			claudePids = append(claudePids, pid)
		case "codex":
			snap.hasCodex = true
			codexPids = append(codexPids, pid)
		}
		if isMuseProcessName(comm) {
			snap.hasMuse = true
			musePids = append(musePids, pid)
		}
		// Managed-harness markers live in the child argv and are matched with
		// NO comm prefilter. Runtimes rename their main thread (Node >=24
		// reports "MainThread", not "node", in /proc/pid/comm), so gating
		// the argv read on comm skipped exactly the processes carrying the
		// marker and the badge never appeared. The client/extension filename
		// is itself the precise managed-only marker — a bare DSH, a wrapper,
		// or a shell never contains it — so no prefilter is needed.
		// (Pi: `node <runtime> … --extension …/pi-approval.mjs`; DeepSeek:
		// `node …/deepseek-terminal.mjs <runtime> <model>`.)
		if !snap.hasPi || !snap.hasDeepseek || !snap.hasMuse {
			cmdline := readHarnessCmdline(pid)
			if !snap.hasPi {
				snap.hasPi = isManagedPiCmdline(cmdline)
			}
			if !snap.hasDeepseek {
				snap.hasDeepseek = isManagedDeepseekCmdline(cmdline)
			}
			// Muse comm matching misses the launcher bash phase (and any
			// wrapper); the argv basename is the stable fallback so the
			// auto-approve badge and Codex-class prompt handling still arm.
			// Deliberately NOT appended to musePids: that set feeds
			// session-identity discovery (foreground + fd-lock verified),
			// while this flag only gates capability UI.
			if !snap.hasMuse {
				snap.hasMuse = isMuseCmdline(cmdline)
			}
		}
	}
	// Degraded detection rides this scan: one small environ read for the
	// claude descendant (cached per pid), only when a claude is present.
	if snap.hasClaude {
		snap.degraded = detectPaneDegraded(p.ID, snap.claudePid)
	}
	// A Codex TUI launched manually from a shell was historically visible only
	// as process liveness: the pane remained a legacy/Claude pane and therefore
	// could not switch to Focus. Bind it to the exact foreground Codex thread
	// once that thread has a resumable identity. Discovery rejects background,
	// subagent, malformed, outside-CODEX_HOME, and multi-candidate cases.
	// Background descendants do not own the pane. The foreground provider may
	// replace a previously persisted provider/thread when a shell is reused.
	identity := p.Snapshot()
	if identity.Kind != "native" {
		museID, foregroundMuse := discoverMuseIdentity(p.ID, musePids)
		codexIdentity := discoverCodexIdentity(p.ID, codexPids)
		foregroundCodex := codexIdentity.active
		// Codex discovery performs bounded filesystem I/O, so sample Claude
		// foreground ownership afterwards to minimize provider-handoff races.
		foregroundClaude := false
		for _, pid := range claudePids {
			if paneDescendantIsForeground(pid) {
				foregroundClaude = true
				break
			}
		}
		switch {
		case foregroundMuse:
			// Never resume an old Codex/Claude conversation over a manually
			// launched Muse. An unverified foreground identity blocks replacement.
			if foregroundCodex || foregroundClaude {
				museID = ""
			}
			adoptForegroundMuse(p, museID)
		case foregroundCodex && !foregroundClaude:
			switch {
			case codexIdentity.threadID != "":
				adoptDiscoveredCodexThread(p, codexIdentity.threadID)
			case codexIdentity.fresh:
				adoptFreshCodexIdentity(p)
			case codexIdentity.resumeThreadID != "":
				// An explicit `codex resume <uuid>` from a user shell is itself
				// a strong identity claim once the process, pane ownership and
				// foreground checks above have passed.  Previously this branch
				// required the pane to already carry the same durable id, which
				// only held for SOLU-created Terminal↔Focus switches and left a
				// manually resumed shell stuck as an untracked `shell` pane.
				// The rollout is still validated by the actual resume/restart
				// path; this early adoption only lets the DTO converge and show
				// the copyable resume identity.
				adoptResumingCodexIdentity(p, codexIdentity.resumeThreadID)
			default:
				adoptDiscoveredCodexThread(p, "")
			}
		case foregroundClaude && !foregroundCodex:
			adoptForegroundClaude(p)
		case !foregroundCodex && !foregroundClaude:
			if p.Snapshot().Status == PaneExited {
				break
			}
			// A transient unidentified-Codex block must not outlive the process;
			// once the shell is foreground again, its last known thread is safe to
			// offer as resumable continuity.
			p.setCodexIdentityBlocked(false)
		case foregroundCodex && foregroundClaude:
			// Ambiguous foreground ownership: retain durable metadata, expose none.
			p.setCodexIdentityBlocked(true)
		}
	}
	// Managed terminal harnesses (DeepSeek today) write their durable session
	// to a per-pane sidecar; adopt it so the resume paths and panes.json see a
	// conversation id, exactly like the Muse/Codex discovery above. Runs for
	// native and terminal panes alike (a descriptor-only no-op when no sidecar
	// exists or the pane carries no harness dirs).
	if p.discoverHarnessSession() && p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
	return snap
}

func adoptDiscoveredCodexThread(p *Pane, threadID string) bool {
	if !p.SetForegroundCodexIdentity(threadID) {
		return false
	}
	if p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
	return true
}

func adoptFreshCodexIdentity(p *Pane) bool {
	if !p.SetForegroundFreshCodexIdentity() {
		return false
	}
	if p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
	return true
}

func adoptResumingCodexIdentity(p *Pane, threadID string) bool {
	if !p.SetForegroundResumingCodexIdentity(threadID) {
		return false
	}
	if p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
	return true
}

func adoptForegroundClaude(p *Pane) bool {
	if !p.SetForegroundClaudeIdentity() {
		return false
	}
	if p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
	return true
}

// approvalHint returns the pane's single open native approval as a wire hint, or
// nil when there are zero or multiple — resolution needs a concrete requestId,
// and an arbitrary pick among parallel approvals could answer the wrong tool.
// Cheap: listNativePending returns nil before sorting for every terminal pane.
func approvalHint(p *Pane) *approvalHintDTO {
	pending := p.listNativePending()
	if len(pending) != 1 {
		return nil
	}
	title, preview := nativeApprovalSummary(pending[0])
	return &approvalHintDTO{RequestID: pending[0].RequestID, ToolName: title, Summary: preview}
}

func makePaneDTO(p *Pane, aria *Aria, att *attentionEngine) paneDTO {
	proc := snapshotPaneProcesses(p)
	return makePaneDTOWithProcessSnapshot(p, aria, att, proc)
}

func makePaneDTOWithProcessSnapshot(p *Pane, aria *Aria, att *attentionEngine, proc paneProcessSnapshot) paneDTO {
	d := paneDTO{
		PaneSnapshot: p.Snapshot(),
		State:        p.computeStateWithHasCodex(proc.hasCodex || proc.hasMuse),
		HasClaude:    proc.hasClaude,
		HasCodex:     proc.hasCodex,
		HasMuse:      proc.hasMuse,
		HasPi:        proc.hasPi,
		HasDeepseek:  proc.hasDeepseek,
		LiveCwd:      proc.liveCwd,
		Turn:         turnSummaryFor(p.ID),
	}
	d.Credential = p.projectLiveCredential(paneCredentialProjection(d.Credential, p.credentialSelectionCallback(), p.credentialTeamSelectionCallback()))
	// Capability badges must not hinge on the live process scan alone: it
	// misses whenever a client argv is unreadable or hasn't appeared yet,
	// while the pane's adopted harness session or spawn argv already says
	// what it is. The scan stays primary (a live client always marks its
	// pane); this only fills the gap so the auto-approve button appears.
	switch managedHarnessBadgeProvider(d.Cmd, d.HarnessProvider) {
	case "deepseek":
		d.HasDeepseek = true
	case "pi", "pi-glm":
		d.HasPi = true
	}
	d.AnnounceDelegation = d.PaneSnapshot.announceDelegationEffective()
	if p.codexIdentityBlocked.Load() {
		d.IdentityBlocked = true
		d.CodexThreadID = ""
		d.MuseSessionID = ""
		d.ClaudeSessionID = ""
		d.ClaudeTranscriptPath = ""
	}
	// Attention reconciled against the state THIS DTO carries (CurrentFor):
	// the stored record is ≤2s stale, and shipping state=choice-prompt with
	// the pre-prompt record made clients flash "needs you" until the next
	// engine tick said auto-pending. nil engine (tests) → last stored record.
	if att != nil {
		a, reason := att.CurrentFor(p, d.State)
		d.Attention, d.AttentionReason = string(a), reason
	} else {
		a, reason := p.AttentionNow()
		d.Attention, d.AttentionReason = string(a), reason
	}
	// Summary only rides needs-user DTOs — anything else would describe a
	// prompt the engine no longer considers the human's problem.
	if d.Attention == string(AttentionNeedsUser) {
		if rec := p.attentionSummaryNow(); rec != nil {
			d.AttentionSummary, d.AttentionRisk, d.AttentionAction = rec.Summary, rec.Risk, rec.Action
		}
	}
	// Derive the switch-confirm signal from the state we just computed (no second
	// buffer strip): only a busy/recent terminal pane pays the small prompt-box
	// scan; native/idle/awaiting/choice short-circuit. Lets the client offer a
	// one-click switch on a just-settled pane instead of the "switch when done"
	// confirm.
	d.AgentMidTurn = p.agentMidTurnForState(d.State)
	// Cached atomic load — the RSS sampler refreshes it every ~60s off the hot
	// path; the DTO never reads /proc itself. Keep the old field populated as a
	// rolling-update compatibility alias.
	d.AgentRssMB = int(p.agentRssMB.Load())
	d.ClaudeRssMB = d.AgentRssMB
	if version, _, proto := p.agentRuntimeInfo(); version != "" {
		d.AgentVersion = version
		d.AgentStale = d.Status == PaneRunning && agentProtoStale(proto)
	}
	// Per-SPAWN id: a change is a native client's only signal that its event ring
	// was rebuilt from seq 1 — on a daemon restart/swap (surviving agent, same pid)
	// AND on a Term↔Focus respawn (new agent, same daemon). It resets + re-seeds on
	// the change for a lossless reattach / non-blank switch. ensureNative stamps it;
	// a terminal pane (never ensureNative'd) reads 0, so fall back to daemonEpoch.
	d.NativeEpoch = p.nativeEpoch.Load()
	if d.NativeEpoch == 0 {
		d.NativeEpoch = daemonEpoch
	}
	// Cheap atomic load — the title is warmed off the request path, never
	// scanned from the transcript here (keeps the list hot path I/O-free).
	d.AITitle = p.AITitleCache()
	// Single open native approval → instant Approve/Deny on the client + a
	// one-tap-resolvable push (nil for terminal + parallel-approval panes).
	d.PendingApproval = approvalHint(p)
	// Background-workflow footer signal. nil unless this native pane has a Workflow
	// latched (gated on one atomic load), so non-workflow panes do no filesystem I/O.
	d.WorkflowStatus = paneWorkflowStatus(p)
	// Gate the header "resume" badge on the transcript actually existing — the id
	// is captured at SessionStart before the file is written. One os.Stat, never a
	// transcript walk (claudeTranscriptOnDisk); only when an id is present.
	if effectiveAgentKind(d.Agent) == agentCodex {
		// Codex app-server owns thread validation during thread/resume; unlike
		// Claude there is no pane-specific transcript path to stat here.
		d.ResumeReady = strings.TrimSpace(d.CodexThreadID) != ""
	} else if effectiveAgentKind(d.Agent) == agentMuse {
		// Muse owns session validation on `muse exec --session-id` /
		// `muse resume` — a known id is the whole readiness signal.
		d.ResumeReady = strings.TrimSpace(d.MuseSessionID) != ""
	} else if sid := strings.TrimSpace(d.ClaudeSessionID); sid != "" {
		d.ResumeReady = claudeTranscriptOnDisk(d.Cwd, sid, d.ClaudeTranscriptPath)
	}
	if aria != nil {
		d.AriaBackoffStreak = aria.BackoffStreak(d.ID)
	}
	// Surface degraded only on a conclusive positive read (never on unknown),
	// so healthy and undetectable panes are indistinguishable on the wire.
	if r := proc.degraded; !r.unknown && r.degraded {
		d.Degraded = true
		d.DegradedReason = r.reason
	}
	// Surface a switch-interrupted notice (set by a graceful mode switch that
	// interrupted an in-flight turn) for EVERY pane; the client only renders it
	// for native panes. Omitted (nil) when there's no outstanding notice.
	d.SwitchNotice = p.switchNoticeSnapshot()
	// Surface an armed-but-not-yet-fired deferred switch so the client can show a
	// "switching after this turn" affordance / offer cancel. target normalized
	// "" → "terminal" (mirrors switchNotice.fromKind). Omitted when none armed.
	if pend := p.pendingSwitchSnapshot(); pend != nil {
		target := pend.Target
		if target == "" {
			target = "terminal"
		}
		d.PendingSwitch = &pendingSwitchDTO{Target: target, At: pend.RequestedAt}
	}
	if pending := p.delegationPendingSnapshot(); pending != nil {
		d.DelegatedPending = &delegatedPendingDTO{
			Count: pending.Count, NeedsYou: pending.NeedsYou, OldestAt: pending.OldestAt.UnixMilli(),
		}
	}
	return d
}

func (a *apiServer) listPanes(w http.ResponseWriter, _ *http.Request) {
	panes := a.mgr.List()
	out := make([]paneDTO, len(panes))
	tree := snapshotProcessTree()
	var revision, selected string
	if a.codexLogin != nil {
		live, _ := os.ReadFile(a.codexLogin.livePath())
		revision = codexAuthFingerprint(live)
		selected = a.credentialAccount("codex")
		// Do not attach a new selection's label to an older credential revision
		// when an activation races this read-only projection.
		latest, err := os.ReadFile(a.codexLogin.livePath())
		if err != nil || codexAuthFingerprint(latest) != revision {
			revision, selected = "", ""
		}
	}
	for i, p := range panes {
		out[i] = makePaneDTOWithProcessSnapshot(p, a.aria, a.attention, snapshotPaneProcessesFromTree(p, tree))
		if a.codexLogin != nil && out[i].Kind == "" && out[i].Status == PaneRunning && (out[i].HasCodex || effectiveAgentKind(out[i].Agent) == agentCodex) {
			st := readCodexLiveStatus(a.mgr.dataDir, p.ID)
			owned := false
			for _, pid := range p.ChildPids() {
				if pid == st.PID {
					owned = true
					break
				}
			}
			if !owned {
				st = codexLiveStatus{}
			}
			out[i].CodexAuth = codexPaneAuthProjection(out[i].PaneSnapshot, st, revision, selected, codexLiveEnabled(a.mgr.dataDir) && codexLiveSupported(), time.Now())
			a.decorateCodexPaneAuth(out[i].CodexAuth, p, out[i].PaneSnapshot)
		}
	}
	writeJSON(w, 200, out)
}

type createPaneReq struct {
	Restore         bool     `json:"restore,omitempty"`
	IdentityBlocked bool     `json:"identityBlocked,omitempty"`
	Cwd             string   `json:"cwd"`
	Cmd             []string `json:"cmd"`
	Title           string   `json:"title"`
	Cols            uint16   `json:"cols"`
	Rows            uint16   `json:"rows"`
	Workspace       string   `json:"workspace,omitempty"`
	WorkspaceTarget string   `json:"workspaceTarget,omitempty"`
	WorkspaceSplit  string   `json:"workspaceSplit,omitempty"`
	// Kind opts a pane into native-renderer mode ("native"). Default ""
	// (terminal) keeps today's byte-identical behavior. Gated by the global
	// SOLU_NATIVE_RENDERER env flag — a native request is rejected (400) when
	// the feature is off, so terminal panes are entirely unaffected.
	Kind string `json:"kind,omitempty"`
	// Agent selects the agent protocol independently from the renderer. Omitted
	// or "claude" keeps the legacy Claude path; "codex" selects the Codex
	// app-server protocol and "muse" the Muse Spark protocol for Focus panes.
	Agent         string `json:"agent,omitempty"`
	CodexThreadID string `json:"codexThreadId,omitempty"`
	// MuseSessionID, when set, resumes an existing Muse session (`muse
	// resume <id>` for terminal panes; the native adapter passes it as
	// `muse exec --session-id`). Empty = a fresh session.
	MuseSessionID string `json:"museSessionId,omitempty"`
	// ClaudeSessionID, when set, resumes an existing Claude conversation:
	// the pane spawns `claude [-p] --resume <id>`, and a native pane also
	// seeds its transcript history (GET /native/history) from it. Empty =
	// a fresh session. This is how you open an existing conversation
	// directly in native mode.
	ClaudeSessionID string `json:"claudeSessionId,omitempty"`
	// Provider routes the pane's claude to "glm" (Z.ai) instead of Anthropic
	// (""/"anthropic"). This is a NAME only — the daemon resolves the Z.ai key
	// server-side from ~/.config/glm/env; the browser NEVER sends a token.
	Provider string `json:"provider,omitempty"`
	// Effort sets the pane's reasoning-effort level (--effort): one of
	// "low" | "medium" | "high" | "xhigh" | "max", or "" for claude's default.
	// Provider-agnostic. For a GLM pane an empty value defaults to "max" (Z.ai's
	// coding recommendation); an Anthropic pane left empty keeps claude's default.
	Effort string `json:"effort,omitempty"`
	// Model sets the pane's Claude model (--model id, e.g. "claude-fable-5-1").
	// "" keeps claude's default (fresh session) or the session's stored model
	// (resume). Native panes only care; a terminal pane ignores it.
	Model string `json:"model,omitempty"`
	// Role sets the pane's product role: "" (plain) or "orchestrator" — the
	// chief-of-staff pane spawned with the delegation brief. Valid for a
	// supported Claude or Codex host: native, or a terminal command running the
	// selected host.
	Role string `json:"role,omitempty"`
	// ContextPack names an absolute or ~-prefixed directory containing
	// CLAUDE.md and/or AGENTS.md. Empty inherits the workspace default; the
	// literal "none" explicitly opts out of that inheritance.
	ContextPack string `json:"contextPack,omitempty"`
	// StartedBy is "user" when the person opened the pane from the app;
	// anything else (scripts, other callers) is recorded as "api". Origin and
	// TitleOverride bring back a saved pane's name and "how it was started"
	// record on Restore; Origin is also kept when a pane is retried.
	StartedBy     string      `json:"startedBy,omitempty"`
	TitleOverride string      `json:"titleOverride,omitempty"`
	Origin        *PaneOrigin `json:"origin,omitempty"`
	// Identity brings back a saved pane's handle and working-on line on
	// Restore; a handle another pane holds by then is dropped.
	Identity *PaneIdentity `json:"identity,omitempty"`
}

// createPaneOrigin is the origin recorded for a pane created over HTTP.
func createPaneOrigin(req createPaneReq, workspace string, now time.Time) (*PaneOrigin, string) {
	if req.Restore {
		return req.Origin.clone(), strings.TrimSpace(req.TitleOverride)
	}
	// A retried pane carries its saved origin (informational, see PaneOrigin).
	if req.Origin != nil {
		return req.Origin.clone(), ""
	}
	source := paneOriginAPI
	if strings.TrimSpace(req.StartedBy) == paneOriginUser {
		source = paneOriginUser
	}
	return &PaneOrigin{Source: source, Workspace: workspace, At: now.UnixMilli()}, ""
}

func (a *apiServer) createPane(w http.ResponseWriter, r *http.Request) {
	var req createPaneReq
	// Cap decode memory at 1MB; oversize folds into the same "invalid json" 400
	// that the global 32MB request-body middleware already yields.
	if !decodeJSON(w, r, &req, 1<<20) {
		return
	}
	if req.Restore {
		if err := preparePaneRestore(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	workspace := strings.TrimSpace(req.Workspace)
	if workspace != "" && workspace != "_current" {
		if a.ws == nil {
			http.Error(w, "workspace store not initialized", 500)
			return
		}
		resolved, ok := resolveWorkspaceName(a.ws, workspace)
		if !ok {
			http.Error(w, "workspace not found: "+workspace, 404)
			return
		}
		workspace = resolved
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "native" && !nativeRendererEnabled() {
		http.Error(w, "native renderer is disabled: Focus mode has been retired — open a terminal pane instead", 400)
		return
	}
	if kind != "" && kind != "native" {
		http.Error(w, "unknown pane kind: "+kind, 400)
		return
	}
	agent, validAgent := normalizeAgentKind(req.Agent, req.Cmd, req.CodexThreadID)
	if !validAgent {
		http.Error(w, "unknown agent: "+strings.TrimSpace(req.Agent), http.StatusBadRequest)
		return
	}
	// An explicit Claude agent paired with a direct Codex command is internally
	// contradictory. More importantly, for native requests it used to silently
	// discard cmd:["codex"] and spawn Claude. Refuse the mismatch rather than
	// guessing and ensure the legacy-shaped native request gets a clear response.
	if strings.EqualFold(strings.TrimSpace(req.Agent), agentClaude) && cmdRunsCodex(req.Cmd) {
		http.Error(w, "agent is claude but command starts Codex", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.Agent), agentClaude) && cmdStartsMuse(req.Cmd) {
		http.Error(w, "agent is claude but command starts Muse", http.StatusBadRequest)
		return
	}
	if kind != "native" && effectiveAgentKind(agent) == agentCodex && !cmdRunsCodex(req.Cmd) {
		http.Error(w, "agent is codex but command does not start Codex", http.StatusBadRequest)
		return
	}
	if kind != "native" && effectiveAgentKind(agent) == agentMuse && !cmdStartsMuse(req.Cmd) {
		http.Error(w, "agent is muse but command does not start Muse", http.StatusBadRequest)
		return
	}
	// Provider: normalize "anthropic" → "" (the default), reject anything other
	// than "glm", and fail fast with a clear 400 if GLM is requested without a
	// key configured — otherwise claude would spawn with a blank token and die
	// confusingly. KeyConfigured resolves ~/.config/glm/env live.
	provider := strings.TrimSpace(req.Provider)
	if provider == "anthropic" {
		provider = ""
	}
	if provider != "" && provider != "glm" {
		http.Error(w, "unknown provider: "+provider, 400)
		return
	}
	if (effectiveAgentKind(agent) == agentCodex || effectiveAgentKind(agent) == agentMuse) && provider != "" {
		http.Error(w, "provider is only supported for Claude panes", http.StatusBadRequest)
		return
	}
	if provider == "glm" && (a.mgr.glmSettings == nil || !a.mgr.glmSettings.KeyConfigured()) {
		http.Error(w, "GLM provider selected but no Z.ai key configured — set ZAI_API_KEY in ~/.config/glm/env", 400)
		return
	}
	// Role availability is capability-driven. Refuse unsupported or unavailable
	// hosts before spawning so a pane can never wear the badge without receiving
	// the brief and delegation tools.
	role := strings.TrimSpace(req.Role)
	if !validPaneRole(role) {
		http.Error(w, "unknown role: "+role, http.StatusBadRequest)
		return
	}
	if role != "" {
		agentForRole := effectiveAgentKind(agent)
		if kind != "native" {
			if fromCmd := agentFromCmd(req.Cmd); fromCmd != "" {
				agentForRole = fromCmd
			}
		}
		if !orchestratorHostSupported(agentForRole) {
			label := orchestratorHostLabel(agentForRole)
			if label == "" {
				label = "this command"
			}
			http.Error(w, "orchestrator role is not available for "+label+": "+orchestratorHostUnsupportedReason(agentForRole), http.StatusBadRequest)
			return
		}
		host, _ := orchestratorHostFor(agentForRole)
		if kind != "native" && !host.runsCmd(req.Cmd) {
			http.Error(w, "orchestrator role requires a claude or codex command", http.StatusBadRequest)
			return
		}
		if avail := a.orchestratorHostAvailability(agentForRole); !avail.Available {
			http.Error(w, "orchestrator role unavailable for "+host.label+": "+avail.Reason, http.StatusBadRequest)
			return
		}
		if agentForRole == agentCodex {
			agent = agentCodex
		}
	}
	// Effort: validate against the known levels and default a GLM pane to "max"
	// (Z.ai recommends max for coding) when the caller left it unset. Anthropic
	// panes left empty keep claude's built-in default (no --effort flag).
	effort := strings.TrimSpace(req.Effort)
	if !validEffortLevel(effort) {
		http.Error(w, "unknown effort: "+effort, 400)
		return
	}
	if provider == "glm" && effort == "" {
		effort = "max"
	}
	model := strings.TrimSpace(req.Model)
	if !validModelID(model) {
		http.Error(w, "invalid model: "+model, 400)
		return
	}
	pack := strings.TrimSpace(req.ContextPack)
	inheritedPack := false
	if pack == contextPackNone {
		pack = ""
	} else if pack == "" && workspace != "" && workspace != "_current" && a.ws != nil {
		if ws, ok := a.ws.Get(workspace); ok {
			pack = ws.ContextPack
			inheritedPack = pack != ""
		}
	}
	absPack, err := validContextPackDir(pack)
	if err != nil && !inheritedPack {
		http.Error(w, "invalid context pack: "+err.Error(), http.StatusBadRequest)
		return
	}
	paneOrigin, restoredTitle := createPaneOrigin(req, workspace, time.Now())
	restoredIdentity, releaseHandle := a.restoredPaneIdentity(req)
	defer releaseHandle()
	// A restored pane's starter or lead may have come back under a new id in
	// an earlier restore.
	paneOrigin = a.originAliases.resolveOrigin(paneOrigin)
	p, err := a.spawnPaneForRequest(SpawnOpts{
		RequireResumableConversation: req.Restore && conversationIDForAgent(agent, req.ClaudeSessionID, req.CodexThreadID, req.MuseSessionID) != "",
		Cwd:                          req.Cwd,
		Cmd:                          req.Cmd,
		Title:                        req.Title,
		Cols:                         req.Cols,
		Rows:                         req.Rows,
		Kind:                         kind,
		Agent:                        agent,
		CodexThreadID:                strings.TrimSpace(req.CodexThreadID),
		MuseSessionID:                strings.TrimSpace(req.MuseSessionID),
		Provider:                     provider,
		Effort:                       effort,
		Model:                        model,
		Role:                         role,
		ContextPack:                  absPack,
		ClaudeSessionID:              strings.TrimSpace(req.ClaudeSessionID),
		Origin:                       paneOrigin,
		TitleOverride:                restoredTitle,
		Identity:                     restoredIdentity,
	})
	if err != nil {
		if _, bad := err.(*SpawnError); bad {
			http.Error(w, err.Error(), 400)
			return
		}
		// The access log only records the status; without this line a
		// failing spawn leaves no reason anywhere on the host.
		log.Printf("create pane: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}
	// Persist for reattach on daemon restart.
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			a.rollbackSpawnedPane(p)
			http.Error(w, "persist pane: "+err.Error(), 500)
			return
		}
	}
	if workspace != "" && workspace != "_current" {
		snap := p.Snapshot()
		_, err := a.ws.AddPane(workspace, PaneDef{
			ID:              snap.ID,
			Cwd:             snap.Cwd,
			Cmd:             snap.Cmd,
			Title:           snap.Title,
			Cols:            snap.Cols,
			Rows:            snap.Rows,
			Flagged:         snap.Flagged,
			FlagMemo:        snap.FlagMemo,
			ClaudeSessionID: snap.ClaudeSessionID,
			Kind:            snap.Kind,
			Agent:           snap.Agent,
			CodexThreadID:   snap.CodexThreadID,
			MuseSessionID:   snap.MuseSessionID,
			Provider:        snap.Provider,
			Effort:          snap.Effort,
			Role:            snap.Role,
			ContextPack:     snap.ContextPack,
			TitleOverride:   snap.TitleOverride,
			Origin:          snap.Origin.clone(),
			Identity:        snap.Identity.clone(),
		}, req.WorkspaceSplit, req.WorkspaceTarget)
		if err != nil {
			a.rollbackSpawnedPane(p)
			http.Error(w, err.Error(), 500)
			return
		}
	}
	writeJSON(w, 201, makePaneDTO(p, a.aria, a.attention))
}

func (a *apiServer) deletePane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Snapshot BEFORE the kill so "reopen" has the live identity and cwd.
	closing, _ := a.mgr.Get(id)
	var closingOrigin *PaneOrigin
	if closing != nil {
		closingOrigin = closing.Snapshot().Origin
	}
	var closingEntry *closedPaneEntry
	if closing != nil && a.closed != nil {
		closing.refreshHarnessSession()
		closingEntry = &closedPaneEntry{Snap: closing.Snapshot(), LiveCwd: liveCwdOfPane(closing)}
	}
	if err := a.mgr.Kill(id); err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	recordPaneClose(id, closingOrigin, "app", false)
	if a.panes != nil {
		_ = a.panes.Remove(id)
	}
	if a.ws != nil {
		if err := a.ws.RemovePane(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	if closingEntry != nil {
		a.closed.add(*closingEntry)
	}
	w.WriteHeader(204)
}

type resizeReq struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (a *apiServer) resizePane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req resizeReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	// External HTTP resize is advisory only. If a sub has claimed
	// the pane, they own the dims — ignore external callers so a
	// stale client can't reflow the terminal out from under the
	// active owner.
	if p.HasOwner() {
		w.WriteHeader(204)
		return
	}
	_ = p.Resize(req.Cols, req.Rows)
	w.WriteHeader(204)
}

// redrawPane runs the daemon's existing Pane.Redraw() (full clear +
// scroll-up + size-bump dance) over HTTP. The same operation is
// already wired through the WebSocket {type:"redraw"} message; the
// HTTP variant exists so the browser can fire it without holding a
// live WS subscription — specifically, loadWorkspace fires it on
// every Claude/codex pane right after mount to nuke any back-buffer
// vs cell-grid divergence accumulated while the pane was streaming
// during the workspace-switch transition.
func (a *apiServer) redrawPane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	p.Redraw()
	w.WriteHeader(204)
}

type renameReq struct {
	Title string `json:"title"`
}

func (a *apiServer) renamePane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req renameReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	prev := p.Snapshot()
	p.Rename(req.Title)
	// Persist the new title so it survives daemon restart.
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.Rename(prev.Title)
			http.Error(w, "persist pane: "+err.Error(), 500)
			return
		}
	}
	writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
}

type autoModeReq struct {
	AutoMode string `json:"autoMode"`
}

func (a *apiServer) setPaneAutoMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req autoModeReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	switch req.AutoMode {
	case "", "always", "cautious":
	default:
		http.Error(w, "autoMode must be one of: '', 'always', 'cautious'", 400)
		return
	}
	// Turning auto-approve on lets the pane's prompts be answered without the
	// person; turning it off is open to anyone.
	if req.AutoMode != "" && !a.requireHumanOrDelegated(w, r, "turn on auto-approve") {
		return
	}
	prev := p.Snapshot()
	p.SetAutoMode(req.AutoMode)
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.SetAutoMode(prev.AutoMode)
			http.Error(w, "persist pane: "+err.Error(), 500)
			return
		}
	}
	if prev.AutoMode != req.AutoMode && a.aria != nil {
		a.aria.resetAutoModeEvaluation(p)
	}
	// Reclassify attention now — toggling auto mode on a pane that's sitting
	// on a prompt flips auto-pending ↔ needs-user, and the alert channels
	// should react immediately, not on the next 2s tick.
	if a.attention != nil {
		a.attention.Refresh(p)
	}
	writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
}

// announceDelegationReq carries the TRI-STATE: true/false are the user's
// explicit choice, null clears the override back to the role default.
type announceDelegationReq struct {
	Announce *bool `json:"announce"`
}

// setPaneAnnounceDelegation toggles the "[solu] delegated work" status line for
// one pane (delegation_nudge.go). Persisted like auto-mode, because a setting
// that silently reset on the next daemon restart would teach the user not to
// trust the toggle.
func (a *apiServer) setPaneAnnounceDelegation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req announceDelegationReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	prev := p.Snapshot()
	// Enabling on an unsupported pane is refused rather than quietly ignored:
	// the nudger would never inject there, and a checkbox that stays checked
	// while nothing happens is worse than an error (same doctrine as the Role
	// validation in pane_spawn.go).
	if req.Announce != nil && *req.Announce && !orchestratorHostSupported(prev.Agent) {
		http.Error(w, "announceDelegationEvents only applies to Claude or Codex panes", 400)
		return
	}
	p.SetAnnounceDelegationEvents(req.Announce)
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.SetAnnounceDelegationEvents(prev.AnnounceDelegationEvents)
			http.Error(w, "persist pane: "+err.Error(), 500)
			return
		}
	}
	writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
}

type flagReq struct {
	Flagged bool   `json:"flagged"`
	Memo    string `json:"memo"`
}

// setPaneFlag toggles the user's "come back to this later" marker on a pane,
// optionally attaching a one-line memo. Persisted alongside the rest of the
// pane metadata so the flag survives daemon restart — the user's whole
// reason for flagging is to find this thread again later.
func (a *apiServer) setPaneFlag(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req flagReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	if err := a.mgr.changeCurrentFlag(p, a.panes, nil, req.Flagged, req.Memo, "user", ""); err != nil {
		http.Error(w, "persist pane: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
}

func (a *apiServer) ackPane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	// Viewing a pane acknowledges its notification, not its prompt. These
	// detector-owned positions identify unresolved questions for attention,
	// auto-decline and Warden; focus must not mint a new prompt or re-arm auto.
	if pusher := a.getPusher(); pusher != nil {
		pusher.AckPane(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

type workspaceSummaryDTO struct {
	Name            string    `json:"name"`
	PaneIds         []string  `json:"paneIds"`
	ActiveAt        time.Time `json:"activeAt"`
	Protected       bool      `json:"protected,omitempty"`
	ContextPack     string    `json:"contextPack,omitempty"`
	PaneMoveVersion uint64    `json:"paneMoveVersion,omitempty"`
}

func (a *apiServer) listWorkspaceSummaries() []workspaceSummaryDTO {
	workspaces := a.ws.List()
	out := make([]workspaceSummaryDTO, 0, len(workspaces))
	for _, ws := range workspaces {
		out = append(out, workspaceSummaryDTO{
			Name:            ws.Name,
			PaneIds:         ws.PaneIds,
			ActiveAt:        ws.ActiveAt,
			Protected:       ws.Protected,
			ContextPack:     ws.ContextPack,
			PaneMoveVersion: ws.PaneMoveVersion,
		})
	}
	return out
}

func (a *apiServer) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("summary") == "1" {
		writeJSON(w, 200, a.listWorkspaceSummaries())
		return
	}
	writeJSON(w, 200, a.ws.List())
}

func (a *apiServer) putWorkspace(w http.ResponseWriter, r *http.Request) {
	var wp Workspace
	if !decodeJSON(w, r, &wp, defaultJSONBodyBytes) {
		return
	}
	if err := a.ws.Put(&wp); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errWorkspaceMoveConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, 200, &wp)
}

func (a *apiServer) moveWorkspacePane(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("name")
	var req struct {
		PaneID          string `json:"paneId"`
		Destination     string `json:"destination"`
		Direction       string `json:"direction,omitempty"`
		TargetPaneID    string `json:"targetPaneId,omitempty"`
		PaneMoveVersion uint64 `json:"paneMoveVersion,omitempty"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	pane, ok := a.mgr.Get(req.PaneID)
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	snap := pane.Snapshot()
	if snap.Role == paneRoleTaskOwner || snap.Status != PaneRunning {
		http.Error(w, "only running ordinary panes can be moved", http.StatusBadRequest)
		return
	}
	src, dst, err := a.ws.MovePane(source, req.Destination, req.PaneID, req.Direction, req.TargetPaneID, req.PaneMoveVersion)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errWorkspacePersistence) {
			status = http.StatusInternalServerError
		}
		if errors.Is(err, errWorkspaceMoveConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Source      *Workspace `json:"source"`
		Destination *Workspace `json:"destination"`
	}{Source: src, Destination: dst})
}

func (a *apiServer) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := a.ws.Delete(name); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

func (a *apiServer) renameWorkspace(w http.ResponseWriter, r *http.Request) {
	old := r.PathValue("name")
	var req struct {
		NewName string `json:"newName"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	newName := strings.TrimSpace(req.NewName)
	if newName == "" {
		http.Error(w, "new name cannot be empty", 400)
		return
	}
	if newName == "_current" {
		http.Error(w, "that name is reserved", 400)
		return
	}
	if err := a.ws.Rename(old, newName); err != nil {
		if errors.Is(err, errWorkspacePersistence) {
			http.Error(w, "Workspace could not be saved. Please retry.", http.StatusInternalServerError)
			return
		}
		http.Error(w, err.Error(), 400)
		return
	}
	w.WriteHeader(204)
}

func (a *apiServer) setWorkspaceContextPack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := a.ws.Get(name); !ok {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	var req struct {
		ContextPack string `json:"contextPack"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	abs, err := validContextPackDir(req.ContextPack)
	if err != nil {
		http.Error(w, "invalid context pack: "+err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := a.ws.SetContextPack(name, abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *apiServer) listPorts(w http.ResponseWriter, _ *http.Request) {
	if warning := a.ports.Warning(); warning != "" {
		w.Header().Set("X-Solu-Ports-Warning", warning)
	}
	ports := a.ports.List()
	for _, p := range ports {
		if p.PaneID != "" {
			if a.mgr != nil {
				if pane, ok := a.mgr.Get(p.PaneID); ok {
					p.PaneTitle = pane.Snapshot().Title
				}
			}
			if a.ws != nil {
				p.Workspace = a.ws.NameForPane(p.PaneID)
			}
		}
	}
	writeJSON(w, 200, ports)
}

func (a *apiServer) probePort(w http.ResponseWriter, r *http.Request) {
	if a.ports == nil {
		http.Error(w, "service discovery unavailable", http.StatusServiceUnavailable)
		return
	}
	var input struct {
		Port     uint16 `json:"port"`
		Selector string `json:"selector"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
		http.Error(w, "invalid listener", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	p, err := a.ports.Probe(ctx, input.Port, input.Selector)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			http.Error(w, "Service check timed out. Wait for startup and try again.", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *apiServer) listDirs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := 30
	// Pull recent cwds: first live panes (most up-to-date), then persisted.
	var recent []string
	seen := map[string]bool{}
	addIf := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		recent = append(recent, p)
	}
	for _, p := range a.mgr.List() {
		addIf(p.Cwd)
	}
	if a.panes != nil {
		for _, p := range a.panes.List() {
			addIf(p.Cwd)
		}
	}
	writeJSON(w, 200, ListDirs(recent, q, limit))
}

func (a *apiServer) paneLiveCwd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	cwd := liveCwdOfPane(p)
	if cwd == "" {
		cwd = p.Cwd // fall back to spawn-time cwd
	}
	writeJSON(w, 200, map[string]string{"cwd": cwd})
}

// Activity snapshot — returns the uiBus's ring-buffer contents so a
// freshly-opened Activity view has history to show even though SSE only
// delivers events it subscribed to live. Events are oldest-first;
// client sorts/filters for display.
func (a *apiServer) activity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"events": a.uiBus.Recent(),
	})
}
