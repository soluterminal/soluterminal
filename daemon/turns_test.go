package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTurnPane struct {
	mu       sync.Mutex
	paneID   string
	paneName string
	st       PaneRuntimeState
	agentKnd string
	dir      string
	input    int64
	activity int64
	exited   bool
}

func (f *fakeTurnPane) id() string    { return f.paneID }
func (f *fakeTurnPane) title() string { return f.paneName }
func (f *fakeTurnPane) running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.exited
}
func (f *fakeTurnPane) state() PaneRuntimeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}
func (f *fakeTurnPane) agent() string         { return f.agentKnd }
func (f *fakeTurnPane) cwd() string           { return f.dir }
func (f *fakeTurnPane) lastInputMs() int64    { f.mu.Lock(); defer f.mu.Unlock(); return f.input }
func (f *fakeTurnPane) lastActivityMs() int64 { f.mu.Lock(); defer f.mu.Unlock(); return f.activity }
func (f *fakeTurnPane) set(st PaneRuntimeState) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "init")
	return dir
}

func newTestTracker(t *testing.T, panes ...turnPane) (*turnTracker, *fakeClock) {
	clock := &fakeClock{t: time.UnixMilli(1_700_000_000_000)}
	tr := &turnTracker{
		dir:   filepath.Join(t.TempDir(), "turns"),
		snap:  gitTurnSnapshotter{},
		now:   clock.now,
		state: map[string]*paneTurnState{},
		panes: func() []turnPane { return panes },
	}
	return tr, clock
}

// step advances the fake clock one tick and runs the tracker, waiting for any
// snapshot goroutine it started.
func step(tr *turnTracker, clock *fakeClock, n int) {
	for i := 0; i < n; i++ {
		clock.advance(time.Second)
		tr.tick()
		tr.pending.Wait()
	}
}

func TestTurnTrackerRecordsTurnDiffWithoutTouchingRepo(t *testing.T) {
	repo := gitInit(t)
	p := &fakeTurnPane{paneID: "pane_a", paneName: "claude A", agentKnd: "claude", dir: repo, st: StateIdle}
	tr, clock := newTestTracker(t, p)
	step(tr, clock, 1)

	// User submits → busy.
	p.mu.Lock()
	p.input = clock.t.UnixMilli()
	p.mu.Unlock()
	p.set(StateBusy)
	step(tr, clock, 1)
	if s := tr.summary("pane_a"); s == nil || !s.Active {
		t.Fatalf("expected an active turn, got %+v", s)
	}

	// The agent edits a tracked file and creates a new one.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\nTWO\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step(tr, clock, 6) // busy for a while
	p.mu.Lock()
	p.activity = clock.t.UnixMilli()
	p.mu.Unlock()
	p.set(StateAwaitingInput)
	step(tr, clock, 5) // settle

	s := tr.summary("pane_a")
	if s == nil || s.Active || s.Last == nil {
		t.Fatalf("expected a completed turn, got %+v", s)
	}
	if s.Last.N != 1 || s.Last.Files != 2 || s.Last.Added != 3 || s.Last.Removed != 1 {
		t.Fatalf("unexpected stats: %+v", *s.Last)
	}
	if s.Last.BaseTree != "" {
		t.Fatalf("summary must not leak tree ids")
	}
	rec, ok := tr.get("pane_a", 1)
	if !ok || !rec.hasDiff() {
		t.Fatalf("expected diff trees, got %+v", rec)
	}

	// Nothing the user owns changed: index untouched, no new refs, a.txt still
	// shows as an unstaged modification and new.go as untracked.
	status, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(status); got != " M a.txt\n?? new.go\n" {
		t.Fatalf("repo status changed: %q", got)
	}
	refs, _ := exec.Command("git", "-C", repo, "for-each-ref").Output()
	if strings.Count(string(refs), "\n") != 1 {
		t.Fatalf("snapshot created refs: %s", refs)
	}

	// Persisted and reloadable by a fresh tracker (daemon restart).
	tr2, _ := newTestTracker(t)
	tr2.dir = tr.dir
	turns, _ := tr2.list("pane_a")
	if len(turns) != 1 || turns[0].Files != 2 {
		t.Fatalf("reload lost history: %+v", turns)
	}
}

func TestTurnTrackerDropsBlipsAndIgnoresNonAgents(t *testing.T) {
	repo := gitInit(t)
	shell := &fakeTurnPane{paneID: "pane_sh", agentKnd: "", dir: repo, st: StateIdle}
	agent := &fakeTurnPane{paneID: "pane_ag", agentKnd: "codex", dir: repo, st: StateIdle}
	tr, clock := newTestTracker(t, shell, agent)
	step(tr, clock, 1)

	// Shell output for a long time: never a turn.
	shell.set(StateBusy)
	step(tr, clock, 8)
	shell.set(StateIdle)
	step(tr, clock, 5)
	if s := tr.summary("pane_sh"); s != nil {
		t.Fatalf("shell pane got a turn: %+v", s)
	}

	// A 2-tick redraw blip with no user input never opens a turn.
	agent.set(StateBusy)
	step(tr, clock, 2)
	agent.set(StateIdle)
	step(tr, clock, 5)
	if s := tr.summary("pane_ag"); s != nil {
		t.Fatalf("blip opened a turn: %+v", s)
	}

	// Self-started (no input) busy run opens a turn after turnSelfStartTicks,
	// but a short, change-free one is dropped.
	agent.set(StateBusy)
	step(tr, clock, 3)
	if s := tr.summary("pane_ag"); s == nil || !s.Active {
		t.Fatalf("expected self-started turn, got %+v", s)
	}
	agent.mu.Lock()
	agent.activity = clock.t.UnixMilli()
	agent.mu.Unlock()
	agent.set(StateIdle)
	step(tr, clock, 4)
	if s := tr.summary("pane_ag"); s != nil {
		t.Fatalf("short change-free turn should be dropped, got %+v", s)
	}
}

func TestTurnTrackerChoicePromptKeepsTurnOpenAndOverlap(t *testing.T) {
	repo := gitInit(t)
	a := &fakeTurnPane{paneID: "pane_a", paneName: "alpha", agentKnd: "claude", dir: repo, st: StateIdle}
	b := &fakeTurnPane{paneID: "pane_b", paneName: "beta", agentKnd: "claude", dir: repo, st: StateIdle}
	tr, clock := newTestTracker(t, a, b)
	step(tr, clock, 1)

	for _, p := range []*fakeTurnPane{a, b} {
		p.mu.Lock()
		p.input = clock.t.UnixMilli()
		p.mu.Unlock()
		p.set(StateBusy)
	}
	step(tr, clock, 3)
	a.set(StateChoicePrompt) // waiting on approval: still the same turn
	step(tr, clock, 10)
	if s := tr.summary("pane_a"); s == nil || !s.Active {
		t.Fatalf("approval wait must not end the turn: %+v", s)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.set(StateIdle)
	step(tr, clock, 5)
	s := tr.summary("pane_a")
	if s == nil || s.Last == nil || s.Last.Files != 1 {
		t.Fatalf("expected completed turn with 1 file, got %+v", s)
	}
	if len(s.Last.OverlapPanes) != 1 || s.Last.OverlapPanes[0] != "beta" {
		t.Fatalf("expected overlap with beta, got %v", s.Last.OverlapPanes)
	}
}

func TestTurnTrackerNonRepoAndClosedPane(t *testing.T) {
	dir := t.TempDir()
	p := &fakeTurnPane{paneID: "pane_x", agentKnd: "claude", dir: dir, st: StateIdle}
	tr, clock := newTestTracker(t, p)
	step(tr, clock, 1)
	p.mu.Lock()
	p.input = clock.t.UnixMilli()
	p.mu.Unlock()
	p.set(StateBusy)
	step(tr, clock, 8)
	p.set(StateIdle)
	step(tr, clock, 5)
	s := tr.summary("pane_x")
	if s == nil || s.Last == nil || s.Last.Note == "" || s.Last.hasDiff() {
		t.Fatalf("non-repo turn should be recorded with a note and no diff: %+v", s)
	}
	if _, err := os.Stat(tr.file("pane_x")); err != nil {
		t.Fatalf("expected persisted file: %v", err)
	}
	// Pane disappears → state and file are dropped.
	tr.panes = func() []turnPane { return nil }
	step(tr, clock, 1)
	if _, err := os.Stat(tr.file("pane_x")); !os.IsNotExist(err) {
		t.Fatalf("closed pane's turn file should be removed, err=%v", err)
	}
}

func TestGitTurnSnapshotSkipsLargeUntracked(t *testing.T) {
	repo := gitInit(t)
	big := make([]byte, turnUntrackedMaxBytes+1)
	if err := os.WriteFile(filepath.Join(repo, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if s.skipped != 1 || s.tree == "" || s.branch != "main" {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
}

// Review finding on #365: the snapshot ran `git add`, which executes the
// repository's clean filters (and an fsmonitor hook) as the DAEMON, outside
// any agent sandbox — a repo or agent could get arbitrary commands run just by
// configuring a filter. The snapshot must never launch repo-configured
// programs, and must record the RAW file bytes.
func TestGitTurnSnapshotNeverRunsRepoConfiguredPrograms(t *testing.T) {
	repo := gitInit(t)
	sentinel := filepath.Join(t.TempDir(), "executed")
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// A clean filter and an fsmonitor hook that each leave a mark if run.
	git("config", "filter.evil.clean", "touch "+sentinel+"; tr a-z A-Z")
	git("config", "filter.evil.smudge", "cat")
	hook := filepath.Join(repo, ".git", "fsmon.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+sentinel+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "core.fsmonitor", hook)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("lower case\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("snapshot executed a repository-configured program (clean filter or fsmonitor)")
	}
	// Raw bytes, not the filter's upper-cased output.
	for path, want := range map[string]string{"a.txt": "lower case\n", "new.txt": "brand new\n"} {
		out, err := exec.Command("git", "-C", repo, "-c", "core.fsmonitor=false", "cat-file", "-p", s.tree+":"+path).Output()
		if err != nil {
			t.Fatalf("cat-file %s: %v", path, err)
		}
		if string(out) != want {
			t.Fatalf("%s in snapshot = %q, want raw %q", path, out, want)
		}
	}
}

// The filter-free snapshot must still describe the worktree exactly:
// unchanged tracked files (reused from the index), modifications, deletions,
// the executable bit and symlinks.
func TestGitTurnSnapshotMatchesWorktree(t *testing.T) {
	repo := gitInit(t)
	run := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	for name, body := range map[string]string{"keep.txt": "same\n", "gone.txt": "bye\n", "run.sh": "#!/bin/sh\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "more")
	time.Sleep(10 * time.Millisecond)
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(repo, "ln")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	// Compare against what plain git (no filters configured here) stages.
	run("add", "-A")
	want := strings.TrimSpace(run("write-tree"))
	if s.tree != want {
		t.Fatalf("snapshot tree %s != git's view of the worktree %s\n%s", s.tree, want, run("diff", "--stat", s.tree, want))
	}
}

// Review finding on #365: with rename detection a renamed path prints as
// "old => new", slipping past the secret-path check, so a secret renamed
// during a turn shipped its contents over the relay.
func TestTurnDiffHidesRenamedSecretOverRelay(t *testing.T) {
	repo := gitInit(t)
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	const secret = "SUPER-SECRET-VALUE-0123456789"
	if err := os.MkdirAll(filepath.Join(repo, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("line of config\n", 20) + "old=" + secret + "\n"
	if err := os.WriteFile(filepath.Join(repo, "cfg", "a.key"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "cfg/a.key")
	git("commit", "-qm", "tracked key")
	base, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	// The agent renames the tracked secret inside its folder and edits it:
	// rename detection would print "cfg/{a.key => b.key}".
	git("mv", "cfg/a.key", "cfg/b.key")
	if err := os.WriteFile(filepath.Join(repo, "cfg", "b.key"), []byte(body+"new="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	end, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	rec := turnRecord{N: 1, Repo: base.repo, BaseTree: base.tree, EndTree: end.tree}
	resp, err := buildTurnDiff(context.Background(), rec, true, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := resp["diff"].(string); strings.Contains(text, secret) {
		t.Fatalf("renamed secret file content leaked through the relay diff:\n%s", text)
	}
	if hidden, _ := resp["hiddenSecret"].(int); hidden < 2 {
		t.Fatalf("both sides of the renamed secret should be hidden: hiddenSecret=%v files=%v", resp["hiddenSecret"], resp["files"])
	}
}

// Review finding (v2): without -z, git C-quotes non-ASCII paths
// ("caf\303\251/server.key"), so the secret checks and the exclude pathspec
// missed them and their contents shipped over the relay.
func TestTurnDiffHidesQuotedSecretPathsOverRelay(t *testing.T) {
	repo := gitInit(t)
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	dir := filepath.Join(repo, "café")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "A=1\n", "server.key": "K1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-qm", "secrets in a non-ascii dir")
	base, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	const envSecret, keySecret = "ENV-SECRET-AAAA", "KEY-SECRET-BBBB"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A="+envSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.key"), []byte(keySecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	end, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := buildTurnDiff(context.Background(), turnRecord{N: 1, Repo: base.repo, BaseTree: base.tree, EndTree: end.tree}, true, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	text, _ := resp["diff"].(string)
	if strings.Contains(text, envSecret) || strings.Contains(text, keySecret) {
		t.Fatalf("secret in a non-ASCII path leaked over the relay:\n%s", text)
	}
	if hidden, _ := resp["hiddenSecret"].(int); hidden != 2 {
		t.Fatalf("want both secrets hidden, got hiddenSecret=%v files=%v", resp["hiddenSecret"], resp["files"])
	}
}

// Review finding (v2): in a partial clone, write-tree / diff lazily fetch a
// missing object by running the remote transport .git/config names — as the
// daemon, outside the sandbox. Such repos are refused, and no transport runs.
func TestGitTurnSnapshotRefusesPartialClone(t *testing.T) {
	repo := gitInit(t)
	sentinel := filepath.Join(t.TempDir(), "fetched")
	for _, kv := range [][2]string{
		{"extensions.partialClone", "origin"},
		{"remote.origin.promisor", "true"},
		{"remote.origin.url", "ext::sh -c touch% " + sentinel},
		{"protocol.ext.allow", "always"},
	} {
		if out, err := exec.Command("git", "-C", repo, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v %s", kv[0], err, out)
		}
	}
	_, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "partial clone") {
		t.Fatalf("partial clone not refused: err=%v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatal("a remote transport ran")
	}
}

// Inherited GIT_* variables (GIT_DIR, GIT_CONFIG_PARAMETERS…) from the
// daemon's own environment must not redirect or reconfigure snapshot git.
func TestGitTurnSnapshotIgnoresInheritedGitEnv(t *testing.T) {
	repo := gitInit(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='/bin/false'")
	if _, err := (gitTurnSnapshotter{}).snapshot(context.Background(), repo); err != nil {
		t.Fatalf("snapshot affected by inherited GIT_* env: %v", err)
	}
}

// Review finding (v2): the snapshot could be made to read files OUTSIDE the
// repository into its object store — via a C-quoted name that git's
// --stdin-paths decodes ("\057tmp\057…"), or a tracked file whose parent
// directory was replaced by a symlink to elsewhere.
func TestGitTurnSnapshotNeverReadsOutsideTheRepo(t *testing.T) {
	repo := gitInit(t)
	outside := t.TempDir()
	const marker = "OUTSIDE-MARKER-c0ffee"
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// (b) tracked d/target.txt, later d → symlink to the outside dir.
	if err := os.MkdirAll(filepath.Join(repo, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "d", "target.txt"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "d/target.txt")
	git("commit", "-qm", "d")
	if err := os.RemoveAll(filepath.Join(repo, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "d")); err != nil {
		t.Fatal(err)
	}
	// (a) an untracked file whose NAME is a C-quoted absolute path.
	quoted := `"` + strings.ReplaceAll(target, "/", `\057`) + `"`
	if err := os.WriteFile(filepath.Join(repo, quoted), []byte("decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "-c", "core.fsmonitor=false", "ls-tree", "-r", "-z", s.tree).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		fields := strings.Fields(rec)
		if len(fields) < 3 {
			continue
		}
		blob, _ := exec.Command("git", "-C", repo, "cat-file", "-p", fields[2]).Output()
		if strings.Contains(string(blob), marker) {
			t.Fatalf("snapshot captured a file from outside the repo: %q", rec)
		}
	}
}

func TestGitTurnSnapshotSkipsHugeTrackedFiles(t *testing.T) {
	repo := gitInit(t)
	big := filepath.Join(repo, "db.sqlite")
	if err := os.WriteFile(big, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "x", "--allow-empty").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	exec.Command("git", "-C", repo, "add", "db.sqlite").Run()
	exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "db").Run()
	if err := os.WriteFile(big, make([]byte, turnTrackedMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := gitTurnSnapshotter{}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if s.skipped < 1 {
		t.Fatalf("huge tracked file should be skipped, skipped=%d", s.skipped)
	}
	blob, _ := exec.Command("git", "-C", repo, "cat-file", "-p", s.tree+":db.sqlite").Output()
	if string(blob) != "v1\n" {
		t.Fatalf("skipped file should keep its recorded content, got %d bytes", len(blob))
	}
}
