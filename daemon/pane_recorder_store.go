package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// recordingHub owns <dataDir>/recordings: folder checks, file creation, the
// shared size cap across every pane's recording, the deletion sweep and the
// person's "delete this pane's recordings". It never reads a recording's
// content; the sweep and the cap only stat files.
type recordingHub struct {
	dataDir string
	now     func() time.Time
	// write is the file write the recorders use; tests replace it to block
	// the disk.
	write func(f *os.File, b []byte) (int, error)
	// totalCap and fileCap default to the policy constants; tests lower them.
	totalCap int64
	fileCap  int64

	mu    sync.Mutex
	total int64
	// pending is reserved but not yet on disk (a writer's unflushed buffer);
	// a recount adds it to what is on disk.
	pending    int64
	totalKnown bool
	open       map[string]bool
	lastSweep  time.Time
}

func newRecordingHub(dataDir string) *recordingHub {
	return &recordingHub{
		dataDir:  dataDir,
		now:      time.Now,
		write:    func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		totalCap: recordingTotalCap,
		fileCap:  recordingFileCap,
		open:     map[string]bool{},
	}
}

func (h *recordingHub) root() string { return filepath.Join(h.dataDir, recordingsDirName) }

var (
	recordingPaneIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// recordingFileNamePattern matches only the recorder's own files; the
	// sweep, the cap and the person's delete touch nothing else.
	recordingFileNamePattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.cast$`)
)

// paneDir returns recordings/<pane id>, refusing ids that could leave it.
func (h *recordingHub) paneDir(paneID string) (string, error) {
	if !recordingPaneIDPattern.MatchString(paneID) {
		return "", fmt.Errorf("pane id %q can't name a recordings folder", paneID)
	}
	return filepath.Join(h.root(), paneID), nil
}

// errRecordingFolder marks a refused recordings/ or pane folder (a symlink,
// another owner, or group/world access). It stops recording; it is shown on
// the indicator.
var errRecordingFolder = errors.New(recordingStopFolder)

// checkRecordingDir refuses a folder that is a symlink, not a folder, owned by
// another user, or open to group or others.
func checkRecordingDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a plain folder", errRecordingFolder, path)
	}
	if !recordingOwnedByUs(info) {
		return fmt.Errorf("%w: %s is owned by another user", errRecordingFolder, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is open to other users (%o)", errRecordingFolder, path, info.Mode().Perm())
	}
	return nil
}

// ensureRecordingDir creates path 0700 if missing (whatever the umask), then
// checks it.
func ensureRecordingDir(path string) error {
	if err := os.Mkdir(path, recordingDirMode); err == nil {
		if err := os.Chmod(path, recordingDirMode); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	return checkRecordingDir(path)
}

// prepare creates and checks recordings/ and recordings/<pane id>. Only a
// switch-on reaches it: with recording off nothing is created.
func (h *recordingHub) prepare(paneID string) (string, error) {
	dir, err := h.paneDir(paneID)
	if err != nil {
		return "", err
	}
	if err := ensureRecordingDir(h.root()); err != nil {
		return "", err
	}
	if err := ensureRecordingDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// createFile makes a new 0600 file named by its UTC start time, never
// following or reusing an existing name. A name taken within the same second
// moves on to the next second.
func (h *recordingHub) createFile(dir string, start time.Time) (*os.File, string, error) {
	if err := checkRecordingDir(dir); err != nil {
		return nil, "", err
	}
	for i := 0; i < 60; i++ {
		path := filepath.Join(dir, start.Add(time.Duration(i)*time.Second).UTC().Format(recordingFileTimeLayout)+".cast")
		f, err := openNewRecordingFile(path)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if err := f.Chmod(recordingFileMode); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, "", err
		}
		h.mu.Lock()
		h.open[path] = true
		h.mu.Unlock()
		return f, path, nil
	}
	return nil, "", fmt.Errorf("no free recording name in %s", dir)
}

func (h *recordingHub) closed(path string) {
	h.mu.Lock()
	delete(h.open, path)
	h.mu.Unlock()
}

type recordingFileInfo struct {
	path  string
	size  int64
	mtime time.Time
}

// listFilesLocked stats every regular file one level under each pane folder.
// It never follows a symlink (recordings/ itself, a pane folder or a file).
func (h *recordingHub) listFilesLocked() ([]recordingFileInfo, []string) {
	root := h.root()
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	var files []recordingFileInfo
	var dirs []string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		dirs = append(dirs, dir)
		inner, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range inner {
			if !f.Type().IsRegular() || !recordingFileNamePattern.MatchString(f.Name()) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			files = append(files, recordingFileInfo{path: filepath.Join(dir, f.Name()), size: info.Size(), mtime: info.ModTime()})
		}
	}
	return files, dirs
}

func (h *recordingHub) recountLocked() {
	files, _ := h.listFilesLocked()
	var total int64
	for _, f := range files {
		total += f.size
	}
	h.total = total + h.pending
	h.totalKnown = true
}

// reserve accounts n more bytes. Over the cap it deletes the oldest finished
// files (any pane) first; when that can't make room it refuses, and the
// recorder stops rather than writing past the cap.
func (h *recordingHub) reserve(n int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.totalKnown {
		h.recountLocked()
	}
	if h.total+n <= h.totalCap {
		h.total += n
		h.pending += n
		return true
	}
	files, _ := h.listFilesLocked()
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if h.total+n <= h.totalCap {
			break
		}
		if h.open[f.path] {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			h.total -= f.size
		}
	}
	if h.total < 0 {
		h.total = 0
	}
	if h.total+n <= h.totalCap {
		h.total += n
		h.pending += n
		return true
	}
	return false
}

// written moves n reserved bytes to disk.
func (h *recordingHub) written(n int64) {
	h.mu.Lock()
	h.pending -= n
	if h.pending < 0 {
		h.pending = 0
	}
	h.mu.Unlock()
}

// unreserve returns n reserved bytes that never reached disk.
func (h *recordingHub) unreserve(n int64) {
	h.mu.Lock()
	h.pending -= n
	h.total -= n
	if h.pending < 0 {
		h.pending = 0
	}
	if h.total < 0 {
		h.total = 0
	}
	h.mu.Unlock()
}

// removeAccounted deletes a recording file and its bytes from the account in
// one step, so a recount can't see one without the other.
func (h *recordingHub) removeAccounted(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if os.Remove(path) == nil {
			h.total -= info.Size()
			if h.total < 0 {
				h.total = 0
			}
		}
	}
}

// truncateAccounted cuts a recording file back to offset and the account with
// it in one step, and returns it open (never through a symlink) at offset, as
// the file being written.
func (h *recordingHub) truncateAccounted(path string, offset int64) (*os.File, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, err := openExistingRecordingFile(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = f.Truncate(offset)
	}
	if err == nil {
		_, err = f.Seek(offset, 0)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if cut := info.Size() - offset; cut > 0 {
		h.total -= cut
		if h.total < 0 {
			h.total = 0
		}
	}
	h.open[path] = true
	return f, nil
}

// release returns n bytes to the account after a truncation or deletion.
func (h *recordingHub) release(n int64) {
	h.mu.Lock()
	h.total -= n
	if h.total < 0 {
		h.total = 0
	}
	h.mu.Unlock()
}

// sweepIfDue runs the deletion sweep when recordingSweepEvery has passed since
// the last one. It is called at daemon start and from the host storage
// sampler's existing ticker.
func (h *recordingHub) sweepIfDue() {
	if h == nil {
		return
	}
	h.mu.Lock()
	due := h.lastSweep.IsZero() || h.now().Sub(h.lastSweep) >= recordingSweepEvery
	h.mu.Unlock()
	if due {
		h.sweep()
	}
}

// sweep deletes every finished file whose last write is recordingMaxAge old,
// removes empty pane folders and recounts the total. It reads only an existing
// recordings/ folder and creates nothing.
func (h *recordingHub) sweep() (deleted int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.lastSweep = now
	files, dirs := h.listFilesLocked()
	for _, f := range files {
		if h.open[f.path] || now.Sub(f.mtime) < recordingMaxAge {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			deleted++
		}
	}
	for _, dir := range dirs {
		_ = os.Remove(dir) // only succeeds when empty
	}
	if files != nil || dirs != nil {
		h.recountLocked()
	}
	return deleted
}

// deletePane removes every file in one pane's folder and the folder. The
// caller has stopped that pane's recorder first.
func (h *recordingHub) deletePane(paneID string) (int, error) {
	dir, err := h.paneDir(paneID)
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Anchor to a checked recordings/: a symlinked or foreign recordings/
	// would point the delete at another folder.
	if _, err := os.Lstat(h.root()); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err := checkRecordingDir(h.root()); err != nil {
		return 0, err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// Never follow it; removing the link itself is safe.
		return 0, os.Remove(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if h.open[path] {
			return n, fmt.Errorf("a recording is still being written")
		}
		// Only the recorder's own files.
		if !e.Type().IsRegular() || !recordingFileNamePattern.MatchString(e.Name()) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return n, err
		}
		n++
	}
	_ = os.Remove(dir)
	h.recountLocked()
	return n, nil
}

// recordingsRelPrefixes returns recordings/ relative to repo, as given and
// resolved, when it lies inside repo (a data dir kept in a worktree), so a
// repository walk can skip it with a string check per file.
func recordingsRelPrefixes(repo, dataDir string) []string {
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	root := filepath.Join(dataDir, recordingsDirName)
	var out []string
	for _, r := range []string{filepath.Clean(repo), resolveExistingAncestor(repo)} {
		for _, d := range []string{filepath.Clean(root), resolveExistingAncestor(root)} {
			if r == "" || d == "" {
				continue
			}
			rel, err := filepath.Rel(r, d)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// underRelPrefix reports whether the repository path rel is one of prefixes or
// inside one.
func underRelPrefix(rel string, prefixes []string) bool {
	rel = filepath.ToSlash(rel)
	for _, p := range prefixes {
		if p == "." || rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}
