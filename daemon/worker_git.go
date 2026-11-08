package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
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

type workerGitProof struct {
	Head, Upstream, Dirty string
	// Paths is each dirty path's own digest, set with Dirty.
	Paths map[string]string `json:",omitempty"`
}

// workerGitMaxDirtyPaths bounds the per-path digests kept in a pane's saved
// origin. A checkout with more dirty paths keeps only the whole digest.
const workerGitMaxDirtyPaths = 2000

// workerGitDirtWithinBaseline reports whether every dirty path in proof was
// already dirty, byte for byte, in the start baseline: dirt a shared checkout
// had before the worker started, some of which others may since have removed
// or committed. A new or changed path is not within it. A baseline from before
// per-path digests needs the whole digest to match, as it did then.
func workerGitDirtWithinBaseline(proof, baseline workerGitProof) bool {
	if proof.Dirty == "" || proof.Dirty == baseline.Dirty {
		return true
	}
	if len(baseline.Paths) == 0 || len(proof.Paths) == 0 {
		return false
	}
	for path, digest := range proof.Paths {
		if baseline.Paths[path] != digest {
			return false
		}
	}
	return true
}

// workerGitStart is captured before spawning the worker. It is immutable and
// stored only in the daemon-owned origin, never accepted from workspace input.
type workerGitStart struct {
	Cwd       string         `json:"cwd"`
	Proof     workerGitProof `json:"proof"`
	Worktrees []string       `json:"worktrees"`
}

// Worker git runs through hardenedGitCommand (git_env.go), the environment
// and options every daemon git uses: no inherited GIT_* variable, no user
// config, no transport, replace refs ignored (a worker cannot graft history to
// change what is published or merged). Repository config still applies, so
// commands are plumbing that runs no programs from it; the options are defence
// in depth for the rest.

func workerWorktrees(ctx context.Context, cwd string) ([]string, error) {
	cmd := hardenedGitCommand(ctx, cwd, "worktree", "list", "--porcelain", "-z")
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
	cmd := hardenedGitCommand(ctx, cwd, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func workerGitCachedPublishedHead(ctx context.Context, cwd, head string) bool {
	cmd := hardenedGitCommand(ctx, cwd, "for-each-ref", "--contains="+head, "--format=%(objectname)", "refs/remotes/")
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
	if baseline != nil && baseline.Cwd == snap.Cwd && workerGitDirtWithinBaseline(proof, baseline.Proof) && (why == "uncommitted work" || why == "unpushed work") {
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

// workerGitUnreadable prefixes git's own reason when a checkout's HEAD
// cannot be read at all (not a repository, owned by another user, …):
// uncertainty, never evidence of unpublished work.
const workerGitUnreadable = "repository cannot be read: "

func workerGitCleanPushed(ctx context.Context, cwd string) (workerGitProof, string) {
	return workerGitInspect(ctx, cwd, true)
}

// workerGitInspect proves a checkout clean, and when published, its HEAD
// pushed. The merged close inspects without publication: a squash-merged
// branch whose remote ref was pruned is clean but has no containing ref.
func workerGitInspect(ctx context.Context, cwd string, published bool) (workerGitProof, string) {
	run := func(args ...string) (string, error) {
		out, err := hardenedGitCommand(ctx, cwd, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	var heads string
	var err error
	if published {
		heads, err = run("rev-parse", "HEAD", "@{upstream}")
	} else if heads, err = run("rev-parse", "--verify", "HEAD"); err != nil {
		return workerGitProof{}, workerGitUnreadable + gitErrorText(err)
	}
	parts := strings.Fields(heads)
	remoteContained := !published
	if !published {
		parts = append(parts, "")
	} else if err != nil || len(parts) != 2 {
		// Detached preview/review checkouts have no upstream. A remote ref
		// containing HEAD supplies cached publication evidence without inventing
		// an upstream or ignoring dirty/unpublished work.
		head, headErr := run("rev-parse", "--verify", "HEAD")
		if headErr != nil {
			return workerGitProof{}, workerGitUnreadable + gitErrorText(headErr)
		}
		refs, refErr := run("for-each-ref", "--contains="+head, "--format=%(objectname)", "refs/remotes/")
		remoteHeads := strings.Fields(refs)
		if refErr != nil {
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
	// Status may invoke clean/process filters: hardenedGitStatusView runs it
	// against a private Git directory with no driver definitions, even if the
	// worker concurrently edits its config or attributes. Metadata-only
	// commands above/below never run drivers. Local info/exclude rules are
	// deliberately omitted (conservative retention).
	out, err := hardenedGitStatusView(ctx, locations[0], locations[1], locations[2], parts[0], "--untracked-files=all")
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
		// Each dirty path also gets its own digest (its status code, index
		// entry and bytes), so a shared checkout's baseline can tell dirt
		// that was already there from dirt that is new or changed (ST-60).
		paths := map[string]string{}
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
			ph := sha256.New()
			ph.Write([]byte(entry[:2] + "\x00"))
			w := io.MultiWriter(h, ph)
			w.Write([]byte(indexByPath[entry[3:]]))
			path := filepath.Join(locations[2], entry[3:])
			st, err := os.Lstat(path)
			if os.IsNotExist(err) && strings.Contains(entry[:2], "D") {
				paths[entry[3:]] = hex.EncodeToString(ph.Sum(nil))
				continue
			}
			if err != nil {
				return workerGitProof{}, "worktree status cannot be verified"
			}
			fmt.Fprintf(w, "%s\x00%s\x00%d\x00", entry[3:], st.Mode(), st.Size())
			if st.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return workerGitProof{}, "worktree status cannot be verified"
				}
				w.Write([]byte(target))
			} else {
				f, info, err := openUserFile(path, -1)
				if err != nil {
					return workerGitProof{}, "worktree status cannot be verified"
				}
				n, err := workerGitHashFile(ctx, w, f, info, hashBuffer)
				f.Close()
				total += n
				if err != nil || ctx.Err() != nil || total > 64<<20 {
					return workerGitProof{}, "worktree status cannot be verified"
				}
			}
			paths[entry[3:]] = hex.EncodeToString(ph.Sum(nil))
		}
		proof.Dirty = hex.EncodeToString(h.Sum(nil))
		if len(paths) <= workerGitMaxDirtyPaths {
			proof.Paths = paths
		}
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

// workerGitWholeFileBytes is the largest dirty file hashed whole. A larger one
// (release binaries copied into a checkout, ST-60) is fingerprinted by its
// size, modification time and first and last workerGitSampleBytes instead of
// making the whole checkout unverifiable: a worker that changes such a file
// changes its time, its length or almost always its first or last MiB.
const (
	workerGitWholeFileBytes = 16 << 20
	workerGitSampleBytes    = 1 << 20
)

// workerGitHashFile writes an open regular file's content proof to h and
// returns the bytes it read. info is the opened file's fstat.
func workerGitHashFile(ctx context.Context, h io.Writer, f *os.File, info os.FileInfo, buf []byte) (int64, error) {
	size := info.Size()
	if size <= workerGitWholeFileBytes {
		n, err := io.CopyBuffer(h, workerGitContextReader{ctx, io.LimitReader(f, workerGitWholeFileBytes+1)}, buf)
		if err == nil && n > workerGitWholeFileBytes {
			err = errors.New("file grew while it was read")
		}
		return n, err
	}
	fmt.Fprintf(h, "sampled\x00%d\x00", info.ModTime().UnixNano())
	head, err := io.CopyBuffer(h, workerGitContextReader{ctx, io.NewSectionReader(f, 0, workerGitSampleBytes)}, buf)
	if err != nil {
		return head, err
	}
	tail, err := io.CopyBuffer(h, workerGitContextReader{ctx, io.NewSectionReader(f, size-workerGitSampleBytes, workerGitSampleBytes)}, buf)
	if err == nil && head+tail != 2*workerGitSampleBytes {
		err = errors.New("file shrank while it was read")
	}
	return head + tail, err
}

func workerGitRun(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := hardenedGitCommand(ctx, cwd, args...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// workerGitDefaultRef is the cached remote default branch. The daemon does not
// fetch: its Git environment has no global credential helpers, and a fetch
// would run repository-configured transports. Any pane's fetch refreshes the
// shared ref; a stale ref only delays (never hastens) a merged close.
func workerGitDefaultRef(ctx context.Context, cwd string) string {
	if ref, err := workerGitRun(ctx, cwd, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && strings.HasPrefix(ref, "refs/remotes/origin/") {
		return ref
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if _, err := workerGitRun(ctx, cwd, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return ""
}

func workerGitNames(ctx context.Context, cwd, from, to string) (map[string]bool, error) {
	out, err := workerGitRun(ctx, cwd, "diff-tree", "-r", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			names[name] = true
		}
	}
	return names, nil
}

// workerGitLanded reports whether head's work is on ref: by ancestry, or for
// cherry-picked/squashed integration, when every path head changed since the
// merge base is byte- and mode-identical on ref. git cherry's patch-ids ignore
// whitespace and skip merge commits, so they alone cannot prove nothing was
// lost. Tree comparison runs no diff drivers. Later edits on ref to the same
// paths fail closed (the worker stays open as before).
func workerGitLanded(ctx context.Context, cwd, head, ref string) bool {
	if head == "" || ref == "" {
		return false
	}
	if _, err := workerGitRun(ctx, cwd, "merge-base", "--is-ancestor", head, ref); err == nil {
		return true
	}
	base, err := workerGitRun(ctx, cwd, "merge-base", head, ref)
	if err != nil || base == "" {
		return false
	}
	changed, err := workerGitNames(ctx, cwd, base, head)
	if err != nil {
		return false
	}
	differ, err := workerGitNames(ctx, cwd, head, ref)
	if err != nil {
		return false
	}
	for name := range changed {
		if differ[name] {
			return false
		}
	}
	return true
}

// workerGitCreatedCommit reports whether a HEAD reflog message records a
// commit made in that checkout, as opposed to moving HEAD to an existing one
// (pull, fetch-and-reset, checkout, merge of another branch).
func workerGitCreatedCommit(msg string) bool {
	if strings.HasPrefix(msg, "commit:") || strings.HasPrefix(msg, "commit (") || strings.HasPrefix(msg, "cherry-pick:") {
		return true
	}
	for _, prefix := range []string{"rebase (", "rebase -i (", "rebase -m (", "pull --rebase ("} {
		if rest, ok := strings.CutPrefix(msg, prefix); ok {
			for _, step := range []string{"pick)", "reword)", "edit)", "squash)", "fixup)", "continue)"} {
				if strings.HasPrefix(rest, step) {
					return true
				}
			}
		}
	}
	return false
}

// workerReflogEntry is one line of a checkout's HEAD reflog.
type workerReflogEntry struct {
	Old, New, Msg string
	At            int64 // unix seconds, as Git records them
}

// workerGitReflog reads a checkout's HEAD reflog, oldest first, as a file:
// no porcelain that could verify signatures. complete is false when only the
// newest 1MiB was read, so the first entry is not the creation entry.
func workerGitReflog(ctx context.Context, cwd string) (entries []workerReflogEntry, complete bool) {
	gitdir, err := workerGitRun(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil || !filepath.IsAbs(gitdir) {
		return nil, false
	}
	f, info, err := openUserFile(filepath.Join(gitdir, "logs", "HEAD"), -1)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	const window = 1 << 20
	complete = info.Size() <= window
	if !complete {
		if _, err := f.Seek(info.Size()-window, io.SeekStart); err != nil {
			return nil, false
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, window))
	if err != nil {
		return nil, false
	}
	lines := strings.Split(string(raw), "\n")
	if !complete && len(lines) > 0 {
		lines = lines[1:] // landed mid-line
	}
	for _, line := range lines {
		meta, msg, _ := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if len(fields) < 4 || !workerGitObjectName(fields[0]) || !workerGitObjectName(fields[1]) {
			continue
		}
		at, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, workerReflogEntry{Old: fields[0], New: fields[1], Msg: msg, At: at})
	}
	return entries, complete
}

func workerGitObjectName(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// workerToolCall is one tool call from the worker's own transcript: when it
// ran, which tool, what it asked for, what it printed and whether the tool
// reported an error.
type workerToolCall struct {
	Start, End          time.Time
	Name, Input, Output string
	Error               bool
}

// workerToolCallSlack covers Git's whole-second reflog times and clock
// rounding between the agent and Git.
const workerToolCallSlack = 2 * time.Second

func (c workerToolCall) ranAt(unix int64) bool {
	at := time.Unix(unix, 0)
	return !at.Before(c.Start.Add(-workerToolCallSlack)) && !at.After(c.End.Add(workerToolCallSlack))
}

// workerTranscriptCalls reads the worker's own transcript (Claude tool_use and
// tool_result pairs, Codex function and custom tool calls with their outputs)
// once per check for the calls that can be evidence: git commands and
// EnterWorktree. Git records when a commit or worktree was made; only the
// worker's transcript says the worker was running a command then. A
// cancelled ctx yields no calls, which proves nothing.
func workerTranscriptCalls(ctx context.Context, path string) []workerToolCall {
	if path == "" || ctx.Err() != nil {
		return nil
	}
	buf, err := tailReadBounded(path, workerTranscriptCallBytes)
	if err != nil {
		return nil
	}
	pending := map[string]workerToolCall{}
	var calls []workerToolCall
	finish := func(id string, end time.Time, output json.RawMessage, failed bool) {
		c, ok := pending[id]
		if !ok || id == "" {
			return
		}
		delete(pending, id)
		c.End, c.Output, c.Error = end, workerTranscriptText(output), failed
		if !c.End.Before(c.Start) && (c.Name == "EnterWorktree" || strings.Contains(c.Input, "git")) {
			calls = append(calls, c)
		}
	}
	for i, line := range bytes.Split(buf, []byte("\n")) {
		if i%256 == 0 && ctx.Err() != nil {
			return nil
		}
		var e struct {
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Payload struct {
				Type      string          `json:"type"`
				Name      string          `json:"name"`
				CallID    string          `json:"call_id"`
				Input     json.RawMessage `json:"input"`
				Arguments json.RawMessage `json:"arguments"`
				Output    json.RawMessage `json:"output"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil || e.Timestamp.IsZero() {
			continue
		}
		switch e.Payload.Type {
		case "function_call", "custom_tool_call":
			input := e.Payload.Input
			if len(input) == 0 {
				input = e.Payload.Arguments
			}
			pending[e.Payload.CallID] = workerToolCall{Start: e.Timestamp, Name: e.Payload.Name, Input: workerTranscriptText(input)}
			continue
		case "function_call_output", "custom_tool_call_output":
			finish(e.Payload.CallID, e.Timestamp, e.Payload.Output, false)
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				pending[b.ID] = workerToolCall{Start: e.Timestamp, Name: b.Name, Input: workerTranscriptText(b.Input)}
			case "tool_result":
				finish(b.ToolUseID, e.Timestamp, b.Content, b.IsError)
			}
		}
	}
	return calls
}

const workerTranscriptCallBytes = 32 << 20

// workerTranscriptText flattens a transcript value: a string as itself, an
// object's string fields and an array's text parts one per line.
func workerTranscriptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, part := range parts {
			texts = append(texts, part.Text)
		}
		return strings.Join(texts, "\n")
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) == nil {
		var texts []string
		for _, v := range fields {
			if s, ok := v.(string); ok {
				texts = append(texts, s)
			}
		}
		slices.Sort(texts)
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

// workerTextNames reports whether text names path segment name after parent
// (".claude/worktrees/x", either separator), not merely a longer name.
func workerTextNames(text, parent, name string) bool {
	for _, sep := range []string{"/", `\`} {
		needle := parent + sep + name
		for i := 0; ; {
			j := strings.Index(text[i:], needle)
			if j < 0 {
				break
			}
			end := i + j + len(needle)
			if end == len(text) || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-", rune(text[end])) {
				return true
			}
			i = end
		}
	}
	return false
}

// workerGitOwnCommit reports whether the worker made a commit in checkout cwd
// that is in head's history and not in base's (the checkout's HEAD when the
// worker started, or when the worker created it). It needs both records: the
// checkout's reflog says a commit was made there and when, and the worker's
// transcript shows a git command of its own running then that printed the
// commit or names the reflog's action. A shared checkout's reflog alone
// counts other people's commits. Missing records prove nothing, so the worker
// keeps the read-receipt rule.
func workerGitOwnCommit(ctx context.Context, cwd, head, base string, calls []workerToolCall) bool {
	if head == "" || base == "" || head == base {
		return false
	}
	entries, _ := workerGitReflog(ctx, cwd)
	var made []workerReflogEntry
	seen := map[string]bool{}
	for i := len(entries) - 1; i >= 0 && len(made) < 200; i-- {
		if e := entries[i]; workerGitCreatedCommit(e.Msg) && !seen[e.New] {
			seen[e.New] = true
			made = append(made, e)
		}
	}
	for _, e := range made {
		verb := strings.TrimSuffix(strings.Fields(e.Msg)[0], ":") // commit, cherry-pick, rebase, pull
		ran := slices.ContainsFunc(calls, func(c workerToolCall) bool {
			return c.ranAt(e.At) && strings.Contains(c.Input, "git") && (strings.Contains(c.Output, e.New[:7]) || strings.Contains(c.Input, verb))
		})
		if !ran {
			continue
		}
		if _, err := workerGitRun(ctx, cwd, "merge-base", "--is-ancestor", e.New, base); err == nil {
			continue
		}
		if _, err := workerGitRun(ctx, cwd, "merge-base", "--is-ancestor", e.New, head); err == nil {
			return true
		}
	}
	return false
}

// workerGitLinkedWorktree proves path is a linked worktree of the repository
// whose common dir is common, under that repository's .claude/worktrees, and
// that its admin entry is its own: <admin>/gitdir points back at <path>/.git,
// as git worktree remove checks, so a crafted .git file cannot borrow a
// sibling's entry. It returns the admin entry and the repository root.
func workerGitLinkedWorktree(ctx context.Context, path, common string) (string, string, error) {
	path = filepath.Clean(path)
	top, err := workerGitRun(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(top) != path {
		return "", "", fmt.Errorf("not a worktree root: %s", path)
	}
	own, err := workerGitRun(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Base(own) != ".git" || (common != "" && filepath.Clean(own) != filepath.Clean(common)) {
		return "", "", fmt.Errorf("%s is not in the worker's repository", path)
	}
	common = filepath.Clean(own)
	root := filepath.Dir(common)
	rel, err := filepath.Rel(filepath.Join(root, ".claude", "worktrees"), path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("%s is outside %s/.claude/worktrees", path, root)
	}
	admin, err := workerGitRun(ctx, path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil || filepath.Dir(filepath.Clean(admin)) != filepath.Join(common, "worktrees") {
		return "", "", fmt.Errorf("%s is not a linked worktree", path)
	}
	admin = filepath.Clean(admin)
	f, _, err := openUserFile(filepath.Join(admin, "gitdir"), 4096)
	if err != nil {
		return "", "", fmt.Errorf("%s has no worktree entry: %v", path, err)
	}
	raw, err := io.ReadAll(f)
	f.Close()
	back := strings.TrimSpace(string(raw))
	if err == nil && back != "" && !filepath.IsAbs(back) {
		back = filepath.Join(admin, back)
	}
	if err != nil || back == "" || filepath.Clean(back) != filepath.Join(path, ".git") {
		return "", "", fmt.Errorf("worktree entry %s does not belong to %s", admin, path)
	}
	return admin, root, nil
}

// workerGitCreatedWorktree is the commit a linked worktree started at, when
// the worker created it: the worktree's first reflog entry (written by git
// worktree add, from nothing) falls inside one of the worker's own tool calls
// that is the creation itself. A worktree the person, a lead or a sibling
// made, before or after, has no such call; a call that only lists or names
// it (git worktree list, ls) is not one.
func workerGitCreatedWorktree(ctx context.Context, path string, calls []workerToolCall) string {
	entries, complete := workerGitReflog(ctx, path)
	if !complete || len(entries) == 0 || strings.Trim(entries[0].Old, "0") != "" {
		return ""
	}
	first := entries[0]
	for _, c := range calls {
		if c.ranAt(first.At) && workerCallCreatesWorktree(c, path, first.New) {
			return first.New
		}
	}
	return ""
}

// workerCallCreatesWorktree reports whether a tool call is the successful
// creation of the worktree at path, whose first commit is head: a git
// worktree add whose own command names the folder and that printed git's
// success line for that commit ("HEAD is now at <head>") and no fatal error,
// or Claude's EnterWorktree creating it by name and printing its path. A
// failed add ("already exists") while a sibling created that folder is not
// creation; a quiet add proves nothing.
func workerCallCreatesWorktree(c workerToolCall, path, head string) bool {
	if c.Error || !workerCallNamesWorktree(c, path) {
		return false
	}
	if c.Name == "EnterWorktree" {
		return strings.Contains(c.Output, path)
	}
	return len(head) >= 7 && strings.Contains(c.Output, "HEAD is now at "+head[:7]) && !strings.Contains(c.Output, "fatal:")
}

// workerCallNamesWorktree reports whether a tool call asks to create the
// worktree at path: a git worktree add whose own command (up to the next &&,
// ||, ;, | or newline) names the folder, or EnterWorktree by its name.
func workerCallNamesWorktree(c workerToolCall, path string) bool {
	name := filepath.Base(path)
	if c.Name == "EnterWorktree" {
		return strings.TrimSpace(c.Input) == name
	}
	for rest := c.Input; ; {
		i := strings.Index(rest, "worktree add")
		if i < 0 {
			return false
		}
		rest = rest[i+len("worktree add"):]
		segment := rest
		if end := strings.IndexAny(segment, "&|;\n"); end >= 0 {
			segment = segment[:end]
		}
		if workerTextNames(segment, "worktrees", name) {
			return true
		}
	}
}

// workerGitMerged decides "merged = done" from workerGitCheckouts' proof.
// Every checkout in it must be clean, except that the start checkout may keep
// dirt that was already there, byte for byte, when the worker started (a
// shared primary checkout always has some), and its HEAD must have landed on
// the remote default branch. And the worker must have merged work of its own: a commit
// it made in its start checkout since it started, or in a worktree it created
// and worked in (worked: its transcript and process cwds). A checkout it only
// worked in must be clean and landed but never counts as its own work, so a
// worker without commits of its own keeps the read-receipt rule.
func workerGitMerged(ctx context.Context, snap PaneSnapshot, proofJSON string, calls []workerToolCall, worked ...string) bool {
	if snap.Origin == nil || snap.Origin.WorkerGit == nil {
		return false
	}
	start := snap.Origin.WorkerGit
	var proofs map[string]workerGitProof
	if json.Unmarshal([]byte(proofJSON), &proofs) != nil || len(proofs) == 0 {
		return false
	}
	for cwd, proof := range proofs {
		if proof.Head == "" || (proof.Dirty != "" && !(cwd == start.Cwd && workerGitDirtWithinBaseline(proof, start.Proof))) {
			return false
		}
		if !workerGitLanded(ctx, cwd, proof.Head, workerGitDefaultRef(ctx, cwd)) {
			return false
		}
	}
	if start.Cwd == snap.Cwd && workerGitOwnCommit(ctx, snap.Cwd, proofs[snap.Cwd].Head, start.Proof.Head, calls) {
		return true
	}
	for _, cwd := range worked {
		if root, base := workerOwnWorktree(ctx, snap, cwd, proofJSON, calls); root != "" && workerGitOwnCommit(ctx, root, proofs[root].Head, base, calls) {
			return true
		}
	}
	return false
}

// workerCreatedWorktree is the commit linked worktree root started at, only
// on positive proof that this worker created it: it shares the start
// checkout's repository (same common dir), sits under that repository's
// .claude/worktrees with its own admin entry, did not exist when the worker
// started, and its creation falls inside the worker's own tool call that
// created it. Anything else (a worktree in another repository, or one the
// person, a lead or a sibling made) is "".
func workerCreatedWorktree(ctx context.Context, snap PaneSnapshot, root string, calls []workerToolCall) string {
	if root == "" || snap.Origin == nil || snap.Origin.WorkerGit == nil {
		return ""
	}
	start := snap.Origin.WorkerGit
	if root == snap.Cwd || root == start.Cwd || slices.Contains(start.Worktrees, root) {
		return ""
	}
	if !slices.ContainsFunc(calls, func(c workerToolCall) bool { return workerCallNamesWorktree(c, root) }) {
		return "" // cheap: no git for a folder the worker never asked to create
	}
	common, err := workerGitRun(ctx, start.Cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !filepath.IsAbs(common) {
		return ""
	}
	if _, _, err := workerGitLinkedWorktree(ctx, root, common); err != nil {
		return ""
	}
	return workerGitCreatedWorktree(ctx, root, calls)
}

// workerGitCheckouts is the merged close's proof: every checkout of the
// worker's own (its start checkout, the repositories of the cwds it worked
// in, and worktrees it provably created since it started) clean, with its
// HEAD. A sibling's worktree, active or abandoned, is not part of it, so it
// never holds this worker open. Empty when any checkout is dirty or cannot be
// inspected.
func workerGitCheckouts(ctx context.Context, snap PaneSnapshot, calls []workerToolCall, cwds ...string) string {
	if snap.Origin == nil || snap.Origin.WorkerGit == nil {
		return ""
	}
	targets := []string{snap.Cwd}
	for _, cwd := range cwds {
		if cwd == "" {
			continue
		}
		if root := workerGitRoot(ctx, cwd); root != "" {
			targets = append(targets, root)
		}
	}
	paths, err := workerWorktrees(ctx, snap.Cwd)
	if err != nil {
		return ""
	}
	for _, path := range paths {
		if workerCreatedWorktree(ctx, snap, path, calls) != "" {
			targets = append(targets, path)
		}
	}
	proofs := map[string]workerGitProof{}
	for _, path := range targets {
		if _, seen := proofs[path]; seen {
			continue
		}
		proof, why := workerGitInspect(ctx, path, false)
		// The start checkout may keep dirt it already had when the worker
		// started (a shared primary checkout, ST-60); anything new or changed
		// there, and any dirt elsewhere, still keeps the worker open.
		within := path == snap.Origin.WorkerGit.Cwd && why == "uncommitted work" && workerGitDirtWithinBaseline(proof, snap.Origin.WorkerGit.Proof)
		if (why != "" || proof.Dirty != "") && !within {
			return ""
		}
		proofs[path] = proof
	}
	raw, _ := json.Marshal(proofs)
	return string(raw)
}

// workerGitBeforeRemove is a test-only seam between the checks and the
// deletion; nil in production.
var workerGitBeforeRemove func(path string)

// workerGitRemoveWorktree removes one clean linked worktree under the
// repository's .claude/worktrees folder with git worktree remove's rules,
// never its --force: a locked worktree, submodules, changes or untracked
// files keep it; ignored files (build output, .env, an ignored nested
// repository) go with it; the branch is kept.
// It does not run git worktree remove itself: that runs git status with the
// repository's config and attributes, which a worker (or a sibling, between
// any check and the removal) can point at a filter driver that would run
// outside its sandbox with the daemon's environment. Cleanliness comes from
// the driver-free proof instead, and only this worktree's folder and its own
// admin entry are deleted (no prune of other entries).
func workerGitRemoveWorktree(ctx context.Context, path string) error {
	path = filepath.Clean(path)
	admin, root, err := workerGitLinkedWorktree(ctx, path, "")
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(admin, "locked")); !os.IsNotExist(err) {
		return fmt.Errorf("%s is locked", path)
	}
	if proof, why := workerGitInspect(ctx, path, false); why != "" || proof.Dirty != "" {
		return fmt.Errorf("%s is not clean: %s", path, why)
	}
	// Status skips files marked assume-unchanged or skip-worktree, so edits
	// to them look clean, and a submodule's own work is in its own repository.
	// Keep the worktree unless every entry is a plain tracked file.
	entries, err := workerGitRun(ctx, path, "ls-files", "-s", "-v", "-z")
	if err != nil {
		return fmt.Errorf("%s index cannot be read: %v", path, err)
	}
	for _, entry := range strings.Split(entries, "\x00") {
		meta, _, _ := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); entry != "" && (len(fields) < 2 || fields[0] != "H" || fields[1] == "160000") {
			return fmt.Errorf("%s has a file status does not check, or a submodule: %s", path, entry)
		}
	}
	if workerGitBeforeRemove != nil {
		workerGitBeforeRemove(path)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Right before deleting: no component from the repository root down may
	// be a symlink, so a parent swapped since the checks cannot redirect the
	// deletion, and the deletion itself runs inside the root (openat-style).
	relPath, err := workerNoSymlinkBelow(root, path)
	if err != nil {
		return err
	}
	relAdmin, err := workerNoSymlinkBelow(root, admin)
	if err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open %s: %v", root, err)
	}
	defer r.Close()
	if err := r.RemoveAll(relPath); err != nil {
		return fmt.Errorf("remove %s: %v", path, err)
	}
	if err := r.RemoveAll(relAdmin); err != nil {
		return fmt.Errorf("remove worktree entry %s: %v", admin, err)
	}
	return nil
}

// workerNoSymlinkBelow returns target relative to root when root and every
// component below it down to target is a plain directory, never a symlink.
func workerNoSymlinkBelow(root, target string) (string, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is not below %s", target, root)
	}
	cur := root
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s is a symlink or not a directory", cur)
		}
	}
	return rel, nil
}
