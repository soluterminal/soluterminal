package main

// Hardened workflow git subprocess runner. Every git invocation is isolated in
// its own process group, bounded by a process-wide timeout, and retried once
// after a timeout. In particular, the post-kill wait is also bounded: a lost
// child exit notification must never strand the landing coordinator forever.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	gitTimeoutDefault = 120 * time.Second
	gitTimeoutMin     = 5 * time.Second
	gitTimeoutMax     = 3600 * time.Second
	gitKillGrace      = 5 * time.Second
)

var (
	gitTimeoutOnce   sync.Once
	gitTimeoutCached time.Duration
)

func gitTimeoutFromEnv(raw string) time.Duration {
	timeout := gitTimeoutDefault
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return timeout
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return timeout
	}
	if seconds < int(gitTimeoutMin/time.Second) {
		seconds = int(gitTimeoutMin / time.Second)
	}
	if seconds > int(gitTimeoutMax/time.Second) {
		seconds = int(gitTimeoutMax / time.Second)
	}
	return time.Duration(seconds) * time.Second
}

// gitTimeout resolves the engine-wide git timeout once for the process.
// SOLU_GIT_TIMEOUT_SEC is deliberately not hot-reloadable.
func gitTimeout() time.Duration {
	gitTimeoutOnce.Do(func() {
		gitTimeoutCached = gitTimeoutFromEnv(os.Getenv("SOLU_GIT_TIMEOUT_SEC"))
	})
	return gitTimeoutCached
}

type gitAttempt struct {
	out           string
	err           error
	exitCode      int
	timedOut      bool
	waitAbandoned bool
	duration      time.Duration
}

type gitRunOnceFunc func(dir string, timeout time.Duration, args []string) gitAttempt

var gitRunner = struct {
	sync.RWMutex
	runOnce gitRunOnceFunc
}{runOnce: gitRunOnceExec}

func gitRunAttempt(dir string, timeout time.Duration, args []string) gitAttempt {
	gitRunner.RLock()
	runOnce := gitRunner.runOnce
	gitRunner.RUnlock()
	return gitRunAttemptUsing(dir, timeout, args, runOnce)
}

func gitRunAttemptUsing(dir string, timeout time.Duration, args []string, runOnce gitRunOnceFunc) gitAttempt {
	started := time.Now()
	var result gitAttempt
	for collision := 0; ; collision++ {
		result = runOnce(dir, timeout-time.Since(started), args)
		// Git refuses before writing when another process holds index.lock.
		// Give short-lived index refreshes a bounded chance to finish; never
		// delete a lock or retry unrelated failures and timeouts.
		if collision >= 4 || result.timedOut || result.err == nil || result.exitCode != 128 ||
			!strings.Contains(result.out, "fatal: Unable to create '") ||
			!strings.Contains(strings.ReplaceAll(result.out, "\\", "/"), "/index.lock': File exists.") {
			return result
		}
		delay := time.Duration(collision+1) * 100 * time.Millisecond
		if time.Since(started)+delay >= timeout {
			return result
		}
		time.Sleep(delay)
	}
}

// setGitRunOnceForTest swaps the attempt seam without racing live driver
// goroutines. Tests using it must not call t.Parallel.
func setGitRunOnceForTest(runOnce gitRunOnceFunc) func() {
	gitRunner.Lock()
	previous := gitRunner.runOnce
	gitRunner.runOnce = runOnce
	gitRunner.Unlock()
	return func() {
		gitRunner.Lock()
		gitRunner.runOnce = previous
		gitRunner.Unlock()
	}
}

// gitRunOnceExec does not give the command a cancellable context. The
// timeout path kills the entire process group and then bounds Cmd.Wait itself,
// covering both descendant processes and the lost-exit-notification incident
// class.
func gitRunOnceExec(dir string, timeout time.Duration, args []string) gitAttempt {
	return gitRunOnceExecIndex(dir, timeout, args, "")
}

// An alternate index uses exactly the shared process-group, timeout and wait
// bounds. The override is explicit and never mutates the daemon environment
// (hardenedGitCommand drops any inherited GIT_INDEX_FILE).
func gitRunOnceExecIndex(dir string, timeout time.Duration, args []string, index string) gitAttempt {
	cmd := hardenedGitCommand(context.Background(), dir, args...)
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
	return gitRunOnceCmd(cmd, timeout)
}

func gitRunOnceCmd(cmd *exec.Cmd, timeout time.Duration) (result gitAttempt) {
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	// Workflow children must remain in the daemon's Windows Job Object. The
	// no-breakaway variant still creates a fresh process group and cannot fail
	// with ERROR_ACCESS_DENIED under a restrictive parent job. On Unix it is a
	// Setsid process group, as before.
	applyDetachedNoBreakaway(cmd)

	started := time.Now()
	if err := startWithoutCoreDumps(cmd); err != nil {
		return gitAttempt{
			out:      output.String(),
			err:      err,
			exitCode: -1,
			duration: time.Since(started),
		}
	}
	defer func() {
		if scope, err := scopeForProcess(cmd.Process); err == nil && scope != nil {
			if _, err := stopOwnedProcessScope(scope, true); err != nil {
				result.err = errors.Join(result.err, fmt.Errorf("git child scope did not stop: %w", err))
				result.exitCode = -1
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- waitOwnedCommand(cmd) }()

	select {
	case err := <-done:
		return gitAttempt{
			out:      output.String(),
			err:      err,
			exitCode: processExitCode(err),
			duration: time.Since(started),
		}
	case <-time.After(timeout):
		_ = killChildTree(cmd.Process)
		err, abandoned := waitOrAbandon(done, gitKillGrace)
		attempt := gitAttempt{
			err:           err,
			exitCode:      -1,
			timedOut:      true,
			waitAbandoned: abandoned,
			duration:      time.Since(started),
		}
		if abandoned {
			// Cmd.Wait may still be blocked and its copy goroutine may still write
			// output. Do not read the shared buffer on this path.
			attempt.out = "process-group killed; wait abandoned (exit notification lost)"
			return attempt
		}
		attempt.out = output.String()
		return attempt
	}
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type gitLastWedge struct {
	Command      string
	RepoKey      string
	LandLockHeld bool
	At           time.Time
}

var gitMetricsState struct {
	SlowCalls               atomic.Int64
	Timeouts                atomic.Int64
	TimeoutRetriesSucceeded atomic.Int64
	TimeoutFailures         atomic.Int64

	mu        sync.Mutex
	lastWedge *gitLastWedge
}

type gitLandProbeFunc func(dir string) (repoKey string, held bool)

// gitLandProbe is installed by newWorkflowEngine. Atomic replacement keeps
// test engines and live driver goroutines race-free.
var gitLandProbe atomic.Pointer[gitLandProbeFunc]

func installGitLandProbe(probe gitLandProbeFunc) func() {
	previous := gitLandProbe.Load()
	gitLandProbe.Store(&probe)
	return func() { gitLandProbe.Store(previous) }
}

func gitProbe(dir string) (string, bool) {
	probe := gitLandProbe.Load()
	if probe == nil {
		return dir, false
	}
	return (*probe)(dir)
}

func gitRecordSlow(dir string, timeout time.Duration, args []string, attempt gitAttempt) {
	repoKey, held := gitProbe(dir)
	command := "git -C " + dir
	if len(args) > 0 {
		command += " " + strings.Join(args, " ")
	}
	gitMetricsState.SlowCalls.Add(1)
	gitMetricsState.mu.Lock()
	gitMetricsState.lastWedge = &gitLastWedge{
		Command:      command,
		RepoKey:      repoKey,
		LandLockHeld: held,
		At:           time.Now().UTC(),
	}
	gitMetricsState.mu.Unlock()
	log.Printf("workflow: WARN git slow: cwd=%s argCount=%d took %s (timeout %s, repo=%s, landLockHeld=%v)",
		dir, len(args), attempt.duration, timeout, repoKey, held)
}

func gitMetricsSnapshot() *statsGit {
	snapshot := &statsGit{
		SlowCalls:               gitMetricsState.SlowCalls.Load(),
		Timeouts:                gitMetricsState.Timeouts.Load(),
		TimeoutRetriesSucceeded: gitMetricsState.TimeoutRetriesSucceeded.Load(),
		TimeoutFailures:         gitMetricsState.TimeoutFailures.Load(),
	}
	gitMetricsState.mu.Lock()
	if last := gitMetricsState.lastWedge; last != nil {
		snapshot.LastWedge = &statsGitWedge{
			Command:      last.Command,
			RepoKey:      last.RepoKey,
			LandLockHeld: last.LandLockHeld,
			At:           last.At,
		}
	}
	gitMetricsState.mu.Unlock()
	return snapshot
}

func runGitResult(dir string, args ...string) (string, int, error) {
	timeout := gitTimeout()
	for attemptNumber := 1; attemptNumber <= 2; attemptNumber++ {
		attempt := gitRunAttempt(dir, timeout, args)
		if attempt.timedOut || attempt.duration > timeout/2 {
			gitRecordSlow(dir, timeout, args, attempt)
		}
		if attempt.timedOut {
			if attemptNumber == 1 {
				// Timeouts counts wedged logical calls, not attempts. A failure of
				// the retry is represented separately by TimeoutFailures.
				gitMetricsState.Timeouts.Add(1)
				continue
			}
			gitMetricsState.TimeoutFailures.Add(1)
			return "", -1, fmt.Errorf("git %s: timed out after %s (2 attempts, group killed)", strings.Join(args, " "), timeout)
		}
		if attempt.err != nil {
			return attempt.out, attempt.exitCode, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), attempt.err, gitFailureReason(attempt.out))
		}
		if attemptNumber == 2 {
			gitMetricsState.TimeoutRetriesSucceeded.Add(1)
		}
		return strings.TrimSpace(attempt.out), 0, nil
	}
	panic("unreachable")
}

func runGit(dir string, args ...string) (string, error) {
	out, _, err := runGitResult(dir, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// runGitFetch is runGit for gitFetchCommand: the release and preview
// checkouts' `git fetch <remote> <refspec>`, the one git allowed a transport.
func runGitFetch(dir, remote, refspec string) error {
	_, err := gitRawUsing(dir, []string{"fetch", remote, refspec}, func(dir string, timeout time.Duration, _ []string) gitAttempt {
		return gitRunOnceCmd(gitFetchCommand(context.Background(), dir, remote, refspec), timeout)
	})
	return err
}

// gitRaw is the binary-safe counterpart of runGit. Git's -z formats may
// begin with a significant ASCII space (for example the unstaged column of
// status --porcelain), so trimming at the command boundary would corrupt the
// first record. It retains the same bounded retry and wedge accounting as
// runGitResult while returning successful stdout byte-for-byte.
func gitRaw(dir string, args ...string) (string, error) {
	return gitRawUsing(dir, args, gitRunAttempt)
}

func gitRawWithIndex(dir, index string, args ...string) (string, error) {
	return gitRawUsing(dir, args, func(dir string, timeout time.Duration, args []string) gitAttempt {
		return gitRunAttemptUsing(dir, timeout, args, func(dir string, timeout time.Duration, args []string) gitAttempt {
			return gitRunOnceExecIndex(dir, timeout, args, index)
		})
	})
}

func gitRawUsing(dir string, args []string, runAttempt gitRunOnceFunc) (string, error) {
	timeout := gitTimeout()
	for attemptNumber := 1; attemptNumber <= 2; attemptNumber++ {
		attempt := runAttempt(dir, timeout, args)
		if attempt.timedOut || attempt.duration > timeout/2 {
			gitRecordSlow(dir, timeout, args, attempt)
		}
		if attempt.timedOut {
			if attemptNumber == 1 {
				gitMetricsState.Timeouts.Add(1)
				continue
			}
			gitMetricsState.TimeoutFailures.Add(1)
			return "", fmt.Errorf("git %s: timed out after %s (2 attempts, group killed)", strings.Join(args, " "), timeout)
		}
		if attempt.err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), attempt.err, gitFailureReason(attempt.out))
		}
		if attemptNumber == 2 {
			gitMetricsState.TimeoutRetriesSucceeded.Add(1)
		}
		return attempt.out, nil
	}
	panic("unreachable")
}
