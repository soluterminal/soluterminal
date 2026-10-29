package main

// Server-side update control plane: a SEPARATE, always-fetched, signed control
// document that lets the operator halt or stage a rollout WITHOUT cutting a new
// release. The per-release update.json is frozen on whichever release is `latest`,
// so it can't retroactively stop an already-published bad release. This control
// file lives at a stable, mutable URL on a pinned `control` release tag; the
// operator flips it with one `gh release upload control ... --clobber`.
//
// Backward compat is load-bearing: encoding/json ignores unknown fields, so old
// daemons fetching this file ignore it entirely, and new daemons fetching an
// absent/old control file get all-zero defaults — which are the safe "do nothing
// special" state. The single most dangerous detail is RolloutPercent being a
// *int: absent (nil) must mean 100 (ship to all). A plain int would make an
// absent field default to 0 = ship to nobody = freeze every new daemon.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"soluterminal-shared/releases"
)

// controlStaleTTL bounds how long a last-known-good control file is trusted when
// a fresh fetch fails transiently. Within the window we ride the cache (a GitHub
// CDN blip must NOT freeze the fleet); past it, with no good fetch, we fail SAFE
// by synthesizing a paused control so we stop shipping rather than barrel past a
// kill switch we can no longer read.
const controlStaleTTL = 30 * time.Minute

// UpdateControl is the signed kill-switch / staged-rollout document. All fields
// are optional; the zero value is permissive ("apply normally").
type UpdateControl struct {
	Schema int `json:"schema"`
	// Paused is the global kill switch: when true, no daemon applies anything.
	Paused bool `json:"paused"`
	// HaltAtOrAbove quarantines a bad release: refuse to apply any candidate
	// version >= this, while older good releases still roll. "" = no ceiling.
	HaltAtOrAbove string `json:"halt_at_or_above"`
	// HaltBelow refuses to apply a candidate below this version (downgrade guard).
	HaltBelow string `json:"halt_below"`
	// MinDaemonVer is the operator-mandated FLOOR on the running daemon: a
	// daemon below it reports Required=true in /api/update/status, which makes
	// the web UI force-apply the update (even for "Notify me" users) and, if it
	// can't apply, raise a blocking modal. "" = no floor. Machines below the
	// floor also bypass the staged-rollout cohort gate (see blocksApply) so a
	// canary percentage can't strand them; paused/halt_* still always win.
	MinDaemonVer string `json:"min_daemon_version"`
	// MinShellVer is the same floor for the desktop-installer (Tauri shell)
	// channel. The shell can't self-apply, so the UI blocks new work behind a
	// "Restart & update" modal instead. "" = no floor.
	MinShellVer string `json:"min_shell_version"`
	// RolloutPercent gates staged rollout. *int so absent → 100 (ship to all);
	// explicit 0 → ship to none. NEVER make this a plain int.
	RolloutPercent *int `json:"rollout_percent"`
	// Message is an optional operator note surfaced in the update badge.
	Message string `json:"message"`
	// ShellAutoApplyPaused stops the UNATTENDED desktop-installer restart
	// ("update when idle"; apply-shell?silent=1). A clicked installer update
	// is never affected. *bool because absent has a per-channel meaning (see
	// shellAutoApplyPaused): each channel reads its own control file, so the
	// operator can leave team on and public off.
	ShellAutoApplyPaused *bool `json:"shell_auto_apply_paused,omitempty"`
}

// shellAutoApplyPaused reports whether the unattended installer restart is
// off for channel under control c, and why.
//
// Absent field (or no control document at all): ON for the team channel, OFF
// for every other channel. The public fleet stays off until the operator
// publishes a public control file with shell_auto_apply_paused: false.
// The global kill switch (paused, including the fail-safe pause synthesized
// when the control file is unreachable) also stops it.
func (c *UpdateControl) shellAutoApplyPaused(channel string) (bool, string) {
	if c != nil && c.Paused {
		return true, "updates paused by operator"
	}
	if c != nil && c.ShellAutoApplyPaused != nil {
		if *c.ShellAutoApplyPaused {
			return true, "automatic desktop restarts paused by operator"
		}
		return false, ""
	}
	if channel == releases.FlavorTeam {
		return false, ""
	}
	return true, "automatic desktop restarts are not enabled for this channel yet"
}

// controlURL derives the control-file URL from the manifest URL by swapping the
// path onto the pinned `control` release tag, so its download URL never moves.
// SOLU_UPDATE_CONTROL_URL overrides it (tests point it at a local server).
func (u *Updater) controlURL() string {
	if v := strings.TrimSpace(os.Getenv("SOLU_UPDATE_CONTROL_URL")); v != "" {
		return v
	}
	base := u.manifestURLNow()
	if i := strings.Index(base, "/releases/"); i >= 0 {
		return base[:i] + "/releases/download/control/update-control.json"
	}
	return base + ".control"
}

// fetchControl fetches + signature-verifies the control file. A nil return with
// nil error means "no control file published" (HTTP 404) — treated as permissive,
// NOT an error, so the feature is dormant until the control release exists.
// Transient failures (5xx/429/network) are wrapped via transient() so the caller
// can ride the last-known-good cache instead of halting on a CDN blip.
func (u *Updater) fetchControl(ctx context.Context) (*UpdateControl, error) {
	url := u.controlURL()
	resp, err := getReleaseAsset(ctx, u.httpClient, url)
	if err != nil {
		return nil, transient(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no control file published — permissive default
	}
	if resp.StatusCode != http.StatusOK {
		e := fmt.Errorf("control %s: HTTP %d", updateAssetLabel(url), resp.StatusCode)
		if retryableStatus(resp.StatusCode) {
			return nil, transient(e)
		}
		return nil, e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// Same verified-signature path as the manifest, against <url>.sig.
	if err := u.verifySignatureAt(ctx, detachedSigURL(url), body); err != nil {
		return nil, err
	}
	var c UpdateControl
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("parse control: %w", err)
	}
	return &c, nil
}

// controlFallbackLocked decides what control to use when a fresh fetch failed.
// Must be called with u.mu held (it reads the cached control + fetch time).
// Within controlStaleTTL of the last good fetch we ride the cache (don't let a
// transient blip freeze the fleet); past it, fail SAFE with a synthesized pause.
func (u *Updater) controlFallbackLocked(fetchErr error) *UpdateControl {
	if u.control != nil && !u.controlFetchedAt.IsZero() && time.Since(u.controlFetchedAt) < controlStaleTTL {
		return u.control
	}
	slog.Warn("update control unreachable and cache stale; halting updates", "err", fetchErr)
	return &UpdateControl{Paused: true, Message: "update control unreachable"}
}

// blocksApply reports whether the control document forbids applying `candidate`
// to THIS daemon, with a human reason. Precedence: the kill switch (paused /
// halt_*) always wins over the staged-rollout cohort gate. A nil control never
// blocks (new-daemon-meets-absent-control compatibility).
func (c *UpdateControl) blocksApply(candidate, currentDaemon, machineID string) (bool, string) {
	if c == nil {
		return false, ""
	}
	if c.Paused {
		return true, "updates paused by operator"
	}
	if c.HaltAtOrAbove != "" && compareVersions(candidate, c.HaltAtOrAbove) >= 0 {
		return true, "release " + candidate + " quarantined (halt_at_or_above " + c.HaltAtOrAbove + ")"
	}
	if c.HaltBelow != "" && compareVersions(candidate, c.HaltBelow) < 0 {
		return true, "release " + candidate + " below halt_below " + c.HaltBelow
	}
	// A daemon below the mandatory floor bypasses the staged-rollout cohort:
	// min_daemon_version means "every machine must reach at least this", which
	// would deadlock against a canary percentage for machines outside the
	// cohort. Deliberately AFTER paused/halt_* — a known-bad release must never
	// apply, floor or not.
	if versionBelowFloor(currentDaemon, c.MinDaemonVer) {
		return false, ""
	}
	if !c.rolloutAllows(candidate, machineID) {
		return true, "not in current rollout cohort"
	}
	return false, ""
}

// blocksRollback is deliberately asymmetric with blocksApply. A recovery
// rollback consults only hard user-protection floors: min_daemon_version and
// halt_below. paused, halt_at_or_above, and rollout_percent control forward
// shipping and must not strand recovery after a bad release (or while a stale
// control cache synthesizes a floor-free pause).
func (c *UpdateControl) blocksRollback(target, currentDaemon string) (bool, string) {
	if c == nil {
		return false, ""
	}
	if c.MinDaemonVer != "" && compareVersions(target, c.MinDaemonVer) < 0 {
		return true, "release " + target + " below min_daemon_version " + c.MinDaemonVer
	}
	if c.HaltBelow != "" && compareVersions(target, c.HaltBelow) < 0 {
		return true, "release " + target + " below halt_below " + c.HaltBelow
	}
	return false, ""
}

// versionBelowFloor reports whether current sits below an operator-mandated
// minimum version. An empty floor means "no floor"; empty and "dev" currents
// are exempt — compareVersions strips non-numeric segments, so "dev" would
// otherwise parse as 0 and permanently flag every `go run` daemon.
func versionBelowFloor(current, floor string) bool {
	if floor == "" || current == "" || current == "dev" {
		return false
	}
	return compareVersions(current, floor) < 0
}

// requiredReason builds the human sentence surfaced in the blocking UI, with
// the operator's control-file message appended when present (that's where
// "critical fix for X" belongs — it's settable without cutting a release).
func requiredReason(what, floor, opMsg string) string {
	r := what + " is below the required minimum " + floor
	if opMsg != "" {
		r += " — " + opMsg
	}
	return r
}

// rolloutAllows reports whether this machine is inside the rollout cohort for
// `candidate`. Deterministic per (machineID, candidate): the SAME machine gets
// the SAME yes/no for a given version, so promoting 5%→100% only ever ADDS
// machines (monotonic), never flips one back out. Hashing the version too means
// each release reshuffles, so the canary cohort isn't the same unlucky machines.
func (c *UpdateControl) rolloutAllows(candidate, machineID string) bool {
	pct := 100
	if c != nil && c.RolloutPercent != nil {
		pct = *c.RolloutPercent
	}
	if pct >= 100 {
		return true
	}
	if pct <= 0 {
		return false
	}
	h := sha256.Sum256([]byte(machineID + "|" + candidate))
	bucket := int(binary.BigEndian.Uint32(h[:4]) % 100) // 0..99
	return bucket < pct
}
