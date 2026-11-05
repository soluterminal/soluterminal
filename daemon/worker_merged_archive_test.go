package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// workerStartFixture is a finished worker whose lead never read its reply,
// started in the main checkout of a repository that ignores .claude/worktrees.
func workerStartFixture(t *testing.T, setup ...func(repo string)) (*apiServer, *Pane, time.Time) {
	t.Helper()
	a, p, now := archiveFixture(t)
	repo := p.Cwd
	putCapabilityFile(t, filepath.Join(repo, ".gitignore"), "/.claude/worktrees/\n")
	archiveGit(t, repo, "add", ".gitignore")
	archiveGit(t, repo, "commit", "-m", "ignore worktrees")
	archiveGit(t, repo, "push", "origin", "main")
	for _, fn := range setup {
		fn(repo)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := captureWorkerGitStart(ctx, repo)
	if start == nil {
		t.Fatal("no worker git start")
	}
	p.mu.Lock()
	o := p.Origin.clone()
	o.WorkerGit = start
	p.Origin = o
	p.mu.Unlock()
	stampTranscriptCwd(t, p, repo)
	a.workerArchives().liveCwd = func(*Pane) string { return "" }
	q := a.workers()
	q.mu.Lock()
	q.settings.WorkerMergedGraceMinutes = 10
	q.mu.Unlock()
	return a, p, now
}

// stampTranscriptCwd records cwd as where the worker's session worked, as
// Claude stamps every transcript entry; the process cwd plays no part.
func stampTranscriptCwd(t *testing.T, p *Pane, cwd string) {
	t.Helper()
	raw, err := os.ReadFile(p.ClaudeTranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		line = strings.Replace(line, `{"cwd":`, `{"was":`, 1)
		lines = append(lines, strings.Replace(line, `{`, `{"cwd":`+strconv.Quote(cwd)+`,`, 1))
	}
	putCapabilityFile(t, p.ClaudeTranscriptPath, strings.Join(lines, "\n")+"\n")
}

// workerRan runs fn as one of the worker's own tool calls: a Claude tool_use
// with input and its tool_result carrying fn's output, recorded in the
// worker's transcript ahead of its final reply.
func workerRan(t *testing.T, p *Pane, input string, fn func() string) {
	t.Helper()
	start := time.Now()
	output := fn()
	end := time.Now()
	raw, err := os.ReadFile(p.ClaudeTranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	id := "toolu_" + strconv.FormatInt(start.UnixNano(), 36)
	use := `{"type":"assistant","timestamp":` + strconv.Quote(start.Format(time.RFC3339Nano)) + `,"message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":` + strconv.Quote(input) + `}}]}}`
	result := `{"type":"user","timestamp":` + strconv.Quote(end.Format(time.RFC3339Nano)) + `,"message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + strconv.Quote(output) + `}]}}`
	lines = append(lines[:len(lines)-1], use, result, lines[len(lines)-1])
	putCapabilityFile(t, p.ClaudeTranscriptPath, strings.Join(lines, "\n")+"\n")
}

// workerCreatesWorktree is the worker adding .claude/worktrees/name on a new
// branch from start, as its own tool call.
func workerCreatesWorktree(t *testing.T, p *Pane, repo, name, branch, start string) string {
	t.Helper()
	wt := filepath.Join(repo, ".claude", "worktrees", name)
	workerRan(t, p, "git worktree add -b "+branch+" .claude/worktrees/"+name+" "+start, func() string {
		return archiveGit(t, repo, "worktree", "add", "-b", branch, wt, start)
	})
	return wt
}

// workerCommits is the worker committing everything in cwd as its own tool call.
func workerCommits(t *testing.T, p *Pane, cwd, msg string) {
	t.Helper()
	workerRan(t, p, "git add -A && git commit -m "+strconv.Quote(msg), func() string {
		archiveGit(t, cwd, "add", "-A")
		return archiveGit(t, cwd, "commit", "-m", msg)
	})
}

// mergedWorkerFixture: the worker also created .claude/worktrees/w on branch
// feat, committed one change there and pushed the branch. Nothing is merged.
func mergedWorkerFixture(t *testing.T, setup ...func(repo string)) (*apiServer, *Pane, time.Time, string) {
	t.Helper()
	a, p, now := workerStartFixture(t, setup...)
	repo := p.Cwd
	wt := workerCreatesWorktree(t, p, repo, "w", "feat", "origin/main")
	putCapabilityFile(t, filepath.Join(wt, "feature.txt"), "worker change\n")
	workerCommits(t, p, wt, "worker change")
	archiveGit(t, wt, "push", "-u", "origin", "feat")
	stampTranscriptCwd(t, p, wt)
	return a, p, now, wt
}

// otherCommitOnMain lands someone else's commit on the remote default branch
// and refreshes the shared remote-tracking ref.
func otherCommitOnMain(t *testing.T, repo, name string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	archiveGit(t, repo, "clone", "-q", "-b", "main", archiveGit(t, repo, "remote", "get-url", "origin"), other)
	putCapabilityFile(t, filepath.Join(other, name), "someone else\n")
	archiveGit(t, other, "add", name)
	archiveGit(t, other, "commit", "-m", "someone else's "+name)
	archiveGit(t, other, "push", "origin", "main")
	archiveGit(t, repo, "fetch", "-q", "origin")
}

// mergeFeat lands feat on the remote default branch the way GitHub's merge
// button does, then refreshes the shared remote-tracking ref.
func mergeFeat(t *testing.T, repo string) {
	t.Helper()
	merger := filepath.Join(t.TempDir(), "merger")
	archiveGit(t, repo, "clone", "-q", "-b", "main", archiveGit(t, repo, "remote", "get-url", "origin"), merger)
	archiveGit(t, merger, "merge", "--no-ff", "-m", "Merge feat", "origin/feat")
	archiveGit(t, merger, "push", "origin", "main")
	archiveGit(t, repo, "fetch", "-q", "origin")
}

func workerArchived(a *apiServer, p *Pane) bool {
	_, live := a.mgr.Get(p.ID)
	return !live && len(a.archivedWorkerSummaries()) == 1
}

func workerKept(t *testing.T, a *apiServer, p *Pane) {
	t.Helper()
	if _, ok := a.mgr.Get(p.ID); !ok || len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("worker was archived")
	}
}

func TestMergedCleanWorkerArchivesAndRemovesItsWorktree(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	mergeFeat(t, repo)
	a.tryArchiveWorker(p, now)
	workerKept(t, a, p) // grace starts at merge detection
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("merged clean worker was not archived")
	}
	if text, ok := a.archivedWorkerReply(p.ID); !ok || text != "Final answer" {
		t.Fatal("archived reply not readable")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("merged worktree kept: %v", err)
	}
	if out := archiveGit(t, repo, "branch", "--list", "feat"); !strings.Contains(out, "feat") {
		t.Fatal("worktree removal deleted the branch")
	}
}

func TestMergedWorkerGraceWindow(t *testing.T) {
	a, p, now, _ := mergedWorkerFixture(t)
	mergeFeat(t, p.Cwd)
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(9*time.Minute))
	workerKept(t, a, p)
	// Input after the reply (a lead's follow-up) cancels the merged close.
	atomic.StoreInt64(&p.LastInputMs, now.UnixMilli())
	p.inputRevision.Add(1)
	a.tryArchiveWorker(p, now.Add(11*time.Minute))
	workerKept(t, a, p)

	a, p, now, _ = mergedWorkerFixture(t)
	q := a.workers()
	q.mu.Lock()
	q.settings.WorkerMergedGraceMinutes = 30
	q.mu.Unlock()
	mergeFeat(t, p.Cwd)
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(29*time.Minute))
	workerKept(t, a, p)
	a.tryArchiveWorker(p, now.Add(30*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("configured grace did not archive")
	}
}

func TestMergedWorkerKeptUnlessEveryCheckoutIsCleanAndLanded(t *testing.T) {
	for _, kind := range []string{"dirty", "untracked", "unpushed", "pushed-unmerged", "no-own-commits", "main-edited-after-cherry-pick"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now, wt := mergedWorkerFixture(t)
			repo := p.Cwd
			switch kind {
			case "dirty":
				mergeFeat(t, repo)
				putCapabilityFile(t, filepath.Join(wt, "feature.txt"), "more\n")
			case "untracked":
				mergeFeat(t, repo)
				putCapabilityFile(t, filepath.Join(wt, "scratch.txt"), "notes\n")
			case "unpushed":
				mergeFeat(t, repo)
				putCapabilityFile(t, filepath.Join(wt, "feature.txt"), "follow-up\n")
				archiveGit(t, wt, "commit", "-am", "follow-up")
			case "pushed-unmerged":
			case "no-own-commits":
				// The worker's checkout is origin/main with nothing of its own.
				archiveGit(t, wt, "reset", "-q", "--hard", "origin/main")
				archiveGit(t, wt, "branch", "--unset-upstream")
			case "main-edited-after-cherry-pick":
				merger := filepath.Join(t.TempDir(), "merger")
				archiveGit(t, repo, "clone", "-q", "-b", "main", archiveGit(t, repo, "remote", "get-url", "origin"), merger)
				// A different parent, so the pick is a new commit, not feat itself.
				putCapabilityFile(t, filepath.Join(merger, "other.txt"), "someone else\n")
				archiveGit(t, merger, "add", "other.txt")
				archiveGit(t, merger, "commit", "-m", "unrelated")
				archiveGit(t, merger, "cherry-pick", archiveGit(t, repo, "rev-parse", "feat"))
				putCapabilityFile(t, filepath.Join(merger, "feature.txt"), "edited on main\n")
				archiveGit(t, merger, "commit", "-am", "edit after pick")
				archiveGit(t, merger, "push", "origin", "main")
				archiveGit(t, repo, "fetch", "-q", "origin")
			}
			a.tryArchiveWorker(p, now)
			a.tryArchiveWorker(p, now.Add(time.Hour))
			workerKept(t, a, p)
			if _, err := os.Stat(wt); err != nil {
				t.Fatal("kept worker lost its worktree")
			}
		})
	}
}

func TestCherryPickedWorkerCountsAsMerged(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	merger := filepath.Join(t.TempDir(), "merger")
	archiveGit(t, repo, "clone", "-q", "-b", "main", archiveGit(t, repo, "remote", "get-url", "origin"), merger)
	putCapabilityFile(t, filepath.Join(merger, "other.txt"), "someone else\n")
	archiveGit(t, merger, "add", "other.txt")
	archiveGit(t, merger, "commit", "-m", "unrelated")
	archiveGit(t, merger, "cherry-pick", archiveGit(t, repo, "rev-parse", "feat"))
	archiveGit(t, merger, "push", "origin", "main")
	archiveGit(t, repo, "fetch", "-q", "origin")
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("patch-equivalent worker was not archived")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("cherry-picked worktree kept")
	}
}

func TestMergedWorkerInMainCheckoutWithLocalChangesIsKept(t *testing.T) {
	// The person's own edit in the main checkout predates the worker, so
	// the existing proof exempts it; the merged close must not.
	a, p, now, _ := mergedWorkerFixture(t, func(repo string) {
		putCapabilityFile(t, filepath.Join(repo, "tracked.txt"), "person's edit\n")
	})
	mergeFeat(t, p.Cwd)
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(time.Hour))
	workerKept(t, a, p)
}

func TestMergedWorkerWorktreeRemovalFailureStillArchives(t *testing.T) {
	for _, kind := range []string{"locked", "another-pane-inside"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now, wt := mergedWorkerFixture(t)
			mergeFeat(t, p.Cwd)
			if kind == "locked" {
				// A locked worktree needs --force twice; the archive never uses it.
				archiveGit(t, p.Cwd, "worktree", "lock", wt)
			} else {
				a.workerArchives().liveCwd = func(*Pane) string { return filepath.Join(wt, "sub") }
				if err := os.Mkdir(filepath.Join(wt, "sub"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			a.tryArchiveWorker(p, now)
			a.tryArchiveWorker(p, now.Add(10*time.Minute))
			if !workerArchived(a, p) {
				t.Fatal("removal failure blocked the archive")
			}
			if _, err := os.Stat(filepath.Join(wt, "feature.txt")); err != nil {
				t.Fatalf("failed removal lost the folder: %v", err)
			}
		})
	}
}

func TestMergedWorkerOnlyRemovesWorktreesUnderClaudeWorktrees(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo := archiveRepo(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	archiveGit(t, repo, "worktree", "add", "-q", "-b", "x", outside)
	if err := workerGitRemoveWorktree(ctx, outside); err == nil {
		t.Fatal("removed a worktree outside .claude/worktrees")
	}
	if err := workerGitRemoveWorktree(ctx, repo); err == nil {
		t.Fatal("removed the main checkout")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestMergedLeadOrUserPaneIsNeverArchived(t *testing.T) {
	for _, kind := range []string{"lead", "user"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now, wt := mergedWorkerFixture(t)
			mergeFeat(t, p.Cwd)
			p.mu.Lock()
			o := p.Origin.clone()
			if kind == "lead" {
				o.TeamKind = paneTeamLead
			} else {
				o.Source, o.TeamKind = paneOriginUser, ""
			}
			p.Origin = o
			p.mu.Unlock()
			a.tryArchiveWorker(p, now)
			a.tryArchiveWorker(p, now.Add(time.Hour))
			workerKept(t, a, p)
			if _, err := os.Stat(wt); err != nil {
				t.Fatal("worktree removed for a non-worker")
			}
		})
	}
}

func TestTranscriptCwdReadsClaudeAndCodexShapes(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude.jsonl")
	putCapabilityFile(t, claude, `{"type":"user","cwd":"/repo"}`+"\n"+`{"type":"assistant","cwd":"/repo/.claude/worktrees/w"}`+"\n"+`{"type":"summary"}`+"\n")
	codex := filepath.Join(dir, "codex.jsonl")
	putCapabilityFile(t, codex, `{"type":"turn_context","payload":{"cwd":"/repo/.claude/worktrees/c"}}`+"\n"+`{"type":"response_item","payload":{"cwd":"ignored"}}`+"\n")
	relative := filepath.Join(dir, "relative.jsonl")
	putCapabilityFile(t, relative, `{"type":"assistant","cwd":"../escape"}`+"\n")
	for path, want := range map[string]string{claude: "/repo/.claude/worktrees/w", codex: "/repo/.claude/worktrees/c", relative: "", "": ""} {
		if got := transcriptCwd(path); got != want {
			t.Fatalf("%s: got %q want %q", path, got, want)
		}
	}
}

func TestSquashMergedWorkerWithPrunedBranchArchives(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	merger := filepath.Join(t.TempDir(), "merger")
	archiveGit(t, repo, "clone", "-q", "-b", "main", archiveGit(t, repo, "remote", "get-url", "origin"), merger)
	archiveGit(t, merger, "merge", "--squash", "origin/feat")
	archiveGit(t, merger, "commit", "-m", "Squash feat")
	archiveGit(t, merger, "push", "origin", "main")
	archiveGit(t, merger, "push", "origin", "--delete", "feat")
	archiveGit(t, repo, "fetch", "-q", "--prune", "origin")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, why := workerGitForArchive(ctx, p.Snapshot(), wt); why == "" {
		t.Fatal("fixture: pruned squash branch still looks published")
	}
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("squash-merged worker with a pruned branch was not archived")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("squash-merged worktree kept")
	}
}

func TestMergedWorktreeRemovalCannotExecuteDrivers(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	mergeFeat(t, repo)
	// Worker-writable attributes and config select a driver for every file,
	// and stale index stats force git status to re-read contents.
	marker := filepath.Join(t.TempDir(), "executed")
	payload := "touch '" + marker + "'"
	putCapabilityFile(t, filepath.Join(repo, ".git", "info", "attributes"), "* filter=Evil.x\n")
	for _, key := range []string{"filter.Evil.x.clean", "filter.Evil.x.smudge", "filter.Evil.x.process"} {
		archiveGit(t, repo, "config", key, payload)
	}
	archiveGit(t, repo, "config", "filter.Evil.x.required", "true")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt, "feature.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("merged worker was not archived")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("worker-controlled Git driver executed during cleanup")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("clean merged worktree kept")
	}
}

// Removal never runs git status (or git worktree remove) on the repository's
// own config: a driver added after any check could otherwise run.
func TestMergedWorktreeRemovalNeverRunsStatusOnRepositoryConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(repo, ".gitignore"), "/.claude/worktrees/\n")
	archiveGit(t, repo, "add", ".gitignore")
	archiveGit(t, repo, "commit", "-m", "ignore worktrees")
	wt := filepath.Join(repo, ".claude", "worktrees", "w")
	archiveGit(t, repo, "worktree", "add", "-q", "-b", "w", wt)
	putCapabilityFile(t, filepath.Join(wt, "build.log"), "ignored\n")
	putCapabilityFile(t, filepath.Join(wt, ".gitignore"), "/.claude/worktrees/\n/build.log\n")
	archiveGit(t, wt, "commit", "-qam", "ignore build output")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	calls := filepath.Join(shim, "calls")
	putCapabilityFile(t, filepath.Join(shim, "git"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+calls+"'\nexec '"+real+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(shim, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := workerGitRemoveWorktree(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("clean worktree kept")
	}
	if out := archiveGit(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, wt) {
		t.Fatalf("worktree entry kept: %s", out)
	}
	if out := archiveGit(t, repo, "branch", "--list", "w"); !strings.Contains(out, "w") {
		t.Fatal("branch deleted")
	}
	raw, _ := os.ReadFile(calls)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, "worktree remove") || (strings.Contains(line, " status ") && !strings.Contains(line, "--git-dir=")) {
			t.Fatalf("removal ran Git with repository config: %s", line)
		}
	}
	if !strings.Contains(string(raw), "status") {
		t.Fatal("removal skipped the clean proof")
	}
}

// A signed worker commit and repository config that verifies signatures in
// git log: the merged close must read the commit without running gpg.program.
func TestMergedWorkerSignedCommitNeverRunsGPGProgram(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	raw := archiveGit(t, wt, "cat-file", "commit", "HEAD")
	header, message, ok := strings.Cut(raw, "\n\n")
	if !ok {
		t.Fatal("unexpected commit object")
	}
	object := filepath.Join(t.TempDir(), "signed")
	putCapabilityFile(t, object, header+"\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEE\n -----END PGP SIGNATURE-----\n\n"+message+"\n")
	signed := archiveGit(t, wt, "hash-object", "-t", "commit", "-w", object)
	archiveGit(t, wt, "reset", "-q", "--soft", signed)
	archiveGit(t, wt, "commit", "-q", "--allow-empty", "-m", "worker change on the signed commit")
	archiveGit(t, wt, "push", "-q", "-f", "origin", "feat")
	mergeFeat(t, repo)
	sentinel := filepath.Join(t.TempDir(), "executed")
	gpg := filepath.Join(t.TempDir(), "gpg")
	putCapabilityFile(t, gpg, "#!/bin/sh\ntouch '"+sentinel+"'\nexit 1\n")
	if err := os.Chmod(gpg, 0700); err != nil {
		t.Fatal(err)
	}
	archiveGit(t, repo, "config", "log.showSignature", "true")
	archiveGit(t, repo, "config", "gpg.program", gpg)
	// Control: the setup does make git log run the program.
	cmd := exec.Command("git", "log", "-1", "--format=%ct", signed)
	cmd.Dir = wt
	_ = cmd.Run()
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("fixture: git log did not run gpg.program")
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("signed merged worker was not archived")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("merged close ran the repository's gpg.program")
	}
}

func TestWorkerGitEnvDropsEveryInheritedGitVariable(t *testing.T) {
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CEILING_DIRECTORIES", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_EXTERNAL_DIFF", "GIT_PAGER", "GIT_SSH_COMMAND", "GIT_ATTR_SOURCE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT"} {
		t.Setenv(name, "/inherited")
	}
	if runtime.GOOS != "windows" {
		t.Setenv("Git_Dir", "/inherited") // Windows treats this as GIT_DIR
	}
	t.Setenv("SOLU_KEEP", "1")
	var git []string
	kept := false
	for _, item := range hardenedGitEnv(os.DevNull) {
		if strings.HasPrefix(strings.ToUpper(item), "GIT_") {
			git = append(git, item)
		}
		kept = kept || item == "SOLU_KEEP=1"
	}
	if strings.Join(git, "\n") != strings.Join(slices.Concat(gitNoTransportEnv, gitNoUserConfigEnv, []string{"GIT_CONFIG_GLOBAL=" + os.DevNull}, gitFixedEnv), "\n") || !kept {
		t.Fatalf("worker git env: %q (other variables kept: %v)", git, kept)
	}
}

// A refs/replace graft that makes the worker's unmerged HEAD look like an
// ancestor of origin/main cannot make it "landed" or close the worker.
func TestMergedWorkerReplaceRefsCannotFakeLanding(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	head := archiveGit(t, wt, "rev-parse", "HEAD")
	tip := archiveGit(t, repo, "rev-parse", "refs/remotes/origin/main")
	archiveGit(t, repo, "replace", "--graft", tip, head)
	// Control: Git honouring replace refs does see the graft.
	if out, err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", head, "refs/remotes/origin/main").CombinedOutput(); err != nil {
		t.Fatalf("fixture: graft not visible: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if workerGitLanded(ctx, wt, head, workerGitDefaultRef(ctx, wt)) {
		t.Fatal("replace ref made unmerged work landed")
	}
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(time.Hour))
	workerKept(t, a, p)
}

// The worker's own merged work, not a newer HEAD, is what closes it. These
// workers have clean, landed checkouts but made no commit of their own.
func TestWorkerWithoutOwnCommitsKeepsReadReceiptRule(t *testing.T) {
	for _, kind := range []string{"pulled-main-checkout", "worktree-from-newer-main", "sibling-merged-worktree", "review-of-merged-branch", "lead-commit-in-start-checkout", "commit-during-unrelated-call", "own-worktree-without-commits"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := workerStartFixture(t)
			repo := p.Cwd
			worked := repo
			switch kind {
			case "pulled-main-checkout":
				otherCommitOnMain(t, repo, "other.txt")
				archiveGit(t, repo, "pull", "-q", "--ff-only")
			case "worktree-from-newer-main":
				otherCommitOnMain(t, repo, "other.txt")
				worked = filepath.Join(repo, ".claude", "worktrees", "r")
				archiveGit(t, repo, "worktree", "add", "-q", "-b", "research", worked, "origin/main")
			case "sibling-merged-worktree":
				// A sibling worker created, committed in and merged its own
				// worktree; this worker stayed in the main checkout.
				sib := filepath.Join(repo, ".claude", "worktrees", "sibling")
				archiveGit(t, repo, "worktree", "add", "-q", "-b", "feat", sib, "origin/main")
				putCapabilityFile(t, filepath.Join(sib, "sibling.txt"), "sibling change\n")
				archiveGit(t, sib, "add", "sibling.txt")
				archiveGit(t, sib, "commit", "-m", "sibling change")
				archiveGit(t, sib, "push", "-q", "-u", "origin", "feat")
				mergeFeat(t, repo)
			case "review-of-merged-branch":
				other := filepath.Join(t.TempDir(), "other")
				archiveGit(t, repo, "clone", "-q", "-b", "main", archiveGit(t, repo, "remote", "get-url", "origin"), other)
				archiveGit(t, other, "checkout", "-q", "-b", "feat")
				putCapabilityFile(t, filepath.Join(other, "theirs.txt"), "their change\n")
				archiveGit(t, other, "add", "theirs.txt")
				archiveGit(t, other, "commit", "-m", "their change")
				archiveGit(t, other, "push", "-q", "origin", "feat")
				mergeFeat(t, repo)
				worked = filepath.Join(repo, ".claude", "worktrees", "review")
				archiveGit(t, repo, "worktree", "add", "-q", "-b", "review", worked, "origin/feat")
			case "lead-commit-in-start-checkout", "commit-during-unrelated-call":
				// Someone else commits in the shared start checkout after the
				// worker started; its reflog records the commit there.
				commit := func() string {
					archiveGit(t, repo, "checkout", "-q", "-b", "feat")
					putCapabilityFile(t, filepath.Join(repo, "lead.txt"), "lead change\n")
					archiveGit(t, repo, "add", "lead.txt")
					archiveGit(t, repo, "commit", "-q", "-m", "lead change")
					return "ok  \tsoluterminal\t12.3s"
				}
				if kind == "commit-during-unrelated-call" {
					workerRan(t, p, "go test ./...", commit)
				} else {
					commit()
				}
				archiveGit(t, repo, "push", "-q", "-u", "origin", "feat")
				mergeFeat(t, repo)
			case "own-worktree-without-commits":
				otherCommitOnMain(t, repo, "other.txt")
				worked = workerCreatesWorktree(t, p, repo, "r", "research", "origin/main")
			}
			stampTranscriptCwd(t, p, worked)
			// Everything but the own-commit rule passes.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if workerGitCheckouts(ctx, p.Snapshot(), workerTranscriptCalls(ctx, p.ClaudeTranscriptPath), worked) == "" {
				t.Fatal("fixture: checkouts not clean")
			}
			for cwd, head := range map[string]string{repo: archiveGit(t, repo, "rev-parse", "HEAD"), worked: archiveGit(t, worked, "rev-parse", "HEAD")} {
				if !workerGitLanded(ctx, cwd, head, workerGitDefaultRef(ctx, cwd)) {
					t.Fatalf("fixture: %s not landed", cwd)
				}
			}
			a.tryArchiveWorker(p, now)
			a.tryArchiveWorker(p, now.Add(time.Hour))
			workerKept(t, a, p)
		})
	}
}

func TestWorkerOwnCommitsInItsStartCheckoutClose(t *testing.T) {
	a, p, now := workerStartFixture(t)
	repo := p.Cwd
	archiveGit(t, repo, "checkout", "-q", "-b", "feat")
	putCapabilityFile(t, filepath.Join(repo, "feature.txt"), "worker change\n")
	workerCommits(t, p, repo, "worker change")
	archiveGit(t, repo, "push", "-q", "-u", "origin", "feat")
	mergeFeat(t, repo)
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("worker with its own merged commits was not archived")
	}
	if _, err := os.Stat(repo); err != nil {
		t.Fatal("start checkout removed")
	}
}

func TestWorkerGitCreatedCommitMessages(t *testing.T) {
	for msg, want := range map[string]bool{
		"commit: worker change": true, "commit (amend): x": true, "commit (merge): x": true, "commit (initial): x": true,
		"cherry-pick: x": true, "rebase (pick): x": true, "rebase -i (fixup): x": true, "pull --rebase (pick): x": true,
		"pull: Fast-forward": false, "pull --rebase (finish): returning to refs/heads/main": false, "reset: moving to origin/main": false,
		"checkout: moving from main to feat": false, "merge origin/main: Merge made by the 'ort' strategy.": false,
		"branch: Created from origin/main": false, "rebase (finish): returning to refs/heads/feat": false, "rebase (start): checkout main": false,
	} {
		if workerGitCreatedCommit(msg) != want {
			t.Fatalf("%q: want %v", msg, want)
		}
	}
}

// Only a worktree this worker provably created is removed: same repository,
// under its .claude/worktrees, absent at spawn, and created during one of the
// worker's own tool calls that named it. The worker here merged its own
// worktree; the other one is clean and landed, and is where its transcript
// says it last worked.
func TestMergedWorkerKeepsWorktreesItDidNotCreate(t *testing.T) {
	for _, kind := range []string{"other-repository", "sibling-later", "pre-existing", "failed-add-collision"} {
		t.Run(kind, func(t *testing.T) {
			var other string
			a, p, now, wt := mergedWorkerFixture(t, func(repo string) {
				if kind == "pre-existing" {
					other = filepath.Join(repo, ".claude", "worktrees", "pre")
					archiveGit(t, repo, "worktree", "add", "-q", "-b", "pre", other, "origin/main")
				}
			})
			repo := p.Cwd
			mergeFeat(t, repo)
			switch kind {
			case "other-repository":
				// Even created by the worker's own call, another repository's
				// worktree is not the worker's to remove.
				elsewhere := archiveRepo(t)
				other = workerCreatesWorktree(t, p, elsewhere, "x", "x", "origin/main")
			case "sibling-later":
				other = filepath.Join(repo, ".claude", "worktrees", "sibling")
				archiveGit(t, repo, "worktree", "add", "-q", "-b", "sibling", other, "origin/main")
			case "failed-add-collision":
				// The worker's own add of the same folder fails because a
				// sibling created it moments before, inside the same call.
				other = filepath.Join(repo, ".claude", "worktrees", "sibling")
				workerRan(t, p, "git worktree add -b mine .claude/worktrees/sibling origin/main", func() string {
					archiveGit(t, repo, "worktree", "add", "-q", "-b", "sibling", other, "origin/main")
					return "fatal: '.claude/worktrees/sibling' already exists"
				})
			}
			stampTranscriptCwd(t, p, other)
			a.workerArchives().liveCwd = func(x *Pane) string {
				if x == p {
					return wt
				}
				return ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			calls := workerTranscriptCalls(ctx, p.ClaudeTranscriptPath)
			checkouts := workerGitCheckouts(ctx, p.Snapshot(), calls, wt, other)
			if checkouts == "" {
				t.Fatal("fixture: checkouts not clean")
			}
			if root, _ := workerOwnWorktree(ctx, p.Snapshot(), other, checkouts, calls); root != "" {
				t.Fatalf("%s counted as the worker's own", root)
			}
			if root, base := workerOwnWorktree(ctx, p.Snapshot(), wt, checkouts, calls); root != wt || base == "" {
				t.Fatalf("own worktree not proven: %q %q", root, base)
			}
			a.tryArchiveWorker(p, now)
			a.tryArchiveWorker(p, now.Add(10*time.Minute))
			if !workerArchived(a, p) {
				t.Fatal("merged worker was not archived")
			}
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Fatal("the worker's own worktree was kept")
			}
			if _, err := os.Stat(other); err != nil {
				t.Fatalf("removed a worktree the worker did not create: %v", err)
			}
		})
	}
}

// A worktree whose .git file points at a sibling's admin entry fails git
// worktree remove's own check (<admin>/gitdir must point back at it), so the
// sibling's entry is never deleted.
func TestMergedWorktreeRemovalRefusesBorrowedAdminEntry(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	mergeFeat(t, repo)
	sibling := filepath.Join(repo, ".claude", "worktrees", "sibling")
	archiveGit(t, repo, "worktree", "add", "-q", "-b", "sibling", sibling, "origin/main")
	admin := filepath.Join(repo, ".git", "worktrees", "sibling")
	putCapabilityFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := workerGitRemoveWorktree(ctx, wt); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("crafted .git file accepted: %v", err)
	}
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(time.Hour))
	for _, path := range []string{admin, sibling, wt} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed: %v", path, err)
		}
	}
	if out := archiveGit(t, sibling, "rev-parse", "--abbrev-ref", "HEAD"); out != "sibling" {
		t.Fatalf("sibling worktree broken: %q", out)
	}
}

// Input before a restart cannot be seen, so a reply finished before the
// daemon started never closes on merge alone (a pre-restart draft survives).
func TestMergedWorkerReplyFromBeforeRestartIsKept(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	mergeFeat(t, p.Cwd)
	q := a.workers()
	q.mu.Lock()
	q.startedAt = now
	q.mu.Unlock()
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(time.Hour))
	workerKept(t, a, p)
	if _, err := os.Stat(wt); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerTranscriptCallsReadClaudeAndCodexShapes(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return strconv.Quote(at.Add(d).Format(time.RFC3339Nano)) }
	path := filepath.Join(t.TempDir(), "t.jsonl")
	putCapabilityFile(t, path, strings.Join([]string{
		`{"type":"assistant","timestamp":` + ts(-time.Second) + `,"message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"git commit -m x"}}]}}`,
		`{"type":"user","timestamp":` + ts(time.Second) + `,"message":{"content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"[feat 1234567] x"}]}]}}`,
		`{"timestamp":` + ts(-time.Second) + `,"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c","name":"exec","input":"tools.exec_command({cmd:\"git worktree add .claude/worktrees/w\"})"}}`,
		`{"timestamp":` + ts(time.Second) + `,"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_text","text":"HEAD is now at 1234567"}]}}`,
		`{"timestamp":` + ts(-time.Hour) + `,"type":"response_item","payload":{"type":"function_call","call_id":"old","arguments":"{\"cmd\":\"git commit\"}"}}`,
		`{"timestamp":` + ts(-time.Hour+time.Second) + `,"type":"response_item","payload":{"type":"function_call_output","call_id":"old","output":"done"}}`,
		`{"type":"assistant","timestamp":` + ts(-time.Second) + `,"message":{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","timestamp":` + ts(time.Second) + `,"message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}}`,
	}, "\n")+"\n")
	// One read keeps every git call (not go test); each check filters by time.
	calls := workerTranscriptCalls(context.Background(), path)
	var now []workerToolCall
	for _, c := range calls {
		if c.ranAt(at.Unix()) {
			now = append(now, c)
		}
	}
	if len(calls) != 3 || len(now) != 2 || !strings.Contains(now[0].Output, "[feat 1234567]") || !workerTextNames(now[1].Input, "worktrees", "w") {
		t.Fatalf("calls: %+v", calls)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if workerTranscriptCalls(cancelled, path) != nil {
		t.Fatal("a cancelled check still read the transcript")
	}
	for text, want := range map[string]bool{".claude/worktrees/w": true, `.claude\worktrees\w x`: true, ".claude/worktrees/web": false, ".claude/worktrees/w.2": false, "worktrees/w/sub": true} {
		if workerTextNames(text, "worktrees", "w") != want {
			t.Fatalf("%q: want %v", text, want)
		}
	}
}

// Only the creation call itself proves creation: a git worktree list or ls
// that ran while a sibling created its worktree does not.
func TestWorkerCallCreatesWorktreeNeedsTheCreation(t *testing.T) {
	path, head := "/repo/.claude/worktrees/w", "1234567"+strings.Repeat("0", 33)
	for _, c := range []struct {
		call workerToolCall
		want bool
	}{
		{workerToolCall{Name: "Bash", Input: "git worktree add -b feat .claude/worktrees/w origin/main", Output: "Preparing worktree (new branch 'feat')\nHEAD is now at 1234567 x"}, true},
		{workerToolCall{Name: "Bash", Input: "cd /repo && git worktree add /repo/.claude/worktrees/w 2>&1 | tail -1", Output: "HEAD is now at 1234567 x"}, true},
		{workerToolCall{Name: "Bash", Input: "git worktree add .claude/worktrees/w", Output: "fatal: '.claude/worktrees/w' already exists"}, false},
		{workerToolCall{Name: "Bash", Input: "git worktree add .claude/worktrees/w", Output: "HEAD is now at 1234567 x", Error: true}, false},
		{workerToolCall{Name: "Bash", Input: "git worktree add -q .claude/worktrees/w"}, false},
		// Success for a different commit: another add printed it.
		{workerToolCall{Name: "Bash", Input: "git worktree add .claude/worktrees/w", Output: "HEAD is now at 7654321 x"}, false},
		{workerToolCall{Name: "Bash", Input: "git worktree list", Output: path}, false},
		{workerToolCall{Name: "Bash", Input: "ls .claude/worktrees/w"}, false},
		{workerToolCall{Name: "Bash", Input: "git worktree add .claude/worktrees/mine && ls .claude/worktrees/w", Output: "HEAD is now at 1234567 x"}, false},
		{workerToolCall{Name: "EnterWorktree", Input: "w", Output: "Created worktree at " + path}, true},
		{workerToolCall{Name: "EnterWorktree", Input: path, Output: "Entered " + path}, false},
	} {
		if workerCallCreatesWorktree(c.call, path, head) != c.want {
			t.Fatalf("%+v: want %v", c.call, c.want)
		}
	}
}

// Status does not look at files marked assume-unchanged or skip-worktree, so
// a worktree with such a file edited must not be removed as clean.
func TestMergedWorktreeRemovalKeepsHiddenIndexEdits(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			a, p, now, wt := mergedWorkerFixture(t)
			mergeFeat(t, p.Cwd)
			archiveGit(t, wt, "update-index", flag, "feature.txt")
			putCapabilityFile(t, filepath.Join(wt, "feature.txt"), "edit status cannot see\n")
			a.tryArchiveWorker(p, now)
			a.tryArchiveWorker(p, now.Add(10*time.Minute))
			if raw, err := os.ReadFile(filepath.Join(wt, "feature.txt")); err != nil || string(raw) != "edit status cannot see\n" {
				t.Fatalf("hidden edit lost: %v", err)
			}
		})
	}
}

func TestWorkerPathInside(t *testing.T) {
	root := t.TempDir()
	for cwd, want := range map[string]bool{root: true, filepath.Join(root, "sub"): true, filepath.Join(root, "..foo"): true, filepath.Dir(root): false, root + "-sibling": false, "": false, "relative": false} {
		if workerPathInside(root, cwd) != want {
			t.Fatalf("%q: want %v", cwd, want)
		}
	}
}

// A worker-written partial-clone config (extensions.partialClone, a promisor
// remote whose upload-pack is a program) cannot make the daemon's history
// walks lazily fetch a missing object and run that program.
func TestWorkerGitNeverLazyFetchesThroughRepositoryConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell upload-pack")
	}
	repo := archiveRepo(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "upload-pack.sh")
	putCapabilityFile(t, script, "#!/bin/sh\ntouch '"+marker+"'\nexit 1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"core.repositoryformatversion", "1"}, {"extensions.partialClone", "origin"}, {"remote.origin.promisor", "true"}, {"remote.origin.uploadpack", script}} {
		archiveGit(t, repo, "config", kv[0], kv[1])
	}
	missing := strings.Repeat("ab", 20)
	// Control: git without the hardened environment does run the program.
	control := exec.Command("git", "merge-base", "--is-ancestor", missing, "main")
	control.Dir = repo
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			control.Env = append(control.Env, kv)
		}
	}
	control.Env = append(control.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	_ = control.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixture: lazy fetch did not run the repository's upload-pack: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workerGitLanded(ctx, repo, missing, "refs/heads/main")
	_, _ = workerGitNames(ctx, repo, missing, "HEAD")
	_, _ = workerGitRun(ctx, repo, "rev-list", "-1", missing)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the daemon's git ran the repository's upload-pack")
	}
}

// One sibling's active, unmerged worktree (created since this worker started)
// is not this worker's checkout: it does not hold the merged worker open, and
// it is never touched.
func TestMergedWorkerIgnoresSiblingWorktreesInProgress(t *testing.T) {
	a, p, now, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	mergeFeat(t, repo)
	sibling := filepath.Join(repo, ".claude", "worktrees", "sibling")
	archiveGit(t, repo, "worktree", "add", "-q", "-b", "sibling", sibling, "origin/main")
	putCapabilityFile(t, filepath.Join(sibling, "wip.txt"), "unsaved sibling work\n")
	putCapabilityFile(t, filepath.Join(sibling, "tracked.txt"), "sibling edit\n")
	a.tryArchiveWorker(p, now)
	a.tryArchiveWorker(p, now.Add(10*time.Minute))
	if !workerArchived(a, p) {
		t.Fatal("a sibling's worktree held the merged worker open")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("the worker's own worktree was kept")
	}
	if raw, err := os.ReadFile(filepath.Join(sibling, "wip.txt")); err != nil || string(raw) != "unsaved sibling work\n" {
		t.Fatalf("sibling worktree touched: %v", err)
	}
}

// A parent swapped for a symlink after the checks (here .claude/worktrees,
// pointing at a folder with a same-named directory) cannot redirect the
// deletion: removal refuses and nothing is deleted on either side.
func TestMergedWorktreeRemovalRefusesSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	_, p, _, wt := mergedWorkerFixture(t)
	repo := p.Cwd
	mergeFeat(t, repo)
	decoy := filepath.Join(t.TempDir(), "decoy")
	putCapabilityFile(t, filepath.Join(decoy, "w", "keep.txt"), "not the worker's\n")
	worktrees := filepath.Join(repo, ".claude", "worktrees")
	moved := filepath.Join(repo, ".claude", "moved")
	workerGitBeforeRemove = func(string) {
		if err := os.Rename(worktrees, moved); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(decoy, worktrees); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { workerGitBeforeRemove = nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := workerGitRemoveWorktree(ctx, wt); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("removal through a symlinked parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(decoy, "w", "keep.txt")); err != nil {
		t.Fatalf("decoy deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "w", "feature.txt")); err != nil {
		t.Fatalf("worktree deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "worktrees", "w")); err != nil {
		t.Fatalf("admin entry deleted: %v", err)
	}
}

// git worktree remove keeps a worktree with submodules; so does this.
func TestMergedWorktreeRemovalKeepsSubmodules(t *testing.T) {
	_, _, _, wt := mergedWorkerFixture(t)
	sub := archiveRepo(t)
	archiveGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	archiveGit(t, wt, "commit", "-qm", "add submodule")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := workerGitRemoveWorktree(ctx, wt); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("removed a worktree with a submodule: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "tracked.txt")); err != nil {
		t.Fatalf("submodule contents deleted: %v", err)
	}
}
