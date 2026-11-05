package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Preview channel — the in-app face of scripts/follow-main.sh.
//
//   Stable  = published releases (what everyone runs).
//   Preview = the latest merged main whose full GitHub CI passed, built on
//             THIS machine from its own source checkout and installed through
//             the normal guarded update swap (POST /api/update/apply-local).
//
// It is only offered where a soluterminal source checkout with
// scripts/follow-main.sh exists (the owner's PC); everywhere else the status
// reports available=false and the UI shows nothing. Switching is accepted
// only from this machine — loopback or a request carrying the owner token
// (the desktop app reaches WSL from the Windows host) — never through the
// cloud relay (owner-only by the cloud's machine check).
//
// GET  /api/update/preview          status (+ refreshes the main check when stale)
// POST /api/update/preview/install  {"target":"preview"|"stable"}

const (
	previewCheckEvery = 10 * time.Minute
	previewJobFile    = "preview_job.json"
	previewLogFile    = "preview-build.log"
)

type previewMainStatus struct {
	Running     string `json:"running,omitempty"`
	MainSha     string `json:"mainSha,omitempty"`
	MainVersion string `json:"mainVersion,omitempty"`
	CI          string `json:"ci,omitempty"` // attested | pending | failed | unavailable …
	// Release: main is the published release's own source commit, so there
	// is nothing to preview (follow-main.sh refuses to build it).
	Release   bool   `json:"release,omitempty"`
	CheckedMs int64  `json:"checkedMs,omitempty"`
	Error     string `json:"error,omitempty"`
}

// previewChange is one merged change not yet released, with the steps its
// pull request gives for trying it (scripts/preview-changes.mjs).
type previewChange struct {
	N         int    `json:"n"`
	Sha       string `json:"sha"`
	PR        int    `json:"pr,omitempty"`
	Title     string `json:"title"`
	HowToTry  string `json:"howToTry,omitempty"`
	Installed bool   `json:"installed"`
}

type previewJob struct {
	Target    string `json:"target"`
	PID       int    `json:"pid"`
	StartedMs int64  `json:"startedMs"`
	// Root: the checkout the switch runs from; kept while it runs.
	Root string `json:"root,omitempty"`
}

type previewChannel struct {
	dataDir string
	repo    func() string
	now     func() time.Time
	run     func(ctx context.Context, repo string, args ...string) ([]byte, error)
	spawn   func(repo, logPath string, args ...string) (int, error)
	alive   func(pid int) bool
	// scripts picks the checkout follow-main.sh runs from (cleanScriptRoot)
	// and returns the call that releases it once that run is over.
	scripts func(repo string) (string, func())
	// sourceMu guards creating and pruning preview-source checkouts and
	// inUse, the count of runs reading each one.
	sourceMu sync.Mutex
	inUse    map[string]int

	mu       sync.Mutex
	main     previewMainStatus
	changes  []previewChange
	checking bool
}

func newPreviewChannel(dataDir string) *previewChannel {
	p := &previewChannel{
		dataDir: dataDir,
		repo:    ownerPreviewSourceRepo,
		now:     time.Now,
		run:     runFollowMain,
		spawn:   spawnFollowMain,
		alive:   followMainAlive,
	}
	p.scripts = p.cleanScriptRoot
	return p
}

// findPreviewSourceRepo returns a soluterminal source checkout on this
// machine that can build previews, or "".
// previewOwnerEnv opts a machine into Preview. Preview is for the product
// owner's own PC only: having a source checkout is not enough (anyone can
// clone the public repo), so the machine must say so explicitly, in
// ~/.solu/env (read at daemon start) or the daemon's environment.
const previewOwnerEnv = "SOLU_PREVIEW_CHANNEL"

func ownerPreviewSourceRepo() string {
	if os.Getenv(previewOwnerEnv) != "1" {
		return ""
	}
	return findPreviewSourceRepo()
}

func findPreviewSourceRepo() string {
	home, _ := os.UserHomeDir()
	candidates := []string{os.Getenv("SOLU_SOURCE_REPO")}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, "wsl-projects", "soluterminal"),
			filepath.Join(home, "soluterminal"),
			filepath.Join(home, "src", "soluterminal"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(c, "scripts", "follow-main.sh")); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, ".git")); err == nil {
			return c
		}
	}
	return ""
}

func runFollowMain(ctx context.Context, repo string, args ...string) ([]byte, error) {
	// A login shell picks up the owner's PATH (nvm node, gh, go).
	script := "exec bash scripts/follow-main.sh " + strings.Join(args, " ")
	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	cmd.Dir = repo
	return cmd.Output()
}

// spawnFollowMain starts the build detached (own session) so it outlives the
// daemon restart it triggers, with output in logPath.
func spawnFollowMain(repo, logPath string, args ...string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command("bash", "-lc", "exec bash scripts/follow-main.sh "+strings.Join(args, " "))
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = previewDetachAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func followMainAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return err == nil && strings.Contains(string(cmdline), "follow-main.sh")
}

func (p *previewChannel) jobPath() string { return filepath.Join(p.dataDir, previewJobFile) }
func (p *previewChannel) logPath() string { return filepath.Join(p.dataDir, previewLogFile) }

func (p *previewChannel) loadJob() (previewJob, bool) {
	var j previewJob
	data, err := os.ReadFile(p.jobPath())
	if err != nil || json.Unmarshal(data, &j) != nil || j.PID <= 0 {
		return previewJob{}, false
	}
	return j, true
}

// lastLogLine is the build's latest progress / result line.
func (p *previewChannel) lastLogLine() string {
	f, err := os.Open(p.logPath())
	if err != nil {
		return ""
	}
	defer f.Close()
	last := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	if len(last) > 300 {
		last = last[:300]
	}
	return last
}

// refreshMain re-reads the latest main state via `follow-main.sh --status
// --json` (git fetch + a GitHub CI lookup; no build).
func (p *previewChannel) refreshMain(repo string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st := previewMainStatus{CheckedMs: p.now().UnixMilli()}
	root, release := p.scriptRoot(repo)
	defer release()
	out, err := p.run(ctx, root, "--status", "--json")
	if err == nil {
		err = json.Unmarshal(out, &st)
		st.CheckedMs = p.now().UnixMilli()
	}
	if err != nil {
		st.Error = "couldn't check main: " + err.Error()
	}
	// The unreleased list is best effort: without it the channel still works.
	var changes struct {
		Changes []previewChange `json:"changes"`
	}
	if out, cerr := p.run(ctx, root, "--changes", "--json"); cerr == nil {
		_ = json.Unmarshal(out, &changes)
	}
	p.mu.Lock()
	p.main = st
	p.changes = changes.Changes
	p.checking = false
	p.mu.Unlock()
}

// previewChangesAhead counts merged changes of `version` (base.N) beyond the
// base release; 0 for a plain release version.
func previewChangesAhead(version string) (base string, n int) {
	parts := strings.Split(version, ".")
	if len(parts) < 4 {
		return version, 0
	}
	n, _ = strconv.Atoi(parts[3])
	return strings.Join(parts[:3], "."), n
}

type previewStatusDTO struct {
	Available bool              `json:"available"`
	Channel   string            `json:"channel,omitempty"` // preview | stable
	Running   string            `json:"running,omitempty"`
	Stable    string            `json:"stable,omitempty"` // release the running build is based on / latest known
	Main      previewMainStatus `json:"main"`
	// NewChanges: merged changes on main not yet running here (0 when
	// current, or when main isn't installable yet).
	NewChanges int  `json:"newChanges"`
	Ahead      int  `json:"ahead"` // running preview's changes beyond Stable
	Checking   bool `json:"checking,omitempty"`
	// Unreleased: merged changes not yet in a release, newest first, each
	// with its "How to try it" steps and whether this app already has it.
	Unreleased []previewChange `json:"unreleased,omitempty"`
	Job        string          `json:"job,omitempty"`     // building-preview | switching-to-stable
	JobLine    string          `json:"jobLine,omitempty"` // latest progress / last result
}

func (p *previewChannel) status(running, latestStable string) previewStatusDTO {
	repo := p.repo()
	if repo == "" {
		return previewStatusDTO{Available: false}
	}
	p.mu.Lock()
	stale := p.now().Sub(time.UnixMilli(p.main.CheckedMs)) > previewCheckEvery
	if stale && !p.checking {
		p.checking = true
		go p.refreshMain(repo)
	}
	main, checking := p.main, p.checking
	unreleased := append([]previewChange(nil), p.changes...)
	p.mu.Unlock()

	dto := previewStatusDTO{Available: true, Running: running, Main: main, Checking: checking, Unreleased: unreleased}
	base, ahead := previewChangesAhead(running)
	dto.Ahead = ahead
	dto.Channel = "stable"
	if ahead > 0 || strings.Count(running, ".") >= 3 {
		dto.Channel = "preview"
	}
	dto.Stable = base
	if compareVersions(latestStable, dto.Stable) > 0 {
		dto.Stable = latestStable
	}
	if main.MainVersion != "" && main.CI == "attested" && !main.Release && isNewer(main.MainVersion, running) {
		mBase, mN := previewChangesAhead(main.MainVersion)
		if mBase == base {
			dto.NewChanges = mN - ahead
		} else {
			dto.NewChanges = mN
		}
		if dto.NewChanges < 1 {
			dto.NewChanges = 1
		}
	}
	if job, ok := p.loadJob(); ok {
		if p.alive(job.PID) {
			dto.Job = map[string]string{"preview": "building-preview", "stable": "switching-to-stable"}[job.Target]
		}
		dto.JobLine = p.lastLogLine()
	}
	return dto
}

var errPreviewBusy = errors.New("a preview switch is already running")

func (p *previewChannel) install(target string) error {
	repo := p.repo()
	if repo == "" {
		return errors.New("no soluterminal source checkout on this machine")
	}
	if job, ok := p.loadJob(); ok && p.alive(job.PID) {
		return errPreviewBusy
	}
	var args []string
	switch target {
	case "preview":
	case "stable":
		args = []string{"--release"}
	default:
		return errors.New(`target must be "preview" or "stable"`)
	}
	// Held until the job (which keeps its root) is recorded below.
	root, release := p.scriptRoot(repo)
	defer release()
	pid, err := p.spawn(root, p.logPath(), args...)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(previewJob{Target: target, PID: pid, StartedMs: p.now().UnixMilli(), Root: root})
	_ = writeFileAtomic(p.jobPath(), data, 0o600)
	p.mu.Lock()
	p.main.CheckedMs = 0 // re-check main after the switch
	p.mu.Unlock()
	return nil
}

// ownerRequest: the machine's owner — loopback, the owner token (the desktop
// app reaching WSL from the Windows host), or the cloud relay. The relay only
// forwards requests from the signed-in owner of this machine (cloud
// handleMachineProxy: m.UserID == caller), and the desktop app in cloud mode
// reaches its own daemon that way. A LAN peer without the token is refused.
// The install itself still happens locally: follow-main.sh calls the
// loopback-only /api/update/apply-local.
func (a *apiServer) ownerRequest(r *http.Request) bool {
	if fileAccessOverRelay(r) || strings.HasPrefix(r.RemoteAddr, "yamux") {
		return true
	}
	if tokenlessLoopbackAllowed(r) {
		return true
	}
	return a.gate != nil && a.gate.isAuthed(r)
}

func (a *apiServer) previewStatus(w http.ResponseWriter, r *http.Request) {
	if a.preview == nil || a.updater == nil {
		writeJSON(w, 200, previewStatusDTO{Available: false})
		return
	}
	st := a.updater.Status()
	if st == nil {
		writeJSON(w, 200, previewStatusDTO{Available: false})
		return
	}
	dto := a.preview.status(st.Current, st.Latest)
	if !a.ownerRequest(r) {
		// Unauthenticated LAN viewers see nothing about the channel.
		dto = previewStatusDTO{Available: false}
	}
	writeJSON(w, 200, dto)
}

func (a *apiServer) previewInstall(w http.ResponseWriter, r *http.Request) {
	if a.preview == nil {
		http.Error(w, "preview channel unavailable", 404)
		return
	}
	if !a.ownerRequest(r) {
		http.Error(w, "switching channels is only possible for this machine's owner", http.StatusForbidden)
		return
	}
	var req struct {
		Target string `json:"target"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}
	if err := a.preview.install(req.Target); err != nil {
		code := 400
		if errors.Is(err, errPreviewBusy) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, 202, map[string]any{"started": true, "target": req.Target})
}

func (p *previewChannel) scriptRoot(repo string) (string, func()) {
	if p.scripts == nil {
		return repo, func() {}
	}
	return p.scripts(repo)
}

// cleanScriptRoot returns a checkout of origin/main, kept under the data dir,
// to run follow-main.sh from. The owner's own checkout can sit on any branch
// with an older copy of the script — one lacking --changes left the "Not
// released yet" list empty. Each main commit gets its own checkout
// (preview-source/<sha>) that is never modified afterwards, so a status check
// and a switch can each run from theirs. A checkout is kept while any run
// holds it (release when done), while a switch runs from it, and as one of
// the newest two. Falls back to repo when git fails.
func (p *previewChannel) cleanScriptRoot(repo string) (string, func()) {
	none := func() {}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	git := func(args ...string) ([]byte, error) { return hardenedGitCommand(ctx, repo, args...).Output() }
	_ = gitFetchCommand(ctx, repo, "origin", "main").Run() // offline: use what is there
	out, err := git("rev-parse", "origin/main^{commit}")
	sha := strings.TrimSpace(string(out))
	if err != nil || len(sha) < 12 {
		return repo, none
	}
	base := filepath.Join(p.dataDir, "preview-source")
	dir := filepath.Join(base, sha[:12])
	p.sourceMu.Lock()
	defer p.sourceMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		_ = os.RemoveAll(dir) // never handed out, so nothing runs from it
		_, _ = git("worktree", "prune")
		// The checkout would run any filter driver the repository's own config
		// defines; this project defines none, so fall back to repo instead.
		if _, err := git("config", "--get-regexp", `^filter\.`); processExitCode(err) != 1 {
			return repo, none
		}
		if _, err := git("worktree", "add", "-q", "--detach", dir, sha); err != nil {
			return repo, none
		}
	}
	if p.inUse == nil {
		p.inUse = map[string]int{}
	}
	p.inUse[dir]++
	release := func() {
		p.sourceMu.Lock()
		defer p.sourceMu.Unlock()
		if p.inUse[dir]--; p.inUse[dir] <= 0 {
			delete(p.inUse, dir)
		}
	}
	keep := map[string]bool{dir: true}
	for d := range p.inUse {
		keep[d] = true
	}
	if job, ok := p.loadJob(); ok && job.Root != "" && p.alive != nil && p.alive(job.PID) {
		keep[job.Root] = true
	}
	p.pruneScriptRoots(ctx, repo, base, keep)
	return dir, release
}

// pruneScriptRoots removes old preview-source checkouts, keeping `keep` (the
// current one, those being read, a running switch's) and the newest other.
func (p *previewChannel) pruneScriptRoots(ctx context.Context, repo, base string, keep map[string]bool) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	type root struct {
		path string
		mod  time.Time
	}
	var others []root
	for _, e := range entries {
		path := filepath.Join(base, e.Name())
		if !e.IsDir() || keep[path] {
			continue
		}
		if info, err := e.Info(); err == nil {
			others = append(others, root{path, info.ModTime()})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].mod.After(others[j].mod) })
	for i, r := range others {
		if i == 0 {
			continue
		}
		_ = hardenedGitCommand(ctx, repo, "worktree", "remove", "--force", r.path).Run()
		_ = os.RemoveAll(r.path)
	}
}
