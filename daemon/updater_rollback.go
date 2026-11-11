package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type rollbackRequest struct {
	TargetVersion string `json:"targetVersion"`
	HoldHours     int    `json:"holdHours"`
	Force         bool   `json:"force"`
}

type updateHeldError struct {
	version     string
	expiresAtMs int64
	reason      string
}

func (e *updateHeldError) Error() string {
	return fmt.Sprintf("update %s is held until %s: %s", e.version,
		time.UnixMilli(e.expiresAtMs).Format(time.RFC3339), e.reason)
}

func rollbackWorkloadCheck(u *Updater) error {
	check := u.checkWorkflowDrain(time.Now())
	switch check.outcome {
	case drainWait:
		return &updateBusyError{cause: updateBusyCauseWorkflow, reason: check.reason, inflight: check.inflight, deadlineMs: check.deadlineMs}
	case drainDefer:
		return &updateDeferredError{reason: check.reason, inflight: check.inflight, nextRetryAt: check.nextRetryAt, attempts: check.attempts, publishTrainID: check.publishTrainID}
	default:
		return nil
	}
}

// Rollback installs a user-selected older release through the same immutable
// signed-manifest, SHA-256, workload-drain, and boot-health machinery as a
// forward update. Force bypasses only the live-workload gates.
func (u *Updater) Rollback(ctx context.Context, req rollbackRequest, exitFn func(int)) (retErr error) {
	target := strings.TrimSpace(req.TargetVersion)
	if !validReleaseVersion(target) {
		return fmt.Errorf("invalid rollback target version %q", target)
	}
	if target == u.current || !isNewer(u.current, target) {
		return fmt.Errorf("rollback target %s must be older than current version %s", target, u.current)
	}
	if u.rollback == nil {
		return errors.New("rollback state unavailable")
	}
	if !req.Force {
		if busy := u.checkBusy(); busy != nil {
			return busy
		}
	}
	if !u.restartGate.CompareAndSwap(false, true) {
		return &updateBusyError{cause: updateBusyCauseRestart, reason: "another daemon restart is already in progress"}
	}
	releaseRestartReservation := true
	defer func() {
		if releaseRestartReservation {
			u.restartGate.Store(false)
		}
	}()

	u.mu.Lock()
	if u.applying || u.swapped {
		u.mu.Unlock()
		return errors.New("update already in progress")
	}
	u.lastDrainDuration = 0
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.applying = false
		var busy *updateBusyError
		var deferred *updateDeferredError
		if !req.Force && (errors.As(retErr, &busy) || errors.As(retErr, &deferred)) {
			// A workload refusal is a live rollback drain, not a terminal
			// failure. Preserve its window/backoff across status polls and the
			// next explicit retry, while releasing any per-attempt lease.
			u.releaseWorkflowLeaseLocked()
		} else if u.rollbackDrain {
			u.clearWorkflowDrainLocked()
		} else {
			u.releaseWorkflowLeaseLocked()
		}
		u.mu.Unlock()
	}()
	if !req.Force {
		u.mu.Lock()
		u.rollbackDrain = true
		u.mu.Unlock()
		if err := rollbackWorkloadCheck(u); err != nil {
			return err
		}
	}

	ctrl, ctrlErr := u.fetchControl(ctx)
	u.mu.Lock()
	if ctrlErr != nil {
		ctrl = u.controlFallbackLocked(ctrlErr)
	} else {
		u.control = ctrl
		u.controlFetchedAt = time.Now()
	}
	u.mu.Unlock()
	if blocked, reason := ctrl.blocksRollback(target, u.current); blocked {
		return fmt.Errorf("rollback blocked: %s", reason)
	}

	m, err := u.fetchPinnedManifest(ctx, target)
	if err != nil {
		return fmt.Errorf("verify rollback manifest: %w", err)
	}
	platform, ok := m.Platforms[platformKeyCurrent()]
	if !ok {
		return fmt.Errorf("no rollback binary in manifest for platform %s", platformKeyCurrent())
	}
	selfPath, err := u.executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	if rp, err := filepath.EvalSymlinks(selfPath); err == nil {
		selfPath = rp
	}
	candidatePath := selfPath + ".rollback"
	_ = os.Remove(candidatePath)
	if err := u.download(ctx, platform.URL, platform.SHA256, candidatePath); err != nil {
		_ = os.Remove(candidatePath)
		return fmt.Errorf("download rollback: %w", err)
	}

	caps, err := probeCandidateCapabilities(ctx, candidatePath)
	if err == nil && caps.Version != target {
		err = fmt.Errorf("capability probe reported version %q instead of %q", caps.Version, target)
	}
	if err != nil {
		_ = os.Remove(candidatePath)
		decline := &rollbackIncompatibleError{Store: "capabilities", RequiredReader: 1, TargetReader: 0, TargetVersion: target, Reason: err.Error()}
		_ = u.rollback.SetLastAttempt(rollbackAttempt{TargetVersion: target, State: "declined", Error: decline.Error(), AtMs: time.Now().UnixMilli()})
		return decline
	}
	if err := checkRollbackStateCompatible(onDiskStateReaderRequirements(u.dataDir), caps); err != nil {
		_ = os.Remove(candidatePath)
		var incompatible *rollbackIncompatibleError
		if errors.As(err, &incompatible) {
			if incompatible.TargetVersion == "" {
				incompatible.TargetVersion = target
			}
			_ = u.rollback.SetLastAttempt(rollbackAttempt{TargetVersion: target, State: "declined", Error: incompatible.Error(), AtMs: time.Now().UnixMilli()})
		}
		return err
	}

	if !req.Force {
		if busy := u.checkBusy(); busy != nil {
			_ = os.Remove(candidatePath)
			return busy
		}
		if err := rollbackWorkloadCheck(u); err != nil {
			_ = os.Remove(candidatePath)
			return err
		}
	}

	now := time.Now()
	duration := defaultRollbackHoldDuration
	if req.HoldHours != 0 {
		duration = time.Duration(req.HoldHours) * time.Hour
	}
	hold := rollbackHold{
		Version: u.current, FromVersion: target,
		Reason:      "rollback from a workflow-regressing release",
		CreatedAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(duration).UnixMilli(),
	}
	if err := u.rollback.SetHold(hold, now); err != nil {
		_ = os.Remove(candidatePath)
		return fmt.Errorf("persist rollback hold: %w", err)
	}
	if err := u.rollback.SetLastAttempt(rollbackAttempt{TargetVersion: target, State: "swapped", AtMs: now.UnixMilli()}); err != nil {
		_ = os.Remove(candidatePath)
		return fmt.Errorf("persist rollback attempt: %w", err)
	}

	u.mu.Lock()
	drainDuration := u.lastDrainDuration
	u.mu.Unlock()
	err = u.commitBinarySwap(selfPath, candidatePath, swapCommit{
		fromVersion: u.current, toVersion: target,
		intentReason: restartIntentHumanRollback, failureLabelPrefix: "rollback_",
		pendingFrom: u.current, drainDuration: drainDuration,
	}, exitFn)
	if err != nil {
		retryable := fmt.Sprintf("rollback failed and can be retried: %v", err)
		_ = u.rollback.SetLastAttempt(rollbackAttempt{TargetVersion: target, State: "failed", Error: retryable, AtMs: time.Now().UnixMilli()})
		slog.Error("human rollback failed", "from", u.current, "to", target, "err", err)
		return err
	}
	releaseRestartReservation = false
	return nil
}
