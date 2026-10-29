package main

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

func init() {
	// Ensure correct MIME types for PWA assets — some distros' /etc/mime.types
	// don't have .webmanifest. PNGs and JS/CSS are always fine.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

//go:embed all:webdist
var webFS embed.FS

// version is overridden at build time via `-ldflags "-X main.version=X.Y.Z"`.
// The default ("dev") is what unbuilt-from-source dev binaries report; the
// auto-updater treats "dev" specially (skips checking) so iterating against
// `go run` doesn't spam the manifest endpoint.
var version = "dev"

// Build provenance, stamped by scripts/release-ldflags.sh. buildChannel is
// "release" only for publish.sh builds, so a hand-installed daemon reports
// "local" (or "preview" for follow-main) even when its version number looks
// like a release. See docs/RUNBOOK-hand-install.md.
var (
	buildSourceSha = "unknown"
	buildDirty     = "unknown"
	buildChannel   = "local"
)

func buildProvenance() map[string]any {
	return map[string]any{
		"sourceSha": buildSourceSha,
		"dirty":     buildDirty == "1",
		"channel":   buildChannel,
		"flavor":    currentBuildFlavor(),
	}
}

// startupHandoffListener is the post-initialization commit point shared by
// ordinary boots and update successors. In successor mode the preflight first
// distinguishes the predecessor-owned port from a genuinely invalid listener;
// only then may the boot marker release the predecessor. Durable workflow
// rehydration starts last, after this process has exclusive listener ownership.
type startupHandoffDeps struct {
	preflight    func(string, string) (net.Listener, error)
	writeMarker  func()
	clearPending func()
	acquire      func(string, string) (net.Listener, error)
}

func startupHandoffListener(network, addr string) (net.Listener, error) {
	return startupHandoffListenerWithDeps(network, addr, startupHandoffDeps{
		preflight:    preflightUpdateSuccessorListener,
		writeMarker:  writeSuccessorBootMarker,
		clearPending: clearUpdatePendingMarker,
		acquire:      listenWithUpdateRetry,
	})
}

func startupHandoffListenerWithDeps(network, addr string, deps startupHandoffDeps) (net.Listener, error) {
	ln, err := deps.preflight(network, addr)
	if err != nil {
		return nil, err
	}
	deps.writeMarker()
	deps.clearPending()
	if ln == nil {
		ln, err = deps.acquire(network, addr)
		if err != nil {
			return nil, err
		}
	}
	return ln, nil
}

func jevBridgeEndpointForListener(addr net.Addr) (string, error) {
	if addr == nil {
		return "", errors.New("listener address unavailable")
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil || strings.TrimSpace(port) == "" {
		return "", errors.New("listener address unavailable")
	}
	// An unspecified listener accepts local traffic on its address family, but
	// an unspecified address is not a usable discovery destination. Preserve a
	// concrete listener host (notably ::1) and map wildcards to same-family
	// loopback.
	ipHost := host
	if zoneAt := strings.LastIndex(ipHost, "%"); zoneAt >= 0 {
		ipHost = ipHost[:zoneAt]
	}
	ip := net.ParseIP(ipHost)
	if ip == nil {
		return "", errors.New("listener address unavailable")
	}
	if ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + jevBridgePath, nil
}

// unknownSubcommandRE matches a bare word that reads like a subcommand.
var unknownSubcommandRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// isVersionCommand reports whether args selects the version subcommand or flag.
func isVersionCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "version", "--version", "-version", "-v":
		return true
	default:
		return false
	}
}

// runVersionCommand prints the daemon version to out and returns exit code 0.
func runVersionCommand(out io.Writer) int {
	fmt.Fprintln(out, version)
	return 0
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == capabilityCommand {
		os.Exit(runCapabilityCommand(os.Args[2:], os.Stdout))
	}
	if run, ok := paneShimCommand(filepath.Base(os.Args[0])); ok && os.Getenv(codexInlineShimEnvKey) != "" {
		os.Exit(run(os.Args[1:]))
	}
	// Offline diagnostics must run before logging or daemon initialization.
	if len(os.Args) >= 2 && os.Args[1] == "support-bundle" {
		os.Exit(runSupportBundleCLI(os.Args[2:], os.Stdout, os.Stderr))
	}
	if isVersionCommand(os.Args) {
		os.Exit(runVersionCommand(os.Stdout))
	}
	// Configure logging before subcommand dispatch so agent / router
	// inherit the same LOG_FORMAT / LOG_LEVEL env knobs without each
	// one needing its own setup. See logging.go.
	initLogging()

	// Subcommand dispatch: `solud agent ...` runs in agent mode,
	// `solud router ...` is a lightweight HTTP reverse proxy used by
	// the Tauri desktop app's dual-backend mode to multiplex two
	// daemons behind a single origin. Everything else is the normal
	// long-running daemon.
	if len(os.Args) >= 2 && os.Args[1] == "agent" {
		if code := runAgent(); code != 0 {
			os.Exit(code)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "router" {
		runRouter()
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "codex-terminal" {
		os.Exit(runCodexTerminal())
	}
	// `solud muse-driver ...` is the long-lived child of a native Muse pane
	// (see muse_driver.go) — it bridges Muse Code's one-shot `muse exec` to
	// the persistent stdio contract native panes expect.
	if len(os.Args) >= 2 && os.Args[1] == "muse-driver" {
		runMuseDriver()
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "gc" {
		fmt.Fprintln(os.Stderr, "solud gc was retired with the task engine; workflow data is preserved")
		os.Exit(2)
	}
	if len(os.Args) >= 2 && os.Args[1] == "host-storage" {
		os.Exit(runHostStorageCommand())
	}
	if len(os.Args) >= 2 && os.Args[1] == "uninstall-plan" {
		os.Exit(runUninstallPlanCommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "uninstall-cleanup" {
		os.Exit(runUninstallCleanupCommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && (os.Args[1] == "--update-capabilities" || os.Args[1] == "-update-capabilities") {
		body, err := updateCapabilitiesJSON(version, defaultDataDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, "update-capabilities:", err)
			os.Exit(1)
		}
		fmt.Println(string(body))
		return
	}
	// Updater rollback. Operator-invoked CLI: swaps the parked .old
	// binary back into the canonical path. Handled here, before flag
	// parsing, so rollback doesn't drag in the rest of the daemon's
	// flag surface (and so a misconfigured --addr / --relay can't
	// block someone from rolling back).
	if len(os.Args) >= 2 && (os.Args[1] == "--restore-previous" || os.Args[1] == "-restore-previous") {
		if err := RestorePrevious(); err != nil {
			fmt.Fprintln(os.Stderr, "restore-previous:", err)
			os.Exit(1)
		}
		fmt.Println("restored previous version. Re-launch the daemon to pick it up.")
		return
	}

	// Everything past this point is a long-running daemon: a fresh launch, a
	// supervisor respawn, a self-update successor forked by spawnSuccessor,
	// or a manual `solud` run. All of them get a durable log they own, so
	// crash evidence no longer depends on whatever stdout the launcher
	// happened to attach — the successor case had none at all, which is why
	// a self-update used to end the on-disk record of a daemon's life.
	// Placed AFTER subcommand dispatch on purpose: pane agents and routers
	// are numerous and short-lived, and dozens of them interleaving into one
	// file would make it useless. Placed BEFORE the rollback check and the
	// clean-env re-exec so both of those decisions are themselves captured;
	// the re-exec simply writes a second boot banner from the new process
	// (an honest record — and Go opens files O_CLOEXEC, so no fd leaks
	// across the exec). See durable_log.go.
	attachDurableLog()

	// Self-clean: if this daemon was launched from inside a Claude Code
	// session it inherited CLAUDE_CODE_SESSION_ID + CLAUDE_CODE_CHILD_SESSION,
	// which would otherwise leak into every pane we spawn (making the pane's
	// own `claude` a nested child session that never writes a transcript) AND
	// propagate to every successor across auto-updates. Re-exec ONCE with the
	// inherited Claude env stripped so this long-lived process — and its whole
	// successor lineage — is clean. Placed AFTER subcommand dispatch (agent /
	// router strip per-command themselves) and BEFORE any port bind or state
	// load, so there is no listener handoff or in-memory state to lose; pane
	// agents are detached (Setsid) and survive the exec exactly as they do
	// across an auto-update. Loop-guarded by SOLU_ENV_CLEANED.
	// FIRST: if a prior auto-update's systemd successor has been failing to boot,
	// restore the parked .old binary before any fatal-prone init runs. Placed
	// BEFORE maybeReexecCleanEnv so a rollback-exec restarts into the restored
	// binary's normal boot (own SOLU_ROLLBACK_DONE loop guard). No-op on every
	// normal boot (no .update-pending marker present).
	maybeRollbackAfterFailedUpdate()
	maybeReexecCleanEnv()
	// Consume the durable pre-swap attribution only after any clean-env re-exec
	// has completed, then hand it to workflow boot adoption exactly once.
	_ = readRestartIntentForSelf(time.Now())

	// If the shell watchdog respawned us after our predecessor died, it left
	// the death notice in env. Capture and UNSET it now — before anything can
	// spawn a child that would inherit and re-confess it — and report it once
	// cloud creds come up below. This is the only path by which a FATAL
	// daemon death (unrecovered panic, log.Fatalf, OOM kill) ever reaches the
	// crash table; recover()-site reports cover only crashes we survived.
	watchdogEvidence := captureWatchdogRespawnEvidence()

	addr := flag.String("addr", "127.0.0.1:8787", "bind address")
	dataDir := flag.String("data", defaultDataDir(), "data directory for workspace persistence")
	devWeb := flag.String("dev-web", "", "if set, serve static files from this path instead of embedded assets (for dev)")
	relayURL := flag.String("relay", "", "if set, also dial out to this cloud relay URL (e.g. wss://soluterminal.app/relay)")
	relayToken := flag.String("relay-token", "", "machine token for authenticating to the relay (overrides claim.json)")
	// Backend identity for cloud-side routing. Two daemons on the same
	// machine (WSL + Windows-native in the Tauri Dual backend) pair
	// with the SAME cloud claim but present different backend IDs on
	// the relay handshake. The cloud keys WS connections on (machineId,
	// backendId) so /m/<mid>/b/<bid>/api/* can route to the right
	// daemon, letting mobile clients see both backends at once instead
	// of whichever one won the most recent reconnect race.
	//
	// Empty string = "legacy / single-backend daemon" — cloud routes
	// /m/<mid>/api/* to this daemon normally (backward compat for
	// installs that predate this flag).
	backendID := flag.String("backend-id", "", "cloud-side identifier for this daemon when multiple daemons share one claim (e.g. 'wsl', 'win'). Empty for single-backend setups.")
	// --shell-version is passed by the Tauri sidecar launcher so the
	// updater can detect when a new NSIS installer is published. Empty
	// when solud runs standalone — the shell-update channel disables
	// itself in that case (no badge, no /api/update/apply-shell action).
	shellVersion := flag.String("shell-version", "", "version of the Tauri shell that spawned this daemon (enables the shell-installer update channel)")
	// Shell PLATFORM is independent of daemon platform: in WSL mode
	// the daemon runs on linux-amd64 but the Tauri shell is a Windows
	// .exe, so the shell-installer manifest key is windows-amd64. Tauri
	// passes this based on its own cfg, not the daemon's runtime.GOOS.
	// Empty falls back to runtime.GOOS+ARCH for backward compat.
	shellPlatform := flag.String("shell-platform", "", "manifest platform key for the Tauri shell (e.g. windows-amd64). Empty = fall back to daemon's runtime platform.")
	doClaim := flag.Bool("claim", false, "pair this machine with the cloud: prints a 6-char code, waits for you to accept it in the web UI, then writes ~/.solu/claim.json and exits")
	flag.Parse()
	// flag.Parse stops at the first non-flag word and the daemon used to
	// ignore it, so a mistyped or newer subcommand (e.g. `solud support-bundle`
	// on an old binary) silently started a full foreground daemon. Refuse a
	// leftover that looks like a subcommand instead.
	if word := flag.Arg(0); word != "" && unknownSubcommandRE.MatchString(word) {
		fmt.Fprintf(os.Stderr, "solud: unknown command %q (this solud may be too old for it)\n", word)
		os.Exit(2)
	}
	// Before anything writes to the data directory: whether this computer
	// starts with approved devices only (trusted_devices.go).
	if policy, err := decideRemoteAccessPolicyOnce(*dataDir, *relayToken != ""); err != nil {
		log.Printf("remote-devices: could not record the starting remote-access policy: %v", err)
	} else if policy == remoteAccessApproved {
		log.Printf("remote-devices: new install starts with approved devices only")
	}
	// Preview/generated/temp cleanup also runs when workflow initialization
	// is unavailable. Load the independent authority first;
	// a corrupt or missing committed journal publishes a refusal, never an
	// empty pin index. This does not activate a retirement mutation route.

	configureGeneratedStorage(filepath.Join(*dataDir, "workflow"))
	retirementAuthority, retirementErr := loadRetirementAuthority(*dataDir)
	retirementCleanupAuthority.Store(retirementAuthority)
	if retirementErr != nil {
		log.Printf("retirement authority unavailable; destructive cleanup held: %v", retirementErr)
	}

	// A mounted acceptance preview proxies /api/* to this daemon so the
	// previewed app runs against live data. Always loopback, never the bind
	// address: the daemon must not be reachable through the preview beyond
	// the exposure that already gates it.
	if _, port, splitErr := net.SplitHostPort(*addr); splitErr == nil && port != "" {
		previewAPIUpstream = "http://127.0.0.1:" + port
	}

	// Fallback for systemd / standalone launches that can't pass the
	// --shell-version flag: the Tauri shell drops ~/.solu/shell-info.json
	// on every startup, and we read it here so the installer-update
	// badge still works when solud was started outside of Tauri.
	// Explicit CLI flags always win over the file.
	if *shellVersion == "" || *shellPlatform == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			fileVer, filePlat, _ := readShellInfo(home)
			if *shellVersion == "" {
				*shellVersion = fileVer
			}
			if *shellPlatform == "" {
				*shellPlatform = filePlat
			}
		}
	}

	// If --claim, run the interactive pairing flow and exit. Requires
	// --relay so we know which cloud to talk to.
	if *doClaim {
		if *relayURL == "" {
			log.Fatalf("--claim requires --relay to specify which cloud to pair with")
		}
		claimCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := RunClaimFlow(claimCtx, *relayURL, *dataDir); err != nil {
			log.Fatalf("claim: %v", err)
		}
		return
	}

	// Own the data directory before anything below loads, reattaches or
	// respawns panes. A second daemon on the same data directory exits here.
	dataDirOwner := dataDirLockHolder{Addr: *addr, Backend: *backendID, Version: version}
	dataDirLockHeld = claimDataDirForBoot(*dataDir, dataDirOwner)

	// Non-claim path: if --relay is set and --relay-token isn't, fall back
	// to the token stored by a previous --claim run.
	//
	// CRITICAL: a malformed/truncated claim.json must NOT crash the daemon
	// (the old log.Fatalf bricked local terminal use on a torn rename). Treat
	// a parse error as "no usable creds yet" and log — the relay supervisor
	// surfaces a distinct `claimCorrupt` state and the user can re-pair.
	// Absence is likewise soft: the supervisor stays alive and connects live
	// the moment a valid claim.json appears (no restart needed).
	if *relayURL != "" && *relayToken == "" {
		if c, err := loadClaimSafe(*dataDir); err != nil {
			log.Printf("claim.json present but unreadable/corrupt: %v — cloud disabled until re-paired (local terminal unaffected)", err)
		} else if c != nil {
			*relayToken = c.Token
		}
	}
	// NOTE: we deliberately no longer unset *relayURL when the token is
	// missing. The boot-once "*relayURL = \"\"" gate is what kept a
	// claimed-after-boot machine permanently dark. The relay supervisor
	// (newRelaySupervisor below) is now the SOLE owner of the relay
	// lifecycle: it re-reads claim.json on a poll and dials live whenever a
	// valid token appears or changes — on every OS, with no restart.

	// Load per-user secrets (GROQ_API_KEY etc.) from <dataDir>/env so the
	// user doesn't have to retype keys at every daemon restart. Shell env
	// takes precedence, so one-off overrides still work.
	loadEnvFile(*dataDir)

	// Write a tuned ~/.dircolors on first run (if the user doesn't have one
	// yet) so GNU ls doesn't render other-writable dirs as blue-on-green.
	// No-op when a ~/.dircolors already exists.
	ensureDircolorsFile()

	mgr := NewManager(*dataDir)
	if _, port, err := net.SplitHostPort(*addr); err == nil {
		mgr.codexDaemonURL = "http://127.0.0.1:" + port
	}
	ws, err := NewWorkspaceStore(*dataDir)
	if err != nil {
		log.Fatalf("workspace store: %v", err)
	}
	// Guarantee at least one named, user-visible workspace. Makes
	// bootstrap obvious and — in dual-backend mode — means the other
	// daemon always has something clickable in the sidebar, so users
	// never get stranded on one side.
	if err := ws.EnsureDefault(); err != nil {
		log.Printf("ensure default workspace: %v (continuing)", err)
	}
	paneStore, err := NewPaneStore(*dataDir)
	if err != nil {
		log.Fatalf("pane store: %v", err)
	}
	recoverySettings, err := newRecoverySettingsStore(*dataDir)
	if err != nil {
		log.Printf("recovery settings unavailable: %v (using defaults)", err)
		recoverySettings = defaultRecoverySettingsStore(*dataDir)
	}
	nativeSettings, err := newNativeSettingsStore(*dataDir)
	if err != nil {
		log.Printf("native settings unavailable: %v (using defaults)", err)
		nativeSettings = defaultNativeSettingsStore(*dataDir)
	}
	updateSettings, err := newUpdateSettingsStore(*dataDir)
	if err != nil {
		log.Printf("update settings unavailable: %v (using defaults)", err)
		updateSettings = defaultUpdateSettingsStore(*dataDir)
	}
	rollbackStore, err := newUpdateRollbackStore(*dataDir)
	if err != nil {
		log.Printf("rollback state unavailable: %v (using defaults)", err)
		rollbackStore = defaultUpdateRollbackStore(*dataDir)
	}
	// promptSuggestionsEnabled is read fresh at every native (re)spawn to decide
	// whether --prompt-suggestions is appended to the headless claude argv.
	mgr.promptSuggestionsEnabled = func() bool { return nativeSettings.PromptSuggestionsEnabled() }

	// Keep the persisted pane list in sync whenever the manager spawns or
	// loses a pane.
	mgr.onPaneExit = func(p *Pane) {
		// Explicit close removes the record in the delete handler. Natural exit
		// must keep the last conversation identity and shell recipe for reboot.
		mgr.withCurrentPane(p, func() {
			if err := paneStore.Put(p); err != nil {
				log.Printf("persist exited pane %s: %v", p.ID, err)
			}
		})
		forgetPaneDegraded(p.ID)
	}
	// Persist + broadcast prompt-history changes. Set BEFORE the
	// reattach loop below so reattached panes get the hook too; the
	// closure reads mgr.bus at fire time because the bus is constructed
	// later in startup (commits can't fire before HTTP is up — they
	// originate from client writes).
	mgr.onOwnerViewersCommit = func(p *Pane) error {
		var err error
		if !mgr.withCurrentPane(p, func() { err = paneStore.Put(p) }) {
			return fmt.Errorf("owner pane changed before viewer persistence")
		}
		return err
	}
	mgr.onPromptsCommit = func(p *Pane) {
		// A DTO/process probe may finish after a mode switch replaced the pane
		// pointer under the same ID. Never let that stale pointer overwrite the
		// newly persisted socket/PID/kind.
		if !mgr.withCurrentPane(p, func() {
			if err := paneStore.Put(p); err != nil {
				log.Printf("persist pane identity %s (will retry): %v", p.ID, err)
			}
		}) {
			return
		}
		if mgr.bus != nil {
			snap := p.Snapshot()
			mgr.bus.publish(uiEvent{
				Type:      "pane-prompt",
				PaneID:    snap.ID,
				PaneTitle: snap.Title,
				Prompts:   snap.LastPrompts,
				Ts:        time.Now().UnixMilli(),
			})
		}
		// Opt-in AI naming of a still-generic agent pane (pane_auto_name.go).
		notifyPaneAutoNamer(p)
	}

	// The origin-pane delegation nudger (delegation_nudge.go): it types one
	// idle-safe status line into the pane that filed a piece of work, and hands
	// a pane's FRESH Claude session a digest of what that pane already has in
	// flight. Constructed unconditionally — without a workflow engine it simply
	// never receives an event — and constructed HERE, before the reattach loop,
	// because the digest's trigger has to be on the Manager before any pane is
	// restored (the same reason onPromptsCommit is wired here). Its one
	// late-bound piece, the digest's content source, arrives via SetInFlight
	// once the engine exists; a reattached pane's read loop is already running
	// by then, so that setter goes through the nudger's own mutex rather than
	// writing the field from here.

	// Persist the durable Codex turn flag the instant it transitions. This is
	// the ONLY evidence that separates "a restart killed your response" from
	// "you interrupted it" — Codex reports both identically on resume — so it
	// has to reach disk before the crash does, not on a debounce. Persist-only
	// (no "pane-prompt" publish like onPromptsCommit): a turn edge changes no
	// prompts. Keeps onPromptsCommit's stale-pointer guard, since a mode switch
	// can replace the pane under this same id while an edge is in flight.
	mgr.onCodexTurnFlagCommit = func(p *Pane) {
		if !mgr.withCurrentPane(p, func() { _ = paneStore.Put(p) }) {
			return
		}
	}

	// Persist dims changes, debounced per pane. Without this, panes.json
	// keeps the dims from spawn/resume time; after a daemon restart the
	// rebuilt panes then report widths the PTY hasn't had for weeks, and
	// clients seeding their attach resize from /api/panes blow the real
	// PTY out to that stale width — the "every app update garbles all
	// panes" bug. The 2s debounce coalesces splitter-drag resize storms
	// into one panes.json write; the liveness check keeps a late-firing
	// timer from resurrecting a pane that was closed (paneStore.Remove)
	// in the window.
	var dimsPersistMu sync.Mutex
	dimsPersistPending := map[string]bool{}
	mgr.onDimsCommit = func(p *Pane) {
		dimsPersistMu.Lock()
		if dimsPersistPending[p.ID] {
			dimsPersistMu.Unlock()
			return
		}
		dimsPersistPending[p.ID] = true
		dimsPersistMu.Unlock()
		time.AfterFunc(2*time.Second, func() {
			dimsPersistMu.Lock()
			delete(dimsPersistPending, p.ID)
			dimsPersistMu.Unlock()
			if cur, ok := mgr.Get(p.ID); !ok || cur != p {
				return
			}
			if p.Snapshot().Status != PaneRunning {
				return
			}
			_ = paneStore.Put(p)
		})
	}

	// attention computes the per-pane "who needs to act" signal every channel
	// (web client, attention inbox, phone push) keys its alerts on. Created
	// before panes attach so the turn-edge hook below can refresh it eagerly;
	// Aria (standing orders + pause/cap gates) is late-bound after
	// construction via SetAria.
	attention := newAttentionEngine(mgr)

	// onTurnStateEdge publishes a pane-state (AgentMidTurn) event the instant a
	// native pane's nativeInTurn flag flips true↔false — so the client spinner
	// reflects turn start/finish in real time instead of waiting on the 2s activity
	// ticker (which skips sub-5s turns) or the 3s panes poll. Reads mgr.bus at fire
	// time (constructed later, like onPromptsCommit). Publishes ONLY pane-state (not
	// pane.reply_complete), so it doesn't double-count title re-warms; the value is
	// computed exactly as the DTO does (agentMidTurnForState) so an SSE edge and the
	// next poll agree (samePaneInfo). A double-fire with the activity watcher is a
	// harmless no-op — the client's applyPaneStateFromSSE is value-guarded.
	mgr.onTurnStateEdge = func(p *Pane) {
		// Approval park/resolve flips the pane's runtime state through this
		// edge — reclassify attention immediately so a native prompt is
		// tagged auto-pending vs needs-user before any client can alert on
		// the raw state (the pre-attention race fired "Approval needed"
		// toasts that auto-approval then cleared seconds later).
		attention.Refresh(p)
		if mgr.bus == nil {
			return
		}
		snap := p.Snapshot()
		// This closure is native-only (every publishTurnEdge call site is a native
		// in-turn flip), and a native pane never has a codex descendant — so pass
		// hasCodex=false to skip the /proc descendant scan computeState() would run.
		// The result equals the poll DTO's state for native panes (api.go passes the
		// same false there), so the SSE edge and the next poll never disagree.
		state := p.computeStateWithHasCodex(false)
		amt := p.agentMidTurnForState(state)
		mgr.bus.publish(uiEvent{
			Type:         "pane-state",
			PaneID:       snap.ID,
			PaneTitle:    snap.Title,
			Ts:           time.Now().UnixMilli(),
			ToState:      string(state),
			AgentMidTurn: &amt,
		})
	}

	// Reattach to any agents still alive from a previous daemon run.
	// Two phases so HTTP becomes reachable fast even with many panes:
	//   1. AttachExisting (synchronous, ~1s timeout per dead pane). Cheap
	//      when the agent is alive; ~instant when pidAlive() returns false.
	//   2. ResumeAgentPane for the dead-but-resumable ones (slow: spawns
	//      a fresh provider resume process per pane and waits up to
	//      3s on dialAgent). Deferred to a background goroutine that runs
	//      after HTTP is listening — with N dead Claude panes that's
	//      N×3s of blocked startup we avoid.
	// A daemon AUTO-UPDATE leaves the agent processes alive (they're separate
	// from the daemon), so AttachExisting reattaches them and nothing resumes.
	// Dead agents only happen on a real teardown — WSL shutdown, reboot, OOM —
	// where ALL of them die at once. Auto-respawning every one then (the old
	// behavior) felt aggressive: dozens of `claude --resume` the user never
	// asked for. So by default we SUSPEND those panes — they show the "Resume
	// session" affordance and come back only when the user clicks. Users can opt
	// back into eager auto-respawn in Preferences; SOLU_AUTO_RESUME=1 seeds the
	// same default for installs that have not saved a preference yet.
	autoResume := recoverySettings.AutoResumeAfterRestart()
	var toResume []PersistedPane
	var suspended int
	ownerHistories, historyErr := loadOwnerHistories(*dataDir)
	if historyErr != nil {
		log.Printf("owner retirement recovery is incomplete; preserving uncertain owners and continuing ordinary workspace restore: %v", historyErr)
	}
	// These records are authoritative before ordinary restore, even when a
	// crash happened between durable closing intent and panes.json removal.
	for id, history := range ownerHistories {
		if history.RestoreEnabled {
			continue
		}
		if err := paneStore.Remove(id); err != nil {
			log.Printf("retained owner restore removal deferred: %v", err)
			historyErr = errors.Join(historyErr, err)
		}
		if history.State.Phase == "retired" {
			continue
		}
		info := history.Pane
		info.OwnerRetirementRestoreDisabled = true
		if p, err := mgr.AttachExisting(&info); err != nil || p == nil {
			log.Printf("retained owner %s: existing socket unavailable; no automatic respawn (%v)", id, err)
		}
	}
	for _, info := range paneStore.List() {
		info := info
		if history, retained := ownerHistories[info.ID]; retained && !history.RestoreEnabled {
			continue
		}
		if historyErr != nil && info.Role == paneRoleTaskOwner {
			// An unreadable record may be this owner's only close intent. A
			// live exact socket may reconnect; never resume or reap it blindly.
			info.OwnerRetirementRestoreDisabled = true
			if p, err := mgr.AttachExisting(&info); err != nil || p == nil {
				log.Printf("uncertain retained owner %s requires explicit history recovery: %v", info.ID, err)
			}
			continue
		}
		// Recover a lost CodexThreadID from the `codex resume <id>` baked into
		// the Cmd (bare or shell-wrapped) BEFORE agent-kind normalization, so
		// the thread-id heal inside normalizeAgentKind composes with it: a pane
		// that lost BOTH its agent field and its thread id still resumes as the
		// Codex conversation instead of being dropped as an unidentified shell
		// (Codex twin of the ClaudeSessionID argv recovery below). Only SOLU's
		// own codex-resume launches bake this argv, always alongside
		// Agent=codex, so a parse hit on an empty field is field-loss by
		// definition. No-op for every legitimate Claude pane.
		if strings.TrimSpace(info.CodexThreadID) == "" {
			if tid := resumeThreadIDFromCodexCmd(info.Cmd); tid != "" {
				info.CodexThreadID = tid
			}
		}
		// Muse twin of the Codex argv recovery above: only SOLU's own
		// muse-resume launches bake `muse resume <uuid>` argv.
		if strings.TrimSpace(info.MuseSessionID) == "" {
			if sid := resumeSessionIDFromMuseCmd(info.Cmd); sid != "" {
				info.MuseSessionID = sid
			}
		}
		var identityNotes []string
		info, identityNotes = healPersistedPane(info)
		for _, note := range identityNotes {
			log.Printf("reattach pane %s: identity heal: %s", info.ID, note)
		}
		// --- Native Focus-pane recovery (repairs the "Focus pane → bash shell
		// after an app update" regression an older daemon's resume path caused) ---
		// 1) Restore a lost native Kind: persisted Kind=="" but the Cmd is the
		//    headless `claude -p … stream-json` argv (see healLostNativeKind).
		info, kindHealed := healLostNativeKind(info)
		// 2) Tear down a broken pre-fix native agent: a Kind=="native" pane still
		//    bound to an agent spawned WITHOUT --no-pty ran claude -p under a PTY,
		//    crashed with "Input must be provided … --print", and fell back to a
		//    zombie `bash -i`. Reattaching would just rebind that dead shell — kill
		//    it and resume the conversation fresh under a proper --no-pty agent.
		//    The identity check (cmdline names THIS pane's socket) keeps
		//    terminatePid safe against a recycled pid.
		if info.Kind == "native" && isStaleNonPtyNativeAgent(info.AgentPid, info.SocketPath) {
			log.Printf("reattach pane %s: live agent lacks --no-pty (broken native) → clean resume", info.ID)
			_ = terminatePid(info.AgentPid)
			info.AgentPid = 0
			info.SocketPath = ""
		}
		// 3) Recover the session id from the `--resume <id>` baked into the Cmd
		//    when the ClaudeSessionID field itself was cleared by the corruption,
		//    so the conversation still resumes instead of the pane being dropped.
		if effectiveAgentKind(info.Agent) == agentClaude && info.Kind == "native" && strings.TrimSpace(info.ClaudeSessionID) == "" {
			if sid := resumeSessionIDFromCmd(info.Cmd); sid != "" {
				info.ClaudeSessionID = sid
			}
		}
		// --- end native recovery ---

		p, err := mgr.AttachExisting(&info)
		if err != nil {
			log.Printf("reattach pane %s: %v", info.ID, err)
			info.RecoveryError = err.Error()
			if sp := mgr.SuspendExisting(&info); sp != nil {
				if perr := paneStore.Put(sp); perr != nil {
					log.Printf("persist failed reattach %s: %v", info.ID, perr)
				}
			}
			continue
		}
		if p == nil {
			conversationID := persistedPaneConversationID(info)
			// LIVE ORPHAN: AttachExisting gave up, but the agent pid is still alive
			// and genuinely serves THIS pane's socket — the dial failed only because
			// the socket FILE was unlinked out from under the still-running agent
			// (resume/restore/switch/sweep paths os.Remove it). Agents with the
			// socket self-heal watchdog (agent.go) re-create the path within ~1s, so
			// FIRST wait briefly and try to RECONNECT — that preserves the in-flight
			// turn. Only if it never re-binds (a pre-watchdog agent, or a genuinely
			// wedged one) do we kill the orphan tree and respawn --resume to recover
			// the conversation. A truly dead agent → agentPidOwnsSocket false → falls
			// through to the suspend default, preserving the "don't mass-resume on a
			// full teardown" intent.
			if conversationID != "" && !info.IdentityBlocked && agentPidOwnsSocket(info.AgentPid, info.SocketPath) {
				if _, statErr := os.Stat(info.SocketPath); os.IsNotExist(statErr) {
					if waitForAgentSocket(info.AgentPid, info.SocketPath, liveOrphanRebindWait) {
						if p2, aerr := mgr.AttachExisting(&info); aerr == nil && p2 != nil {
							if kindHealed || len(identityNotes) > 0 {
								if perr := paneStore.Put(p2); perr != nil {
									log.Printf("persist healed pane %s: %v", info.ID, perr)
								}
							}
							log.Printf("reattached pane %s → %s (agent pid %d, socket self-healed)", info.ID, info.Title, info.AgentPid)
							continue
						}
					}
					log.Printf("reattach pane %s: agent %d alive but socket file gone and did not re-bind → kill + respawn", info.ID, info.AgentPid)
					killAgentTree(info.AgentPid)
					info.AgentPid = 0
					info.SocketPath = ""
					toResume = append(toResume, info)
					continue
				}
				// Socket file present but dial failed — a rare transient under
				// restart load; fall through to suspend (recoverable via Resume).
			}
			if (conversationID != "" || plainShellRecovery(info)) && !info.IdentityBlocked {
				if plainShellRecovery(info) && conversationID == "" {
					info.RecoveryError = missingPaneIdentityMessage(info)
				}
				if autoResume {
					toResume = append(toResume, info)
					continue
				}
				if sp := mgr.SuspendExisting(&info); sp != nil {
					suspended++
					// Persist the kind/session repair so panes.json is corrected.
					if kindHealed || len(identityNotes) > 0 {
						if perr := paneStore.Put(sp); perr != nil {
							log.Printf("persist healed pane %s: %v", info.ID, perr)
						}
					}
					continue
				}
			}
			// Keep unidentified sessions and shells visible. Never turn a missing
			// conversation ID into a fresh conversation or erase the saved tile.
			info.RecoveryError = missingPaneIdentityMessage(info)
			if sp := mgr.SuspendExisting(&info); sp != nil {
				if err := paneStore.Put(sp); err != nil {
					log.Printf("persist recovery pane %s: %v", info.ID, err)
				}
			}
			log.Printf("pane %s retained for recovery: %s", info.ID, info.RecoveryError)
			continue
		}
		// Reattached to a live, healthy agent. If we healed a lost Kind, persist
		// the repair so panes.json is corrected for future restarts.
		if kindHealed || len(identityNotes) > 0 {
			if perr := paneStore.Put(p); perr != nil {
				log.Printf("persist healed pane %s: %v", info.ID, perr)
			}
		}
		log.Printf("reattached pane %s → %s (agent pid %d)", info.ID, info.Title, info.AgentPid)
	}
	if suspended > 0 {
		log.Printf("suspended %d resumable agent pane(s) for manual resume (enable auto-resume in Preferences to auto-respawn)", suspended)
	}

	// Sweep orphan agent sockets. Anything in ~/.solu/agents/ that doesn't
	// belong to a live pane is leftover from a crashed agent, a killed
	// daemon, or a prior panes.json that got rewritten. Safe to delete —
	// a live agent's socket is kept open by its process, and removing the
	// filesystem entry doesn't affect an already-bound Unix socket.
	agentsDir := filepath.Join(*dataDir, "agents")
	if entries, err := os.ReadDir(agentsDir); err == nil && historyErr == nil {
		live := map[string]bool{}
		for _, history := range ownerHistories {
			if !history.RestoreEnabled && history.State.Phase != "retired" {
				live[filepath.Base(history.Pane.SocketPath)] = true
			}
		}
		for _, p := range mgr.List() {
			live[filepath.Base(p.SocketPath)] = true
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sock") {
				continue
			}
			if live[e.Name()] {
				continue
			}
			if err := os.Remove(filepath.Join(agentsDir, e.Name())); err == nil {
				log.Printf("removed orphan socket %s", e.Name())
			}
		}
	}

	// Reap leaked agent PROCESSES (the socket sweep above only deletes files): a
	// surviving-but-unreattached agent, or a duplicate a prior resume left on the
	// same socket, that no RUNNING pane owns. Runs after the restore loop so every
	// reattached/respawned agent is already a live pane's Pid and is spared. Scoped
	// to agentsDir so it ONLY reaps our own agents — never another daemon's.
	if historyErr == nil {
		reapOrphanAgents(mgr, agentsDir, ownerHistories)
	}
	// A managed harness client is DETACHED from its pane's process group, so a
	// pane restart can orphan it; its harness child then holds the session's
	// cross-process flock and later /resume calls answer "Internal error". Reap
	// those left behind by a predecessor agent now that every live pane's agent
	// pid is published.
	if historyErr == nil {
		reapOrphanHarnessClients(mgr, *dataDir, ownerHistories)
	}

	for i := range toResume {
		mgr.SuspendExisting(&toResume[i])
	}

	// Background: respawn agent panes whose processes died across the
	// daemon restart. Each call resumes the provider conversation and waits
	// up to 3s on dialAgent, so doing this synchronously could stall
	// startup for tens of seconds with many panes. Running it after
	// HTTP/ports/etc. are set up means the web UI mounts immediately
	// and resumed panes pop in as they come back up.
	if len(toResume) > 0 {
		go func(items []PersistedPane) {
			log.Printf("background resume: %d agent pane(s) queued", len(items))
			for _, info := range items {
				resumed, rerr := mgr.ResumeAgentPane(info)
				if rerr != nil {
					conversationID := persistedPaneConversationID(info)
					log.Printf("resume %s pane %s conversation %s: %v", effectiveAgentKind(info.Agent), info.ID, conversationID, rerr)
				}
				if resumed != nil {
					if err := paneStore.Put(resumed); err != nil {
						log.Printf("persist resumed pane %s: %v", resumed.ID, err)
					}
					conversationID := persistedPaneConversationID(info)
					log.Printf("resumed %s pane %s → %s (conversation %s, agent pid %d)", effectiveAgentKind(info.Agent), resumed.ID, resumed.Title, conversationID, resumed.Pid)
					continue
				}
				// Failed restore remains durable and retryable across another reboot.
				info.RecoveryError = "Could not restore this session"
				if rerr != nil {
					info.RecoveryError = rerr.Error()
				}
				if sp := mgr.SuspendExisting(&info); sp != nil {
					if err := paneStore.Put(sp); err != nil {
						log.Printf("persist failed recovery pane %s: %v", info.ID, err)
					}
				}
			}
		}(toResume)
	}

	bus := newUIBus()
	// Let the manager emit terminal-signal events (cmd.exit,
	// build.failed) from readLoop once a pane's process exits.
	mgr.bus = bus

	ports := NewPortRegistry(mgr)
	// A pane's dev server coming up is a moment, not just a table row:
	// publish it so every connected client can toast "Preview :5173".
	// Set before Start — the scan goroutine reads this field.
	ports.onPaneServerUp = func(p Port) {
		title := p.Process
		if pane, ok := mgr.Get(p.PaneID); ok {
			snapshot := pane.Snapshot()
			paneTitle := snapshot.TitleOverride
			if paneTitle == "" {
				paneTitle = snapshot.Title
			}
			if paneTitle != "" {
				title += " · " + paneTitle
			}
		}
		bus.publish(uiEvent{
			Type:   "port-up",
			Port:   &p,
			URL:    localhostURLForPort(&p),
			Text:   title,
			PaneID: p.PaneID,
			Ts:     time.Now().UnixMilli(),
		})
	}
	// 5s interval: the scan does an expensive /proc/*/fd readlink walk, and
	// ports rarely change sub-second, so a 2s cadence was paying that cost
	// 2.5x more often than the registry's freshness needs.
	ports.Start(5 * time.Second)

	// Make sure local `claude` and `codex` sessions can reach our MCP
	// tools — add the registration if it's missing, repair it if it points
	// at a bind address. Own goroutines: they shell out to the agent CLIs.
	// (Muse has no MCP client — see codex_mcp_register.go.)
	// Team-only engines (workflow tasks, release train, managed-agent
	// triggers, the Aria responder, the Muse probe) start only on team
	// installs. Public beta installs run the terminal app without them.
	// The operator flag decides; it equals the team flag until a cloud
	// grants team without operator (a customer team plan).
	bootTeam, bootTeamSource := bootOperatorVerdict(*dataDir)
	if !bootTeam {
		log.Printf("team engines off for this boot (verdict source %s); workflow tasks, managed agents and the release train are operator-only", bootTeamSource)
	}
	// Public installs get a minimal Codex AGENTS.md block (context packs
	// only), never the workflow delegation brief.
	codexAgentsPublicPolicy.Store(!bootTeam)
	// Opt-in for the machine-wide Claude Code / Codex registration
	// (agent_integration.go). Decide before the registrations below run: an
	// install that already carries them is recorded as "on" with a notice.
	agentIntegration := newAgentIntegrationStore(*dataDir)
	activeAgentIntegration.Store(agentIntegration)
	log.Printf("agent integration: %s", bootAgentIntegration(agentIntegration, *addr, bootTeam, bootTeamSource == "env", legacyAgentRegistrationPresent))
	go ensureClaudeMcpRegistration(*addr)
	go ensureCodexMcpRegistration(*addr)
	scheds, err := NewScheduleStore(*dataDir, mgr, bus)
	if err != nil {
		log.Fatalf("schedule store: %v", err)
	}

	// Todos was removed: save a one-time, read-only export of the old list.
	runRemovedTodosExport(*dataDir)

	// Agent control plane (docs/agent-control-plane/): deterministic staged
	// engineering workflows. Init failure disables the feature, never boot.
	_, durableInstallIDErr := loadOrCreateInstallID(*dataDir)
	if durableInstallIDErr != nil {
		// Updater cohorting continues with installCohortID's per-run key, but
		// native browser ownership cannot: the error is retained on the workflow
		// engine and surfaces only if native acceptance is requested.
		log.Printf("install identity unavailable: %v", durableInstallIDErr)
	}
	// Optional features' boot steps (extensions.go) run at the phases below;
	// boot.api and boot.updater are filled in once they exist.
	boot := &bootEnv{
		dataDir: *dataDir, addr: *addr, backendID: *backendID, version: version, bootTeam: bootTeam,
		mgr: mgr, scheds: scheds, bus: bus,
	}
	defer boot.exit()
	// One invocation boundary is shared by workflows, managed agents and
	// automatic release activation.
	restartAdmission := newRestartAdmissionBarrier()
	boot.restartAdmission = restartAdmission
	runBootSteps(bootEarly, boot)

	// Warden settings (opt-in fleet caretaker — DEFAULT OFF). A load failure
	// is non-fatal: fall back to defaults (disabled) rather than refuse boot.
	wardenStore, err := newWardenSettingsStore(*dataDir)
	if err != nil {
		log.Printf("warden: settings load failed (%v) — starting disabled", err)
		wardenStore = &wardenSettingsStore{path: filepath.Join(*dataDir, "warden_settings.json"), s: wardenSettings{}}
	}

	metrics := NewMetrics()
	scheds.SetMetrics(metrics)
	scheds.SetBackendID(*backendID)
	updater := NewUpdater(version, *shellVersion, *shellPlatform, bus, metrics)
	// Stable per-install id for the staged-rollout cohort gate (independent of
	// cloud pairing so local-only daemons participate). Persisted under dataDir
	// by loadOrCreateInstallID above; unlike browser ownership, the cohort key
	// never fails closed to "" and keeps grandfathered legacy values verbatim.
	updater.machineID = installCohortID(*dataDir)
	updater.rollback = rollbackStore
	updater.dataDir = *dataDir
	rollbackStore.RecordRan(version, time.Now())
	reconcileRollbackAttemptAtBoot(rollbackStore, version, time.Now())
	// Did an installer overwrite us with an older binary since our last run?
	// If so this arms a machine-local required-update floor that undoes the
	// downgrade on the next check (see updater_downgrade.go).
	updater.DetectExternalDowngrade()
	// Server-side update-safety gate: a non-forced Apply is refused (409) while
	// any Focus/native pane is mid-turn — the Focus turn's control loop lives in
	// this process, so a swap would degrade it, while terminal turns ride out a
	// swap untouched. Same predicate family as the DTO's agentMidTurn, so the
	// client's defer-while-busy watcher and this gate agree by construction.
	updater.busyCheck = func() (bool, string, string) {
		for _, p := range mgr.List() {
			if p.Kind == "native" && p.IsAgentWorking() {
				return true, "a Focus pane is mid-turn (pane " + p.ID + ")", p.ID
			}
		}
		return false, "", ""
	}
	// Drain workflow model invocations before a non-forced swap. The gate is
	// two-phase: WINDOW-ARMED pauses new admissions while draining between
	// Apply attempts (the updater defers after 10m and reopens admission), then LEASE-HELD
	// keeps them paused through the whole apply attempt — download, pre-swap
	// re-check, and swap — so a momentary zero-inflight can never reopen
	// admission mid-download. force=true bypasses the gate for an explicit
	// immediate update.
	// Auto-apply must be daemon-driven as well as client-driven. In Dual mode
	// the webview talks to only the selected backend, so the inactive sibling
	// otherwise detects releases but can remain old indefinitely.
	updater.autoApplyWhenIdle = updateSettings.AutoApplyWhenIdle

	api := &apiServer{
		closed:             newClosedPaneStore(*dataDir),
		harnessAutoInstall: true,
		mgr:                mgr,
		ws:                 ws,
		ports:              ports,
		panes:              paneStore,
		uiBus:              bus,
		attention:          attention,
		recentFiles:        newRecentFilesRing(),
		scheds:             scheds,
		updater:            updater,
		preview:            newPreviewChannel(*dataDir),
		metrics:            metrics,
		recoverySettings:   recoverySettings,
		nativeSettings:     nativeSettings,
		updateSettings:     updateSettings,
		rollback:           rollbackStore,
		mcpScopes:          newMcpScopeStore(),
		wardenSettings:     wardenStore,
		startT:             time.Now(),
		ver:                version,
		addr:               *addr,
		backendID:          *backendID,
		shellVersion:       *shellVersion,
		shellPlatform:      *shellPlatform,
		dataDir:            *dataDir,
	}
	boot.api, boot.updater = api, updater
	runBootSteps(bootServer, boot)
	mgr.SetCredentialSelection(api.credentialAccount)
	// The browser decision bridge resolves the TypeSafe key from the runtime
	// secrets (secrets.go); a boot step may point it at a shared store.
	api.jevBridge = newJevBridge(*dataDir, func() secretResolver { return openRuntimeSecrets(*dataDir) }, log.Default())
	api.modelGateway = newModelGateway(*dataDir, log.Default())
	api.modelGateway.harnessStore = api.harnessStore
	api.modelGateway.harnessScope = api.harnessScope
	runBootSteps(bootStores, boot)
	// Agents in `digest` notification mode ride the attention digest the user
	// already gets, rather than a second periodic push (§6.3).
	// Debug-only (GET /api/debug/fleet); never on a poll path. Inventory stays
	// scoped to this Manager.
	fleetExecutable, fleetExecutableErr := os.Executable()
	if fleetExecutableErr != nil {
		log.Printf("fleet inventory: executable discovery failed: %v", fleetExecutableErr)
	}
	api.fleet = newFleetBinaryInspector(fleetExecutable, version, mgr)
	// Nudge dismissals (docs/DESIGN-agents-surface.md §7.1 rule 3). Unlike the
	// agent store this one does NOT fail boot: the worst a missing dismissals
	// file costs is a strip somebody already waved off coming back, which is a
	// far cheaper failure than a daemon that will not start.

	runBootSteps(bootRelease, boot)
	runBootSteps(bootReadiness, boot)
	// Live-exit parity for managed terminal harnesses: when restart recovery is
	// "auto", a DeepSeek/Pi client that exits on its own (crash, or an update
	// kill) is resumed in place instead of parking the Resume card, matching the
	// shell-fallback behavior of the Claude/Codex/Muse panes. A clean /exit is
	// left alone, and a rolling budget bounds a crash-on-start loop.
	{
		prevExit := mgr.onPaneExit
		mgr.onPaneExit = func(p *Pane) {
			if prevExit != nil {
				prevExit(p)
			}
			go api.autoResumeHarnessPane(p)
		}
	}
	api.storage = newHostStorageService(*dataDir, api.resources)
	coordLog.setDir(*dataDir)
	api.originAliases = newPaneOriginAliases(*dataDir)

	// Re-arm deferred-switch watchers for panes whose armed "switch when Claude
	// finishes" survived the restart in panes.json (AttachExisting restored the
	// entry onto the live pane; the watcher goroutine could only be created once
	// the API server existed). SuspendExisting never restores one, so every hit
	// here is a live reattached pane.
	for _, p := range mgr.List() {
		if p.pendingSwitchSnapshot() != nil {
			log.Printf("switch-mode: re-armed deferred switch after restart (pane=%s)", p.ID)
			api.ensureSwitchWatcher(p)
		}
	}
	// In-app vs out-app session ledger for Claude Code usage capture. Fed by
	// the hook handler (every Claude session fires our global hooks) and read
	// by the transcript scanner, which is started later only when the cloud
	// relay is configured. A load failure is non-fatal — unknown sessions
	// just degrade to out-app.
	ccLedger := newInAppLedger(*dataDir)
	if err := ccLedger.Load(); err != nil {
		log.Printf("cc-usage: load in-app ledger: %v (starting fresh)", err)
	}
	api.ccLedger = ccLedger
	// ARIA autonomous auto-approver. cloudBase/cloudToken get populated
	// below when --relay is active; until then the cautious-mode LLM
	// path is unavailable (always-mode direct fires need no cloud).
	{
		aria, err := NewAria(mgr, bus, ws, ports, "", "", *dataDir, metrics)
		if err != nil {
			log.Fatalf("aria: %v", err)
		}
		api.aria = aria
		// Cloud smart approvals follow the cached account verdict. The cache
		// is created later in boot, so read it through an atomic reference;
		// until then (or until the cloud answers) the relay's refusal decides.
		aria.jevCloudEntitled = jevCloudEntitledFromCache
		// Non-team installs answer "Approve with caution" with Jev alone.
		aria.teamFeatures = teamFeaturesFromCache
		// Late-bind the attention engine ↔ ARIA both ways: the engine reads
		// standing orders + the pause/cap gate when classifying, and ARIA's
		// eval-outcome hook refreshes the engine the moment an auto-eval
		// declines (instant needs-user with the decline reason attached).
		attention.SetAria(aria)
		aria.attention = attention
		// Extend the existing pane-exit hook so ARIA also drops its
		// per-pane map entries when a pane goes away. Without this the
		// sweepStreaks / autoPending / inFlightEvals maps keep dead
		// pane ids forever; over days of resume cycles that's a slow
		// memory leak.
		prevExit := mgr.onPaneExit
		mgr.onPaneExit = func(p *Pane) {
			if prevExit != nil {
				prevExit(p)
			}
			aria.OnPaneExit(p.ID)
		}
	}

	// Each provider CLI's own login; a credential manager registered for
	// bootAccounts replaces it with its store.
	api.useCLILogins()
	runBootSteps(bootAccounts, boot)
	runBootSteps(bootCredentials, boot)

	// Refresh the embedded harness launcher assets on every boot and self-heal
	// the managed `deepseek`/`pi`/`pi-glm`/`zcode` shortcuts. The web UI only
	// re-extracts them when the user clicks Run/Install in Credentials, so a
	// daemon self-update would otherwise leave the on-disk client at the
	// PREVIOUS release; the shortcut self-heal also means a user's shell command
	// always runs the current client without a repo checkout or `--install`.
	// Best-effort: this writes small embedded files (atomically) and must never
	// block boot.
	api.ensureHarnessCommands()
	// Let the resume/restart paths rebuild a managed harness launch (the user
	// typed `deepseek` in a shell) without holding the apiServer.
	managedHarnessLaunchCmd = api.managedHarnessLaunchCmd

	mux := http.NewServeMux()
	api.routes(mux)

	// Diagnostics: /debug/pprof/* heap/goroutine/cpu profiles. Gated by
	// the same authGate as the rest of the API (see srv.Handler below),
	// so the daemon's bearer token is required — not exposed to anyone
	// who can reach :8787. Without this we cannot diagnose OOM episodes
	// (last incident: failed systemd unit, 22.5 GB peak, no journal,
	// no profile data). The side-effect import above registers the
	// handlers on http.DefaultServeMux; we delegate the prefix here.
	mux.Handle("/debug/pprof/", http.DefaultServeMux)

	// Static web assets (embedded build, or a live local dir via the dev
	// override). The override is resolved through env/marker files as well as
	// the flag, because the desktop app launches BOTH daemons (WSL +
	// Windows-native) with no CLI args — and the UI shell is served by the
	// Windows one, so a developer needs to point both at a live web/dist
	// without rebuilding either binary.
	if devDir := resolveDevWebDir(*devWeb, *dataDir); devDir != "" {
		log.Printf("web assets: serving %s (dev override; embedded dist bypassed)", devDir)
		mux.Handle("/", spaFileServer(http.Dir(devDir)))
	} else {
		sub, err := fs.Sub(webFS, "webdist")
		if err == nil {
			mux.Handle("/", spaFileServer(http.FS(sub)))
		}
	}

	token, err := loadOrCreateToken(*dataDir)
	if err != nil {
		log.Fatalf("auth token: %v", err)
	}
	gate := &authGate{token: token}
	api.gate = gate
	api.entitlements = newTeamEntitlements(*dataDir, api.cloudCreds)
	jevEntitlementsRef.Store(api.entitlements)
	api.view = newPersonalView(*dataDir)
	api.entitlements.onChange = api.publishEntitlementsChanged
	// The update channel follows the account (update_channel.go). Wired
	// after the entitlements cache so the first decision sees the cached
	// account verdict.
	updater.channelStore = newUpdateChannelStore(*dataDir)
	updater.channelEvidence = func() channelEvidence {
		return entitlementChannelEvidence(api.entitlements)
	}
	// A public build has no team update path of its own: it uses the one the
	// paired cloud names in its team answer, for that cloud only
	// (team_updates.go). A team build keeps its stamp.
	updater.SetCloudTeamUpdates(api.entitlements.cloudTeamUpdatesPath)
	api.entitlements.credsChanged = api.cloudCredsChangedCh
	api.agentAuthority = newAgentAuthority(*dataDir)
	api.trustedDevices = newTrustedDevices(*dataDir)
	// Reminder names already mirrored to the cloud are replaced as soon as
	// approved-only is turned on (schedules_reminders.go reminderPayload).
	api.trustedDevices.onPolicyChange = scheds.scheduleReminderSync
	api.trustedDevices.daemonKey = func() *daemonIdentity {
		id, err := loadDaemonIdentity(*dataDir)
		if err != nil {
			log.Printf("daemon identity: %v", err)
			return nil
		}
		return id
	}
	api.trustedDevices.identity = func() (string, string) {
		machineID := ""
		if c, err := loadClaimSafe(*dataDir); err == nil && c != nil {
			machineID = c.MachineID
		}
		return machineID, *backendID
	}
	api.privacy = newPrivacySettings(*dataDir, func() bool {
		team, _ := api.entitlements.verdict()
		return team
	})
	activePrivacy.Store(api.privacy)
	api.naming = newPaneNaming(*dataDir)
	autoNamer := api.maybeAutoNamePane
	paneAutoNamer.Store(&autoNamer)
	workflowHandler := api.teamFeatureGate(mux)
	shinoqueTokens := &shinoqueTokenCache{}
	api.shinoqueTokens = shinoqueTokens

	srv := &http.Server{
		Addr:              *addr,
		Handler:           denyLocalShinoque(gate.middleware(maxRequestBodyMiddleware(defaultJSONBodyBytes, logMiddleware(workflowHandler)))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	boot.ctx = ctx
	go api.entitlements.run(ctx)
	if bootTeamSource != "env" {
		go settleAgentIntegrationVerdict(ctx, agentIntegration, *addr, cachedOperatorVerdictFresh, legacyAgentRegistrationPresent)
	}
	api.checkin = newInstallCheckin(*dataDir, version, api.checkinCloudBase, api.checkinSignedIn)
	go api.checkin.run(ctx)
	previewMgr, previewErr := newPreviewManager(*dataDir, *backendID, func() (string, string) {
		return api.cloudCreds()
	}, filepath.Join(*dataDir, "preview-cache"))
	if previewErr != nil {
		log.Printf("preview registry load failed: %v", previewErr)
	}
	api.previews = previewMgr

	go previewMgr.run(ctx)

	runBootSteps(bootLoops, boot)
	// Poll-independent Codex identity sweep: the /api/panes poll performs this
	// scan only while a client is connected, so a Codex session running with
	// every window closed would never get its thread id adopted — and after a
	// teardown its pane would be dropped as an unidentified shell. 30s matches
	// the adoption latency a backgrounded client already tolerates while
	// keeping the /proc walk (one tree snapshot per sweep) negligible.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepCodexIdentities(mgr)
			}
		}
	}()

	// The relay supervisor is the SOLE owner of the relay tunnel lifecycle.
	// It is constructed (and started) unconditionally whenever --relay is set,
	// so a machine paired AFTER boot connects live with no restart. It owns
	// dialing/redialing; the cloud-side integrations below (pusher, cc-usage,
	// rotation poller, ARIA creds) are wired separately, exactly once, the
	// first time a usable machine token is observed. Voice/push/ARIA consult
	// the supervisor's LIVE state (api.cloudSup.connected()) instead of the
	// boot-time relayURL flag, so they come up the moment pairing happens.
	// Inside a private relay channel the device is already authenticated by
	// the handshake, so its requests skip the per-request device gate.
	api.privateRelayInner = markRelayTransport(logMiddleware(newShinoqueRelayHandler(workflowHandler, shinoqueTokens)))
	sup := newRelaySupervisor(ctx, *dataDir, *relayURL, *backendID, markRelayTransport(api.relayDeviceGate(logMiddleware(newShinoqueRelayHandler(workflowHandler, shinoqueTokens)))))
	api.cloudSup = sup
	// Revokes this computer in the cloud after Disconnect, and resumes any
	// revoke an earlier run couldn't deliver (cloud_revoke.go).
	api.cloudRevoker = newCloudRevoker(*dataDir)
	go api.cloudRevoker.run(ctx)
	// Feed the relay handshake the daemon's CURRENT shell/installer version +
	// platform (params sv/sp) so the cloud can report a per-machine installer
	// version. Read live each handshake — liveShellVersion() prefers the
	// updater's fresh shell-info.json over the boot --shell-version flag.
	liveShellInfo = func() (string, string) {
		return api.liveShellVersion(), api.shellPlatform
	}

	// Server-driven pairing manager (POST /api/cloud/pair/start + /cancel). It
	// writes claim.json on a successful pair; the supervisor above OWNS dialing
	// and observes claim.json, so on success we only nudge it to reconnect
	// without waiting a full poll interval. The manager NEVER dials the relay
	// itself — that decoupling preserves the exactly-one-tunnel invariant. nil
	// relayURL yields a manager whose start() reports "not configured".
	// ctx is the daemon's root context — it parents the pairing poll goroutines
	// so they survive the short-lived /pair/start request (net/http cancels the
	// request ctx on response write) yet still die with the daemon.
	api.pairing = newPairingManager(ctx, *relayURL, *dataDir, sup.signalTokenChanged)

	// Probe for the Claude Code CLI (+ auth) so GET /api/health can guide
	// install/auth instead of letting a Claude pane dead-end with "command not
	// found". Boot probe + a slow self-healing ticker (catches a manual install
	// without a daemon restart). Detection spawns a login shell, so it runs HERE
	// (off the health hot path) and the result is cached — see claude_bootstrap.go.
	// The Muse Code CLI probe (muse_bootstrap.go) rides the same loop so both
	// harnesses stay consistent; each probe coalesces onto its own login shell.
	go func() {
		api.refreshClaudeStatus()
		if bootTeam {
			api.refreshMuseStatus()
		}
		// Boot warm-up retry. A successor daemon's FIRST probe frequently hits the
		// 8s timeout on a COLD login shell — it sources nvm/asdf/pyenv and resolves
		// `claude --version` (a node cold-start) over a PATH stuffed with /mnt/c
		// Windows dirs on the slow 9p filesystem. A timed-out probe is a
		// claudeProbeError and no longer overwrites the cached verdict (see
		// claude_bootstrap.go), so it can't surface a spurious "Claude isn't
		// installed" banner anymore — but on a FRESH boot there is no prior
		// verdict yet (checked=false, UI stuck on "checking…"), so re-probe on a
		// short backoff until Claude resolves (the filesystem warms within
		// seconds, so a later probe succeeds); if it's genuinely absent this is
		// just a few harmless extra probes before the steady-state ticker.
		// refreshClaudeStatus coalesces concurrent probes, so this never races
		// the health-driven recheck into a shell storm.
		for _, backoff := range []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second, 45 * time.Second} {
			if api.currentClaudeStatus().installed {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			api.refreshClaudeStatus()
		}
		// Steady-state self-heal, with a cadence that ADAPTS to the cached verdict.
		// While we believe claude is NOT installed (installed==false) — the state a
		// spurious false-absent gets stuck in, and also the state right after a user
		// installs claude in a pane — re-probe briskly so a wrong "not installed"
		// (or a genuine just-happened install) heals within ~1 min instead of up to
		// 5. While an absence is SUSPECTED but unconfirmed (a definitive absent
		// arrived on a confirmed install — likely a Claude self-update window),
		// re-probe brisker still so the hysteresis converges fast either way. Once
		// installed is confirmed, back off to the cheap heartbeat that only needs
		// to catch a later uninstall. Bounded either way: refreshClaudeStatus
		// coalesces onto one login shell, so this is at most one probe per interval.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(api.claudeProbeInterval()):
				api.refreshClaudeStatus()
			}
		}
	}()

	// Muse Code CLI probe loop — the parallel of the Claude loop above
	// (muse_bootstrap.go). Separate goroutine so each harness keeps its own
	// adaptive cadence; each probe coalesces onto its own login shell.
	// Muse is a team-only harness: public installs skip the login-shell probe.
	go func() {
		if !bootTeam {
			return
		}
		for _, backoff := range []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second, 45 * time.Second} {
			if api.currentMuseStatus().installed {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			api.refreshMuseStatus()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(api.museProbeInterval()):
				api.refreshMuseStatus()
			}
		}
	}()

	// Codex CLI probe loop (codex_bootstrap.go): the setup checklist's Codex
	// row. Same boot warm-up and adaptive cadence as Claude, on its own
	// goroutine and its own coalesced login shell.
	go func() {
		api.refreshCodexStatus()
		for _, backoff := range []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second, 45 * time.Second} {
			if api.currentCodexStatus().installed {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			api.refreshCodexStatus()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(api.codexProbeInterval()):
				api.refreshCodexStatus()
			}
		}
	}()

	// startCloudIntegrations wires the HTTPS-side cloud features (push, usage
	// capture, ARIA creds, token-rotation poller) for a known machine token.
	// Idempotent + token-keyed: it runs once per distinct token. The relay
	// TUNNEL itself is NOT started here anymore — the supervisor owns that.
	//
	// Token rotation no longer bounces the relay: it only writes the new token
	// to claim.json (UpdateClaimToken) and nudges the supervisor, which
	// observes the change and redials EXACTLY ONCE. That is what eliminates the
	// double-dial. The rotation callback here only swaps in-memory token copies
	// for the HTTPS-side features.
	// cloudIntegrationsToken is the idempotency guard: the HTTPS-side wiring
	// (pusher / scanner / rotation poller) is one-shot per distinct token. It's
	// touched from TWO goroutines — the supervisor's onCreds (which calls
	// startCloudIntegrations) and the rotation callback — so it lives behind its
	// own mutex. claimCloudToken atomically reports whether THIS token is new and,
	// if so, records it; setCloudIntegrationsToken records a rotated token so the
	// supervisor's follow-up onCreds(newToken) is a no-op (the pusher/scanner are
	// already live; only the in-memory token copies needed swapping).
	var cloudIntTokMu sync.Mutex
	cloudIntegrationsToken := ""
	// cloudIntCancel cancels the child context that owns the current wiring's
	// background goroutines (pusher / usage scanner / rotation poller). Recreated
	// per distinct token in startCloudIntegrations; cancelled by
	// stopCloudIntegrations on disconnect. Guarded by cloudIntTokMu (only the
	// supervisor goroutine touches it via onCreds/onCredsLost, but the rotation
	// callback shares the mutex for cloudIntegrationsToken).
	var cloudIntCancel context.CancelFunc
	// cloudIntPusher is the current wiring's pusher, so a disconnect can
	// disable it even where another component still holds a reference.
	var cloudIntPusher *Pusher
	// cloudIntRevoked holds tokens the user disconnected. A supervisor
	// reconcile that read claim.json just before Disconnect deleted it must
	// not wire them back up.
	cloudIntRevoked := map[string]bool{}
	reporters := cloudReporters{
		api: api, updater: updater, crash: globalCrashSink,
	}
	claimCloudToken := func(tok string) bool {
		cloudIntTokMu.Lock()
		defer cloudIntTokMu.Unlock()
		if tok == "" || tok == cloudIntegrationsToken || cloudIntRevoked[tok] {
			return false
		}
		cloudIntegrationsToken = tok
		return true
	}
	setCloudIntegrationsToken := func(tok string) {
		cloudIntTokMu.Lock()
		cloudIntegrationsToken = tok
		cloudIntTokMu.Unlock()
	}
	startCloudIntegrations := func(relayToken string) {
		if !claimCloudToken(relayToken) {
			return
		}
		cloudBase := httpsBaseFromRelayURL(*relayURL)
		if cloudBase == "" {
			return
		}
		// Each distinct token's background goroutines run under their own
		// cancelable child context so a later disconnect (onCredsLost →
		// stopCloudIntegrations) tears THEM down without killing the daemon.
		// claimCloudToken already proved this token is new, so cancel any prior
		// wiring before replacing the cancel func (defensive — covers a re-pair
		// to a different account without an intervening disconnect).
		cloudIntCtx, cancel := context.WithCancel(ctx)
		cloudIntTokMu.Lock()
		if cloudIntegrationsToken != relayToken {
			// A disconnect landed since claimCloudToken: don't wire.
			cloudIntTokMu.Unlock()
			cancel()
			return
		}
		if cloudIntCancel != nil {
			cloudIntCancel()
		}
		cloudIntCancel = cancel
		pusher := NewPusher(cloudBase, relayToken, mgr, ws, metrics)
		pusher.private = func() bool { return api.trustedDevices.policy() == remoteAccessApproved }
		cloudIntPusher = pusher
		cloudIntTokMu.Unlock()
		// Atomically publish base+token+pusher to the live cloud state read by
		// the HTTP handlers (voice / cc-usage / ack / status).
		api.setCloudCreds(cloudBase, relayToken, pusher)
		// Schedules push schedule-error / schedule-fired through the same relay.
		scheds.SetPusher(pusher)
		// Optional features push through it too (extensions.go).
		runCloudPusherHooks(api, pusher)
		// Give ARIA the same cloud creds so its tool-use loop can reach
		// /api/aria/complete. Reusing the machine token means no new
		// credential to manage.
		api.aria.setCloudCreds(cloudBase, relayToken)
		go pusher.Start(cloudIntCtx)
		// One-shot: if we just came up as the successor of an auto-update,
		// post a "from→to ok" report so the cloud sees per-machine
		// update-success rate. No-op when not in successor mode.
		startUpdateOutcomeReporter(cloudIntCtx, cloudBase, relayToken, version)
		// Let the updater post an update-FAILURE outcome if a successor bricks,
		// and let the crash sink ship redacted panics — both reach the cloud only
		// once we have creds, and follow token rotation below.
		updater.SetCloudCreds(cloudBase, relayToken)
		updater.startFleetStatusReporter(cloudIntCtx)
		globalCrashSink.setCreds(cloudBase, relayToken)
		// Now that the sink has creds, file our predecessor's death (no-op
		// unless the watchdog handed us evidence at boot).
		go reportWatchdogRespawn(watchdogEvidence)
		// Claude Code usage capture: scans transcript JSONL incrementally and
		// POSTs billed turns to the cloud (HMAC-signed).
		go startCCUsageScanner(cloudIntCtx, *dataDir, cloudBase, relayToken, ccLedger)
		// OpenAI Codex usage capture: same pipeline for Codex CLI rollouts
		// (provider="codex" rows), labelled from the saved Codex logins.
		go startCodexUsageScanner(cloudIntCtx, *dataDir, cloudBase, relayToken, ccLedger, api.codexLogin)
		// A disconnect that landed while the creds above were being handed
		// out cancelled cloudIntCtx; clear again so none of them survive it.
		if cloudIntCtx.Err() != nil {
			reporters.clear(pusher)
			return
		}
		log.Printf("push: state-change notifications enabled → %s", cloudBase)
		if voiceBuiltIn {
			log.Printf("voice: cloud-proxied transcription enabled → %s (no local GROQ_API_KEY needed)", cloudBase)
		}
		log.Printf("aria: commander agent enabled → %s", cloudBase)

		// Token rotation poller (Mode A): when the cloud signals a pending
		// rotation, swap the daemon's in-memory token + persist to claim.json
		// (UpdateClaimToken, inside the poller) + confirm to the cloud. It does
		// NOT bounce the relay — that's the supervisor's job. We nudge the
		// supervisor so it picks up the new token from claim.json immediately.
		startTokenRotationPoller(cloudIntCtx, cloudBase, relayToken, func(newToken string) {
			// A rotation finishing after Disconnect must not hand the
			// cleared reporters a fresh token.
			if cloudIntCtx.Err() != nil {
				return
			}
			// Swap the token everywhere it lives, under the cloud locks.
			api.rotateCloudToken(newToken)
			setCloudIntegrationsToken(newToken)
			updater.SetCloudCreds(cloudBase, newToken)
			globalCrashSink.setCreds(cloudBase, newToken)
			if api.aria != nil {
				api.aria.setCloudToken(newToken)
			}
			if pusher != nil {
				pusher.setToken(newToken)
			}
			if cloudIntCtx.Err() != nil {
				reporters.clear(pusher) // disconnected mid-swap
				return
			}
			// Hand off to the SOLE relay owner: the poller already wrote the
			// new token to claim.json; signal so the supervisor re-reads it and
			// redials exactly once (debounced on the token value).
			sup.signalTokenChanged()
		})
	}

	// stopCloudIntegrations tears the HTTPS-side wiring down when the machine is
	// disconnected. Disconnect calls it directly, before it returns, and the
	// supervisor calls it again on the having→none edge (claim.json deleted or
	// corrupt); it is idempotent. It cancels the pusher / usage scanners /
	// fleet-status and update-outcome reporters / rotation poller (which also
	// closes the rotation resurrection race — a cancelled poller can't recreate
	// claim.json) and clears the token from every reporter (cloudReporters), so
	// crash and update reports stop too. Resets cloudIntegrationsToken so a
	// later re-pair wires from scratch. Safe when nothing was wired.
	stopCloudIntegrations := func() {
		cloudIntTokMu.Lock()
		if cloudIntCancel != nil {
			cloudIntCancel()
			cloudIntCancel = nil
		}
		pusher := cloudIntPusher
		cloudIntPusher = nil
		wasWired := cloudIntegrationsToken != ""
		cloudIntegrationsToken = ""
		cloudIntTokMu.Unlock()
		reporters.clear(pusher)
		if wasWired {
			log.Printf("cloud: machine disconnected — HTTPS-side integrations torn down")
		}
	}
	// Disconnect: never wire this token again in this process, then stop.
	api.onCloudDisconnect = func(token string) {
		if token != "" {
			cloudIntTokMu.Lock()
			cloudIntRevoked[token] = true
			cloudIntTokMu.Unlock()
		}
		stopCloudIntegrations()
	}

	// Drive cloud-integration wiring from the supervisor's live view of the
	// token. onCreds fires whenever the supervisor first sees a (new) usable
	// token — at boot if claim.json already exists, or the instant a post-boot
	// pairing writes it. Idempotent per token via cloudIntegrationsToken.
	// onCredsLost fires on the having→none edge (disconnect) to tear it down.
	sup.onCreds = startCloudIntegrations
	sup.onCredsLost = stopCloudIntegrations

	// Normal startup binds the local listener before cloud relay or background
	// integrations. A daemon that fails to own its local port must not keep
	// running as a hidden cloud-only duplicate; that split-brain was hard to
	// diagnose because local traffic hit one process while cloud traffic could
	// register another.
	//
	// Update successors are the exception. Their predecessor is still holding
	// the local port for a short handoff window, and cloud users otherwise lose
	// the relay until the port becomes bindable. The successor has already
	// loaded state and can serve the same mux over yamux, so connect the cloud
	// relay first, then keep retrying the local bind below.
	if os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") == "1" {
		// Eagerly wire HTTPS-side integrations + start the supervisor so the
		// relay reconnects before the local bind during the handoff window.
		// The supervisor also (re)fires onCreds; this eager call just avoids
		// waiting on the first reconcile when a token is already known.
		if *relayToken != "" {
			startCloudIntegrations(*relayToken)
		}
		go sup.run()
	}

	// Commit the successor handoff only after listener preflight, then acquire
	// exclusive ownership before the potentially slow workflow rehydration.
	ln, err := startupHandoffListener(daemonListenerNetwork(*addr, isInsideWSL()), *addr)
	if err != nil {
		// A bind failure means another process holds the port. Make the log
		// line actionable (identify the squatter via /api/health) instead of
		// an opaque "address already in use".
		log.Print(describeListenConflict(*addr, err))
		os.Exit(1)
	}
	claimDataDirAfterHandoff(*dataDir, dataDirOwner)
	log.Printf("soluterminal daemon %s listening on http://%s (data=%s)", version, *addr, *dataDir)
	if endpoint, endpointErr := jevBridgeEndpointForListener(ln.Addr()); endpointErr != nil {
		log.Printf("jev bridge: listener address unavailable — optional bridge disabled")
	} else {
		if bridgeErr := api.jevBridge.Start(endpoint); bridgeErr != nil {
			log.Printf("jev bridge: initial reconciliation unavailable — optional bridge disabled until retry")
		}
	}
	if endpoint, endpointErr := jevBridgeEndpointForListener(ln.Addr()); endpointErr == nil {
		endpoint = strings.TrimSuffix(endpoint, jevBridgePath) + modelGatewayPrefix
		if err := api.modelGateway.Start(endpoint); err != nil {
			log.Printf("model gateway: initial reconciliation unavailable")
		}
	}

	// MCP registration happens in ensureClaudeMcpRegistration (goroutine
	// above) — the single writer, loopback-normalized. Do not add a direct
	// write here: a raw *addr write is how LAN-bound daemons kept breaking
	// their own registration to http://0.0.0.0:8787/mcp.
	// Register our PostToolUse hook so every Claude Code tool call
	// (Edit / Write / Read / Bash / …) POSTs to the daemon and shows
	// up in the Activity feed. Opt-out via SOLU_SKIP_HOOK_REGISTER=1.
	ensureClaudeHooksRegistered(*addr)

	// Self-update: poll the GitHub Releases manifest every 30 seconds and
	// publish "update-available" on the bus when a newer build appears.
	// No-op for `dev` builds so `go run` iterations don't spam the
	// endpoint.
	updater.StartBackgroundPoll(ctx)

	// Activity watcher publishes pane-state transitions on the uiBus so
	// the web's Activity feed shows "busy → idle after 8m" style rows.
	// Unconditional (unlike Pusher, which is --relay-gated) so a local-
	// only user still gets the feed.
	go startActivityWatcher(ctx, mgr, bus, api.refreshPaneAITitleCache)

	// Focus retirement: move native panes back to the terminal renderer once
	// each one's current turn settles (focus_retire.go).
	if !nativeRendererEnabled() {
		go api.runFocusRetirement(ctx)
	}

	// Turn tracker: per-turn git snapshots for terminal agent panes (turns.go)
	// behind the pane header's "what changed this turn" chip and timers.
	// SOLU_DISABLE_TURN_SNAPSHOTS=1 turns it off.
	if os.Getenv("SOLU_DISABLE_TURN_SNAPSHOTS") != "1" {
		turns := newTurnTracker(*dataDir, mgr)
		activeTurnTracker.Store(turns)
		go turns.run(ctx)
	}

	// Delegation nudger: flushes queued origin-pane status lines the moment
	// their pane goes idle. Ticks cheaply (a pane with an empty queue costs
	// nothing) and is woken directly by the engine's sink.

	// Attention engine ticker: the backstop that notices auto-pending
	// escalation and terminal-pane prompt transitions. Native approval
	// park/resolve and eval declines refresh eagerly via hooks, so their
	// latency doesn't ride this 2s cadence.
	go attention.Start(ctx)

	// Warden summoner: the opt-in fleet caretaker. Always ticks (cheap) but
	// spawns nothing until warden_settings.enabled is set, so this is inert
	// for every install that hasn't opted in. Uses the same scoped-token
	// store the /mcp handler enforces against.
	warden := NewWarden(wardenStore, mgr, api.mcpScopes, bus, *dataDir, *addr)
	warden.panes = api.panes
	api.warden = warden
	go warden.Start(ctx)

	// Long-running-command watcher: emits a single cmd.long_running
	// uiBus event when a pane stays busy >30s without ever reaching
	// awaiting-input, so the aria-watcher can surface slow commands.
	go startLongRunningWatcher(ctx, mgr, bus)

	// ARIA observation watcher: surfaces "pane X is waiting on input"
	// cards in the ARIA panel so the user sees prompts from non-focused
	// panes without having to tab through. Notify-only in this phase;
	// auto-reply layers on top later once the signal is proven.
	go startAriaWatcher(ctx, mgr, bus, api.aria)

	// Optional heap-growth instrumentation for the pane-refresh feature: logs each
	// pane's Claude/Codex descendant VmRSS/VmHWM on a slow timer. See
	// rss_sampler.go; the sampler is default-on with an environment kill-switch.
	startRSSSampler(ctx, mgr)

	memCfg := daemonMemGuardConfig(*dataDir)
	api.system = startSystemSampler(ctx, systemSamplerDeps{
		dataDir: *dataDir,
		clock:   time.Now,
		listPanes: func() []systemPaneRef {
			return systemPaneRefs(mgr)
		},
		listWorkers: func() []systemWorkerProcess {
			{
				return nil
			}

		},
		memAvailKB: memAvailableKB, memTotalKB: readMemTotalKB,
		diskUsage: systemDiskUsage, selfMemMB: osSelfRSSMB, goHeapMB: goLiveHeapMB,
		guardUsedMB: selfMemMB, goRuntimeMB: goRuntimeMB,
		pressure: func() pressureSample {
			return pressureSample{}
		},
		hostMem: func() hostMemSample {
			return hostMemSample{Status: "n/a"}
		},
		memGuardMB:  memCfg.softMB,
		diskFloorMB: func() int { return resolveDiskFloorMBOverride(api.resourceOverrides().DiskFloorMB).Value },
		memFloorMB:  func() int { return resolveMemFloorMB(api.resourceOverrides().MemFloorMB).Value },
		hostDisk: func() *systemHostDiskSample {
			if api.storage == nil {
				return nil
			}
			return systemHostDiskFromPressure(api.storage.diskPressure(context.Background(), "generic"))
		},
		repoPaths: func() []string {
			{
				return nil
			}

		},
		scanInventory: func(ctx context.Context, req storageScanRequest) storageInventory {
			return ScanStorageInventory(ctx, req)
		},
		onInventory: func(inv storageInventory) {
			{
				return
			}

		},
		decorate: api.decorateSystemSample,
	})
	if api.system == nil {
		log.Printf("[system] sampler disabled")
	} else {
		log.Printf("[system] sampler enabled (fast 15s, tree 60s)")
	}
	memCfg.restartFn = func() error { return restartSelfForMemGuard(version, os.Exit) }
	if memCfg.softMB == 0 {
		log.Printf("[memguard] disabled role=daemon (SOLU_MEM_GUARD_MB=0)")
	} else {
		log.Printf("[memguard] enabled role=daemon threshold=%dMB (SOLU_MEM_GUARD_MB; 0 disables)", memCfg.softMB)
		if limit, ok := applyMemGuardHeapLimit(memCfg.softMB); ok {
			log.Printf("[memguard] GC soft memory limit %dMB (%d%% of threshold; GOMEMLIMIT overrides)", limit>>20, memGuardHeapLimitPercent)
		}
	}
	startMemGuard(ctx, memCfg)

	runBootSteps(bootOwned, boot)

	// Workflow drivers start only once this process owns the local port.
	// A twin that loses the bind race exits before reaching here, so it can
	// never drive — and never write — tasks the winner is already running.
	// (2026-08-08: a twin during a restart wrote an invocation completion
	// whose start event the winner never had, and the orphaned event made
	// that task fail to load.)

	// Preview boot reconciliation (adopt expiry, orphan cloud revokes, task
	// projection sync) runs only once this process owns the local port — the
	// same rule as workflow drivers above: it revokes cloud rows and writes
	// task events, which a losing twin must never do. Cloud creds may not be
	// paired yet; unresolved revokes are retried by previewMgr.sweep.
	{
		go api.reconcilePreviewsAtBoot(ctx)
		// Host disk upkeep the retired engine's janitor used to drive: the
		// growth ledger, opt-in crash-dump rotation and the launcher policy.
		if api.storage != nil {
			go api.storage.runSampler(ctx, 5*time.Minute)
		}
	}

	go func() {
		log.Printf("auth: local (loopback) use needs no token")
		// Logs end up in problem reports, so they name where the token is
		// kept rather than carrying it.
		for _, url := range remoteLoginURLs(*addr) {
			log.Printf("auth: remote login → %s?token=<%s>", url, filepath.Join(*dataDir, "token"))
		}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Start the relay supervisor (the SOLE owner of the relay tunnel). It
	// re-reads claim.json on a poll, dials whenever a valid token appears or
	// changes, guarantees exactly one tunnel per backendID, and surfaces live
	// state at GET /api/cloud/status. Relay requests skip the local authGate
	// because the tunnel itself is already authenticated by the machine token,
	// and downstream user-level auth is handled by the cloud. Already started
	// above in the update-successor path; guard so we never run two loops.
	if os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") != "1" {
		go sup.run()
	}

	<-ctx.Done()
	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scheds.Stop()
	if api.jevBridge != nil {
		if err := api.jevBridge.Close(); err != nil {
			log.Printf("jev bridge: shutdown cleanup unavailable")
		}
	}
	_ = srv.Shutdown(shutCtx)
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".solu"
	}
	return filepath.Join(home, ".solu")
}

// remoteLoginURLs returns the addresses a user can open from another
// device, without the token. We discover every non-loopback IP on the
// machine so the right URL is printed for LAN, Tailscale, etc.
func remoteLoginURLs(bindAddr string) []string {
	_, portStr, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		host := ipn.IP.String()
		if ipn.IP.To4() == nil {
			host = "[" + host + "]"
		}
		out = append(out, "http://"+host+":"+portStr+"/")
	}
	return out
}

// spaFileServer serves files from fsys, falling back to /index.html for
// unknown paths so client-side routing works.
//
// Cache policy is load-bearing: without it, the embedded webdist is served with
// NO Cache-Control (embed.FS has no modtime, so http.FileServer emits no
// validators either), and a WebView2 / browser then HEURISTICALLY caches the app
// shell — index.html keeps pointing at the OLD content-hashed chunks even after
// the daemon auto-updates its bundle, so shipped web features silently never
// appear (the recurring "I'm not getting the update" bug). The version-mismatch
// self-heal can't beat it: window.location.reload() just re-serves the cached
// shell, and its one-shot guard then gives up. So:
//   - /assets/* are content-hash-named → immutable, cache hard.
//   - everything else (the shell: /, index.html, sw.js, manifest, icons) →
//     no-store, so every load fetches a FRESH index.html → the current chunks.
func spaFileServer(fsys http.FileSystem) http.Handler {
	fs := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setStaticCacheHeaders(w, r.URL.Path)
		// let the API handlers take their own paths
		f, err := fsys.Open(r.URL.Path)
		if err == nil {
			f.Close()
			fs.ServeHTTP(w, r)
			return
		}
		// Provenance is a machine-readable endpoint, never an SPA route. A
		// pre-provenance/dev webdist must return a real miss so clients cannot
		// mistake index.html's 200 response for provenance.
		if r.URL.Path == "/webdist.json" {
			http.NotFound(w, r)
			return
		}
		// No SPA route lives under /api. An unknown API path (or a known one
		// with an unregistered method, which the mux also routes here) must be
		// a real miss: answering index.html with 200 made clients treat it as
		// success — the optional MCP GET stream reconnected every second.
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// fall back to index.html for SPA routes (always a no-store shell)
		setStaticCacheHeaders(w, "/index.html")
		// Clone the URL too: a shallow request copy rewrites the caller's
		// path, making the access log report every SPA fallback as GET /.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		r2.URL.RawPath = ""
		fs.ServeHTTP(w, r2)
	})
}

// setStaticCacheHeaders applies the SPA cache policy for a served path. Hashed
// build assets are immutable; the app shell must never be cached so a fresh
// index.html (pointing at the current chunks) is fetched every load.
func setStaticCacheHeaders(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
}

// resolveDevWebDir resolves the local webdist override used instead of the
// embedded bundle: the --dev-web flag wins, then SOLU_WEB_DEV_DIR, then a
// one-line `dev-web` marker next to the daemon binary or in the data dir.
//
// Why a marker file: the desktop app launches both daemons (WSL + the
// Windows-native one that serves the UI shell) with fixed argv and no
// developer-controlled environment. A marker lets `npm run build` output be
// served live by BOTH daemons — no daemon rebuild, no version bump, no restart
// dance — which is the only way to iterate on the web UI without tripping the
// "which daemon has the new webdist" trap.
func resolveDevWebDir(flagValue, dataDir string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("SOLU_WEB_DEV_DIR")); v != "" {
		return v
	}
	markerDirs := make([]string, 0, 2)
	if exe, err := os.Executable(); err == nil {
		markerDirs = append(markerDirs, filepath.Dir(exe))
	}
	if strings.TrimSpace(dataDir) != "" {
		markerDirs = append(markerDirs, dataDir)
	}
	for _, dir := range markerDirs {
		if v := readDevWebMarker(filepath.Join(dir, "dev-web")); v != "" {
			return v
		}
	}
	return ""
}

// readDevWebMarker reads the first non-empty, non-comment line of a dev-web
// marker file. Missing/unreadable files simply mean "no override".
func readDevWebMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack forwards to the underlying ResponseWriter so WebSocket upgrades work
// through this middleware.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Flush forwards to the underlying ResponseWriter for streaming responses.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// quietPollPaths are endpoints the frontend polls every few seconds.
// Logging every successful hit drowns out real events (spawns, kills,
// errors). Non-2xx still logs so failures stay visible.
var quietPollPaths = map[string]bool{
	"/api/health":     true,
	"/api/panes":      true,
	"/api/ports":      true,
	"/api/workspaces": true,
}

func logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		// Suppress high-frequency successful polls.
		if r.Method == "GET" && rec.status < 400 && quietPollPaths[r.URL.Path] {
			return
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}

func maxRequestBodyMiddleware(limit int64, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.ContentLength != 0 {
			r.Body = http.MaxBytesReader(w, r.Body, requestBodyLimit(r, limit))
		}
		h.ServeHTTP(w, r)
	})
}
