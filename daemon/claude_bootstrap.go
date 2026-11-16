package main

// Claude Code CLI bootstrap: detect whether the `claude` binary exists in the
// environment panes actually run in, and whether it's authenticated, then
// surface that on GET /api/health so the web/native UI can guide install + auth
// instead of letting a Claude pane dead-end with "command not found".
//
// DESIGN (hardened after multi-agent review):
//   - Detection runs in a LOGIN SHELL, not exec.LookPath. The daemon inherits
//     the launcher's slim PATH (systemd/Tauri), so a bare LookPath misses
//     nvm-managed claude AND the native installer's ~/.local/bin. We reuse the
//     SAME init as pane spawning (loginShellInit) so detection and execution
//     agree by construction.
//   - The probe is CACHED. /api/health is a 10s heartbeat on a multiplexed
//     pipe; forking a shell per request would add latency to the reachability
//     signal. We probe at boot, on a slow ticker, after a guided install, and
//     when the setup checklist opens (a forced recheck) — never inside health().
//   - A probe FAILURE is not a probe ANSWER. Detection returns a three-way
//     claudeProbeOutcome; only found/absent (a shell that ran to completion)
//     may overwrite the cache. A timeout/exec error keeps the prior verdict —
//     a loaded post-auto-update boot must never flip a working install to
//     "not installed" and banner the user to install what they're running.
//   - Only DURABLE, UNCONTRADICTED absence may flip a confirmed install. Even a
//     correctly-observed absent can be transient: a Claude self-update removes
//     and re-creates the binary on disk, so for a window of seconds `command -v`
//     AND the disk cross-check both legitimately miss. An absent verdict is
//     therefore (1) contradicted by a binary on disk or a live claude process
//     (corroborateClaudeAbsent), and (2) damped by hysteresis — it must persist
//     across claudeAbsentConfirmProbes consecutive probes spanning
//     claudeAbsentConfirmWindow before installed:true flips (confirmClaudeAbsent).
//   - "Authenticated" is the presence of a valid LIVE credential file, the only
//     cheap, sound signal (accounts.length / active-name are ambiguous: a fresh
//     login is an unsaved live file; a stale pointer can outlive a deleted
//     file). macOS stores creds in the Keychain, not a file, so authed reads
//     false there — macOS auth is out of scope (we ship the Linux/WSL daemon).

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// claudeStatus is the cached result of a Claude CLI probe. Unexported fields;
// the health handler reads it via apiServer.currentClaudeStatus().
type claudeStatus struct {
	checked   bool   // false until the first DEFINITIVE probe completes (UI shows "checking…")
	installed bool   // a runnable `claude` resolved in the login-shell env
	version   string // first line of `claude --version`, "" if unknown
	authed    bool   // signed in: a valid live credential file, or an API key (resolveClaudeAuthed)
	// planLogin: signed in with the Claude login itself, not an API key. Only
	// this lets a background run (dictation cleanup) bill the user's plan.
	planLogin bool
}

// claudeProbeOutcome separates a probe's ANSWER from its FAILURE. A probe that
// times out or whose shell dies has produced no information — treating it as
// "not installed" is how a machine running 45 working Claude panes ended up
// with an "install Claude Code" banner after every loaded auto-update restart
// (the boot probe raced 45 respawning panes and lost). Only a definitive
// outcome may overwrite the cached claudeStatus.
type claudeProbeOutcome int

const (
	claudeProbeFound  claudeProbeOutcome = iota // `command -v claude` resolved (version best-effort)
	claudeProbeAbsent                           // the probe shell COMPLETED and said "no claude"
	claudeProbeError                            // timeout / exec failure — no verdict, keep prior state
)

// Steady-state re-probe cadence (main.go's self-heal loop). The interval is
// chosen by the cached verdict: brisk while we believe claude is missing (so a
// residual false-absent or a fresh manual install heals in ~1 min, not up to 5),
// brisker still while an absence is SUSPECTED but not yet confirmed (the
// hysteresis below needs samples to converge either way), relaxed once
// installed is confirmed (only a later uninstall needs catching).
const (
	claudeProbeIntervalMissing = 60 * time.Second
	claudeProbeIntervalSuspect = 30 * time.Second
	claudeProbeIntervalPresent = 5 * time.Minute
)

// Absence-confirmation hysteresis. A Claude Code SELF-UPDATE (autoUpdates:true,
// npm-global path) removes and re-creates the binary/symlink on disk: for a
// window of seconds `command -v claude` misses AND the on-disk cross-check
// misses (dangling symlink) — a *correctly observed*, definitive-looking absence
// that is not an uninstall. On a many-pane machine the updater runs often, so a
// ticker probe eventually lands inside such a window (observed live 2026-07-08:
// banner during the 2.1.201→2.1.202 npm reinstall). No single sample can tell
// that apart from a real uninstall; DURABILITY can — an uninstall stays absent,
// an update window heals in seconds. So a confirmed install flips to "not
// installed" only after this many consecutive definitive absents spanning at
// least this window (any found-probe resets both). A real uninstall is thus
// reported ~2 min late — acceptable for an advisory banner; the false alarm it
// prevents told the user to install what they were actively running.
const (
	claudeAbsentConfirmProbes = 3
	claudeAbsentConfirmWindow = 2 * time.Minute
)

// loginShellInit is the prelude sourced before resolving a bare command in a
// login shell, so nvm/asdf/pyenv-managed tools AND the native Claude installer's
// ~/.local/bin resolve the way the user's interactive shell would. SHARED by
// pane spawning (tryLoginShellWrap) and Claude detection so the two never
// disagree — a binary detection reports "installed" must also be spawnable.
//
// The native installer (claude.ai/install.sh) drops claude in ~/.local/bin but
// does NOT reliably persist that dir onto the login-shell PATH (Claude issue
// #21069 — current session only), so we prepend it explicitly here.
func loginShellInit() string {
	return strings.Join([]string{
		`export PATH="$HOME/.local/bin:$HOME/.volta/bin:$PATH"`,
		`[ -s "$HOME/.nvm/nvm.sh" ] && . "$HOME/.nvm/nvm.sh"`,
		`[ -s "$HOME/.asdf/asdf.sh" ] && . "$HOME/.asdf/asdf.sh"`,
		`command -v pyenv >/dev/null 2>&1 && eval "$(pyenv init -)"`,
		`true`, // anchor exit 0 so a missing manager doesn't sink the chain
	}, "; ")
}

// refreshClaudeStatus probes for the Claude CLI + auth and stores the result.
// Safe to call from any goroutine; serialized by claudeStatMu. Call at boot,
// after a guided install, on a slow ticker, and on explicit re-check.
func (a *apiServer) refreshClaudeStatus() {
	// Coalesce concurrent probes onto one login shell: boot + the 5-min ticker +
	// on-demand rechecks can overlap, and the recheck endpoint is reachable from
	// a local page, so a loop must not spawn N concurrent 8s shells.
	if !a.claudeProbeMu.TryLock() {
		// A probe is already running. WAIT for it, then return — so an on-open
		// recheck reflects the in-flight BOOT probe's result instead of a
		// premature checked:false (which would render a false "Not installed").
		// The TryLock fast-path still prevents N concurrent shells: only one
		// goroutine probes; the rest block here and then read the fresh
		// claudeStat via currentClaudeStatus().
		a.claudeProbeMu.Lock()
		a.claudeProbeMu.Unlock()
		return
	}
	defer a.claudeProbeMu.Unlock()
	prev := a.currentClaudeStatus()
	detect := a.detectClaude
	if detect == nil {
		detect = detectClaudeCLI
	}
	outcome, version, hasAPIKey := detect()

	// Only a DEFINITIVE outcome may change the cached verdict. A probe error
	// (timeout under load, shell killed) carries no information — overwriting
	// a confirmed install with "not installed" on one is how the spurious
	// "install Claude Code" banner appeared after loaded auto-update restarts,
	// and it stuck until the next probe happened to win the load race.
	next := prev
	switch outcome {
	case claudeProbeFound:
		a.resetClaudeAbsentHysteresis()
		next.checked, next.installed = true, true
		// Presence is proven by `command -v`; the version is best-effort (a
		// killed `claude --version` leaves it empty) — keep the last known one
		// rather than blanking the UI.
		if version != "" {
			next.version = version
		}
		if prev.checked && !prev.installed {
			slog.Info("claude-detect: claude CLI detected", "version", next.version)
		}
	case claudeProbeAbsent:
		if !a.confirmClaudeAbsent(prev) {
			// Absence observed but not yet DURABLE. A Claude self-update window
			// looks identical to an uninstall on any single sample; keep the
			// prior verdict and let the brisk suspect-cadence ticker converge —
			// a real uninstall confirms within ~2 min, an update window resets
			// on the next found-probe.
			return
		}
		next.checked, next.installed, next.version = true, false, ""
	case claudeProbeError:
		// No verdict: keep prev wholesale (authed included — a failed probe
		// can't see the rc-file ANTHROPIC_API_KEY, so recomputing here would
		// flap an API-key user's authed state).
		return
	}
	credPresent := a.claudeLogin != nil && a.claudeLogin.LiveCredentialPresent()
	next.authed = resolveClaudeAuthed(next.installed, credPresent, hasAPIKey, runtime.GOOS)
	next.planLogin = resolveClaudeAuthed(next.installed, credPresent, false, runtime.GOOS)
	a.claudeStatMu.Lock()
	a.claudeStat = next
	a.claudeStatMu.Unlock()

	// A claude that appeared (or got signed in) AFTER boot never had our MCP
	// server / hooks registered — those run once at startup, and MCP no-ops
	// while ~/.claude.json is absent (true for a fresh pre-login install). Re-run
	// them whenever a post-boot probe sees claude installed: the ensure* funcs
	// are idempotent (no-op once present), so this SELF-HEALS the pre-login case
	// on a later probe without one-shot edge bugs or file churn. Guard on
	// prev.checked so the boot probe doesn't double-register (main does that at
	// startup). registerClaudeIntegrations serializes against tmp-file races.
	if next.installed && prev.checked {
		reg := a.registerIntegrationsFn
		if reg == nil {
			reg = a.registerClaudeIntegrations
		}
		go reg()
	}
}

// confirmClaudeAbsent is the hysteresis gate for a definitive absent probe: it
// reports whether the absence is DURABLE enough to overwrite a confirmed
// install. A first-ever verdict (fresh machine — the banner should guide a new
// user promptly) or an already-absent state applies immediately; only the
// installed:true → absent transition is damped, because that is the transition
// a Claude self-update window forges (see claudeAbsentConfirmProbes). Counts
// consecutive definitive absents (a probe ERROR neither advances nor resets;
// only a found-probe resets, via resetClaudeAbsentHysteresis).
func (a *apiServer) confirmClaudeAbsent(prev claudeStatus) bool {
	if !prev.checked || !prev.installed {
		return true
	}
	now := time.Now
	if a.nowFn != nil {
		now = a.nowFn
	}
	a.claudeStatMu.Lock()
	defer a.claudeStatMu.Unlock()
	if a.claudeAbsentStreak == 0 {
		a.claudeFirstAbsentAt = now()
	}
	a.claudeAbsentStreak++
	elapsed := now().Sub(a.claudeFirstAbsentAt)
	if a.claudeAbsentStreak < claudeAbsentConfirmProbes || elapsed < claudeAbsentConfirmWindow {
		slog.Info("claude-detect: absent probe on a confirmed install — keeping verdict until absence is durable",
			"streak", a.claudeAbsentStreak, "needProbes", claudeAbsentConfirmProbes,
			"elapsed", elapsed.Round(time.Second), "needWindow", claudeAbsentConfirmWindow)
		return false
	}
	slog.Warn("claude-detect: absence confirmed as durable — flipping installed→false",
		"streak", a.claudeAbsentStreak, "elapsed", elapsed.Round(time.Second))
	a.claudeAbsentStreak, a.claudeFirstAbsentAt = 0, time.Time{}
	return true
}

// resetClaudeAbsentHysteresis ends a suspected-absent episode on a found probe.
func (a *apiServer) resetClaudeAbsentHysteresis() {
	a.claudeStatMu.Lock()
	defer a.claudeStatMu.Unlock()
	if a.claudeAbsentStreak > 0 {
		slog.Info("claude-detect: found probe ended a suspected-absent episode (transient absence, likely a self-update window)",
			"streak", a.claudeAbsentStreak)
		a.claudeAbsentStreak, a.claudeFirstAbsentAt = 0, time.Time{}
	}
}

// claudeProbeInterval picks the self-heal ticker cadence from the cached
// verdict: brisk while absent (heal a wrong verdict / catch a fresh install
// fast), brisker while an absence is suspected-but-unconfirmed (the hysteresis
// needs samples to converge either way), relaxed once presence is confirmed.
func (a *apiServer) claudeProbeInterval() time.Duration {
	a.claudeStatMu.Lock()
	installed := a.claudeStat.installed
	suspect := a.claudeAbsentStreak > 0
	a.claudeStatMu.Unlock()
	switch {
	case !installed:
		return claudeProbeIntervalMissing
	case suspect:
		return claudeProbeIntervalSuspect
	default:
		return claudeProbeIntervalPresent
	}
}

// registerClaudeIntegrations (re-)registers the soluterminal MCP server + hooks
// into the user's Claude config after a post-boot install/login. Serialized by
// claudeRegMu so overlapping probes never race the config tmp files; both
// ensure* funcs are idempotent and MCP no-ops until ~/.claude.json exists.
func (a *apiServer) registerClaudeIntegrations() {
	a.claudeRegMu.Lock()
	defer a.claudeRegMu.Unlock()
	ensureClaudeMcpRegistration(a.addr)
	ensureClaudeHooksRegistered(a.addr)
}

// resolveClaudeAuthed decides the "signed in" signal the UI's Log-in nudge
// rides on. Authed when a valid live OAuth credential file exists, and ALSO
// when ANTHROPIC_API_KEY is present in the pane environment: an API-key user
// authenticates without ever writing a credential file, so without this they'd
// be nudged to "Log in" forever — and clicking it may not even present a login
// (claude just runs on the key). macOS keeps Claude creds in the Keychain, not
// a file, so the file-based check can't see them and would report "not signed
// in" for a signed-in macOS user — which a "Log in" nudge would act on falsely.
// We can't read the Keychain here, so be conservative: an installed claude on
// darwin is treated as authed. Worst case a genuinely-signed-out macOS user
// isn't nudged (unchanged from before); we never false-nag.
func resolveClaudeAuthed(installed, credPresent, hasAPIKey bool, goos string) bool {
	if !installed {
		return false
	}
	if credPresent || hasAPIKey {
		return true
	}
	return goos == "darwin"
}

// currentClaudeStatus returns the last cached probe result (zero value, with
// checked=false, before the first probe completes).
func (a *apiServer) currentClaudeStatus() claudeStatus {
	a.claudeStatMu.Lock()
	defer a.claudeStatMu.Unlock()
	return a.claudeStat
}

// detectClaudeCLI reports whether a runnable `claude` exists (as a three-way
// outcome — see claudeProbeOutcome), its version string, and whether
// Claude runs on an API key: ANTHROPIC_API_KEY in the environment panes run in,
// or a key Claude stored itself (see claudeStoredAPIKeyConfigured). API-key auth
// never writes a credential file — see resolveClaudeAuthed.
// Unix: a login shell with the shared init (matches pane spawn). Windows: the
// live registry PATH (matches pane spawn — findOnWindowsPath) plus the native
// installer's %USERPROFILE%\.local\bin.
func detectClaudeCLI() (outcome claudeProbeOutcome, version string, hasAPIKey bool) {
	outcome, version, hasAPIKey = probeClaudeCLI()
	if !hasAPIKey && outcome == claudeProbeFound {
		hasAPIKey = claudeStoredAPIKeyConfigured()
	}
	return outcome, version, hasAPIKey
}

// probeClaudeCLI is detectClaudeCLI's CLI and environment probe, before the
// stored-key check.
func probeClaudeCLI() (outcome claudeProbeOutcome, version string, hasAPIKey bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if runtime.GOOS == "windows" {
		outcome, version = detectClaudeWindows(ctx)
		// Panes inherit the daemon's env (mergeRegistryPath rebuilds PATH only),
		// so the daemon's own env — not the registry — is exactly what a pane's
		// claude will see. A registry-only key that panes can't inherit must NOT
		// clear the sign-in nudge (false "authed" beats a dismissible nag).
		return outcome, version, os.Getenv("ANTHROPIC_API_KEY") != ""
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		// No bash → fall back to a bare lookup; better than a false negative
		// on an exotic *nix without bash (rare for our targets). LookPath is a
		// pure filesystem walk — load-immune — so its miss counts as absent.
		hasAPIKey = os.Getenv("ANTHROPIC_API_KEY") != ""
		if p, e := exec.LookPath("claude"); e == nil {
			v := ""
			if out, e2 := exec.CommandContext(ctx, p, "--version").Output(); e2 == nil {
				v = firstLine(strings.TrimSpace(string(out)))
			}
			return claudeProbeFound, v, hasAPIKey
		}
		// Same corroboration as the login-shell path: a slim-PATH LookPath miss is
		// not proof of absence when claude lives under an nvm/asdf prefix.
		return corroborateClaudeAbsent(claudeProbeAbsent, claudeBinaryOnDisk, claudeProcessAlive), "", hasAPIKey
	}
	// The probe must make ABSENCE positively observable, not inferred from a
	// failure: `command -v` prints the absent marker in its else-branch, so
	// "no claude" is a marker in stdout from a shell that ran to completion.
	// No marker at all means the shell never finished (timeout under load,
	// OOM-kill) — an ERROR, not an answer. A login shell sources ~/.profile
	// etc., which may echo a banner to stdout — so claude's own output is
	// FENCED with a marker and only what follows it is read, preventing a
	// banner from masquerading as the version. The API-key probe emits its own
	// marker BEFORE the version fence (a login shell is where rc-file
	// `export ANTHROPIC_API_KEY=…` becomes visible — the daemon's own env is a
	// slim launch-time snapshot that may miss it).
	script := loginShellInit() +
		`; [ -n "$ANTHROPIC_API_KEY" ] && printf '%s' '` + claudeAPIKeyMarker + `'` +
		"; if command -v claude >/dev/null 2>&1; then printf '%s' '" + claudeVersionMarker + "'; claude --version; else printf '%s' '" + claudeAbsentMarker + "'; fi"
	cmd := exec.CommandContext(ctx, bash, "-lc", script)
	cmd.WaitDelay = 2 * time.Second // close pipes / kill if it lingers past ctx cancel
	out, _ := cmd.Output()
	// Markers are parsed regardless of the exit code: Output() still returns
	// captured stdout on an ExitError (e.g. a found claude whose --version
	// exits non-zero — presence is already proven by its marker), and a
	// timeout may kill the shell after a marker was printed. Union the key
	// with the daemon's own env for setups that inject it at launch (systemd
	// Environment=) without an rc-file export.
	hasAPIKey = strings.Contains(string(out), claudeAPIKeyMarker) || os.Getenv("ANTHROPIC_API_KEY") != ""
	outcome, version = classifyClaudeProbeOutput(string(out))
	// A login shell that RAN TO COMPLETION and reported "absent" is still not
	// trusted blind. Sourcing nvm/asdf only prepends the managed node's bin to
	// PATH if a chain of FORKED command substitutions (nvm_auto → resolve default
	// → nvm use) all succeed; under fork pressure — a 48-pane auto-update
	// successor boot exhausting the pids cgroup / memory — any of those forks can
	// fail SILENTLY (nvm redirects them to /dev/null), leaving node off PATH. The
	// shell then exits 0 having printed the absent marker while `claude` sits on
	// disk the whole time: a definitive-looking FALSE absent, indistinguishable
	// from a real uninstall by the marker alone. Cross-check the filesystem
	// (load-immune: stat/readdir, never a fork) — and, when the disk ALSO misses,
	// the live process table (a running claude with no binary on disk = the
	// binary is mid-self-update, not uninstalled) — and downgrade a contradicted
	// "absent" to a no-verdict ERROR so it can't overwrite a working install.
	// This brings the Unix path up to the Windows path, which already refuses to
	// call a resolvable-but-unrun binary "absent". See corroborateClaudeAbsent.
	outcome = corroborateClaudeAbsent(outcome, claudeBinaryOnDisk, claudeProcessAlive)
	return outcome, version, hasAPIKey
}

// classifyClaudeProbeOutput maps the probe shell's stdout to a three-way
// outcome. Version-marker → found (version best-effort, may be empty if
// `claude --version` printed nothing before dying); absent-marker → the shell
// completed and `command -v` said no; neither → the shell never reached the
// probe (killed/timed out) and there is NO verdict.
func classifyClaudeProbeOutput(out string) (claudeProbeOutcome, string) {
	if strings.Contains(out, claudeVersionMarker) {
		return claudeProbeFound, versionAfterMarker(out, claudeVersionMarker)
	}
	if strings.Contains(out, claudeAbsentMarker) {
		return claudeProbeAbsent, ""
	}
	return claudeProbeError, ""
}

// corroborateClaudeAbsent second-guesses ONLY a claudeProbeAbsent, and only
// with a hard contradiction, downgrading it to claudeProbeError (no verdict,
// keep prior) instead of flipping a working install to "not installed":
//   - onDisk: a claude binary provably exists on disk → the probe shell's PATH
//     was under-populated (nvm/asdf auto-use lost a fork under load) — a probe
//     failure, not an uninstall.
//   - processAlive: NO binary on disk but a claude process is RUNNING right now
//     → the binary is mid-replacement (Claude self-update: npm removes and
//     re-creates it, leaving a seconds-long genuinely-absent window) — again
//     not an uninstall. A user who really uninstalls while panes still run
//     claude sees the banner only once those processes exit; that lag is
//     harmless for an advisory banner, the false alarm it prevents is not.
//
// Found/Error pass through untouched, and both checks are LAZY (evaluated only
// for an absent outcome; processAlive only when the disk misses). Deliberately
// asymmetric: a contradiction can only SUPPRESS a false absent (it never
// manufactures a false "present" — presence still requires a real found probe),
// and a full miss is not treated as extra proof, so genuine uninstalls stay
// detectable (durability is then judged by confirmClaudeAbsent's hysteresis).
func corroborateClaudeAbsent(outcome claudeProbeOutcome, onDisk, processAlive func() bool) claudeProbeOutcome {
	if outcome != claudeProbeAbsent {
		return outcome
	}
	if onDisk() {
		slog.Info("claude-detect: completed probe said absent but a claude binary is on disk — treating as probe failure, keeping prior verdict")
		return claudeProbeError
	}
	if processAlive() {
		slog.Info("claude-detect: completed probe said absent with no binary on disk, but a claude process is running (binary likely mid-self-update) — treating as probe failure, keeping prior verdict")
		return claudeProbeError
	}
	return claudeProbeAbsent
}

// claudeBinaryOnDisk reports whether a `claude` executable exists on disk in any
// well-known install location, using ONLY load-immune filesystem lookups (a PATH
// walk plus stat/readdir — no shell, no fork). A stat cannot be starved by the
// fork pressure that makes a login-shell probe spuriously fail to resolve claude,
// which is exactly why it is the sound cross-check for a suspicious "absent".
// Used ONLY to contradict an absent verdict (reconcileProbeAbsent), never to
// assert presence.
func claudeBinaryOnDisk() bool {
	if _, err := exec.LookPath("claude"); err == nil {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	return claudeBinaryUnderHome(home)
}

// claudeBinaryUnderHome is the home-relative half of claudeBinaryOnDisk, split
// out so it is unit-testable against a synthetic $HOME. It scans the install
// layouts the daemon's slim launch-time PATH never contains: the native
// installer's ~/.local/bin, and the version-manager trees (nvm/asdf) where a
// managed `claude` lives — the nvm-versions glob is the signal that catches the
// reported WSL false-absent (claude installed only under ~/.nvm/versions/node/*).
func claudeBinaryUnderHome(home string) bool {
	if isExistingFile(filepath.Join(home, ".local", "bin", "claude")) {
		return true
	}
	if isExistingFile(filepath.Join(home, ".asdf", "shims", "claude")) {
		return true
	}
	for _, pattern := range []string{
		filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "claude"),
		filepath.Join(home, ".asdf", "installs", "nodejs", "*", "bin", "claude"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if isExistingFile(m) {
				return true
			}
		}
	}
	return false
}

// isExistingFile reports whether p resolves to an existing non-directory. os.Stat
// FOLLOWS symlinks, so a dangling symlink — e.g. an nvm bin entry left behind by
// an uninstalled package — correctly reports false, keeping genuine uninstalls
// detectable rather than pinned to a stale "installed".
func isExistingFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// claudeVersionMarker fences `claude --version` output from any login-shell
// banner printed before it on stdout.
const claudeVersionMarker = "<<<SOLU_CLAUDE_VERSION>>>"

// claudeAPIKeyMarker is emitted by the probe script when ANTHROPIC_API_KEY is
// set in the login-shell environment (the value itself never leaves the shell).
const claudeAPIKeyMarker = "<<<SOLU_CLAUDE_APIKEY>>>"

// claudeAbsentMarker is emitted when `command -v claude` finds nothing —
// making definitive absence distinguishable from a probe shell that died
// before answering (which must NOT count as "not installed").
const claudeAbsentMarker = "<<<SOLU_CLAUDE_ABSENT>>>"

// versionAfterMarker returns the first line of the output following the marker
// (the login-shell banner, if any, precedes it).
func versionAfterMarker(out, marker string) string {
	if i := strings.LastIndex(out, marker); i >= 0 {
		out = out[i+len(marker):]
	}
	return firstLine(strings.TrimSpace(out))
}

// detectClaudeWindows probes the host-native Windows daemon: the live registry
// PATH (PATHEXT resolves claude.exe/.cmd) then the native installer's default
// dir. Resolving via the registry — not the daemon's stale process PATH — keeps
// detection in lockstep with what pane spawn can actually run (the v0.0.793 fix).
func detectClaudeWindows(ctx context.Context) (claudeProbeOutcome, string) {
	candidates := []string{"claude"}
	if home, _ := os.UserHomeDir(); home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "claude.exe"))
	}
	// Path resolution (registry read / LookPath / Stat) is load-immune, so a
	// full miss across all candidates is definitive absence. But a RESOLVED
	// path whose `--version` errors is a probe FAILURE (node cold-start losing
	// to the shared 8s ctx on a loaded machine), not absence — the file is
	// right there on disk.
	resolvedAny := false
	for _, c := range candidates {
		path := c
		if strings.ContainsAny(c, `\/`) {
			if _, err := os.Stat(c); err != nil {
				continue
			}
		} else {
			// Resolve against the LIVE registry PATH first — the same source pane
			// spawn uses (findOnWindowsPath / mergeRegistryPath, the v0.0.793 fix).
			// The daemon's own process PATH is a stale launch-time snapshot that
			// misses a `claude` added to the user PATH after the daemon started, so
			// a bare exec.LookPath here reports "not installed" for a claude that a
			// PowerShell pane can actually spawn. Fall back to the process PATH when
			// the registry read finds nothing.
			lp, ok := findOnWindowsPath(c)
			if !ok {
				var err error
				lp, err = exec.LookPath(c)
				if err != nil {
					continue
				}
			}
			path = lp
		}
		resolvedAny = true
		cmd := exec.CommandContext(ctx, path, "--version")
		applyNoWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		return claudeProbeFound, firstLine(strings.TrimSpace(string(out)))
	}
	if resolvedAny {
		return claudeProbeError, ""
	}
	return claudeProbeAbsent, ""
}

// firstLine returns the first line of s, trimmed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// handleClaudeRecheck re-probes Claude status on demand (the setup checklist
// forces a fresh probe each time it opens) and returns the fresh status.
// LOCAL-ONLY: a relay-forwarded request is rejected 403 — probing spawns a
// shell on the host, so only the local UI may trigger it.
func (a *apiServer) handleClaudeRecheck(w http.ResponseWriter, r *http.Request) {
	if pairRequestOverRelay(r) {
		http.Error(w, "claude re-check is local-only", http.StatusForbidden)
		return
	}
	a.refreshClaudeStatus()
	st := a.currentClaudeStatus()
	writeJSON(w, 200, map[string]any{
		"claudeChecked":   st.checked,
		"claudeInstalled": st.installed,
		"claudeVersion":   st.version,
		"claudeAuthed":    st.authed,
	})
}

// claudeInstallCommandSpec is what GET /api/setup/claude/install-command
// returns: the OS-correct argv to spawn in a pane, plus a human display string
// for up-front disclosure (we never run a remote installer without showing it).
// supported=false on platforms with no scripted installer.
type claudeInstallCommandSpec struct {
	Supported bool     `json:"supported"`
	Cmd       []string `json:"cmd"`
	Display   string   `json:"display"`
	Note      string   `json:"note"`
}

// claudeInstallCommandForOS builds the official Claude Code installer invocation
// for the daemon's runtime. Unix (WSL/Linux/macOS): the native installer — no
// Node, no sudo, lands in ~/.local/bin; we source loginShellInit first so the
// post-install shell and future panes resolve the new binary. Windows
// host-native: the PowerShell installer. The command ECHOES itself + its source
// before running, so the pane is the disclosure.
func claudeInstallCommandForOS() claudeInstallCommandSpec {
	if runtime.GOOS == "windows" {
		display := "irm https://claude.ai/install.ps1 | iex"
		ps := "Write-Host 'soluterminal will run the official Claude Code installer:'; " +
			"Write-Host '  " + display + "'; " +
			"Write-Host '  (source: https://claude.ai/install.ps1)'; Write-Host ''; " +
			"try { " + display + " } catch { Write-Host ('[soluterminal] Installer failed: ' + $_.Exception.Message) }; " +
			"$claude = Get-Command claude -ErrorAction SilentlyContinue; " +
			"$native = Join-Path $env:USERPROFILE '.local\\bin\\claude.exe'; " +
			"if (-not $claude -and (Test-Path $native)) { $claude = Get-Item $native }; " +
			"if ($claude) { Write-Host '[soluterminal] Claude Code installed - you can close this pane (or run: claude).' } " +
			"else { Write-Host '[soluterminal] Install did not complete - see the output above, then retry or install Claude Code manually.' }"
		return claudeInstallCommandSpec{
			Supported: true,
			Cmd:       []string{"powershell.exe", "-NoExit", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps},
			Display:   display,
			Note:      "Runs the official Windows installer in a new pane (no admin needed).",
		}
	}
	display := "curl -fsSL https://claude.ai/install.sh | bash"
	script := loginShellInit() + "; " +
		"echo 'soluterminal will run the official Claude Code installer:'; " +
		"echo '  " + display + "'; " +
		"echo '  (source: https://claude.ai/install.sh)'; echo; " +
		display + "; echo; " +
		// Verify by actually RESOLVING claude (PATH has ~/.local/bin via the
		// sourced init), NOT the pipe's exit: `curl … | bash` returns 0 even when
		// curl failed (no pipefail), which would print a false "installed".
		"if command -v claude >/dev/null 2>&1; then echo '[soluterminal] Claude Code installed — you can close this pane (or run: claude).'; " +
		"else echo '[soluterminal] Install did not complete — see the output above, then retry.'; fi; " +
		// Drop to an interactive shell so output + any prompts stay visible and
		// the user can act. Completion is detected by an INDEPENDENT recheck poll,
		// not this shell's exit.
		"exec bash -i"
	return claudeInstallCommandSpec{
		Supported: true,
		Cmd:       []string{"bash", "-lc", script},
		Display:   display,
		Note:      "Runs the official installer in a new pane (no Node, no sudo; installs to ~/.local/bin).",
	}
}

// handleClaudeInstallCommand returns the OS-correct installer command for the
// web to spawn in a pane. LOCAL-ONLY (403 over the relay): installing is a host
// action, so only the local UI may request it.
func (a *apiServer) handleClaudeInstallCommand(w http.ResponseWriter, r *http.Request) {
	if pairRequestOverRelay(r) {
		http.Error(w, "claude install is local-only", http.StatusForbidden)
		return
	}
	writeJSON(w, 200, claudeInstallCommandForOS())
}
