package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWfGitTimeoutRetriesOnceThenErrors(t *testing.T) {
	calls := 0
	restore := setGitRunOnceForTest(func(string, time.Duration, []string) gitAttempt {
		calls++
		return gitAttempt{timedOut: true, waitAbandoned: true, duration: gitTimeout()}
	})
	defer restore()
	before := gitMetricsSnapshot()
	done := make(chan error, 1)
	go func() {
		_, err := runGit(t.TempDir(), "status", "--short")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runGit blocked after the simulated lost exit notification")
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2", calls)
	}
	after := gitMetricsSnapshot()
	if after.Timeouts-before.Timeouts != 1 {
		t.Fatalf("timeouts delta = %d, want 1", after.Timeouts-before.Timeouts)
	}
	if after.TimeoutFailures-before.TimeoutFailures != 1 {
		t.Fatalf("timeout failures delta = %d, want 1", after.TimeoutFailures-before.TimeoutFailures)
	}
}

func TestWfGitTimeoutRetrySucceeds(t *testing.T) {
	calls := 0
	restore := setGitRunOnceForTest(func(string, time.Duration, []string) gitAttempt {
		calls++
		if calls == 1 {
			return gitAttempt{timedOut: true, duration: gitTimeout()}
		}
		return gitAttempt{out: " recovered\n"}
	})
	defer restore()
	before := gitMetricsSnapshot()
	out, err := runGit(t.TempDir(), "rev-parse", "HEAD")
	if err != nil || out != "recovered" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2", calls)
	}
	after := gitMetricsSnapshot()
	if after.TimeoutRetriesSucceeded-before.TimeoutRetriesSucceeded != 1 {
		t.Fatalf("retry successes delta = %d, want 1", after.TimeoutRetriesSucceeded-before.TimeoutRetriesSucceeded)
	}
}

func TestWfGitNonTimeoutErrorNoRetry(t *testing.T) {
	calls := 0
	restore := setGitRunOnceForTest(func(string, time.Duration, []string) gitAttempt {
		calls++
		return gitAttempt{out: " fatal output\n", err: errors.New("exit status 2"), exitCode: 2}
	})
	defer restore()
	_, err := runGit(t.TempDir(), "status")
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1", calls)
	}
	if got, want := err.Error(), "git status: exit status 2: fatal output"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestWfGitIndexLockCollisionIsBounded(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(strconv.FormatBool(persistent), func(t *testing.T) {
			calls := 0
			restore := setGitRunOnceForTest(func(string, time.Duration, []string) gitAttempt {
				calls++
				if !persistent && calls > 1 {
					return gitAttempt{out: "checkpoint ready"}
				}
				return gitAttempt{exitCode: 128, err: errors.New("exit status 128"), out: "fatal: Unable to create '/repo/.git/worktrees/task/index.lock': File exists."}
			})
			defer restore()
			out, err := runGit(t.TempDir(), "rm", "--cached", "--ignore-unmatch", "cache")
			if persistent {
				if err == nil || calls != 5 {
					t.Fatalf("persistent lock: calls=%d err=%v", calls, err)
				}
			} else if err != nil || out != "checkpoint ready" || calls != 2 {
				t.Fatalf("transient lock: calls=%d out=%q err=%v", calls, out, err)
			}
		})
	}
}

func TestWfGitIndexLockNeverRemoved(t *testing.T) {
	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte("owned by another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "change"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "change"); err == nil {
		t.Fatal("ignored another writer's lock")
	}
	if raw, err := os.ReadFile(lock); err != nil || string(raw) != "owned by another writer" {
		t.Fatal("changed another writer's lock")
	}
}

func TestWfWaitOrAbandon(t *testing.T) {
	started := time.Now()
	err, abandoned := waitOrAbandon(make(chan error), 25*time.Millisecond)
	if err != nil || !abandoned {
		t.Fatalf("err=%v abandoned=%v, want nil,true", err, abandoned)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded wait took %s", elapsed)
	}
}

func TestWfGitTimeoutConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{value: "", want: 120 * time.Second},
		{value: " 45 ", want: 45 * time.Second},
		{value: "2", want: 5 * time.Second},
		{value: "7200", want: 3600 * time.Second},
		{value: "invalid", want: 120 * time.Second},
	} {
		if got := gitTimeoutFromEnv(test.value); got != test.want {
			t.Fatalf("SOLU_GIT_TIMEOUT_SEC=%q: got %s want %s", test.value, got, test.want)
		}
	}
}

func TestWfGitSlowWarningAndSnapshot(t *testing.T) {
	originalWriter := log.Writer()
	defer log.SetOutput(originalWriter)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	restoreProbe := installGitLandProbe(func(string) (string, bool) { return "repo-key", true })
	defer restoreProbe()
	restoreRun := setGitRunOnceForTest(func(string, time.Duration, []string) gitAttempt {
		return gitAttempt{out: "ok", duration: gitTimeout()/2 + time.Second}
	})
	defer restoreRun()
	before := gitMetricsSnapshot()
	if _, err := runGit("/repo", "status", "--short"); err != nil {
		t.Fatal(err)
	}
	after := gitMetricsSnapshot()
	if after.SlowCalls-before.SlowCalls != 1 || after.LastWedge == nil {
		t.Fatalf("git metrics = %+v", after)
	}
	if after.Timeouts != before.Timeouts {
		t.Fatalf("slow successful call incremented timeouts: before=%d after=%d", before.Timeouts, after.Timeouts)
	}
	if after.LastWedge.Command != "git -C /repo status --short" || after.LastWedge.RepoKey != "repo-key" || !after.LastWedge.LandLockHeld {
		t.Fatalf("last wedge = %+v", after.LastWedge)
	}
	if strings.Contains(logs.String(), "status --short") {
		t.Fatal("command arguments reached application log")
	}
	for _, want := range []string{"workflow: WARN git slow", "cwd=/repo argCount=2", "timeout", "repo=repo-key", "landLockHeld=true"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("warning missing %q: %s", want, logs.String())
		}
	}
}

func TestWfGitExitCodeAndOutputPreserved(t *testing.T) {
	restore := setGitRunOnceForTest(func(string, time.Duration, []string) gitAttempt {
		return gitAttempt{out: "different\n", err: errors.New("exit status 1"), exitCode: 1}
	})
	defer restore()

	out, exitCode, err := runGitResult(t.TempDir(), "diff", "--no-index")
	if out != "different\n" || exitCode != 1 || err == nil {
		t.Fatalf("out=%q exitCode=%d err=%v", out, exitCode, err)
	}
}

func TestGitHeadShaTimeoutReturnsEmpty(t *testing.T) {
	calls := 0
	restore := setGitRunOnceForTest(func(_ string, timeout time.Duration, _ []string) gitAttempt {
		calls++
		return gitAttempt{timedOut: true, waitAbandoned: true, duration: timeout}
	})
	defer restore()

	if got := gitHeadSha("/repo"); got != "" {
		t.Fatalf("gitHeadSha=%q, want empty on timeout", got)
	}
	if calls != 2 {
		t.Fatalf("attempts=%d, want 2", calls)
	}
}

func TestWfGitRealProcessGroupKill(t *testing.T) {
	for _, index := range []string{"", "alternate-index"} {
		t.Run("index="+index, func(t *testing.T) {
			if _, err := exec.LookPath("sh"); err != nil {
				t.Skipf("sh unavailable: %v", err)
			}
			temp := t.TempDir()
			pidFile := filepath.Join(temp, "git.pid")
			childPIDFile := filepath.Join(temp, "git-child.pid")
			shim := filepath.Join(temp, "git")
			script := "#!/bin/sh\nprintf '%s\\n' \"$$\" > \"$WF_GIT_SHIM_PID_FILE\"\nsleep 300 &\nprintf '%s\\n' \"$!\" > \"$WF_GIT_CHILD_PID_FILE\"\nexec sleep 300\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WF_GIT_SHIM_PID_FILE", pidFile)
			t.Setenv("WF_GIT_CHILD_PID_FILE", childPIDFile)
			t.Setenv("PATH", temp+string(os.PathListSeparator)+os.Getenv("PATH"))

			started := time.Now()
			attempt := gitRunOnceExecIndex(temp, time.Second, []string{"status"}, index)
			if !attempt.timedOut {
				t.Fatalf("attempt = %+v, want timeout", attempt)
			}
			if elapsed := time.Since(started); elapsed > 7*time.Second {
				t.Fatalf("timeout and bounded kill wait took %s", elapsed)
			}
			for _, pidPath := range []string{pidFile, childPIDFile} {
				raw, err := os.ReadFile(pidPath)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				for processAlive(pid) && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if processAlive(pid) {
					t.Fatalf("git process %d from %s remains alive after process-group kill", pid, filepath.Base(pidPath))
				}
			}
		})
	}
}

func TestWfGitAlternateIndexOverridesInheritedIndex(t *testing.T) {
	repo := testGitRepo(t)
	testRepoCommit(t, repo, "tracked.txt", "original\n", "fixture")
	realIndex, err := runGit(repo, "rev-parse", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(repo, realIndex)
	}
	before, err := os.ReadFile(realIndex)
	if err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(alternate, before, 0600); err != nil {
		t.Fatal(err)
	}
	inherited := filepath.Join(t.TempDir(), "must-not-be-written")
	t.Setenv("GIT_INDEX_FILE", inherited)
	if _, err := gitRawWithIndex(repo, alternate, "update-index", "--force-remove", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	out, err := gitRawWithIndex(repo, alternate, "ls-files", "-z")
	if err != nil || strings.Contains(out, "tracked.txt") {
		t.Fatalf("alternate index not selected: %q %v", out, err)
	}
	after, err := os.ReadFile(realIndex)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("alternate index operation changed real index")
	}
	if _, err := os.Stat(inherited); !os.IsNotExist(err) || os.Getenv("GIT_INDEX_FILE") != inherited {
		t.Fatal("alternate index operation changed inherited environment or index")
	}
}

func gitHeadSha(dir string) string {
	v, _ := runGit(dir, "rev-parse", "HEAD")
	return strings.TrimSpace(v)
}
