package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Read-only git-diff endpoint backing the DiffTile — "review what the agent
// just changed" without leaving the terminal. GET /api/git/diff?path=<dir or
// file>&staged=<0|1>. Strictly read-only (no apply/stage/commit). Over the
// cloud relay, secret/credential files are excluded from the diff text and
// flagged in the file list, mirroring the file-read guard.

const (
	maxDiffBytes = 512 * 1024 // 512 KB of unified diff — past this the UI truncates
)

type gitDiffFile struct {
	Path    string `json:"path"`    // relative to repo root
	Status  string `json:"status"`  // M, A, D, R…, C…, etc.
	Added   int    `json:"added"`   // -1 for binary
	Removed int    `json:"removed"` // -1 for binary
	Secret  bool   `json:"secret"`  // hidden from the diff text (relay only)
}

type gitDiffResp struct {
	Repo         string        `json:"repo"`   // repo toplevel (absolute)
	Branch       string        `json:"branch"` // current branch, or detached SHA
	Staged       bool          `json:"staged"`
	Files        []gitDiffFile `json:"files"`
	Diff         string        `json:"diff"`
	Truncated    bool          `json:"truncated"`    // diff text hit maxDiffBytes
	HiddenSecret int           `json:"hiddenSecret"` // # secret files omitted from diff (relay)
	// SkippedUntracked: files the working-tree snapshot left out or kept as
	// recorded (secret-looking, over the size limit, or too many).
	SkippedUntracked int `json:"skippedUntracked,omitempty"`
}

func (a *apiServer) gitDiff(w http.ResponseWriter, r *http.Request) {
	abs, err := canonicalizeFilePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if reason := credentialPathRefusal(abs, a.dataDir); reason != "" {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "cannot stat: "+err.Error(), 404)
		return
	}
	dir := abs
	if !info.IsDir() {
		dir = filepath.Dir(abs)
	}
	// Over the relay both the path and its repository must be within a
	// workspace root: a directory diff covers the whole repository, so a root
	// nested inside a repo must not expose its siblings (files_relay_roots.go).
	if scope := a.relayFileScopeFor(r); scope != nil {
		reason := scope.refusal(abs)
		if top := gitTopLevel(dir); reason == "" && top != "" {
			reason = scope.refusal(top)
		}
		if reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}
	staged := r.URL.Query().Get("staged") == "1" || r.URL.Query().Get("staged") == "true"
	resp, status, err := buildWorkingDiff(r.Context(), dir, abs, !info.IsDir(), staged, fileAccessOverRelay(r), a.dataDir)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, 200, resp)
}

// buildWorkingDiff answers /api/git/diff with the same hardening as turn
// diffs: git never reads the working tree (so no clean filter, fsmonitor,
// external diff or textconv the repository configures can run as the
// daemon), no transport can run, and output is capped. The unstaged view is
// the index against a daemon-built snapshot of the working tree, which also
// carries untracked files (the snapshot itself never reads outside the repo
// and skips secret-looking untracked files).
func buildWorkingDiff(parent context.Context, dir, abs string, isFile, staged, overRelay bool, dataDir string) (gitDiffResp, int, error) {
	ctx, cancel := context.WithTimeout(parent, turnSnapshotTimeout)
	defer cancel()
	release, err := acquireDiffEndpointSlot(ctx)
	defer release()
	if err != nil {
		return gitDiffResp{}, 503, err
	}
	repoRoot, err := turnResolveRepo(ctx, dir)
	if err != nil {
		return gitDiffResp{}, 400, err
	}
	if err := turnRepoRefusal(ctx, repoRoot); err != nil {
		return gitDiffResp{}, 409, fmt.Errorf("diffs aren't available for partial-clone repositories (git would fetch missing files from the remote)")
	}
	scope := ""
	if isFile {
		real, err1 := filepath.EvalSymlinks(abs)
		rootReal, err2 := filepath.EvalSymlinks(repoRoot)
		rel, err3 := filepath.Rel(rootReal, real)
		if err1 != nil || err2 != nil || err3 != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return gitDiffResp{}, 400, fmt.Errorf("file is outside the repository")
		}
		scope = filepath.ToSlash(rel)
	}
	branch, _ := turnGit(ctx, repoRoot, nil, nil, "rev-parse", "--abbrev-ref", "HEAD")

	var rev []string
	secretSkipped, skipped := 0, 0
	if staged {
		rev = []string{"--cached"} // HEAD vs index: git reads no worktree file
	} else {
		snap, err := gitTurnSnapshotter{dataDir: dataDir}.snapshot(ctx, repoRoot)
		if errors.Is(err, errTurnSparse) {
			return gitDiffResp{}, 409, fmt.Errorf("the unstaged diff isn't available for sparse-checkout repositories — use Staged")
		}
		if err != nil {
			return gitDiffResp{}, 409, fmt.Errorf("working-tree diff unavailable: %v", err)
		}
		// -R --cached <snapshot>: the index against the snapshot, i.e. what
		// changed in the working tree (untracked files show as added).
		rev = []string{"-R", "--cached", snap.tree}
		secretSkipped, skipped = snap.secretSkipped, snap.skipped
		if scope != "" {
			// Repo-wide counts don't describe a single-file view.
			secretSkipped, skipped = 0, 0
		}
	}
	d, err := hardenedDiff(ctx, repoRoot, rev, scope, overRelay, dataDir)
	if err != nil {
		return gitDiffResp{}, 500, err
	}
	hidden := d.hidden
	if overRelay {
		hidden += secretSkipped // "hidden from this remote diff" is a relay-only notice
	}
	return gitDiffResp{
		Repo:         repoRoot,
		Branch:       strings.TrimSpace(branch),
		Staged:       staged,
		Files:        d.files,
		Diff:         d.text,
		Truncated:    d.truncated,
		HiddenSecret: hidden,
		// Locally, secret-looking untracked files are still left out of the
		// snapshot (never written into .git/objects) — say so.
		SkippedUntracked: skipped,
	}, 200, nil
}

type hardenedDiffResult struct {
	files     []gitDiffFile
	text      string
	truncated bool
	hidden    int
}

// hardenedDiff runs numstat / name-status / patch for rev (trees or
// --cached forms, never the working tree) with every repository-configured
// program disabled. The file list is read in full (-z, stdout only, capped —
// an over-long list is an error, never a silently partial one), so every
// secret path is flagged before the patch text is produced with those paths
// excluded.
func hardenedDiff(ctx context.Context, repo string, rev []string, scope string, overRelay bool, dataDir string) (hardenedDiffResult, error) {
	var res hardenedDiffResult
	// Explicit prefixes (diff.noprefix / mnemonicPrefix can't change them);
	// -R swaps them, so pass them pre-swapped to keep "a/old b/new" headers.
	prefixes := []string{"--src-prefix=a/", "--dst-prefix=b/"}
	if len(rev) > 0 && rev[0] == "-R" {
		prefixes = []string{"--src-prefix=b/", "--dst-prefix=a/"}
	}
	// --submodule=short: diff.submodule=diff would run a nested git inside the
	// submodule, honouring ITS config (diff.external …) without our flags.
	base := append(append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short"}, prefixes...), rev...)
	spec := []string{"--", "."}
	if scope != "" {
		spec = []string{"--", ":(literal)" + scope}
	}
	withArgs := func(extra ...string) []string {
		out := append(append([]string{}, base...), extra...)
		return append(out, spec...)
	}
	numOut, err := turnGit(ctx, repo, nil, nil, withArgs("--numstat", "-z")...)
	if err != nil {
		return res, err
	}
	statOut, err := turnGit(ctx, repo, nil, nil, withArgs("--name-status", "-z")...)
	if err != nil {
		return res, err
	}
	files, secretRel, hidden := parseGitDiffFiles(repo, numOut, statOut, overRelay, dataDir)
	textArgs := withArgs()
	for _, p := range secretRel {
		textArgs = append(textArgs, ":(exclude,literal)"+p)
	}
	text, truncated, err := turnGitLimited(ctx, repo, nil, nil, maxDiffBytes, textArgs...)
	if err != nil {
		return res, err
	}
	res.files, res.text, res.truncated, res.hidden = files, text, truncated, hidden
	if res.files == nil {
		res.files = []gitDiffFile{}
	}
	return res, nil
}

// parseGitDiffFiles merges `git diff --numstat` (counts) and
// `--name-status` (status letter) into a per-file summary. When overRelay,
// secret/credential files are flagged and their relative paths returned so
// the caller can exclude them from the diff text.
func parseGitDiffFiles(repoRoot, numstat, namestatus string, overRelay bool, dataDirs ...string) (files []gitDiffFile, secretRel []string, hidden int) {
	dataDir := ""
	if len(dataDirs) > 0 {
		dataDir = dataDirs[0]
	}
	type numRow struct {
		added, removed int
		path           string
	}
	var rows []numRow
	status := map[string]string{}
	if strings.Contains(numstat, "\x00") || strings.Contains(namestatus, "\x00") {
		// -z output (preferred): paths arrive raw. Without -z git C-quotes any
		// path with non-ASCII, tab, quote or backslash ("caf\303\251/.env"),
		// which defeats both the secret-path check and the exclude pathspec.
		for _, rec := range strings.Split(strings.TrimRight(numstat, "\x00"), "\x00") {
			parts := strings.SplitN(rec, "\t", 3)
			if len(parts) < 3 || parts[2] == "" {
				continue
			}
			rows = append(rows, numRow{numstatCount(parts[0]), numstatCount(parts[1]), parts[2]})
		}
		fields := strings.Split(strings.TrimRight(namestatus, "\x00"), "\x00")
		for i := 0; i+1 < len(fields); i += 2 {
			status[fields[i+1]] = fields[i]
		}
	} else {
		for _, line := range strings.Split(strings.TrimSpace(namestatus), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) < 2 {
				continue
			}
			// For renames/copies the new path is the last field.
			status[parts[len(parts)-1]] = parts[0]
		}
		for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) < 3 {
				continue
			}
			rows = append(rows, numRow{numstatCount(parts[0]), numstatCount(parts[1]), parts[len(parts)-1]})
		}
	}
	for _, r := range rows {
		p := r.path
		f := gitDiffFile{Path: p, Status: status[p], Added: r.added, Removed: r.removed}
		// A path git still had to quote can't be checked reliably: hide it
		// over the relay rather than guess.
		quoted := strings.HasPrefix(p, "\"")
		if credentialPathRefusal(filepath.Join(repoRoot, p), dataDir) != "" ||
			(overRelay && (quoted || secretPathRefusal(filepath.Join(repoRoot, p)) != "")) {
			f.Secret = true
			secretRel = append(secretRel, p)
			hidden++
		}
		files = append(files, f)
	}
	return files, secretRel, hidden
}

// numstatCount parses a --numstat count; "-" (binary) is -1.
func numstatCount(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return -1
}
