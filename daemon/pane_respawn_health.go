package main

import (
	"errors"
	"time"
)

// Health check shared by every in-place kill→respawn (resume, refresh, restart,
// role/effort/model/context-pack changes): respawnInPlace retries a respawn that
// dies inside the grace window, which is the session-lock / socket handoff race.

// switchModeHealthGrace is how long we wait after a respawn to confirm the
// agent didn't die on us (the `--resume` failure mode). A bad --resume exits
// within a fraction of a second.
const switchModeHealthGrace = 600 * time.Millisecond

// switchModeHealthFloor is the MINIMUM alive time before a positive health
// signal (first output) may end the grace early. A process that prints an
// error and exits does both within this floor, so requiring signal AND floor
// keeps the quick-exit detection intact while letting the common healthy
// respawn confirm in ~250ms.
const switchModeHealthFloor = 250 * time.Millisecond

// switchModeRetryDelay is how long a respawn waits before each retry, letting
// the just-killed agent's child fully release the reused session lock (and any
// stale socket settle) so the retried `--resume` wins.
const switchModeRetryDelay = 700 * time.Millisecond

// switchModeSpawnAttempts is the total spawn tries (1 initial + retries) before
// a respawn gives up. Each retry waits switchModeRetryDelay.
const switchModeSpawnAttempts = 3

// confirmSwitchHealthy verifies a freshly-respawned pane actually came up: a
// `claude --resume` can die immediately when the just-killed agent's child has
// not released the session lock yet (the handoff race), or when the session
// can't be replayed. A process that exits inside the grace window is a failure
// and the caller retries.
//
// The grace is a CAP, not a fixed cost: once the pane has been alive for
// switchModeHealthFloor AND has painted its first PTY output, we confirm early.
// fullGrace=true (retry attempts) disables the early exit: retries follow a
// session-lock/socket race, exactly where a process can emit output and STILL
// die late, so they wait out the whole window.
func (a *apiServer) confirmSwitchHealthy(p *Pane, fullGrace bool) error {
	if p == nil {
		return errPaneRespawnNil
	}
	start := time.Now()
	deadline := start.Add(switchModeHealthGrace)
	floor := start.Add(switchModeHealthFloor)
	for time.Now().Before(deadline) {
		if p.Snapshot().Status == PaneExited {
			return errNativeSessionDied
		}
		if !fullGrace && !time.Now().Before(floor) && p.buffer.TotalWritten() > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.Snapshot().Status == PaneExited {
		return errNativeSessionDied
	}
	return nil
}

// errNativeSessionDied is returned by the respawn health check when a freshly
// respawned pane exits within the grace window — the classic `--resume <id>`
// failure (the session can't be replayed on this machine).
var errNativeSessionDied = errors.New("agent exited immediately after respawn (resume failed)")

// errPaneRespawnNil guards respawns against a spawn hook that returns
// (nil, nil) — treated as a respawn failure rather than dereferencing nil.
var errPaneRespawnNil = errors.New("respawn returned no pane")
