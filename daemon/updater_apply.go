package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// selfRestartInProgress serializes every path that can launch a successor.
// In particular, the memory guard and an auto-update must never both fork a
// replacement daemon during the same handoff window.
var selfRestartInProgress atomic.Bool

func describeListenConflict(addr string, listenErr error) string {
	if h := probeListenSquatter(addr); h != nil {
		return fmt.Sprintf("listen: port %s is held by another soluterminal daemon "+
			"(backend=%s version=%s pid=%d) — stop it or switch backend in the tray",
			addr, valueOrUnknown(h.BackendID), valueOrUnknown(h.Version), h.Pid)
	}
	return fmt.Sprintf("listen: cannot bind %s — another process is holding the port "+
		"(stop it or switch backend in the tray): %v", addr, listenErr)
}

// listenSquatterHealth is the subset of GET /api/health we parse to identify
// whoever already owns the port. Tagged to match api.go's health payload.
type listenSquatterHealth struct {
	BackendID string `json:"backendID"`
	Version   string `json:"version"`
	Pid       int    `json:"pid"`
}

// probeListenSquatter does a short-timeout GET /api/health against the
// loopback at addr's port. Returns the parsed identity if a soluterminal
// daemon answers, or nil if nothing answers, the response isn't ours, or it
// times out — in which case the caller falls back to the generic message.
func probeListenSquatter(addr string) *listenSquatterHealth {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return nil
	}
	url := "http://127.0.0.1:" + port + "/api/health"
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var h listenSquatterHealth
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil
	}
	return &h
}

func valueOrUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// updateBusyError is Apply's non-forced refusal while a Focus/native turn must
// settle or critical workflow invocations must drain. The HTTP handler maps it
// to a typed 409 so clients can defer and retry without guessing from prose;
// every other Apply error stays a plain 500.
const (
	updateBusyCauseFocus    = "focus"
	updateBusyCauseWorkflow = "workflow"
	updateBusyCauseRestart  = "restart"
)

type updateBusyError struct {
	cause      string
	reason     string
	paneID     string
	inflight   int
	deadlineMs int64
}

// updateDeferredError is distinct from the short "still draining" busy hold:
// the bounded drain window ended, admission has reopened, and automatic apply
// must wait until nextRetryAt before trying another quiet window.
type updateDeferredError struct {
	inflight       int
	nextRetryAt    time.Time
	attempts       int
	reason         string
	publishTrainID string
}

func (e *updateDeferredError) Error() string {
	if e.publishTrainID != "" {
		return fmt.Sprintf("update deferred: %s (release train %s)", e.reason, e.publishTrainID)
	}
	return "update deferred: " + e.reason
}

func (e *updateBusyError) Error() string {
	if e.cause == updateBusyCauseRestart {
		return "update deferred: " + e.reason + " — retry after the daemon restart finishes"
	}
	return "update deferred: " + e.reason + " — retry when idle or force with ?force=1"
}

// checkBusy consults the injected busy gate (nil-safe).
func (u *Updater) checkBusy() (bool, string, string) {
	if u.busyCheck == nil {
		return false, "", ""
	}
	return u.busyCheck()
}

// Apply downloads the platform-specific binary, verifies its SHA-256,
// atomically swaps it for the running binary, spawns a successor
// process from the same path (which inherits the Tauri shell's Job
// Object membership on Windows so it dies with Tauri), and arranges
// for THIS process to exit cleanly so its listener releases :8787
// and the successor's bind succeeds.
//
// We don't ask the Tauri shell to respawn us — that would require a
// Rust-side watcher loop and SidecarGuard refactor. Spawning our own
// successor keeps the entire mechanism inside solud.
//
// exitFn is os.Exit in production; tests inject a stub so the test
// process doesn't actually exit. The 600ms sleep before exit gives
// net/http time to flush the response, the web UI time to start its
// reconnect loop, and the successor process time to launch and
// start its bind retry loop.
//
// force bypasses the busyCheck, workflow drain, and quiet-window backoff only
// for the user's explicit override. It WILL interrupt in-flight workflow model
// invocations. It does NOT bypass the kill-switch control file.
func (u *Updater) Apply(ctx context.Context, exitFn func(int), force bool) error {
	return u.apply(ctx, exitFn, force, false)
}

// ApplyRollPastWorkflow is the release-train handoff for a bounded safe-boundary
// drain whose long model invocations outlast the drain grace. The caller has
// already closed model admission and waited its bounded grace, so a still-open
// invocation must not stall the release forever: this keeps every gate except
// the workflow drain. An interrupted invocation is a normal, already-handled
// state that boot adoption closes and resumes. The Focus-busy check and the kill switch are deliberately kept.
func (u *Updater) ApplyRollPastWorkflow(ctx context.Context, exitFn func(int)) error {
	return u.apply(ctx, exitFn, false, true)
}

// localUpdateCandidate is a daemon binary built on this machine (the owner's
// follow-main channel, scripts/follow-main.sh). It is installed through the
// SAME guarded swap as a release — busy and workflow drain checks, release
// publish deferral, kill switch, successor boot verification and rollback —
// only the "download a signed release asset" step is replaced by "copy this
// file and verify its hash".
type localUpdateCandidate struct {
	path           string
	sha256         string
	version        string
	allowDowngrade bool // returning from follow-main to the (older) release
}

// ApplyLocal installs a locally built daemon binary. Callers (the
// /api/update/apply-local handler) must already have restricted path,
// ownership and transport; this validates hash and version ordering.
func (u *Updater) ApplyLocal(ctx context.Context, exitFn func(int), c localUpdateCandidate) error {
	return u.applyCandidate(ctx, exitFn, false, false, &c)
}

func (u *Updater) apply(ctx context.Context, exitFn func(int), force, rollPastWorkflow bool) error {
	return u.applyCandidate(ctx, exitFn, force, rollPastWorkflow, nil)
}

func (u *Updater) applyCandidate(ctx context.Context, exitFn func(int), force, rollPastWorkflow bool, local *localUpdateCandidate) (resultErr error) {
	// An asynchronously loading board is not an idle board. Fail closed before
	// the restart reservation or workflow admission lease: otherwise an empty
	// unpublished task map can authorize a second handoff over durable work.
	if !force && !u.workflowBootIsReady() {
		return &updateBusyError{
			cause:  updateBusyCauseWorkflow,
			reason: "workflow boot is loading or failed; waiting for the complete board before updating",
		}
	}
	// Server-side update-safety gate: refuse a non-forced apply while a Focus
	// pane is mid-turn. The client already defers locally, so a well-behaved UI
	// never trips this — it's the backstop for curl/scripts/stale clients (and
	// the no-open-tab case, where the client's deferred auto-apply can't run).
	if !force {
		if busy, reason, paneID := u.checkBusy(); busy {
			return &updateBusyError{cause: updateBusyCauseFocus, reason: reason, paneID: paneID}
		}
	}
	u.mu.Lock()
	swapAlreadyCommitted := u.swapped
	u.mu.Unlock()
	if swapAlreadyCommitted {
		return errors.New("update already in progress")
	}
	if !u.restartGate.CompareAndSwap(false, true) {
		return &updateBusyError{
			cause:  updateBusyCauseRestart,
			reason: "another daemon restart is already in progress",
		}
	}
	releaseRestartReservation := true
	defer func() {
		if releaseRestartReservation {
			u.restartGate.Store(false)
		}
	}()

	// Validate that there is an applicable update before arming the workflow
	// drain. A no-manifest/no-update request must not pause worker admission.
	u.mu.Lock()
	if u.applying || u.swapped {
		u.mu.Unlock()
		return errors.New("update already in progress")
	}
	var platform ManifestPlatform
	var candidateVersion string
	// candidateChannel is the channel whose manifest named the candidate; a
	// check that switches channels during the download cancels the swap.
	candidateChannel := u.channel
	if local != nil {
		candidateVersion = local.version
		if candidateVersion == "" || candidateVersion == "dev" {
			u.mu.Unlock()
			return errors.New("local build has no release-style version")
		}
		if candidateVersion == u.current || (!local.allowDowngrade && !isNewer(candidateVersion, u.current)) {
			u.mu.Unlock()
			return fmt.Errorf("local build %s is not newer than the running %s", candidateVersion, u.current)
		}
	} else {
		if u.manifest == nil {
			u.mu.Unlock()
			return errors.New("no manifest cached; run /api/update/check first")
		}
		if !isNewer(u.manifest.Version, u.current) {
			u.mu.Unlock()
			return fmt.Errorf("no update available (current %s, latest %s)", u.current, u.manifest.Version)
		}
		var ok bool
		platform, ok = u.manifest.Platforms[platformKeyCurrent()]
		if !ok {
			u.mu.Unlock()
			return fmt.Errorf("no binary in manifest for platform %s", platformKeyCurrent())
		}
		candidateVersion = u.manifest.Version
	}
	localDrainCandidate := ""
	if local != nil {
		localDrainCandidate = local.version + ":" + local.sha256
	}
	if u.localDrainCandidateID != localDrainCandidate {
		u.clearWorkflowDrainLocked()
	}
	u.localDrainCandidateID = localDrainCandidate
	u.mu.Unlock()

	// restartGate is the pre-admission reservation. Keep the public `applying`
	// bit false while we inspect the workflow gate and download the candidate:
	// clients use that bit to mount a full-screen handoff overlay, and neither
	// of those phases guarantees that a swap will be admitted.
	u.mu.Lock()
	if u.swapped {
		u.mu.Unlock()
		return errors.New("update already in progress")
	}
	// Drain duration belongs to this Apply attempt. Reset it before either
	// drain check so a prior failed/deferred attempt can never leak into the
	// durable restart-intent record. An already-armed window is sampled below.
	u.lastDrainDuration = 0
	u.mu.Unlock()

	// Arm cleanup before either workflow check can acquire the admission lease.
	// This releases that lease on every exit path (entry wait, control block,
	// download failure, pre-swap abort, swap failure, successor rollback, and
	// post-exitFn in tests). It also clears the post-admission signal after a
	// failure or after Apply returns to hand off to the successor.
	defer func() {
		u.mu.Lock()
		u.applying = false
		u.releaseWorkflowLeaseLocked()
		if local != nil && resultErr != nil {
			var busy *updateBusyError
			var deferred *updateDeferredError
			if !(errors.As(resultErr, &busy) && busy.cause == updateBusyCauseWorkflow) && !errors.As(resultErr, &deferred) {
				// Refused/corrupt candidates cannot leave workflow admission held.
				u.clearWorkflowDrainLocked()
			}
		}
		u.mu.Unlock()
	}()

	if !force && !rollPastWorkflow {
		check := u.checkWorkflowDrain(time.Now())
		switch check.outcome {
		case drainWait:
			return &updateBusyError{cause: updateBusyCauseWorkflow, reason: check.reason, inflight: check.inflight, deadlineMs: check.deadlineMs}
		case drainDefer:
			return &updateDeferredError{reason: check.reason, inflight: check.inflight, nextRetryAt: check.nextRetryAt, attempts: check.attempts, publishTrainID: check.publishTrainID}
		}
	}
	// Roll-past skips the workflow drain but must still refuse to swap under a
	// release publish (incident rtr_26224e70). The check has no side effects.
	if rollPastWorkflow {
		if trainID, inflight := u.releasePublishInflightNow(); inflight {
			return &updateDeferredError{
				reason:         fmt.Sprintf("release train %s publish in flight — deferring update until the train records its outcome", trainID),
				publishTrainID: trainID,
				nextRetryAt:    time.Now().Add(publishDeferRetry),
			}
		}
	}

	// Re-fetch the control file so a just-flipped kill switch is honored even on
	// a FORCED apply — the "Update now" button (force) bypasses every client-side
	// hold, so Apply, not Check, is the real enforcement boundary. A halt here
	// must NOT block rollback/RestorePrevious; those paths don't consult control.
	{
		ctrl, ctrlErr := u.fetchControl(ctx)
		u.mu.Lock()
		if ctrlErr != nil {
			ctrl = u.controlFallbackLocked(ctrlErr)
		} else {
			u.control = ctrl
			u.controlFetchedAt = time.Now()
		}
		candidate := candidateVersion
		machineID := u.machineID
		cur := u.current
		u.mu.Unlock()
		if blocked, reason := ctrl.blocksApply(candidate, cur, machineID); blocked {
			return fmt.Errorf("update blocked: %s", reason)
		}
		// Returning from follow-main to an older release is a downgrade: it must
		// respect the operator's minimum-version floor like a rollback does.
		if local != nil && compareVersions(candidate, cur) < 0 {
			if blocked, reason := ctrl.blocksRollback(candidate, cur); blocked {
				return fmt.Errorf("downgrade blocked: %s", reason)
			}
		}
		if u.rollback != nil {
			if hold, active := u.rollback.ActiveHold(time.Now()); active && hold.Version == candidate {
				if ctrl != nil && versionBelowFloor(cur, ctrl.MinDaemonVer) {
					_ = u.rollback.ClearHold("operator minimum version floor")
					slog.Info("update hold released for operator minimum version floor", "version", candidate)
				} else {
					return &updateHeldError{version: hold.Version, expiresAtMs: hold.ExpiresAtMs, reason: hold.Reason}
				}
			}
		}
	}

	selfPath, err := u.executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	// Resolve symlinks so we swap the real file, not whatever the
	// user may have symlinked /usr/local/bin/solud to.
	if rp, err := filepath.EvalSymlinks(selfPath); err == nil {
		selfPath = rp
	}

	newPath := selfPath + ".new"
	if local != nil {
		if err := copyVerifiedLocalBuild(local.path, local.sha256, newPath); err != nil {
			_ = os.Remove(newPath)
			return fmt.Errorf("local build: %w", err)
		}
	} else if err := u.download(ctx, platform.URL, platform.SHA256, newPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("download: %w", err)
	}

	// Re-check the busy gate now that the (possibly slow) download is done: a
	// Focus pane idle at entry can be mid-turn by now, and past this point we
	// freeze the UI and swap — the point of no return. Forced applies skip it.
	if !force {
		if busy, reason, paneID := u.checkBusy(); busy {
			_ = os.Remove(newPath)
			return &updateBusyError{cause: updateBusyCauseFocus, reason: reason, paneID: paneID}
		}
		if !rollPastWorkflow {
			check := u.checkWorkflowDrain(time.Now())
			if check.outcome == drainWait {
				_ = os.Remove(newPath)
				return &updateBusyError{cause: updateBusyCauseWorkflow, reason: check.reason, inflight: check.inflight, deadlineMs: check.deadlineMs}
			}
			if check.outcome == drainDefer {
				_ = os.Remove(newPath)
				return &updateDeferredError{reason: check.reason, inflight: check.inflight, nextRetryAt: check.nextRetryAt, attempts: check.attempts, publishTrainID: check.publishTrainID}
			}
		}
	}

	u.mu.Lock()
	if local == nil && u.channel != candidateChannel {
		now := u.channel
		u.mu.Unlock()
		_ = os.Remove(newPath)
		return fmt.Errorf("update channel changed to %s while downloading; not installing the %s release", now, candidateChannel)
	}
	fromVersion := u.current
	toVersion := candidateVersion
	required := local == nil && u.status.Required
	drainDuration := u.lastDrainDuration
	u.mu.Unlock()
	reason := restartIntentAutoQuiet
	if force {
		reason = restartIntentHumanForced
	} else if required {
		reason = restartIntentCriticalPolicy
	}
	err = u.commitBinarySwap(selfPath, newPath, swapCommit{
		fromVersion: fromVersion, toVersion: toVersion, intentReason: reason,
		failureLabelPrefix: "", pendingFrom: fromVersion, drainDuration: drainDuration,
	}, exitFn)
	if err == nil {
		releaseRestartReservation = false
	}
	return err
}

func (u *Updater) workflowBootIsReady() (ready bool) {
	if u == nil || u.workflowBootReady == nil {
		return true
	}
	defer func() {
		if recover() != nil {
			ready = false
		}
	}()
	return u.workflowBootReady()
}

type swapCommit struct {
	fromVersion        string
	toVersion          string
	intentReason       string
	failureLabelPrefix string
	pendingFrom        string
	drainDuration      time.Duration
}

func (u *Updater) commitBinarySwap(selfPath, newPath string, c swapCommit, exitFn func(int)) error {
	u.mu.Lock()
	u.applying = true
	u.mu.Unlock()
	if u.bus != nil {
		u.bus.publish(uiEvent{Type: "update-applying", Text: c.toVersion})
	}
	sleep := u.commitSleepFn
	if sleep == nil {
		sleep = time.Sleep
	}
	sleep(400 * time.Millisecond)

	inflight, invocationIDs := u.workflowInflightSnapshot()
	if err := writeRestartIntent(selfPath, restartIntent{
		Reason: c.intentReason, From: c.fromVersion, To: c.toVersion,
		DrainDurationMs: c.drainDuration.Milliseconds(), Inflight: inflight,
		InvocationIDs: invocationIDs, WrittenAtMs: time.Now().UnixMilli(),
	}); err != nil {
		_ = os.Remove(newPath)
		u.publishUpdateAborted()
		return fmt.Errorf("write restart intent: %w", err)
	}
	slog.Info("update restart intent recorded", "reason", c.intentReason, "from", c.fromVersion,
		"to", c.toVersion, "drainDuration", c.drainDuration, "inflight", inflight,
		"invocationCount", len(invocationIDs))

	swap := u.swapBinaryFn
	if swap == nil {
		swap = swapBinary
	}
	if err := swap(selfPath, newPath); err != nil {
		clearRestartIntent(selfPath)
		_ = os.Remove(newPath)
		u.reportApplyFailureAsync(c.failureLabelPrefix + "swap_failed")
		u.publishUpdateAborted()
		return fmt.Errorf("swap: %w", err)
	}
	u.mu.Lock()
	u.clearWorkflowDrainLocked()
	u.mu.Unlock()
	if u.metrics != nil {
		u.metrics.UpdaterAppliesTotal.Add(1)
	}
	writeVersionMarker(selfPath, c.toVersion)

	if os.Getenv("INVOCATION_ID") != "" {
		writeUpdatePending(selfPath, c.toVersion, c.pendingFrom)
		slog.Info("update applied; exiting for systemd restart", "from", c.fromVersion, "to", c.toVersion)
		u.finishCommittedSwap(exitFn, sleep)
		return nil
	}

	bootMarker := selfPath + ".successor-ok"
	_ = os.Remove(bootMarker)
	spawn := u.spawnSuccessorFn
	if spawn == nil {
		spawn = spawnSuccessor
	}
	proc, err := spawn(selfPath, c.fromVersion, bootMarker)
	if err != nil {
		clearRestartIntent(selfPath)
		u.restoreAfterFailedSwap(selfPath, c.fromVersion)
		u.reportApplyFailureAsync(c.failureLabelPrefix + "successor_spawn_failed")
		u.publishUpdateAborted()
		return fmt.Errorf("spawn successor: %w", err)
	}
	wait := u.waitSuccessorBootFn
	if wait == nil {
		wait = waitForSuccessorBoot
	}
	if !wait(bootMarker, successorBootTimeout) {
		clearRestartIntent(selfPath)
		if proc != nil {
			kill := u.killSuccessorFn
			if kill == nil {
				kill = func(p *os.Process) error { return p.Kill() }
			}
			_ = kill(proc)
		}
		u.restoreAfterFailedSwap(selfPath, c.fromVersion)
		_ = os.Remove(bootMarker)
		u.reportApplyFailureAsync(c.failureLabelPrefix + "successor_boot_failed")
		u.publishUpdateAborted()
		return fmt.Errorf("successor failed to boot within %s; rolled back to %s", successorBootTimeout, c.fromVersion)
	}
	slog.Info("update applied", "from", c.fromVersion, "to", c.toVersion, "platform", platformKeyCurrent())
	u.finishCommittedSwap(exitFn, sleep)
	return nil
}

func (u *Updater) publishUpdateAborted() {
	if u.bus != nil {
		u.bus.publish(uiEvent{Type: "update-aborted"})
	}
}

func (u *Updater) restoreAfterFailedSwap(selfPath, fromVersion string) {
	if err := rollbackSwappedBinary(selfPath); err != nil {
		slog.Error("update rollback after failed successor failed", "err", err)
		return
	}
	writeVersionMarker(selfPath, fromVersion)
}

func (u *Updater) finishCommittedSwap(exitFn func(int), sleep func(time.Duration)) {
	u.mu.Lock()
	u.swapped = true
	u.mu.Unlock()
	gate := u.restartGate
	go func() {
		sleep(600 * time.Millisecond)
		exitFn(ExitCodeRestartForUpdate)
		gate.Store(false)
	}()
}

func (u *Updater) workflowInflightSnapshot() (count int, ids []string) {
	if u.inflightCount != nil {
		func() {
			defer func() { _ = recover() }()
			count = u.inflightCount()
		}()
	}
	if u.inflightIDs != nil {
		func() {
			defer func() { _ = recover() }()
			ids = append([]string(nil), u.inflightIDs()...)
		}()
	}
	if len(ids) > restartIntentMaxInvocationIDs {
		ids = ids[:restartIntentMaxInvocationIDs]
	}
	return count, ids
}

// restartSelfForMemGuard starts a healthy same-binary successor before
// allowing the memory-heavy predecessor to exit. Unlike Apply it performs no
// swap, rollback bookkeeping, or busy check: at the observed 2-3GB/min growth,
// deferring for an active turn risks freezing the machine. Detached agents
// survive the handoff and AttachExisting reconnects them after boot, so an
// interrupted turn is recoverable.
type memGuardRestartDeps struct {
	executable func() (string, error)
	spawn      func(string, string, string) (*os.Process, error)
	wait       func(string, time.Duration) bool
	sleep      func(time.Duration)
	gate       *atomic.Bool
}

func restartSelfForMemGuard(currentVersion string, exitFn func(int)) error {
	return restartSelfForMemGuardWithDeps(currentVersion, exitFn, memGuardRestartDeps{})
}

func restartSelfForMemGuardWithDeps(currentVersion string, exitFn func(int), deps memGuardRestartDeps) error {
	if deps.gate == nil {
		deps.gate = &selfRestartInProgress
	}
	if !deps.gate.CompareAndSwap(false, true) {
		err := errors.New("daemon restart already in progress")
		slog.Error("[memguard] restart refused; current daemon remains serving", "err", err)
		return err
	}
	defer deps.gate.Store(false) // reached only by failures or test stubs
	if deps.executable == nil {
		deps.executable = os.Executable
	}
	if os.Getenv("INVOCATION_ID") != "" {
		slog.Info("[memguard] exiting for systemd to restart memory-heavy daemon", "version", currentVersion)
		exitFn(ExitCodeRestartForUpdate)
		return nil
	}
	selfPath, err := deps.executable()
	if err != nil {
		slog.Error("[memguard] locate daemon binary for restart failed", "err", err)
		return fmt.Errorf("locate running binary: %w", err)
	}
	return restartDaemonFromBinaryWithDeps(selfPath, currentVersion, exitFn, deps, true)
}

// restartSafeBoundaryTimeout bounds how long the release train waits at the
// shared model-admission boundary before a daemon restart.
const restartSafeBoundaryTimeout = 10 * time.Minute

// restartDaemonFromBinaryWithDeps is the boot-marker-verified handoff used
// by memguard. reservationHeld means the caller
// already owns deps.gate. Every failure happens before predecessor exit.
func restartDaemonFromBinaryWithDeps(binPath, targetVersion string, exitFn func(int), deps memGuardRestartDeps, reservationHeld bool) error {
	if deps.gate == nil {
		deps.gate = &selfRestartInProgress
	}
	acquiredHere := false
	if !reservationHeld {
		if !deps.gate.CompareAndSwap(false, true) {
			return errors.New("daemon restart already in progress")
		}
		acquiredHere = true
	}
	if acquiredHere {
		defer deps.gate.Store(false)
	}
	if deps.spawn == nil {
		deps.spawn = spawnSuccessor
	}
	if deps.wait == nil {
		deps.wait = waitForSuccessorBoot
	}
	if deps.sleep == nil {
		deps.sleep = time.Sleep
	}
	if exitFn == nil {
		exitFn = os.Exit
	}
	if strings.TrimSpace(binPath) == "" {
		return errors.New("restart target executable is empty")
	}

	if os.Getenv("INVOCATION_ID") != "" {
		slog.Info("exiting for systemd to restart daemon", "version", targetVersion)
		exitFn(ExitCodeRestartForUpdate)
		return nil
	}

	bootMarker := binPath + ".successor-ok"
	_ = os.Remove(bootMarker)
	proc, err := deps.spawn(binPath, targetVersion, bootMarker)
	if err != nil {
		slog.Error("[memguard] successor spawn failed; current daemon remains serving", "err", err)
		return fmt.Errorf("spawn successor: %w", err)
	}
	if !deps.wait(bootMarker, successorBootTimeout) {
		_ = proc.Kill()
		_ = os.Remove(bootMarker)
		slog.Error("[memguard] successor did not report healthy boot; current daemon remains serving",
			"timeout", successorBootTimeout)
		return fmt.Errorf("successor failed to boot within %s", successorBootTimeout)
	}

	slog.Info("[memguard] healthy successor booted; exiting memory-heavy predecessor",
		"version", targetVersion, "successor_pid", proc.Pid)
	deps.sleep(600 * time.Millisecond)
	exitFn(ExitCodeRestartForUpdate)
	return nil
}

// successorBootTimeout bounds how long the predecessor waits for the
// successor's boot marker before declaring the update failed and rolling
// back. Generous because the successor's startup includes synchronous
// pane reattach (~1s per dead pane), so a healthy-but-busy boot on a
// machine with many panes can legitimately take several seconds; during
// the wait the predecessor is still fully serving, so erring long is
// cheap, while erring short would roll back good updates.
//
// 25s erred short. The marker is written only after ALL init, pane
// reattach included, and on a loaded host with ~70 panes that took minutes:
// on 2026-09-24 a successor needed 21s just to log its first line, a cold
// boot of the same daemon took ~2m40s from spawn to bind, and the
// 0.0.1140 -> 0.0.1141 update was rolled back 10 times in one day, each
// attempt booting a multi-GB successor into a host already out of swap and
// then killing it. The predecessor serves throughout, so wait long enough
// for a slow-but-healthy boot to finish.
const successorBootTimeout = 4 * time.Minute

// waitForSuccessorBoot polls for the successor's boot marker file,
// returning true as soon as it appears or false if the timeout elapses.
func waitForSuccessorBoot(markerPath string, timeout time.Duration) bool {
	return waitForSuccessorBootWithDeps(markerPath, timeout, successorBootWaitDeps{
		now: time.Now, stat: os.Stat, sleep: time.Sleep,
	})
}

type successorBootWaitDeps struct {
	now   func() time.Time
	stat  func(string) (os.FileInfo, error)
	sleep func(time.Duration)
}

func waitForSuccessorBootWithDeps(markerPath string, timeout time.Duration, deps successorBootWaitDeps) bool {
	deadline := deps.now().Add(timeout)
	for deps.now().Before(deadline) {
		if _, err := deps.stat(markerPath); err == nil {
			return true
		}
		deps.sleep(100 * time.Millisecond)
	}
	return false
}

// writeSuccessorBootMarker is called by a freshly-spawned successor
// (SOLU_SUCCESSOR_OF_UPDATE=1) after it has cleared all fatal-prone
// startup and is about to bind the port. Writing it tells the still-alive
// predecessor "the new binary boots — release the port and exit." No-op
// when not running as an update successor.
func writeSuccessorBootMarker() {
	markerPath := os.Getenv("SOLU_SUCCESSOR_BOOT_MARKER")
	if markerPath == "" {
		return
	}
	if err := os.WriteFile(markerPath, []byte("ok\n"), 0o644); err != nil {
		slog.Warn("update successor boot marker write failed", "path", markerPath, "err", err)
	}
}

func rollbackSwappedBinary(currentPath string) error {
	oldParked := currentPath + ".old"
	if _, err := os.Stat(oldParked); err != nil {
		return fmt.Errorf("stat rollback binary %s: %w", oldParked, err)
	}
	failedNew := currentPath + ".spawn-failed"
	_ = os.Remove(failedNew)
	if err := os.Rename(currentPath, failedNew); err != nil {
		return fmt.Errorf("park failed successor binary → %s: %w", failedNew, err)
	}
	if err := os.Rename(oldParked, currentPath); err != nil {
		_ = os.Rename(failedNew, currentPath)
		return fmt.Errorf("restore %s → %s: %w", oldParked, currentPath, err)
	}
	_ = os.Remove(failedNew)
	rotateOldRingBackward(currentPath)
	return nil
}

// RestorePrevious rolls back the most recent auto-update by swapping
// the parked .old binary back into the canonical name. Single-version
// rollback — every successful update overwrites .old, so this works
// only against the very last update. The intended use is "I just
// shipped a bad release; revert before any more installs roll forward
// past the bug." For older versions the operator should publish a new
// release that re-includes the fix.
//
// This is invoked from the CLI (`solud --restore-previous`); it does
// not run inside a long-lived daemon. We deliberately don't try to
// re-exec into the rolled-back binary here — the caller is expected
// to be running the binary they want to roll back FROM, so on success
// we exit cleanly and let the operator (or a respawning Tauri shell /
// systemd unit / etc.) launch the restored binary themselves.
func RestorePrevious() error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	if rp, err := filepath.EvalSymlinks(selfPath); err == nil {
		selfPath = rp
	}
	oldParked := selfPath + ".old"
	if _, err := os.Stat(oldParked); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no previous version to restore: %s does not exist", oldParked)
		}
		return fmt.Errorf("stat %s: %w", oldParked, err)
	}
	// Two-stage swap symmetric to swapBinary: park current → .new
	// (so we have a way back if the second rename fails), then
	// install .old → current. Use a fresh ".restore-tmp" name to
	// avoid colliding with an in-flight update's .new download.
	tmpPath := selfPath + ".restore-tmp"
	_ = os.Remove(tmpPath)
	if err := os.Rename(selfPath, tmpPath); err != nil {
		return fmt.Errorf("park current → %s: %w", tmpPath, err)
	}
	if err := os.Rename(oldParked, selfPath); err != nil {
		// Best-effort un-swap so the binary at the canonical path
		// is still runnable.
		_ = os.Rename(tmpPath, selfPath)
		return fmt.Errorf("install %s → %s: %w", oldParked, selfPath, err)
	}
	// We just promoted .old — there's nothing useful left in tmpPath
	// (it's the binary we just rolled back FROM, which we don't want
	// to re-install). Clean it up; if it's locked (Windows defender
	// scanning, etc.), that's a cosmetic leak, not a correctness bug.
	_ = os.Remove(tmpPath)
	// Walk the rest of the ring one step newer so a chained
	// restore-previous goes back another step. .old.1 → .old,
	// .old.2 → .old.1, etc. Best-effort.
	rotateOldRingBackward(selfPath)
	return nil
}

// listenWithUpdateRetry wraps net.Listen with EADDRINUSE retry logic
// for the self-update successor case. If we were spawned via Apply()
// (env SOLU_SUCCESSOR_OF_UPDATE=1), our predecessor still owns the
// port for ~600ms while it flushes the apply response and exits;
// retry every 100ms for up to 5s. Without that env var (normal
// startup) we fail fast on EADDRINUSE — a stale daemon should be
// surfaced to the user, not silently waited on.
func listenWithUpdateRetry(network, addr string) (net.Listener, error) {
	if os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") != "1" {
		return listenOnConfiguredPort(network, addr)
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ln, err := listenOnConfiguredPort(network, addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("predecessor still bound after 5s: %w", lastErr)
}

// preflightUpdateSuccessorListener proves that the configured listener is
// viable before an update successor tells its predecessor to exit. Ordinary
// boots preserve the existing clear-pending-before-bind path. For a successor,
// an address already owned by its predecessor is the expected EADDRINUSE
// handoff state; every other error is fatal and therefore must precede marker
// emission. A free address is retained so no third process can win the gap.
func preflightUpdateSuccessorListener(network, addr string) (net.Listener, error) {
	return preflightUpdateSuccessorListenerWithListen(network, addr, listenOnConfiguredPort)
}

func preflightUpdateSuccessorListenerWithListen(network, addr string, listen func(string, string) (net.Listener, error)) (net.Listener, error) {
	if os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") != "1" {
		return nil, nil
	}
	ln, err := listen(network, addr)
	if err == nil {
		return ln, nil
	}
	if updateListenerAddrInUse(err) {
		return nil, nil
	}
	return nil, err
}

// spawnSuccessor starts a new solud process from the just-updated
// binary path, with the same CLI args our process was launched with.
// The successor is detached so it survives our exit and joins (on
// Windows) Tauri's Job Object via process-tree inheritance.
//
// The successor's bind on :8787 will fail until we exit a moment
// later and release the port — see waitForBindableAddr in main.go for
// the retry loop the successor runs.
func spawnSuccessor(binPath, predecessorVersion, bootMarker string) (*os.Process, error) {
	// An update-only daemon has no successor (update_only_boot.go): it never
	// restarts into another version. If anything ever got here, exit.
	if updateOnlyDaemon() {
		log.Printf("update-only daemon: refusing to start a successor; exiting")
		os.Exit(0)
	}
	args := os.Args[1:]
	cmd := exec.Command(binPath, args...)
	cmd.SysProcAttr = successorSysProcAttr()

	// Open a dedicated log file for the successor instead of inheriting
	// our stdio. Two reasons:
	//   1. Our stdio fd is hooked to start-wsl.sh's daemon.log, which
	//      gets TRUNCATED on every Tauri restart's start-wsl.sh
	//      invocation. If we inherit it, the successor's startup logs
	//      vanish when the user happens to restart Tauri later. This
	//      file persists.
	//   2. When our process exits, our stdio fd's refcount drops to
	//      zero on our side; the kernel keeps the file open for the
	//      successor (refcount survives via inheritance), but in
	//      certain process-group teardown scenarios writes to a
	//      "dying" fd have unpredictable behavior. Owning the fd
	//      from the successor's first instruction sidesteps that.
	logPath := filepath.Join(filepath.Dir(binPath), "successor.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// Fall back to inherited stdio if we can't open our own log —
		// don't fail the whole update over a logging issue.
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = logF
		cmd.Stderr = logF
		// Don't close logF here — exec.Cmd.Start dups the fd into the
		// child; after Start returns it's safe to close our handle,
		// but we leak it intentionally so subsequent log lines we might
		// still want to write before our os.Exit also land somewhere
		// readable.
	}

	// Mark the successor with an env var so it knows to give the
	// listener-bind a few extra seconds before giving up — its parent
	// (us) is still holding :8787 for the next ~600ms. Also stamp the
	// predecessor's version so the successor can post a "from→to"
	// update-report to the cloud once it boots cleanly; this is how
	// we measure update-success rate per machine.
	// Strip any inherited CLAUDE_CODE_* / CLAUDECODE so the successor doesn't
	// re-inherit a dirty launch env across the update (the env that makes pane
	// `claude`s nested child sessions). Without this the dirt is immortal: a
	// daemon first launched from inside a claude session would hand it to every
	// successor generation forever. Combined with maybeReexecCleanEnv this ends
	// the propagation entirely.
	cmd.Env = append(stripInheritedClaudeEnv(os.Environ()),
		"SOLU_SUCCESSOR_OF_UPDATE=1",
		"SOLU_PREDECESSOR_VERSION="+predecessorVersion,
		// The successor writes this marker after it clears all
		// fatal-prone startup (state/workspace/schedule/aria/token load)
		// and just before it attempts to bind the port. Seeing it is the
		// predecessor's proof that the new binary actually boots, so it's
		// safe to release the port and exit. See Apply()'s wait loop.
		"SOLU_SUCCESSOR_BOOT_MARKER="+bootMarker,
	)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Printf("update: spawned successor pid=%d from %s (log: %s)", cmd.Process.Pid, binPath, logPath)
	updateManagedWslPidFile(binPath, cmd.Process.Pid)
	// Don't wait — successor lives on its own from here. Release our
	// handle to the cmd so the kernel reaps the child cleanly when it
	// eventually exits (or we exit, in which case init reaps it).
	go func() { _ = waitOwnedCommand(cmd) }()
	return cmd.Process, nil
}

func writeVersionMarker(binPath, ver string) {
	ver = strings.TrimSpace(ver)
	if ver == "" || ver == "dev" {
		return
	}
	marker := binPath + ".version"
	if err := os.WriteFile(marker, []byte(ver+"\n"), 0o644); err != nil {
		slog.Warn("update version marker write failed", "path", marker, "err", err)
	}
}

func updateManagedWslPidFile(binPath string, pid int) {
	if pid <= 0 || !isManagedWslDaemonArgs(os.Args[1:]) {
		return
	}
	pidFile := filepath.Join(filepath.Dir(binPath), "solud-wsl.pid")
	if _, err := os.Stat(pidFile); err != nil {
		return
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		slog.Warn("update WSL pidfile write failed", "path", pidFile, "pid", pid, "err", err)
	}
}

func isManagedWslDaemonArgs(args []string) bool {
	return argValue(args, "--addr") == "0.0.0.0:8787" &&
		argValue(args, "--backend-id") == "wsl" &&
		argValue(args, "--shell-platform") == "windows-amd64"
}

func argValue(args []string, key string) string {
	for i, arg := range args {
		if arg == key {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, key+"=") {
			return strings.TrimPrefix(arg, key+"=")
		}
	}
	return ""
}

// copyVerifiedLocalBuild copies an owner-built binary to dst (0755) and
// verifies its SHA-256 matches the hash the caller computed at build time.
func copyVerifiedLocalBuild(src, wantHex, dst string) error {
	// Re-check on the OPEN file (no symlink follow, non-blocking): the path was
	// validated earlier, and it must not be swapped for a FIFO, device or a
	// link elsewhere in between.
	in, err := openLocalBuildNoFollow(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > localBuildMaxBytes || !fileOwnedByCurrentUser(info) {
		return errors.New("local build changed after validation (not a regular, owner-owned file within the size limit)")
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, localBuildMaxBytes)); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, strings.TrimSpace(wantHex)) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantHex)
	}
	return nil
}
