package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type workerGitContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r workerGitContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

type workerGitProof struct{ Head, Upstream, Dirty string }

// workerGitStart is captured before spawning the worker. It is immutable and
// stored only in the daemon-owned origin, never accepted from workspace input.
type workerGitStart struct {
	Cwd       string         `json:"cwd"`
	Proof     workerGitProof `json:"proof"`
	Worktrees []string       `json:"worktrees"`
}

func workerGitEnv() []string {
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

var workerGitOptions = []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "core.untrackedCache=false"}

func workerWorktrees(ctx context.Context, cwd string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, workerGitOptions...), "worktree", "list", "--porcelain", "-z")...)
	cmd.Dir, cmd.Env = cwd, workerGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	var path, head string
	prunable := false
	appendEntry := func() {
		if path == "" {
			return
		}
		// Only ignore a deleted, prunable entry whose commit is already in
		// the remote-ref cache. Existing directories or unpublished commits
		// still need inspection; a stale gitdir must not hide their work.
		_, statErr := os.Stat(path)
		if !(prunable && os.IsNotExist(statErr) && head != "" && workerGitCachedPublishedHead(ctx, cwd, head)) {
			paths = append(paths, path)
		}
		path, head, prunable = "", "", false
	}
	for _, item := range strings.Split(string(out), "\x00") {
		switch {
		case item == "":
			appendEntry()
		case strings.HasPrefix(item, "worktree "):
			appendEntry()
			path = strings.TrimPrefix(item, "worktree ")
		case strings.HasPrefix(item, "HEAD "):
			head = strings.TrimPrefix(item, "HEAD ")
		case item == "prunable" || strings.HasPrefix(item, "prunable "):
			prunable = true
		}
	}
	appendEntry()
	return paths, nil
}
func workerGitRoot(ctx context.Context, cwd string) string {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, workerGitOptions...), "rev-parse", "--show-toplevel")...)
	cmd.Dir, cmd.Env = cwd, workerGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func workerGitCachedPublishedHead(ctx context.Context, cwd, head string) bool {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, workerGitOptions...), "for-each-ref", "--contains="+head, "--format=%(objectname)", "refs/remotes/")...)
	cmd.Dir, cmd.Env = cwd, workerGitEnv()
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func captureWorkerGitStart(ctx context.Context, cwd string) *workerGitStart {
	proof, why := workerGitCleanPushed(ctx, cwd)
	if why != "" && why != "uncommitted work" && why != "unpushed work" {
		return nil
	}
	paths, err := workerWorktrees(ctx, cwd)
	if err != nil {
		return nil
	}
	return &workerGitStart{Cwd: cwd, Proof: proof, Worktrees: paths}
}

// No mapping from a newly created linked worktree to a pane exists in Git.
// Check every new entry conservatively instead of attributing another pane's
// work to this one. A live process cwd provides an additional concrete target.
func workerGitForArchive(ctx context.Context, snap PaneSnapshot, live string) (string, string) {
	var baseline *workerGitStart
	if snap.Origin != nil {
		baseline = snap.Origin.WorkerGit
	}
	proof, why := workerGitCleanPushed(ctx, snap.Cwd)
	if baseline != nil && baseline.Cwd == snap.Cwd && proof.Dirty == baseline.Proof.Dirty && (why == "uncommitted work" || why == "unpushed work") {
		// An unrelated clean commit must not make the person's unchanged dirt
		// become worker work. A changed HEAD still needs publication proof.
		if proof.Head == baseline.Proof.Head || workerGitCachedPublishedHead(ctx, snap.Cwd, proof.Head) {
			why = ""
		}
	}
	if why != "" {
		return "", why
	}
	proofs := map[string]workerGitProof{snap.Cwd: proof}
	// A subprocess in /tmp or $HOME is not evidence of a worker checkout.
	// Normalize repo subdirectories so the start snapshot still applies.
	liveRoot := ""
	if live != "" {
		liveRoot = workerGitRoot(ctx, live)
	}
	if liveRoot != "" && liveRoot != workerGitRoot(ctx, snap.Cwd) {
		live = liveRoot
		p, reason := workerGitCleanPushed(ctx, live)
		if reason != "" {
			return "", reason + " (worker process cwd)"
		}
		proofs[live] = p
	}
	if baseline != nil {
		paths, err := workerWorktrees(ctx, snap.Cwd)
		if err != nil {
			return "", "worktree list cannot be verified"
		}
		for _, path := range paths {
			if slices.Contains(baseline.Worktrees, path) || path == snap.Cwd {
				continue
			}
			p, reason := workerGitCleanPushed(ctx, path)
			if reason != "" {
				return "", reason + " (new worktree: " + path + ")"
			}
			proofs[path] = p
		}
	}
	raw, _ := json.Marshal(proofs) // sorted map keys bind both archive checks
	return string(raw), ""
}

func workerGitCleanPushed(ctx context.Context, cwd string) (workerGitProof, string) {
	// Do not inherit injected git config, alternate indexes or external diff.
	env := workerGitEnv()
	options := workerGitOptions
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append(append([]string{}, options...), args...)...)
		cmd.Dir, cmd.Env = cwd, env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	heads, err := run("rev-parse", "HEAD", "@{upstream}")
	parts := strings.Fields(heads)
	remoteContained := false
	if err != nil || len(parts) != 2 {
		// Detached preview/review checkouts have no upstream. A remote ref
		// containing HEAD supplies cached publication evidence without inventing
		// an upstream or ignoring dirty/unpublished work.
		head, headErr := run("rev-parse", "--verify", "HEAD")
		refs, refErr := run("for-each-ref", "--contains="+head, "--format=%(objectname)", "refs/remotes/")
		remoteHeads := strings.Fields(refs)
		if headErr != nil || refErr != nil {
			return workerGitProof{}, "work has no pushed upstream"
		}
		if len(remoteHeads) == 0 {
			// A valid local HEAD with no containing remote ref is known
			// unpublished work, unlike a failed repository/ref inspection.
			// Keep the qualified reason distinct from baseline-capable proofs:
			// this early return has not inspected the checkout's dirty bytes.
			return workerGitProof{}, "unpushed work (no pushed upstream)"
		}
		parts = []string{head, remoteHeads[0]}
		remoteContained = true
	}
	proof := workerGitProof{Head: parts[0], Upstream: parts[1]}
	paths, err := run("rev-parse", "--path-format=absolute", "--git-path", "objects", "--git-path", "index", "--show-toplevel")
	locations := strings.Split(paths, "\n")
	if err != nil || len(locations) != 3 {
		return workerGitProof{}, "worktree paths cannot be verified"
	}
	index, err := run("ls-files", "--stage", "-z")
	if err != nil {
		return workerGitProof{}, "worktree index cannot be verified"
	}
	indexByPath := map[string]string{}
	for _, line := range strings.Split(index, "\x00") {
		if metadata, path, ok := strings.Cut(line, "\t"); ok {
			indexByPath[path] += metadata + "\x00"
		}
		if strings.HasPrefix(line, "160000 ") {
			return workerGitProof{}, "submodule work requires manual review"
		}
	}
	// Status may invoke clean/process filters. Use a private Git directory
	// with no driver definitions, even if the worker concurrently edits its
	// config or attributes. Metadata-only commands above/below never run drivers.
	// Local info/exclude rules are deliberately omitted (conservative retention).
	view, err := os.MkdirTemp("", "solu-worker-git-*")
	if err != nil {
		return workerGitProof{}, "worktree status cannot be verified"
	}
	defer os.RemoveAll(view)
	for _, dir := range []string{"objects", "refs"} {
		if err := os.Mkdir(filepath.Join(view, dir), 0700); err != nil {
			return workerGitProof{}, "worktree status cannot be verified"
		}
	}
	if err := os.WriteFile(filepath.Join(view, "HEAD"), []byte(parts[0]+"\n"), 0600); err != nil {
		return workerGitProof{}, "worktree status cannot be verified"
	}
	cmd := exec.CommandContext(ctx, "git", append(options, "--git-dir="+view, "--work-tree="+locations[2], "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all", "--ignore-submodules=all")...)
	cmd.Dir = locations[2]
	cmd.Env = append(env, "GIT_OBJECT_DIRECTORY="+locations[0], "GIT_INDEX_FILE="+locations[1])
	out, err := cmd.Output()
	status := strings.TrimSpace(string(out))
	if err != nil {
		return workerGitProof{}, "worktree status cannot be verified"
	}
	if status != "" {
		// Porcelain alone cannot distinguish a worker editing an already-dirty
		// file. Bind the baseline to index entries and actual bytes, without
		// executing diff/clean drivers or following symlinks.
		h := sha256.New()
		h.Write(out)
		var total int64
		hashBuffer := make([]byte, 64<<10)
		for _, entry := range strings.Split(string(out), "\x00") {
			if ctx.Err() != nil {
				return workerGitProof{}, "worktree status cannot be verified"
			}
			if entry == "" {
				continue
			}
			if len(entry) < 4 {
				return workerGitProof{}, "worktree status cannot be verified"
			}
			h.Write([]byte(indexByPath[entry[3:]]))
			path := filepath.Join(locations[2], entry[3:])
			st, err := os.Lstat(path)
			if os.IsNotExist(err) && strings.Contains(entry[:2], "D") {
				continue
			}
			if err != nil {
				return workerGitProof{}, "worktree status cannot be verified"
			}
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00", entry[3:], st.Mode(), st.Size())
			if st.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return workerGitProof{}, "worktree status cannot be verified"
				}
				h.Write([]byte(target))
			} else {
				f, _, err := openUserFile(path, 16<<20)
				if err != nil {
					return workerGitProof{}, "worktree status cannot be verified"
				}
				n, err := io.CopyBuffer(h, workerGitContextReader{ctx, io.LimitReader(f, 16<<20+1)}, hashBuffer)
				f.Close()
				total += n
				if err != nil || ctx.Err() != nil || n > 16<<20 || total > 64<<20 {
					return workerGitProof{}, "worktree status cannot be verified"
				}
			}
		}
		proof.Dirty = hex.EncodeToString(h.Sum(nil))
		return proof, "uncommitted work"
	}
	if !remoteContained {
		ahead, err := run("rev-list", "--count", "@{upstream}..HEAD")
		if err != nil || ahead != "0" {
			return proof, "unpushed work"
		}
	}
	return proof, ""
}
