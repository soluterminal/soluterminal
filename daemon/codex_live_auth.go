package main

// Live Codex authentication is deliberately separate from account selection.
// The existing selector owns auth.json; a per-pane local app-server receives
// its current access token. Refresh grants stay exclusively with the daemon.
import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

type codexLiveConfig struct {
	Enabled bool `json:"enabled"`
}

func codexLiveEnabled(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "codex_live_auth.json"))
	// New supported terminal launches use the handoff automatically. Retain an
	// explicit opt-out, and fail closed on unreadable/malformed settings.
	if errors.Is(err, os.ErrNotExist) {
		return codexLiveSupported()
	}
	var cfg codexLiveConfig
	return err == nil && json.Unmarshal(b, &cfg) == nil && cfg.Enabled
}

// Do not turn an ordinary login/API-key/older-CLI launch into a broken bridge.
// This preflight belongs to the daemon's spawn path, not the pane wrapper.
func codexLiveLaunchReady(cmd []string, env []string) bool {
	// Routed launches use the same control relay for capacity recovery, with
	// route overrides and no personal auth.json login/refresh path.
	if paneRouteActive(env) {
		return codexLiveSupported() && codexLiveTerminalArgs(cmd) && codexLiveCLISupported(cmd, env)
	}
	if !codexLiveSupported() || !codexLiveTerminalArgs(cmd) || !codexLiveDefaultHome(env) || os.Getenv("CODEX_HOME") != "" {
		return false
	}
	if strings.ContainsAny(cmd[0], `/\`) && !filepath.IsAbs(cmd[0]) {
		return false // relative executables resolve against the pane's cwd
	}
	for _, value := range env {
		if strings.HasPrefix(value, "PATH=") {
			return false // preserve a pane-specific CLI selection
		}
	}
	for _, key := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if os.Getenv(key) != "" {
			return false
		}
		for _, value := range env {
			if strings.HasPrefix(value, key+"=") && strings.TrimPrefix(value, key+"=") != "" {
				return false
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	codexDir := filepath.Join(home, ".codex")
	if codexRequireFileStorage(codexDir) != nil {
		return false
	}
	config, configErr := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return false
	}
	if !codexLiveAutomaticConfig(config) {
		return false
	}
	b, err := os.ReadFile(filepath.Join(codexDir, "auth.json"))
	access, _, account, ok := codexAuthTokens(b)
	if err != nil || !ok || access == "" || account == "" {
		return false
	}
	return codexLiveCLISupported(cmd, env)
}

func codexLiveCLISupported(cmd, env []string) bool {
	if len(cmd) == 0 || (strings.ContainsAny(cmd[0], `/\`) && !filepath.IsAbs(cmd[0])) {
		return false
	}
	for _, value := range env {
		if strings.HasPrefix(value, "PATH=") {
			return false
		}
	}
	bin, err := exec.LookPath(cmd[0])
	if err != nil && cmd[0] == "codex" {
		bin, err = resolveCodexBin()
	}
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	versionCmd := exec.CommandContext(ctx, bin, "--version")
	versionCmd.Env = providerChildEnvForBin(bin, os.Environ())
	out, err := versionCmd.Output()
	return err == nil && codexLiveVersionSupported(string(out))
}

// Conservative auto-opt-in, not a TOML interpreter. Custom provider/profile
// declarations and quoted/escaped assignment keys retain their ordinary CLI
// transport. The explicit bridge still supports its existing test/config seam.
func codexLiveAutomaticConfig(config []byte) bool {
	inUIPreferences := false
	quotedModelKey := regexp.MustCompile(`^(?:"(?:gpt-|codex-|o[1-9])[A-Za-z0-9._-]*"|'(?:gpt-|codex-|o[1-9])[A-Za-z0-9._-]*')$`)
	for _, line := range strings.Split(string(config), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.Contains(line, "model_providers") || strings.Contains(line, "profiles") {
				return false
			}
			inUIPreferences = false
			if header := tomlTableRe.FindStringSubmatch(line); header != nil {
				name := strings.TrimSpace(header[1])
				inUIPreferences = name == "tui" || strings.HasPrefix(name, "tui.") || name == "notice" || strings.HasPrefix(name, "notice.")
			}
			continue
		}
		key, _, assignment := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		// Codex writes quoted model-name keys to its own UI preference tables.
		// Only literal model-name keys receive the exception; auth/provider keys
		// remain rejected even if a multiline value confused section tracking.
		modelPreference := inUIPreferences && quotedModelKey.MatchString(key)
		if assignment && !modelPreference && (strings.HasPrefix(key, `"`) || strings.HasPrefix(key, "'") || key == "profile" || key == "model_provider" || strings.HasPrefix(key, "model_providers.")) {
			return false
		}
	}
	return true
}

func codexLiveVersionSupported(output string) bool {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) != 2 || fields[0] != "codex-cli" {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(fields[1], "v"), ".")
	if len(parts) != 3 {
		return false
	}
	major, e1 := strconv.Atoi(parts[0])
	minor, e2 := strconv.Atoi(parts[1])
	patch, e3 := strconv.Atoi(parts[2])
	return e1 == nil && e2 == nil && e3 == nil && major >= 0 && minor >= 0 && patch >= 0 && (major > 0 || minor >= 154)
}

type codexLiveStatus struct {
	Mode          string `json:"mode,omitempty"`
	PaneID        string `json:"paneId"`
	PID           int    `json:"pid"`
	ServerPID     int    `json:"serverPid"`
	ClientPID     int    `json:"clientPid"`
	State         string `json:"state"`
	AccountID     string `json:"accountId,omitempty"`
	Revision      string `json:"revision,omitempty"`
	UpdatedAt     int64  `json:"updatedAt"`
	Error         string `json:"error,omitempty"`
	Recovery      string `json:"recovery,omitempty"`
	RetryAt       int64  `json:"retryAt,omitempty"`
	RetryAttempts int    `json:"retryAttempts,omitempty"`
}

// Session evidence is separate from the immutable account selected at launch.
// Never expose token revisions, raw account IDs or process IDs to the badge.
type codexPaneAuthInfo struct {
	State        string `json:"state"`
	Account      string `json:"account,omitempty"`
	CanReconnect bool   `json:"canReconnect,omitempty"`
	Recovery     string `json:"recovery,omitempty"`
	RetryAt      int64  `json:"retryAt,omitempty"`
	// ReconnectMode is "shell" when reconnecting replaces a shell pane's prompt
	// with the resumed Codex conversation (shell variables and jobs are lost).
	ReconnectMode string `json:"reconnectMode,omitempty"`
	// Blocker names why a pane without a bridge cannot reconnect in place:
	// team, disabled or custom-launch.
	Blocker        string `json:"blocker,omitempty"`
	ReconnectArmed bool   `json:"reconnectArmed,omitempty"`
	ReconnectError string `json:"reconnectError,omitempty"`
}

func codexPaneAuthProjection(snap PaneSnapshot, st codexLiveStatus, revision, selected string, enabled bool, now time.Time) *codexPaneAuthInfo {
	if st.Mode == "broker" || (snap.Credential != nil && snap.Credential.Source == "broker") {
		return nil // Team transport follows its own scoped selection contract.
	}
	info := &codexPaneAuthInfo{State: "reconnect-required"}
	age := now.UnixMilli() - st.UpdatedAt
	if st.PaneID == snap.ID && age >= 0 && age < 15000 && st.UpdatedAt >= snap.CreatedAt.UnixMilli() && st.State != "stopped" {
		info.State = st.State
		info.Recovery, info.RetryAt = st.Recovery, st.RetryAt
		if info.State == "live" && (st.Revision == "" || st.Revision != revision) {
			info.State = "switching"
		}
		if paneRouteAccount(selected) {
			info.State = "reconnect-required"
		} else if info.State == "live" {
			info.Account = selected
		}
		return info
	}
	_, resumable := codexLiveResumeCommand(snap.Cmd, snap.CodexThreadID)
	info.CanReconnect = enabled && resumable && !paneRouteAccount(selected)
	switch {
	case info.CanReconnect:
	case paneRouteAccount(selected):
		info.Blocker = "team"
	case !enabled:
		info.Blocker = "disabled"
	default:
		info.Blocker = "custom-launch"
	}
	return info
}

func codexLiveStatusPath(dir, paneID string) string {
	return filepath.Join(dir, "codex-live", paneID+".json")
}

func readCodexLiveStatus(dir, paneID string) codexLiveStatus {
	var st codexLiveStatus
	if paneID == "" || filepath.Base(paneID) != paneID {
		return st
	}
	b, err := os.ReadFile(codexLiveStatusPath(dir, paneID))
	if err != nil || len(b) > 8192 || json.Unmarshal(b, &st) != nil || st.PaneID != paneID {
		return codexLiveStatus{}
	}
	return st
}

// No credentials cross this HTTP endpoint. The rejected revision makes refresh
// single-flight across panes and prevents a late 401 from rotating a new login.
func (a *apiServer) refreshCodexLiveAuth(w http.ResponseWriter, r *http.Request) {
	if a.codexLoginUnavailable(w) || a.codexMutationOverRelay(w, r) {
		return
	}
	var req struct {
		Revision string `json:"revision"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req) != nil || !codexLiveValidRevision(req.Revision) {
		http.Error(w, "invalid credential revision", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if codexLiveAuthRefresh == nil {
		http.Error(w, "Codex login could not be refreshed; check Credentials", http.StatusConflict)
		return
	}
	if err := codexLiveAuthRefresh(a, ctx, req.Revision); err != nil {
		// Provider errors can contain token material; only fixed local copy is
		// allowed on this boundary. The credential row carries reauth status.
		http.Error(w, "Codex login could not be refreshed; check Credentials", http.StatusConflict)
		return
	}
	a.emitCodexAccountsChanged()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func codexLiveValidRevision(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

func (a *apiServer) getCodexLiveAuth(w http.ResponseWriter, r *http.Request) {
	if a.codexLoginUnavailable(w) {
		return
	}
	var sessions []codexLiveStatus
	live, _ := os.ReadFile(a.codexLogin.livePath())
	revision := codexAuthFingerprint(live)
	for _, p := range a.mgr.List() {
		snap := p.Snapshot()
		if effectiveAgentKind(snap.Agent) != agentCodex || snap.Status != PaneRunning {
			continue
		}
		st := readCodexLiveStatus(a.mgr.dataDir, snap.ID)
		if st.PaneID == "" || time.Now().UnixMilli()-st.UpdatedAt > 15000 {
			st = codexLiveStatus{PaneID: snap.ID, State: "reconnect-required"}
		} else if st.State == "live" && st.Revision != revision {
			st.State = "switching"
		}
		st.Revision = "" // opaque credential hashes are not a UI identity
		sessions = append(sessions, st)
	}
	writeJSON(w, 200, map[string]any{"enabled": codexLiveEnabled(a.mgr.dataDir), "supported": codexLiveSupported(), "sessions": sessions})
}

func (a *apiServer) putCodexLiveAuth(w http.ResponseWriter, r *http.Request) {
	if a.codexLoginUnavailable(w) || a.codexMutationOverRelay(w, r) {
		return
	}
	var cfg codexLiveConfig
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&cfg) != nil {
		http.Error(w, "invalid settings", 400)
		return
	}
	if cfg.Enabled && !codexLiveSupported() {
		http.Error(w, "live Codex terminal authentication is currently supported on Linux", 400)
		return
	}
	if cfg.Enabled {
		if err := a.codexLogin.requireCodexFileStorage(); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	}
	b, _ := json.Marshal(cfg)
	if err := writeFileAtomicSynced(filepath.Join(a.mgr.dataDir, "codex_live_auth.json"), b, 0600); err != nil {
		http.Error(w, "could not save live auth settings", 500)
		return
	}
	a.getCodexLiveAuth(w, r)
}

// Pure launch selection: explicit remotes/custom-provider/profile invocations
// remain under the user's own transport/auth control. Never rewrite headless jobs.
func codexLiveTerminalArgs(cmd []string) bool {
	if !cmdStartsCodex(cmd) {
		return false
	}
	model, headless := codexUnixModelInvocation(cmd[1:])
	if !model || headless || codexShellProviderOverride(cmd[1:]) {
		return false
	}
	for i := 1; i < len(cmd); i++ {
		arg := cmd[i]
		if arg == "--" {
			break
		}
		if arg == "-p" || strings.HasPrefix(arg, "-p=") || (strings.HasPrefix(arg, "-p") && !strings.HasPrefix(arg, "--")) || arg == "--profile" || strings.HasPrefix(arg, "--profile=") {
			return false
		}
		if codexShellOptionTakesValue(arg) {
			i++
		}
	}
	return true
}

// Accept only a literal Codex command or our exact generated login wrapper.
// Preserve explicit launch policy; never replay a positional initial prompt,
// evaluate arbitrary shell code, or silently drop a custom profile on migration.
func codexLiveResumeCommand(cmd []string, thread string) ([]string, bool) {
	if !cmdStartsCodex(cmd) {
		if len(cmd) != 3 || filepath.Base(cmd[0]) != "bash" || cmd[1] != "-lc" {
			return nil, false
		}
		file, err := syntax.NewParser().Parse(strings.NewReader(cmd[2]), "")
		if err != nil {
			return nil, false
		}
		var found []string
		count := 0
		syntax.Walk(file, func(node syntax.Node) bool {
			call, ok := node.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 || len(call.Assigns) != 0 {
				return true
			}
			name, _ := literalWord(call.Args[0])
			if name != "codex" {
				return true
			}
			count++
			var args []string
			for _, word := range call.Args {
				value, ok := literalWord(word)
				if !ok {
					return false
				}
				args = append(args, value)
			}
			found = args
			return false
		})
		wrapped, ok := tryLoginShellWrap(found, "", nil)
		if count != 1 || !ok || !slices.Equal(wrapped, cmd) {
			return nil, false
		}
		cmd = found
	}
	_, validThread := canonicalCodexThreadID(thread)
	if !codexLiveTerminalArgs(cmd) || !validThread {
		return nil, false
	}
	args := cmd[1:]
	if len(args) > 0 && (args[0] == "resume" || args[0] == "fork") {
		if len(args) < 2 {
			return nil, false
		}
		if _, ok := canonicalCodexThreadID(args[1]); !ok {
			return nil, false
		}
		args = args[2:]
	}
	for i := 0; i < len(args); i++ {
		key, _, attached := strings.Cut(args[i], "=")
		switch key {
		case "-c", "--config", "-m", "--model", "-s", "--sandbox", "-C", "--cd", "-a", "--ask-for-approval", "--enable", "--disable", "--add-dir":
			if !attached {
				i++
				if i >= len(args) {
					return nil, false
				}
			}
		case "--full-auto", "--dangerously-bypass-approvals-and-sandbox", "--yolo", "--search", "--no-alt-screen", "--no-daemon":
			if attached {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return append([]string{cmd[0], "resume", thread}, args...), true
}

func codexLiveDefaultHome(env []string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, "CODEX_HOME=") {
			return false
		}
	}
	return true
}

// Migration is a one-time explicit action, separate from routine credential updates.
// Only the currently rendered, empty/placeholder composer is eligible. A draft,
// unknown chrome, approval or background terminal waits for the user to settle it.
func codexLiveMigrationScreenSafe(screen string) bool {
	if !looksLikeCodexPrompt(screen) || codexActiveTurnRE.MatchString(screen) {
		return false
	}
	background := regexp.MustCompile(`(?i)\b[1-9][0-9]* background terminals? running\b`)
	if background.MatchString(screen) {
		return false
	}
	lines := strings.Split(screen, "\n")
	composer := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "›") {
			composer = i
		}
	}
	if composer < 0 {
		return false
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[composer]), "›"))
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r >= 0x2800 && r <= 0x28ff {
			return -1
		}
		return r
	}, text))
	if text != "" && text != "Ask Codex to do anything" {
		return false
	}
	for i, line := range lines[composer+1:] {
		if codexFooterLineRE.MatchString(line) {
			// A shell prompt below an old Codex footer is not a live TUI.
			return strings.TrimSpace(strings.Join(lines[composer+i+2:], "\n")) == ""
		}
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return false
}

// A body of {"when":"idle"} queues the reconnect when the pane is busy instead
// of refusing; {"cancel":true} withdraws a queued one. No body keeps the
// original reconnect-now contract for older clients.
func (a *apiServer) migrateCodexLiveAuth(w http.ResponseWriter, r *http.Request) {
	if a.codexLoginUnavailable(w) || a.codexMutationOverRelay(w, r) {
		return
	}
	var req struct {
		When   string `json:"when"`
		Cancel bool   `json:"cancel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid reconnect request", 400)
		return
	}
	if req.When != "" && req.When != "now" && req.When != "idle" {
		http.Error(w, "when must be now or idle", 400)
		return
	}
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	if req.Cancel {
		a.codexReconnects.disarm(p.ID)
		writeJSON(w, 200, map[string]any{"paneId": p.ID, "armed": false})
		return
	}
	snap := p.Snapshot()
	thread := snap.CodexThreadID
	// A conversation that can never be resumed is reported on the click, not
	// after a busy pane finally goes idle. Checked once here: the eligibility
	// walk is too costly for every poll of a queued reconnect, which re-checks
	// it under the latch only once the pane is idle.
	if effectiveAgentKind(snap.Agent) == agentCodex && snap.Kind == "" {
		if blocker := fleetRespawnEligibility(p).Blocker; blocker != "" {
			if refusal := codexReconnectBlocked(blocker); !refusal.retryable {
				http.Error(w, refusal.message, refusal.status)
				return
			}
		}
	}
	result, refusal := a.tryCodexLiveReconnect(p, codexReconnectQuietNow, thread)
	if refusal == nil {
		a.codexReconnects.disarm(p.ID)
		writeJSON(w, 200, result)
		return
	}
	if req.When == "idle" && refusal.retryable {
		a.armCodexReconnect(p.ID, thread)
		writeJSON(w, http.StatusAccepted, map[string]any{"paneId": p.ID, "armed": true, "waiting": refusal.message})
		return
	}
	http.Error(w, refusal.message, refusal.status)
}

type codexReconnectRefusal struct {
	status  int
	message string
	// retryable refusals clear on their own (busy turn, draft, approval,
	// another respawn); a queued reconnect keeps waiting through them.
	retryable bool
	// settling marks an unverified conversation identity, which normally
	// clears within seconds; a queued reconnect gives up if it persists.
	settling bool
}

func codexReconnectWait(message string) *codexReconnectRefusal {
	return &codexReconnectRefusal{status: 409, message: message, retryable: true}
}

func codexReconnectRefuse(status int, message string) *codexReconnectRefusal {
	return &codexReconnectRefusal{status: status, message: message}
}

// codexReconnectBlocked classifies a respawn blocker. Identity still being
// established, or a fresh conversation whose rollout is not flushed yet,
// settles on its own (bounded by codexReconnectSettleAttempts); a missing
// conversation never will.
func codexReconnectBlocked(blocker string) *codexReconnectRefusal {
	refusal := codexReconnectRefuse(409, "cannot reconnect: "+blocker)
	for _, settling := range []string{"not currently verified", "not yet recorded", "not durably resumable"} {
		if strings.Contains(blocker, settling) {
			refusal.retryable, refusal.settling = true, true
		}
	}
	return refusal
}

// codexReconnectLooksIdle judges idleness without the respawn latch or the
// input-rejecting migration flag, so a busy pane with a queued reconnect keeps
// accepting input. The attempt re-checks all of it under the latch.
func codexReconnectLooksIdle(p *Pane, quiet time.Duration) bool {
	if fleetPaneBusyForRestart(p) || time.Now().UnixMilli()-atomic.LoadInt64(&p.LastInputMs) < quiet.Milliseconds() {
		return false
	}
	screen, ok := p.screenVisibleText()
	return ok && codexLiveMigrationScreenSafe(screen)
}

// tryCodexLiveReconnect makes one guarded attempt to relaunch the pane's Codex
// conversation inside the live-auth bridge. quiet is how long the pane must
// have gone without user input; a non-empty thread pins the attempt to the
// conversation the user asked to reconnect.
func (a *apiServer) tryCodexLiveReconnect(p *Pane, quiet time.Duration, thread string) (map[string]any, *codexReconnectRefusal) {
	if !codexLiveEnabled(a.mgr.dataDir) || !codexLiveSupported() {
		return nil, codexReconnectRefuse(409, "live authentication is not enabled")
	}
	if selfRestartInProgress.Load() {
		return nil, codexReconnectWait("a daemon handoff is already in progress")
	}
	initial := p.Snapshot()
	if effectiveAgentKind(initial.Agent) != agentCodex || initial.Kind != "" {
		return nil, codexReconnectRefuse(400, "a Codex terminal pane is required")
	}
	st := readCodexLiveStatus(a.mgr.dataDir, initial.ID)
	if st.State == "live" && time.Now().UnixMilli()-st.UpdatedAt < 15000 {
		return map[string]any{"paneId": initial.ID, "alreadyConnected": true}, nil
	}
	if codexLivePaneHasBridge(p) {
		return nil, codexReconnectRefuse(409, "this pane already has a live-auth bridge; inspect its connection status")
	}
	if thread != "" && initial.CodexThreadID != thread {
		return nil, codexReconnectRefuse(409, "the Codex conversation changed since the reconnect was requested")
	}
	if !codexReconnectLooksIdle(p, quiet) {
		return nil, codexReconnectWait("waiting for an idle Codex prompt with no draft, approval or background terminal")
	}
	if !p.tryAcquireRespawn() {
		return nil, codexReconnectWait("pane reconnect already in progress")
	}
	defer p.releaseRespawn()
	p.codexLiveMigrating.Store(true)
	defer p.codexLiveMigrating.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lockPaneWriter(ctx, &p.agentWriteMu); err != nil {
		return nil, codexReconnectWait("pane input is still active")
	}
	p.agentWriteMu.Unlock()
	eligibility := fleetRespawnEligibility(p)
	if eligibility.Blocker != "" {
		return nil, codexReconnectBlocked(eligibility.Blocker)
	}
	screen, screenOK := p.screenVisibleText()
	if eligibility.Busy || !screenOK || !codexLiveMigrationScreenSafe(screen) || time.Now().UnixMilli()-atomic.LoadInt64(&p.LastInputMs) < quiet.Milliseconds() {
		return nil, codexReconnectWait("waiting for an idle Codex prompt with no draft, approval or background terminal")
	}
	if eligibility.Snapshot.CodexThreadID != initial.CodexThreadID || !p.HasCodexDescendant() {
		// Codex exiting or restarting looks like this; it must not keep a
		// queued reconnect polling for hours after the user left Codex.
		return nil, &codexReconnectRefusal{status: 409, message: "Codex is no longer running the requested conversation", retryable: true, settling: true}
	}
	// Validate that the host login is usable before stopping the old UI. Tokens
	// themselves never enter a pane command, HTTP result, or lifecycle event.
	live, err := os.ReadFile(a.codexLogin.livePath())
	_, _, id, valid := codexAuthTokens(live)
	if err != nil || !valid || id == "" {
		return nil, codexReconnectRefuse(409, "a working ChatGPT login is required; sign in through Credentials")
	}
	snap := eligibility.Snapshot
	if current, ok := a.mgr.Get(snap.ID); !ok || current != p || p.Snapshot().Status != PaneRunning {
		return nil, codexReconnectRefuse(409, "pane changed before reconnect")
	}
	opts := resumeSpawnOpts(snap, snap.Cols, snap.Rows)
	resumeCmd, cwd, _, safe := codexReconnectInvocation(p, snap)
	if !safe {
		return nil, codexReconnectRefuse(409, "custom Codex launch requires a manual reconnect to preserve its settings")
	}
	opts.Cwd = cwd
	if info, err := os.Stat(opts.Cwd); err != nil || !info.IsDir() {
		return nil, codexReconnectRefuse(409, "the Codex working directory no longer exists; current session was left untouched")
	}
	if !codexLiveLaunchReady(resumeCmd, opts.Env) {
		return nil, codexReconnectRefuse(409, "Codex configuration or CLI does not support live authentication; current session was left untouched")
	}
	opts.Cmd = resumeCmd
	opts.RequireResumableConversation = true
	next, err := a.respawnInPlace(snap, opts)
	if err != nil {
		return nil, codexReconnectRefuse(500, "could not reconnect Codex; saved conversation is retained")
	}
	return map[string]any{"paneId": next.ID, "threadId": snap.CodexThreadID, "state": "connecting"}, nil
}

func (a *apiServer) codexMutationOverRelay(w http.ResponseWriter, r *http.Request) bool {
	if fileAccessOverRelay(r) {
		http.Error(w, "credential changes are local-only", http.StatusForbidden)
		return true
	}
	return false
}

func (a *apiServer) emitCodexAccountsChanged() {
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "codex-accounts-changed", Ts: time.Now().UnixMilli()})
	}
}

// codexLoginUnavailable answers 503 when this daemon has no Codex login to
// report on (CODEX_HOME unresolvable).
func (a *apiServer) codexLoginUnavailable(w http.ResponseWriter) bool {
	if a.codexLogin == nil {
		http.Error(w, "codex credentials unavailable", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// codexLiveAuthRefresh refreshes the live Codex login a bridged terminal
// rejected (revision is that login's fingerprint). Only a credential manager
// that owns the login can; without one the user signs
// in again with the Codex CLI.
var codexLiveAuthRefresh func(a *apiServer, ctx context.Context, revision string) error
