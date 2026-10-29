package main

// Self-update mechanism for solud. The daemon periodically fetches a
// JSON manifest from GitHub Releases advertising the latest version +
// per-platform binary URLs + SHA-256 hashes. When the advertised
// version is newer than our embedded build-time version, the web UI
// surfaces an "Update available" prompt. On user confirm, the daemon
// downloads the new binary, verifies the hash, atomically swaps it for
// the currently-running binary on disk, then hands off to a successor it
// spawns itself and exits with ExitCodeRestartForUpdate once that
// successor reports a clean boot (see Apply in updater_apply.go).
//
// Who restarts a daemon that exits WITHOUT arranging a successor — i.e.
// one that crashed — depends on the backend, and this comment used to
// claim (wrongly) that the Tauri shell always did. It does now, but only
// because it was taught to: host-native is watched by the shell's
// watchdog (desktop/src-tauri/src/daemon.rs), WSL by the wait/PIDFILE
// loop in its start script, and a Linux install by systemd's
// Restart=always. Before that watchdog existed, a crashed host-native
// daemon simply stayed dead until the user restarted the app.
//
// Why not Tauri's official updater plugin: the web bundle lives INSIDE
// solud via //go:embed, and the Tauri shell is ~500 lines that barely
// ever changes. Shipping a full MSI for every web/daemon iteration is
// wasted bytes + ceremony. This updater swaps just the daemon (which
// carries the web bundle) — the Tauri shell stays untouched.
//
// Manifest signing: newer builds can enforce an Ed25519 detached
// signature over the raw update.json bytes. The public key is baked by
// publish.sh via -ldflags or supplied by SOLU_UPDATE_MANIFEST_PUBKEY;
// the private key stays outside the repo in SOLU_UPDATE_SIGNING_PRIVATE_KEY.
// If no public key is configured, the updater keeps the legacy HTTPS +
// SHA-256 behavior for dev/private installs.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Exit code used by Apply() after the successor daemon has been
// spawned. We exit clean (0) so the Tauri shell doesn't think the
// sidecar crashed — the new daemon process is what serves traffic
// going forward, joined to the same Job Object on Windows so Tauri's
// shutdown still cleans it up.
const ExitCodeRestartForUpdate = 0

// Default manifest URL. Points at the PUBLIC `soluterminal-releases`
// repo (not the private source repo) so the daemon can fetch
// anonymously — GitHub Releases asset downloads on private repos
// require a token, which we don't ship in the binary. Source stays
// private; only the compiled binaries + update manifest are public.
//
// Uses GitHub Releases' "latest" alias so the URL never has to know
// the current version — whatever the most recent non-prerelease tag
// in soluterminal-releases is, its update.json is at this path.
//
// The alias is only ever used to RESOLVE the current version, though:
// GitHub serves latest/download/<asset> through per-asset redirect caches
// that can briefly point at DIFFERENT releases right after a publish
// (observed shipping v0.0.987: update.json still redirected to v0.0.986
// while update.json.sig already redirected to v0.0.987). Verified bytes
// are always fetched from the immutable /releases/download/v<version>/
// URLs — see fetchManifest in updater_manifest.go.
//
// The public channel's owner/repo comes from shared/releases/repo.txt.
// Before the public repo moves to repo.txt's owner, a 404 falls back to the
// legacy owner (getReleaseAsset). The team channel reads the team build's
// releases through the cloud instead (team_updates.go); it never falls back.

// defaultUpdateManifestPubKey is the base64 raw Ed25519 public key used
// to verify update.json.sig. It can be overridden at build time with
// -ldflags "-X main.defaultUpdateManifestPubKey=<base64 raw ed25519 pubkey>".
// Empty means "do not enforce signed manifests".
var defaultUpdateManifestPubKey = "sm+MMrCMET4jQkfdIHySZ7RJWHLSV22qcBqKUVeU5k4="

// requireSignedManifest is stamped "1" by release builds via
// -ldflags "-X main.requireSignedManifest=1" (see scripts/release-ldflags.sh).
// When "1", the updater REFUSES to apply any manifest/control file that isn't
// validly signed: a blank/missing public key hard-fails instead of degrading to
// checksum-only. Dev builds (`go run`) and tests leave it "" so unsigned local
// manifests keep working.
var requireSignedManifest = ""

func mustVerifySignatures() bool { return requireSignedManifest == "1" }

// How often the background poll runs after the initial startup check.
// 30s makes published updates feel near-real-time (max 30s lag between
// `./publish.sh` finishing and every running install showing the
// badge). Manifest fetch is ~1KB hitting GitHub's CDN, so per-user
// bandwidth is negligible (~5MB/day) and there's no per-fetch cost.
const pollInterval = 30 * time.Second

const (
	defaultWorkflowDrainWindow = 10 * time.Minute
	quietBackoffBase           = 2 * time.Minute
	quietBackoffMax            = 30 * time.Minute
	// A release-train publish can run for up to 90 minutes and cannot be
	// drained. Retry its defer on a short fixed cadence instead of arming the
	// invocation drain window or escalating the quiet backoff.
	publishDeferRetry = 2 * time.Minute
)

// maxTransientFailures is how many consecutive transient check failures
// (GitHub CDN 5xx, network blips) the updater swallows before surfacing
// the error to the UI badge. At pollInterval=30s this is ~1.5 min of
// sustained trouble before the user sees anything — long enough to ride
// out a CDN hiccup, short enough to still report a genuine outage.
const maxTransientFailures = 3

// Manifest is the JSON document served from GitHub Releases. Kept
// permissive on unknown fields so we can add metadata (release notes,
// min-upgrade-from version, etc.) without breaking old clients.
type Manifest struct {
	Version   string                      `json:"version"`
	Platforms map[string]ManifestPlatform `json:"platforms"`
	Notes     string                      `json:"notes,omitempty"`
	// Flavor is the build this release is for: "public" or "team". It is
	// inside the signed bytes, and the updater refuses a manifest whose
	// flavour is missing or is not its own (update_flavor.go). Clients from
	// before flavours (0.0.1224 and older) ignore it.
	Flavor string `json:"flavor,omitempty"`
	// Source cryptographically attributes release artifacts to the private
	// source repository. It is inside the signed manifest bytes; old clients
	// ignore it and old manifests decode with Source nil.
	Source *ManifestSource `json:"source,omitempty"`
	// Shell is OPTIONAL. Present only when a release also ships a new
	// Tauri installer (NSIS / future MSI / DMG / AppImage). The daemon
	// auto-update channel above swaps just solud; the shell channel
	// here lets the running app prompt the user to download + run a
	// new installer for the rare Tauri-shell rebuilds. Old clients
	// that don't know about Shell will simply ignore it.
	Shell *ManifestShell `json:"shell,omitempty"`
}

type ManifestSource struct {
	Sha        string           `json:"sha"`
	BuiltAt    string           `json:"builtAt,omitempty"`
	Repo       string           `json:"repo,omitempty"`
	CutID      string           `json:"cutId,omitempty"`
	CutVersion string           `json:"cutVersion,omitempty"`
	Waivers    []ManifestWaiver `json:"waivers,omitempty"`
	Bypass     string           `json:"bypass,omitempty"`
}

type ManifestWaiver struct {
	TaskID string `json:"taskId"`
	Reason string `json:"reason"`
}

type ManifestPlatform struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// ManifestShell describes installer artifacts. By the validated build
// convention, Version identifies both the Tauri shell and the daemon bundled
// inside that installer: build-sidecar-binaries.sh stamps both from
// tauri.conf.json. On a daemon-only release publish.sh intentionally carries
// the preceding shell entry forward, so Manifest.Version may be newer than
// Shell.Version.
// Platforms keys match platformKeyCurrent() (e.g. "windows-amd64") so the same
// lookup logic works for both channels.
type ManifestShell struct {
	Version   string                      `json:"version"`
	Platforms map[string]ManifestPlatform `json:"platforms"`
	Notes     string                      `json:"notes,omitempty"`
}

// ShellLaunchState records the last shell-installer launch attempt. A failed
// state deliberately remains visible until the next attempt so the web UI can
// keep showing the staged installer path after the transient toast expires.
type ShellLaunchState struct {
	State       string `json:"state,omitempty"`
	StagedPath  string `json:"stagedPath,omitempty"`
	Method      string `json:"method,omitempty"`
	PID         string `json:"pid,omitempty"`
	Session     string `json:"session,omitempty"`
	Error       string `json:"error,omitempty"`
	UpdatedAtMs int64  `json:"updatedAtMs,omitempty"`
}

// UpdateStatus is what /api/update/status returns to the web UI. The
// badge component renders off this shape; keep it stable.
type UpdateStatus struct {
	Current       string `json:"current"`
	Latest        string `json:"latest,omitempty"`
	Available     bool   `json:"available"`
	LastCheckedMs int64  `json:"lastCheckedMs,omitempty"`
	// LastSuccessMs is when a check last SUCCEEDED. LastCheckedMs advances on
	// every attempt (including failures) and Latest/Notes are last-known-good
	// cache — so without this field a machine that hasn't reached the update
	// server in days still renders as "Up to date · checked 30s ago". The UI
	// keys its "can't reach update server since X" state off this. Zero =
	// never succeeded this daemon lifetime.
	LastSuccessMs    int64  `json:"lastSuccessMs,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	Notes            string `json:"notes,omitempty"`
	SourceSha        string `json:"sourceSha,omitempty"`
	SourceProvenance string `json:"sourceProvenance,omitempty"`
	// Applying is true only after download and all final safety checks admit
	// the binary swap. Pre-admission restart serialization lives in restartGate
	// and must not be exposed as a client handoff signal.
	Applying  bool      `json:"applying"`
	CheckedAt time.Time `json:"-"`

	// User-driven rollback and exact-version update hold. This shape mirrors
	// web/src/types.ts; zero values keep older clients backward-compatible.
	HoldVersion    string `json:"holdVersion,omitempty"`
	HoldReason     string `json:"holdReason,omitempty"`
	HoldExpiresMs  int64  `json:"holdExpiresMs,omitempty"`
	RollbackState  string `json:"rollbackState,omitempty"`
	RollbackTarget string `json:"rollbackTarget,omitempty"`
	RollbackError  string `json:"rollbackError,omitempty"`

	WorkflowDeferred        bool   `json:"workflowDeferred,omitempty"`
	WorkflowDeferReason     string `json:"workflowDeferReason,omitempty"`
	WorkflowDeferSinceMs    int64  `json:"workflowDeferSinceMs,omitempty"`
	WorkflowDeferDeadlineMs int64  `json:"workflowDeferDeadlineMs,omitempty"`
	WorkflowInflight        int    `json:"workflowInflight,omitempty"`
	WorkflowDeferAttempts   int    `json:"workflowDeferAttempts,omitempty"`
	WorkflowNextRetryMs     int64  `json:"workflowNextRetryMs,omitempty"`
	WorkflowDeferState      string `json:"workflowDeferState,omitempty"`

	// Tauri-shell update channel. Empty/false when the running build
	// didn't pass --shell-version (older daemons launched outside the
	// Tauri shell, or the shell didn't plumb the flag yet) — the web
	// UI hides the shell badge in that case.
	ShellCurrent   string            `json:"shellCurrent,omitempty"`
	ShellLatest    string            `json:"shellLatest,omitempty"`
	ShellAvailable bool              `json:"shellAvailable"`
	ShellApplying  bool              `json:"shellApplying"`
	ShellLaunch    *ShellLaunchState `json:"shellLaunch,omitempty"`
	// ShellCarriesLatestDaemon is true when Shell.Version (which also identifies
	// the bundled daemon) is at least Manifest.Version. It is false for an
	// intentional daemon-only publish that carries the last shell installer
	// forward. Installer admission must compare Shell.Version with the actually
	// installed versions; this manifest relationship is informational only.
	ShellCarriesLatestDaemon bool `json:"shellCarriesLatestDaemon,omitempty"`

	// Mandatory-minimum enforcement, driven by the signed control file.
	// Required: this daemon is below min_daemon_version — the web UI reacts by
	// force-enabling the "install when idle" auto-apply (even for "Notify me"
	// users) and raising a blocking modal if the update can't proceed.
	// ShellRequired: the desktop installer is below min_shell_version — the UI
	// blocks new work behind a "Restart & update" modal (the installer channel
	// can't self-apply). Both fail OPEN: no control file, or control
	// unreachable past the stale TTL, clears them — a CDN outage must never
	// lock users out of their own terminal.
	Required            bool   `json:"required"`
	RequiredReason      string `json:"requiredReason,omitempty"`
	ShellRequired       bool   `json:"shellRequired"`
	ShellRequiredReason string `json:"shellRequiredReason,omitempty"`
	// ShellAutoApplyPaused: the signed control file (or this channel's default
	// when it says nothing) has the unattended installer restart off. The web
	// then never arms its countdown and shows the ordinary installer banner;
	// apply-shell?silent=1 is refused. Clicked updates are unaffected.
	ShellAutoApplyPaused       bool   `json:"shellAutoApplyPaused"`
	ShellAutoApplyPausedReason string `json:"shellAutoApplyPausedReason,omitempty"`

	// UpdateChannel is the build this install follows: "public" or "team"
	// (update_channel.go). Empty until the first decision is known.
	UpdateChannel string `json:"updateChannel,omitempty"`
}

type Updater struct {
	manifestURL string
	// manifestURLOverride is true when SOLU_UPDATE_MANIFEST_URL set
	// manifestURL; a channel switch then keeps the URL (the flavour check
	// still applies).
	manifestURLOverride bool
	// teamViaCloud is set while the install follows the team channel, which
	// is read through the paired cloud (team_updates.go): manifestURL is then
	// unused and every URL derives from the cloud credentials.
	teamViaCloud bool
	// channelWaiting is a stored channel this build has no source for yet
	// (logged once per wait, syncChannel in update_channel.go).
	channelWaiting string
	// channel is the install's update channel (update_channel.go): every
	// manifest the updater trusts must be signed for it. It starts as the
	// build's initial channel and follows channelStore once main wires it.
	// Guarded by mu; tests may set it directly before use.
	channel string
	// channelStore and channelEvidence are wired by main after the
	// entitlements cache exists. Until then the stored decision (or the
	// initial channel) is used and nothing is decided or persisted.
	channelStore    *updateChannelStore
	channelEvidence func() channelEvidence
	current         string
	shellCurrent    string
	shellPlatform   string
	httpClient      *http.Client
	metrics         *Metrics // optional
	rollback        *updateRollbackStore
	dataDir         string

	// downloadIdleTimeout aborts a binary download only after this long with
	// no bytes received; downloadMaxDuration is the absolute ceiling for one
	// download. Both default in NewUpdater. They exist because the
	// manifest/control client's fixed total timeout cut a ~43MB release off
	// mid-body on any link slower than ~17 Mbps (see updater_download.go).
	downloadIdleTimeout time.Duration
	downloadMaxDuration time.Duration

	// restartGate is the process-wide successor-launch reservation shared with
	// the memory guard. Production uses selfRestartInProgress; tests that call
	// Apply must inject a per-test gate as required by
	// docs/ai-workflow/daemon-test-memory.md.
	restartGate *atomic.Bool
	// executable resolves the daemon binary that Apply replaces. Production
	// uses os.Executable; parallel Apply tests inject per-test paths so their
	// download and swap artifacts cannot collide.
	executable func() (string, error)
	// Binary handoff seams are nil in production and injected by disposable
	// updater tests so no real process is replaced or launched.
	swapBinaryFn        func(string, string) error
	spawnSuccessorFn    func(string, string, string) (*os.Process, error)
	waitSuccessorBootFn func(string, time.Duration) bool
	// killSuccessorFn is nil in production, where Apply calls Process.Kill.
	// Tests inject it so rollback coverage never signals an arbitrary PID.
	killSuccessorFn func(*os.Process) error
	commitSleepFn   func(time.Duration)

	// homeDir is where ~/.solu/shell-info.json is read from. Defaults to
	// os.UserHomeDir() in NewUpdater; overridable in tests.
	homeDir string

	// machineID is the stable per-install id used as the staged-rollout cohort
	// key. Set by main after NewUpdater (installID(dataDir)); "" in tests, which
	// is harmless because the rollout gate only consults it when rollout_percent
	// < 100. Read under mu.
	machineID string

	// busyCheck, when set (main.go wires it over the pane manager), reports
	// whether applying an update RIGHT NOW would interrupt live agent work —
	// concretely: any Focus/native pane mid-turn. A Focus turn's control loop
	// (pending approvals, stream parse state) lives in THIS daemon process, so
	// a swap mid-turn degrades the turn even though the detached agent + claude
	// child survive; a terminal turn is daemon-independent and never blocks an
	// update. Apply() consults it at entry AND immediately before the binary
	// swap (a pane can go busy during the download); only the user's explicit
	// force=true override bypasses it.
	// The client keeps its own defer-while-busy watcher — this is the
	// server-side backstop for curl/scripts/stale clients/no-open-tab. nil =
	// no gate (tests, minimal setups). Set once at boot, before serving.
	busyCheck func() (busy bool, reason string, paneID string)

	// inflightCount, when set, reports open workflow model invocations. Apply()
	// uses it to drain model workers before a non-forced binary swap. nil = no
	// gate (tests, workflow-disabled boots). Set once at boot before serving.
	inflightCount func() int
	// workflowBootReady reports whether the durable workflow board is fully
	// published. nil means workflow is disabled. Production wires this once at
	// boot; tests use it to hold the asynchronous load deterministically.
	workflowBootReady func() bool
	// inflightIDs supplies engine-internal invocation ids for the durable
	// pre-swap restart-intent audit. It contains no prompt or user content.
	inflightIDs func() []string
	// releasePublishInflight reports the release train whose durable cut has
	// not yet reached a published or failed outcome. nil disables this gate.
	releasePublishInflight func() (trainID string, inflight bool)

	// autoApplyWhenIdle reads the daemon-scoped update preference. Keeping the
	// callback here (rather than a copied bool) means a PUT /api/update/settings
	// takes effect on the next background check without restarting the daemon.
	// The background poll uses it to update headless/inactive dual backends too;
	// previously only the currently-visible web client drove auto-apply.
	autoApplyWhenIdle func() bool

	// onManifestChange is set once at boot by main.go and fired (async, off the
	// lock) from Check() whenever a successful fetch changes the manifest
	// version or source sha. It is the daemon's "a publish happened" signal:
	// release readiness uses it to re-anchor on the freshly published release
	// immediately rather than serving a stale anchor until its own TTL lapses.
	// nil = no listener (tests, minimal setups).
	onManifestChange func()

	// launchShell is a seam for the OS-specific installer dispatcher. Tests use
	// it to exercise the staged/launching/confirmed/failed state machine without
	// opening a real installer.
	launchShell func(path, shellPlatform string, lg *shellLaunchLog) (launchResult, error)
	// launchShellSilent is the unattended counterpart (updater_shell_silent.go):
	// it starts the courier that installs with /S after the app exits.
	launchShellSilent func(path, shellPlatform, sha256 string, lg *shellLaunchLog) (launchResult, error)
	// The last silent courier this daemon started, so a second app window's
	// countdown cannot start another one while the first is waiting. Guarded by mu.
	shellSilentVersion string
	shellSilentPath    string
	shellSilentAt      time.Time
	// The courier's attempt id, until its outcome has been read from the
	// launch log (observeSilentCourierLocked).
	shellSilentAttempt string
	// shellDisplayPath converts a WSL path to its Windows spelling. It is a
	// narrow test seam because process-wide WSL detection cannot be disabled on
	// a WSL test runner whose Windows interop is intentionally unavailable.
	shellDisplayPath func(path, shellPlatform string) (string, error)
	// shellLogf is injectable so admission tests can assert the refusal audit
	// message without mutating the process-global logger.
	shellLogf func(format string, args ...any)

	mu     sync.Mutex
	status UpdateStatus
	// Fleet diagnostics are separate from UI error suppression and check-success time.
	fleetLastCheckError string
	fleetLastSuccessMs  int64
	fleetPollDisabled   string
	// verifiedManifest is the last signature-verified manifest, keyed by its
	// Version: the latest-alias resolve step compares the resolved version
	// against it to skip re-fetching an immutable per-tag pair it already
	// verified. Guarded by mu. It must ONLY ever be written after successful
	// Ed25519 verification (fetchManifestResolveThenPin) — that invariant is
	// what makes serving a poll from this cache signature-safe.
	verifiedManifest *Manifest
	manifest         *Manifest
	// applying is the post-admission daemon handoff signal mirrored by
	// UpdateStatus.Applying. restartGate, not this field, owns pre-admission
	// serialization while workflow gates and downloads are still pending.
	applying          bool
	shellApplying     bool
	shellLaunchLog    *shellLaunchLog
	drainStart        time.Time
	drainInflight     int
	drainWindow       time.Duration
	deferAt           time.Time
	deferInflight     int
	deferAttempts     int
	nextRetryTime     time.Time
	publishDeferTrain string
	publishDeferredAt time.Time
	lastDrainDuration time.Duration
	// leaseHeld/leaseSince are the workflow admission lease: held ONLY
	// inside a single non-forced Apply attempt, from the moment
	// checkWorkflowDrain returns proceed until Apply returns (released by
	// Apply's defer) or the binary swap commits (cleared by
	// clearWorkflowDrainLocked). While held, WorkflowDrainPending stays true
	// through the download and swap so a momentary zero-inflight can never
	// reopen workflow admission mid-apply. Deliberately in-memory only — a
	// daemon crash or restart mid-download must forget it so queued tasks are
	// never starved by a stale lease (matches the drain* fields above, which
	// are also never persisted).
	leaseHeld  bool
	leaseSince time.Time
	// rollbackDrain keeps the same drain window/backoff visible and active
	// when the user is rolling back the currently-latest release. In that
	// topology status.Available is false, so forward-update availability cannot
	// be the admission predicate. It is deliberately in-memory and is cleared
	// on a terminal rollback outcome or daemon restart.
	rollbackDrain bool
	// Local Preview candidates are independent of the published Stable
	// manifest. Keep their bounded drain across Stable's background polls.
	// Bind it to the exact version/hash so a different candidate starts fresh.
	localDrainCandidateID string
	// swapped is set once Apply has successfully swapped the binary and
	// scheduled this process's exit. Unlike the post-admission `applying` signal
	// (reset by Apply's defer the moment it returns), swapped is TERMINAL: it
	// closes the ~600ms window
	// between Apply returning and os.Exit firing, during which a second client
	// (another device, or a page that reloaded and lost its client-side
	// once-per-version guard) could otherwise start a redundant download and
	// double swap. Failure paths (rollback after a bad successor) never set it,
	// so retries stay possible.
	swapped bool

	// control is the most recently fetched update control file (kill switch /
	// staged rollout); controlFetchedAt is the time of the last SUCCESSFUL fetch
	// (used by the stale-cache fail-safe). Both guarded by mu.
	control          *UpdateControl
	controlFetchedAt time.Time

	// ctrlEffective is the control as last EVALUATED by Check() — i.e. the
	// post-fallback value (the cached copy during a transient blip, the
	// synthesized pause once the cache is stale, nil when no control is
	// published). recomputeShellLocked derives ShellRequired from it so a
	// just-installed shell drops the required flag on the very next Status()
	// read instead of waiting out the 30s Check tick. Guarded by mu.
	ctrlEffective *UpdateControl

	// cloudBase/cloudToken let Apply() POST an update-FAILURE outcome (the
	// successor bricked) to the cloud — the predecessor is still alive and can
	// report the casualty the dead successor can't. Set once cloud integrations
	// come up and refreshed on token rotation. Separate mutex so Apply never
	// blocks on mu while reporting.
	cloudCredsMu sync.Mutex
	cloudBase    string
	cloudToken   string
	// cloudTeamUpdates is SetCloudTeamUpdates' seam (nil: stamp only).
	cloudTeamUpdates func(cloudBase string) string

	// localFloor is a machine-local min_daemon_version armed at boot when the
	// running binary is OLDER than its own version marker — i.e. something
	// outside the updater (the NSIS installer) downgraded us. See
	// updater_downgrade.go for the full story. Guarded by mu.
	localFloor string

	// transientFailures counts consecutive transient Check failures (see
	// maxTransientFailures). Reset to 0 on any success or non-transient
	// error. Guarded by mu.
	transientFailures int

	// Bus for notifying the web UI when a new version is detected —
	// lets the topbar badge appear without the UI having to poll.
	bus *uiBus
}

// CurrentShellVersion returns the freshest known Tauri-shell version for
// status/health display. It prefers ~/.solu/shell-info.json (rewritten by the
// desktop app on every startup, so it reflects the *currently installed*
// shell) over the in-memory shellCurrent — which itself started from the
// --shell-version launch flag, a boot snapshot that goes stale when the
// desktop app is upgraded under a long-lived (auto-updating, --shell-version-
// forwarding) daemon. Mirrors the adopt-if-newer rule in Check so /api/health
// never reports a stale flag (the 0.0.485-vs-0.0.621 mismatch that looked like
// a stuck shell but wasn't). Never downgrades to an older file.
func (u *Updater) CurrentShellVersion() string {
	u.mu.Lock()
	cur := u.shellCurrent
	home := u.homeDir
	u.mu.Unlock()
	if home != "" {
		if v, _, _ := readShellInfo(home); v != "" && (cur == "" || isNewer(v, cur)) {
			return v
		}
	}
	return cur
}

// NewUpdater constructs the updater. shellCurrent is the Tauri-shell
// version (passed via --shell-version by the Tauri sidecar launcher);
// shellPlatform is the manifest platform key for the SHELL (e.g.
// windows-amd64) — distinct from the daemon's runtime.GOOS because the
// WSL backend runs a Linux daemon but the shell is still Windows.
// Pass "" for both when solud is run standalone.
func NewUpdater(current, shellCurrent, shellPlatform string, bus *uiBus, metrics *Metrics) *Updater {
	// SOLU_UPDATE_MANIFEST_URL overrides the manifest location (tests, the
	// e2e harness, mirrors). A custom URL containing /releases/latest/download/
	// engages the resolve-then-pin protocol and therefore must ALSO serve the
	// GitHub-style immutable /releases/download/v<version>/update.json{,.sig}
	// pair; query-bearing/tokenized latest aliases are unsupported because the
	// derived immutable asset URLs do not inherit the alias query. Any other
	// custom URL keeps the plain single-fetch path.
	//
	// Without an override the URL is the build flavour's own releases repo.
	// An override may point anywhere, but the signed manifest must still name
	// this build's flavour, so it cannot switch a build to the other one.
	channel := initialUpdateChannel()
	url := os.Getenv("SOLU_UPDATE_MANIFEST_URL")
	override := url != ""
	viaCloud := false
	if !override {
		// Only the stamp is known this early; a path the cloud names is
		// picked up by syncChannel on the first Check.
		url, viaCloud, _ = channelSource(channel, teamUpdatesConfigured())
	}
	// Backward compat: if no explicit shell platform was passed, infer
	// it. Old Tauri shells (pre-v0.0.330) don't pass --shell-platform,
	// and dev launches of `./solud` don't pass --shell-version either;
	// we still want both to see installer updates. Two cases:
	//   1. Running inside WSL — daemon is linux but the shell that
	//      spawned us is necessarily Windows (WSL backend is Windows-
	//      only). Detect via /proc/version or env vars.
	//   2. Otherwise — fall back to the daemon's runtime platform
	//      (host-native install where shell == daemon platform).
	// Cloud Linux daemons land on linux-amd64, which doesn't appear in
	// any installer manifest's shell.platforms — that's the gate that
	// keeps them dark.
	if shellPlatform == "" {
		if runtime.GOOS == "linux" && isInsideWSL() {
			shellPlatform = "windows-amd64"
		} else {
			shellPlatform = platformKeyCurrent()
		}
	}
	home, _ := os.UserHomeDir()
	launchShell := launchInstaller
	launchShellSilent := launchSilentInstaller
	if os.Getenv("SOLU_SHELL_LAUNCH_FORCE_FAIL") == "1" {
		launchShell = func(path, shellPlatform string, lg *shellLaunchLog) (launchResult, error) {
			return launchResult{}, fmt.Errorf("%w: forced by SOLU_SHELL_LAUNCH_FORCE_FAIL", errLaunchNotObserved)
		}
		launchShellSilent = func(path, shellPlatform, _ string, lg *shellLaunchLog) (launchResult, error) {
			return launchShell(path, shellPlatform, lg)
		}
	}
	// ResponseHeaderTimeout bounds a stalled connection before the response
	// headers arrive. The manifest/control client keeps the short total
	// timeout; the binary download deliberately drops it (see download) so a
	// slow-but-steady transfer is bounded by progress, not wall-clock.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	u := &Updater{
		manifestURL:         url,
		manifestURLOverride: override,
		teamViaCloud:        viaCloud,
		channel:             channel,
		current:             current,
		shellCurrent:        shellCurrent,
		shellPlatform:       shellPlatform,
		restartGate:         &selfRestartInProgress,
		executable:          os.Executable,
		homeDir:             home,

		downloadIdleTimeout: defaultUpdateDownloadIdleTimeout,
		downloadMaxDuration: defaultUpdateDownloadMaxDuration,
		status:              UpdateStatus{Current: current, ShellCurrent: shellCurrent, UpdateChannel: channel},
		drainWindow:         defaultWorkflowDrainWindow,
		launchShell:         launchShell,
		launchShellSilent:   launchShellSilent,
		shellDisplayPath:    shellInstallerDisplayPath,
		shellLogf:           log.Printf,
		bus:                 bus,
		metrics:             metrics,
	}
	// The machine token rides only on requests for the paired cloud's team
	// update endpoint (team_updates.go).
	u.httpClient = &http.Client{Transport: &teamUpdatesAuth{next: transport, creds: u.cloudCreds, path: u.teamUpdatesPathFor}, Timeout: 20 * time.Second}
	return u
}

// StartBackgroundPoll kicks off an immediate check (async) + a 30-second
// ticker for the life of ctx. Dev builds (version == "dev") are
// skipped entirely so `go run` iterations don't spam the endpoint.
func (u *Updater) StartBackgroundPoll(ctx context.Context) {
	if u.current == "dev" {
		return
	}
	if u.noUpdateSource() {
		// The team channel in a build without the team update endpoint
		// (a public build): it must never fall back to the public repo.
		u.mu.Lock()
		u.fleetPollDisabled = "no-releases-repo"
		u.mu.Unlock()
		slog.Warn("auto-update disabled", "reason", "this build has no releases repo for its update channel", "channel", u.channelSnapshot())
		return
	}
	// Surface which signature-verification mode we're in so a misbuilt
	// release (no baked-in key, no env override) doesn't silently fall
	// back to HTTPS+SHA256 only. SOLU_UPDATE_REQUIRE_SIGNATURE=1 turns
	// the "no key configured" case from a warning into a hard refusal:
	// auto-update is disabled entirely until a key shows up.
	pub, keyErr := updateManifestPublicKey()
	switch {
	case keyErr != nil:
		u.mu.Lock()
		u.fleetPollDisabled = "signature-key-invalid"
		u.mu.Unlock()
		slog.Error("auto-update manifest public key invalid; refusing to poll", "err", keyErr)
		return
	case pub == nil:
		if mustVerifySignatures() || strings.TrimSpace(os.Getenv("SOLU_UPDATE_REQUIRE_SIGNATURE")) == "1" {
			u.mu.Lock()
			u.fleetPollDisabled = "signature-key-missing"
			u.mu.Unlock()
			slog.Error("auto-update refusing to poll", "reason", "signed manifest required but no manifest public key configured")
			return
		}
		slog.Warn("auto-update running unsigned", "reason", "no manifest public key compiled in or set via SOLU_UPDATE_MANIFEST_PUBKEY", "fallback", "HTTPS+SHA256 only")
	default:
		slog.Info("auto-update verification enabled", "alg", "ed25519", "fingerprint", fmt.Sprintf("%x", pub[:4]))
	}
	go func() {
		checkAndMaybeApply := func() {
			st, err := u.Check(ctx)
			if err != nil {
				return
			}
			enabled := u.autoApplyWhenIdle != nil && u.autoApplyWhenIdle()
			err = runAutoApplyIfEligible(ctx, enabled, st, func(applyCtx context.Context) error {
				return u.Apply(applyCtx, os.Exit, false)
			})
			if err == nil {
				return
			}
			var busy *updateBusyError
			if errors.As(err, &busy) {
				slog.Info("auto-update waiting for idle", "reason", busy.reason)
				return
			}
			var deferred *updateDeferredError
			if errors.As(err, &deferred) {
				slog.Info("auto-update deferred to a later quiet window",
					"inflight", deferred.inflight, "attempts", deferred.attempts, "nextRetryAt", deferred.nextRetryAt)
				return
			}
			slog.Warn("auto-update apply failed", "err", err)
		}

		// Initial check slightly delayed so startup isn't blocked on
		// a network round-trip — the port-bind + webview load should
		// come up first for good perceived latency.
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
		checkAndMaybeApply()
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				checkAndMaybeApply()
			}
		}
	}()
}

// runAutoApplyIfEligible is the policy seam shared by the production poll and
// focused unit tests. Required daemon updates keep the existing fail-safe
// behavior and apply even when the user's optional auto-update preference is
// off. Apply itself still enforces the busy gate, signed control file, rollout
// cohort, checksum, and successor health handshake.
func runAutoApplyIfEligible(
	ctx context.Context,
	autoApplyWhenIdle bool,
	st *UpdateStatus,
	apply func(context.Context) error,
) error {
	if st == nil || !st.Available || st.Applying {
		return nil
	}
	if st.HoldVersion != "" && st.HoldVersion == st.Latest {
		return nil
	}
	if !autoApplyWhenIdle && !st.Required {
		return nil
	}
	if st.WorkflowNextRetryMs > time.Now().UnixMilli() {
		return nil
	}
	return apply(ctx)
}

// Check fetches the manifest, updates internal status, and — if this
// is a transition from "no update" to "update available" — publishes
// a uiEvent so the web UI can light up the badge without having to
// poll /api/update/status itself.
func (u *Updater) Check(ctx context.Context) (*UpdateStatus, error) {
	if u.metrics != nil {
		u.metrics.UpdaterChecksTotal.Add(1)
	}
	u.syncChannel()
	m, err := u.fetchManifest(ctx)
	// Fetch the control file alongside the manifest (no lock held during I/O).
	// Only when the manifest itself succeeded — no manifest, nothing to gate.
	var ctrl *UpdateControl
	var ctrlErr error
	if err == nil {
		ctrl, ctrlErr = u.fetchControl(ctx)
	}
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()

	u.status.CheckedAt = now
	u.status.LastCheckedMs = now.UnixMilli()
	u.fleetLastCheckError = fleetCheckError(err)
	if err == nil && ctrlErr != nil {
		u.fleetLastCheckError = "control"
	}

	if err != nil {
		if u.metrics != nil {
			u.metrics.UpdaterErrorsTotal.Add(1)
		}
		var flavorErr *manifestFlavorError
		if errors.As(err, &flavorErr) && !flavorErr.newer {
			// The channel's repo still serves a release of another flavour
			// that is not newer than this daemon (the one-time migration
			// release, read by installs that already run it). There is
			// nothing to install either way; report up to date, not a
			// failure, and keep any manifest already verified for the
			// channel.
			u.transientFailures = 0
			u.fleetLastCheckError = ""
			u.status.LastError = ""
			u.status.LastSuccessMs = now.UnixMilli()
			if u.manifest == nil {
				u.status.Available = false
			}
			return u.statusCopy(), nil
		}
		if isTransientUpdateErr(err) {
			u.transientFailures++
			// Swallow the first few consecutive transient failures:
			// GitHub's release-assets CDN intermittently 500s and
			// self-heals on the next poll. Keep the last-known-good
			// status (Latest/Notes) visible and leave LastError untouched
			// so the badge doesn't flash a scary error for a problem that
			// isn't the user's.
			if u.transientFailures < maxTransientFailures {
				slog.Info("auto-update transient check failure (suppressed)",
					"err", err, "consecutive", u.transientFailures, "threshold", maxTransientFailures)
				return u.statusCopy(), err
			}
			slog.Warn("auto-update transient failure persisted; surfacing to UI",
				"err", err, "consecutive", u.transientFailures)
		} else {
			// Genuine, non-transient failure (manifest missing, bad
			// signature, parse error) — break the streak and surface now.
			u.transientFailures = 0
		}
		if !isTransientUpdateErr(err) {
			slog.Warn("auto-update check failed", "err", err)
		}
		u.status.LastError = redactUpdateURLs(err.Error())
		return u.statusCopy(), err
	}
	if m.Flavor != u.channel {
		// The channel switched while this check was fetching (a concurrent
		// check ran syncChannel). Drop the result; the next check fetches
		// the new channel's manifest.
		return u.statusCopy(), errors.New("update channel changed during the check; retrying")
	}
	u.status.LastError = ""
	u.status.LastSuccessMs = now.UnixMilli()
	u.transientFailures = 0
	u.manifest = m
	// Effective update control (kill switch / staged rollout). On a failed fetch,
	// ride the last-known-good cache within the stale TTL; past it, fail SAFE
	// (controlFallbackLocked synthesizes a pause). A 404 (no control published)
	// returns a nil control with nil error → permissive.
	if ctrlErr != nil {
		ctrl = u.controlFallbackLocked(ctrlErr)
	} else {
		u.control = ctrl
		u.controlFetchedAt = now
	}
	// Capture the previous Latest BEFORE overwriting so we can detect
	// "a newer version appeared while one was already pending" — see
	// the publish guard below.
	prevLatest := u.status.Latest
	prevSourceSha := u.status.SourceSha
	u.status.Latest = m.Version
	if prevLatest != "" && m.Version != prevLatest && !u.rollbackDrain {
		// Backoff belongs to one release only. A newly observed release starts
		// with a fresh quiet-window opportunity.
		u.deferAt = time.Time{}
		u.deferInflight = 0
		u.deferAttempts = 0
		u.nextRetryTime = time.Time{}
		u.lastDrainDuration = 0
	}
	u.status.Notes = m.Notes
	u.status.SourceSha = ""
	u.status.SourceProvenance = ""
	if m.Source != nil && strings.TrimSpace(m.Source.Sha) != "" {
		u.status.SourceSha = strings.TrimSpace(m.Source.Sha)
		u.status.SourceProvenance = "exact"
	}
	// A changed version or source sha means a release was published since our
	// last successful fetch — tell whoever is listening (release readiness) so
	// it can re-anchor now. Firing on the first successful fetch after boot
	// (prevLatest "") is intended: a daemon that just self-updated re-anchors
	// promptly instead of serving the previous release's anchor. Goroutine
	// because u.mu is held until this function returns.
	if (m.Version != prevLatest || u.status.SourceSha != prevSourceSha) && u.onManifestChange != nil {
		go u.onManifestChange()
	}

	wasAvailable := u.status.Available
	available := isNewer(m.Version, u.current) && u.platformKey(m) != ""
	// A halted release (paused / quarantined / outside the rollout cohort) must
	// never even prompt — suppress the badge and surface the reason as Notes.
	if available && ctrl != nil {
		if blocked, reason := ctrl.blocksApply(m.Version, u.current, u.machineID); blocked {
			available = false
			u.status.Notes = reason
		}
	}
	u.populateRollbackStatusLocked(now)
	if u.status.HoldVersion != "" && u.status.HoldVersion == m.Version {
		if ctrl != nil && versionBelowFloor(u.current, ctrl.MinDaemonVer) {
			if u.rollback != nil {
				_ = u.rollback.ClearHold("operator minimum version floor")
			}
			u.status.HoldVersion = ""
			u.status.HoldReason = ""
			u.status.HoldExpiresMs = 0
			slog.Info("update hold released for operator minimum version floor", "version", m.Version)
		} else {
			available = false
			u.status.Notes = fmt.Sprintf("updates to %s are paused until %s: %s", m.Version,
				time.UnixMilli(u.status.HoldExpiresMs).Format(time.RFC3339), u.status.HoldReason)
		}
	}
	u.status.Available = available
	if !available && !u.rollbackDrain && u.localDrainCandidateID == "" {
		// A kill switch, rollout exclusion, or superseding no-update check
		// cancels any old drain lifecycle so a future release starts with a
		// fresh deadline instead of inheriting an expired pass-through.
		u.clearWorkflowDrainLocked()
	}

	// Mandatory-minimum (forced update) enforcement for the daemon channel.
	// Derived from the same effective ctrl as the gate above so the fail-open
	// story matches: no control published → never required; control
	// unreachable past the stale TTL → the synthesized fallback carries no
	// floors → enforcement lifts rather than locking users out on an outage.
	wasRequired := u.status.Required
	u.status.Required = false
	u.status.RequiredReason = ""
	if ctrl != nil && versionBelowFloor(u.current, ctrl.MinDaemonVer) {
		u.status.Required = true
		u.status.RequiredReason = requiredReason("version "+u.current, ctrl.MinDaemonVer, ctrl.Message)
	}
	// Machine-local floor: an installer downgraded this binary beneath the
	// version the updater had left here (updater_downgrade.go). Same Required
	// machinery — the embedded client force-applies even with auto-apply off —
	// but gated on Available: unlike the operator floor, which blocks by
	// design, this must never trap the user behind a modal when there is
	// nothing to apply (offline, halted release). The operator floor's reason
	// wins when both fire — it carries the operator's message.
	if !u.status.Required && u.status.Available && versionBelowFloor(u.current, u.localFloor) {
		u.status.Required = true
		u.status.RequiredReason = "a shell installer downgraded this daemon (it previously ran " +
			u.localFloor + "); updating to restore the newer version"
	}
	// Publish for recomputeShellLocked (which owns the shell-channel floor).
	u.ctrlEffective = ctrl

	// Shell-update channel. Re-read the installed shell version and recompute
	// the installer badge via the shared helper (also used by Status(), so a
	// freshly-installed shell clears the prompt the instant the new app asks
	// for status rather than waiting for the next 30s tick). Capture the prior
	// values first so we can fire the bus event on any change below.
	prevShellLatest := u.status.ShellLatest
	wasShellAvailable := u.status.ShellAvailable
	wasShellRequired := u.status.ShellRequired
	u.recomputeShellLocked()

	// Fire the bus event when:
	//   1. We just transitioned no→yes (first detection of an update), OR
	//   2. An update was already pending and the Latest version changed
	//      (e.g. user ignored 0.0.75 and 0.0.76 shipped; the badge needs
	//      to re-render with the new Latest).
	// The old code only covered (1), which meant back-to-back releases
	// silently updated status.Latest without notifying the UI — the user
	// only saw the new version after Ctrl+R re-bootstrapped the store.
	// Still deliberately NOT firing on every 30s poll while Available
	// stays constant: that would spam the web UI for no benefit.
	//
	// A Required transition (either direction) also fires: the client handler
	// just refetches /api/update/status, which is exactly what's needed to
	// kick the forced auto-apply or raise/drop the blocking modal without
	// waiting for the UI's own slower poll.
	requiredChanged := u.status.Required != wasRequired
	if u.bus != nil && ((available && (!wasAvailable || prevLatest != m.Version)) || requiredChanged) {
		u.bus.publish(uiEvent{Type: "update-available"})
	}
	// Fire on first detection / version bump (badge must light up) AND on the
	// clear transition yes→no (installer applied → badge must disappear). The
	// clear case is what fixes the lingering "install the installer" prompt:
	// without it, a long-lived daemon that just noticed the shell upgraded out
	// from under it would never tell the UI to drop the badge, so the prompt
	// would persist until the next manual refresh. The frontend handler gates
	// its toast on the fresh ShellAvailable, so a clear event shows no toast.
	shellAvailable := u.status.ShellAvailable
	shellChanged := shellAvailable != wasShellAvailable ||
		(shellAvailable && prevShellLatest != u.status.ShellLatest) ||
		u.status.ShellRequired != wasShellRequired
	if shellChanged && u.bus != nil {
		u.bus.publish(uiEvent{Type: "shell-update-available"})
	}
	return u.statusCopy(), nil
}

// recomputeShellLocked re-reads ~/.solu/shell-info.json (adopting a newer
// installed shell version, never downgrading) and recomputes
// ShellLatest/ShellAvailable from the cached manifest. Pure-local — one small
// file read, no network — so it's cheap enough to run on the Status() path as
// well as inside Check(). Caller must hold u.mu.
//
// The file is rewritten by the Tauri shell on every startup, so it reflects the
// *currently installed* shell — fresher than the --shell-version flag, which is
// only a snapshot from whenever this daemon was spawned. Adopt the file version
// when it's newer (a genuine upgrade) or when we have no version at all; never
// let an older/stale file downgrade an explicit flag.
//
// Why Status() needs this: after a desktop installer update the relaunched
// shell rewrites shell-info.json, but the long-lived (systemd-owned) daemon
// that survives the swap only re-reads it on its 30s Check() tick. Without a
// refresh here, the new shell's very first GET /api/update/status returns a
// stale ShellAvailable=true and the "install the installer" prompt lingers for
// up to ~a minute after the new app opens.
func (u *Updater) recomputeShellLocked() {
	if u.homeDir != "" {
		if v, p, _ := readShellInfo(u.homeDir); v != "" {
			if u.shellCurrent == "" || isNewer(v, u.shellCurrent) {
				u.shellCurrent = v
				u.status.ShellCurrent = v
				if p != "" {
					u.shellPlatform = p
				}
			}
		}
	}
	// We treat an empty shellCurrent as "older than anything" so a daemon
	// launched without --shell-version (dev runs of `./solud`, or pre-v0.0.330
	// Tauri shells that didn't plumb the flag) still surfaces the badge whenever
	// a shell binary exists for our inferred platform. Use shellPlatform (the
	// SHELL's OS), not the daemon's runtime platform — under WSL the daemon is
	// linux-amd64 but the shell is windows-amd64, and the manifest is keyed on
	// the latter. Cloud Linux deployments stay dark because their shellPlatform
	// is never present in shell.platforms.
	shellAvailable := false
	shellLatest := ""
	if u.manifest != nil && u.manifest.Shell != nil {
		shellLatest = u.manifest.Shell.Version
		if _, ok := u.manifest.Shell.Platforms[u.shellPlatform]; ok &&
			(u.shellCurrent == "" || isNewer(u.manifest.Shell.Version, u.shellCurrent)) {
			shellAvailable = true
		}
	}
	u.status.ShellLatest = shellLatest
	u.populateRollbackStatusLocked(time.Now())
	if u.status.HoldVersion != "" && u.status.HoldVersion == shellLatest {
		shellAvailable = false
	}
	u.status.ShellAvailable = shellAvailable
	// Does this installer carry the manifest's latest daemon? Shell.Version is
	// also the bundled daemon version by build convention. Daemon-only publishes
	// intentionally make this false; update admission still compares the target
	// with installed versions rather than prescribing a daemon-first sequence.
	u.status.ShellCarriesLatestDaemon = u.manifest != nil && u.manifest.Shell != nil &&
		!isNewer(u.manifest.Version, u.manifest.Shell.Version)

	// Mandatory installer floor (min_shell_version). Re-derived here — not in
	// Check() — because the shell version can change between 30s ticks (the
	// user just ran the installer): the fresh shell-info.json adopted above
	// must drop ShellRequired on the very next Status() read, or the blocking
	// modal lingers up to a minute after a successful update. An empty
	// shellCurrent (standalone/browser daemon) is exempt via versionBelowFloor.
	u.status.ShellRequired = false
	u.status.ShellRequiredReason = ""
	if u.ctrlEffective != nil && versionBelowFloor(u.shellCurrent, u.ctrlEffective.MinShellVer) {
		u.status.ShellRequired = true
		u.status.ShellRequiredReason = requiredReason(
			"the desktop app ("+u.shellCurrent+")", u.ctrlEffective.MinShellVer, u.ctrlEffective.Message)
	}
	u.status.ShellAutoApplyPaused, u.status.ShellAutoApplyPausedReason = u.ctrlEffective.shellAutoApplyPaused(u.channel)
	u.observeSilentCourierLocked()
}

func (u *Updater) populateRollbackStatusLocked(now time.Time) {
	u.status.HoldVersion = ""
	u.status.HoldReason = ""
	u.status.HoldExpiresMs = 0
	u.status.RollbackState = ""
	u.status.RollbackTarget = ""
	u.status.RollbackError = ""
	if u.rollback == nil {
		return
	}
	if hold, ok := u.rollback.ActiveHold(now); ok {
		u.status.HoldVersion = hold.Version
		u.status.HoldReason = hold.Reason
		u.status.HoldExpiresMs = hold.ExpiresAtMs
	}
	if attempt := u.rollback.Snapshot().LastAttempt; attempt != nil {
		state := attempt.State
		if state == "swapped" {
			state = "applying"
		}
		if state == "applying" || state == "completed" || state == "failed" {
			u.status.RollbackState = state
			u.status.RollbackTarget = attempt.TargetVersion
			u.status.RollbackError = attempt.Error
		}
	}
}

// Status returns the cached status without making any network call. The web UI
// hits /api/update/status on mount for the initial badge paint; subsequent
// refreshes come via the bus. It does re-read the local shell-info.json first
// (see recomputeShellLocked) so the installer badge reflects a just-installed
// shell between 30s Check() ticks.
func (u *Updater) Status() *UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	// Refresh the installer badge from the on-disk shell version so the cached
	// status reflects a just-installed shell between 30s Check() ticks (see
	// recomputeShellLocked). Cheap local file read; safe to run on every poll.
	u.recomputeShellLocked()
	return u.statusCopy()
}

// LatestManifestSource returns the exact private-source commit from the last
// successfully checked manifest. Legacy manifests intentionally report ok=false.
// The note explains a degraded answer to the release-readiness payload — a
// fetch that is failing, a manifest not yet fetched, a legacy manifest — so the
// operator sees why the anchor was degraded instead of only a silently older
// number. It accompanies an ok=TRUE answer too: a cached manifest we can no
// longer refresh still anchors, but may already be a release behind.
func (u *Updater) LatestManifestSource() (version, sha, note string, ok bool) {
	if u == nil {
		return "", "", "", false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.manifest == nil {
		switch {
		case u.current == "dev":
			return "", "", "update polling disabled (dev build)", false
		case u.status.LastError != "":
			return "", "", "update manifest fetch failing: " + u.status.LastError, false
		default:
			return "", "", "update manifest not yet fetched", false
		}
	}
	if u.manifest.Source == nil || strings.TrimSpace(u.manifest.Source.Sha) == "" {
		return "", "", "update manifest has no source commit (legacy manifest)", false
	}
	// A manifest we still HAVE but can no longer refresh is the best anchor
	// available (ok stays true) and may already be a release behind: anything
	// published during the outage is invisible to us. Say so, because the
	// alternative is a payload indistinguishable from a healthy one.
	if u.status.LastError != "" {
		return u.manifest.Version, strings.TrimSpace(u.manifest.Source.Sha),
			"anchor from the last good manifest; update manifest fetch failing: " + u.status.LastError, true
	}
	return u.manifest.Version, strings.TrimSpace(u.manifest.Source.Sha), "", true
}

func (u *Updater) statusCopy() *UpdateStatus {
	s := u.status
	s.Applying = u.applying
	s.ShellApplying = u.shellApplying
	if !u.drainStart.IsZero() && (u.status.Available || u.rollbackDrain) && time.Since(u.drainStart) < u.drainWindow {
		s.WorkflowDeferred = true
		s.WorkflowDeferSinceMs = u.drainStart.UnixMilli()
		s.WorkflowDeferDeadlineMs = u.drainStart.Add(u.drainWindow).UnixMilli()
		s.WorkflowInflight = u.drainInflight
		_, _, s.WorkflowDeferReason = drainDecision(time.Now(), u.drainStart, u.drainWindow, u.drainInflight, nil)
		s.WorkflowDeferState = "draining"
	} else if u.publishDeferTrain != "" && !u.nextRetryTime.IsZero() && time.Now().Before(u.nextRetryTime) {
		s.WorkflowDeferred = true
		s.WorkflowDeferReason = fmt.Sprintf("update deferred while release train %s publishes", u.publishDeferTrain)
		s.WorkflowDeferSinceMs = u.publishDeferredAt.UnixMilli()
		s.WorkflowDeferAttempts = u.deferAttempts
		s.WorkflowNextRetryMs = u.nextRetryTime.UnixMilli()
		s.WorkflowDeferState = "publish"
	} else if !u.nextRetryTime.IsZero() && time.Now().Before(u.nextRetryTime) {
		s.WorkflowDeferred = true
		s.WorkflowDeferReason = fmt.Sprintf("update deferred while %d workflow invocations finish", u.deferInflight)
		s.WorkflowDeferSinceMs = u.deferAt.UnixMilli()
		s.WorkflowInflight = u.deferInflight
		s.WorkflowDeferAttempts = u.deferAttempts
		s.WorkflowNextRetryMs = u.nextRetryTime.UnixMilli()
		s.WorkflowDeferState = "backoff"
	}
	return &s
}

// SetCloudCreds publishes the cloud base URL + machine token so Apply() can post
// an update-FAILURE outcome if a successor fails to boot. Called when cloud
// integrations come up and again on every token rotation.
func (u *Updater) SetCloudCreds(base, token string) {
	u.cloudCredsMu.Lock()
	u.cloudBase = base
	u.cloudToken = token
	u.cloudCredsMu.Unlock()
}

func (u *Updater) cloudCreds() (string, string) {
	u.cloudCredsMu.Lock()
	defer u.cloudCredsMu.Unlock()
	return u.cloudBase, u.cloudToken
}

// SetCloudTeamUpdates wires the team update endpoint path the paired cloud
// named in its own team answer (teamEntitlements.cloudTeamUpdatesPath). fn
// gets the paired cloud base and returns "" unless that very cloud named
// the path. A team build ignores it and keeps its stamp.
func (u *Updater) SetCloudTeamUpdates(fn func(cloudBase string) string) {
	u.cloudCredsMu.Lock()
	u.cloudTeamUpdates = fn
	u.cloudCredsMu.Unlock()
}

// teamUpdatesPathFor is the team update endpoint path for the paired cloud
// cloudBase: the team build's stamp, else the path that cloud itself named
// (validated again here), else "".
func (u *Updater) teamUpdatesPathFor(cloudBase string) string {
	if teamUpdatesConfigured() {
		return strings.TrimSpace(teamUpdatesPath)
	}
	u.cloudCredsMu.Lock()
	fn := u.cloudTeamUpdates
	u.cloudCredsMu.Unlock()
	if fn == nil || teamUpdatesOrigin(cloudBase) == "" {
		return ""
	}
	if p := strings.TrimSpace(fn(cloudBase)); validTeamUpdatesPath(p) {
		return p
	}
	return ""
}

// teamUpdatesSource is the paired cloud base and its endpoint path ("" when
// there is none).
func (u *Updater) teamUpdatesSource() (base, path string) {
	base, _ = u.cloudCreds()
	return base, u.teamUpdatesPathFor(base)
}

// teamUpdatesUnavailable says why the team channel has nowhere to ask.
func (u *Updater) teamUpdatesUnavailable() error {
	if base, _ := u.cloudCreds(); strings.TrimSpace(base) == "" {
		return errTeamUpdatesNoCloud
	}
	return errTeamUpdatesNoSource
}

// reportApplyFailureAsync fires a fire-and-forget update-FAILURE report to the
// cloud so a release that bricks successors is VISIBLE across the fleet (the
// signal staged rollout needs — without it every release looks falsely 100%
// healthy because you only ever hear from survivors). No-op on a local-only
// daemon with no cloud creds.
func (u *Updater) reportApplyFailureAsync(outcome string) {
	base, tok := u.cloudCreds()
	if base == "" || tok == "" {
		return
	}
	u.mu.Lock()
	to := ""
	if u.manifest != nil {
		to = u.manifest.Version
	}
	from := u.current
	u.mu.Unlock()
	go reportUpdateOutcome(context.Background(), base, tok, from, to, outcome)
}

// isInsideWSL reports whether this Linux process is running inside
// WSL. Used by the shell-update fallback when an old Tauri shell
// didn't pass --shell-platform: a Linux daemon under WSL implies the
// SHELL is Windows, so the manifest key should be windows-amd64
// rather than the daemon's own linux-amd64.
//
// Cheap and authoritative: WSL_INTEROP / WSL_DISTRO_NAME env vars are
// set by wsl.exe for every shell session it spawns, and /proc/version
// contains "microsoft" / "WSL" on every WSL kernel.
func isInsideWSL() bool {
	if os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	v := strings.ToLower(string(b))
	return strings.Contains(v, "microsoft") || strings.Contains(v, "wsl")
}

// isNewer compares two dot-separated numeric versions. Returns true iff
// candidate > current. Non-numeric segments (e.g. "0.0.49-beta") sort
// after numeric ones at the same depth, so a prerelease tag never
// accidentally downgrades the user below the stable release.
func isNewer(candidate, current string) bool {
	if candidate == "" || current == "" || candidate == current {
		return false
	}
	return compareVersions(candidate, current) > 0
}

func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, aok := segment(as, i)
		bi, bok := segment(bs, i)
		if aok && !bok {
			return 1
		}
		if !aok && bok {
			return -1
		}
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

func segment(parts []string, i int) (int, bool) {
	if i >= len(parts) {
		return 0, false
	}
	// Strip any non-numeric suffix (e.g. "49-beta" → "49") so
	// prereleases still compare sensibly against stable.
	s := parts[i]
	for j, r := range s {
		if r < '0' || r > '9' {
			s = s[:j]
			break
		}
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, true
	}
	return n, true
}

func (u *Updater) WorkflowDrainPending() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.leaseHeld ||
		(!u.drainStart.IsZero() && time.Since(u.drainStart) < u.drainWindow &&
			(u.status.Available || u.rollbackDrain || u.localDrainCandidateID != ""))
}
