package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ST-251, @secure's condition: a worker archived while a new worktree it was
// excused from (workerGitNotOwn) still has unsaved work leaves its lead a
// worker notice naming each such worktree and the rule that excused it, so
// nothing goes unseen. The notice outlives the worker: it closes once every
// worktree is clean and pushed or gone, or when it is dismissed.

// workerExcusedNote is one archived worker's excused, unsaved worktrees:
// path → its state and the rule that excused it. Saved in worker-excused.json.
// Hosts are the panes that may carry the notice, nearest first: the worker's
// lead, then that lead's own leads, as they were at archive time. Posted is
// set once the notice was saved; until then every recheck retries it.
// StartCwd and ExcludeSHA are the worker's start checkout and exclude hash,
// so a recheck inspects as the archive check did (workerGitArchiveOptions).
type workerExcusedNote struct {
	Hosts      []string          `json:"hosts"`
	Label      string            `json:"label"`
	Paths      map[string]string `json:"paths"`
	Posted     bool              `json:"posted,omitempty"`
	StartCwd   string            `json:"startCwd,omitempty"`
	ExcludeSHA string            `json:"excludeSha,omitempty"`
}

// workerExcusedRecheck is how often a notice's worktrees are inspected again.
const workerExcusedRecheck = 30 * time.Second

func workerExcusedAskKey(worker string) string { return "wt-" + strings.ToLower(worker) }

func workerExcusedKey(key string) bool { return strings.HasPrefix(key, "wt-") }

// workerExcusedText fits the ask limit (askTextMaxRunes): as many worktrees
// as fit, home shortened to ~, then how many more the record holds.
func workerExcusedText(note workerExcusedNote) string {
	paths := make([]string, 0, len(note.Paths))
	for path := range note.Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	head := "Worker " + note.Label + " archived; unsaved worktrees it was excused from: "
	// Leave room for "; and N more" whenever a worktree is left out.
	const moreRoom = len("; and 999 more")
	text := head
	for i, path := range paths {
		part := workerExcusedShown(path) + " (" + note.Paths[path] + ")"
		if i > 0 {
			part = "; " + part
		}
		budget := askTextMaxRunes
		if i < len(paths)-1 {
			budget -= moreRoom
		}
		if i > 0 && len([]rune(text+part)) > budget {
			return text + fmt.Sprintf("; and %d more", len(paths)-i)
		}
		text += part
	}
	return text
}

// workerExcusedShown is path as the notice shows it, the home folder as ~.
func workerExcusedShown(path string) string {
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

func (a *apiServer) loadWorkerExcusedLocked(s *workerArchiveState) {
	if s.excused != nil {
		return
	}
	s.excused = map[string]workerExcusedNote{}
	s.excusedChecked = map[string]time.Time{}
	s.excusedArchiving = map[string]bool{}
	if a.dataDir == "" {
		return
	}
	file := filepath.Join(a.dataDir, "worker-excused.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return
	}
	if err := json.Unmarshal(raw, &s.excused); err != nil || s.excused == nil {
		// Keep a damaged file aside instead of overwriting it at the next
		// save: the open notices it held stay on the asks list either way.
		aside := file + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
		if rerr := os.Rename(file, aside); rerr != nil {
			log.Printf("worker excused worktrees: %s is unreadable (%v) and could not be kept aside: %v", file, err, rerr)
		} else {
			log.Printf("worker excused worktrees: %s is unreadable (%v); kept as %s", file, err, aside)
		}
		s.excused = map[string]workerExcusedNote{}
	}
}

func (a *apiServer) saveWorkerExcusedLocked(s *workerArchiveState) {
	if a.dataDir == "" {
		return
	}
	raw, err := json.Marshal(s.excused)
	if err == nil {
		err = writeFileAtomicSynced(filepath.Join(a.dataDir, "worker-excused.json"), raw, 0600)
	}
	if err != nil {
		log.Printf("worker excused worktrees save: %v", err)
	}
}

// workerExcusedAsk is the open notice with key, on whichever pane holds it
// now (a human-authorized lead move relocates it).
func (a *apiServer) workerExcusedAsk(key string) (Ask, bool) {
	for _, x := range a.asks.list() {
		if x.State == askOpen && x.Kind == askKindWorker && x.Key == key {
			return x, true
		}
	}
	return Ask{}, false
}

// workerExcusedHosts is the worker's notice lead followed by that lead's own
// leads, nearest first.
func (a *apiServer) workerExcusedHosts(lead string) []string {
	var hosts []string
	seen := map[string]bool{}
	for id := lead; id != "" && !seen[id] && len(hosts) < 8; {
		seen[id] = true
		hosts = append(hosts, id)
		p, ok := a.mgr.Get(id)
		if !ok {
			break
		}
		id = a.workerLeadFor(p.Snapshot().Origin)
	}
	return hosts
}

// workerExcusedHost is the nearest host that is present (running, or
// suspended and resumable) and can carry an ask; "" when none is.
func (a *apiServer) workerExcusedHost(hosts []string) string {
	for _, id := range hosts {
		if _, ok := a.mgr.Get(id); ok && a.workerLeadPresent(id) {
			return id
		}
	}
	return ""
}

// workerRecordExcusedWorktrees saves an archiving worker's excused, unsaved
// worktrees before the worker is torn down, not yet posted: the archive
// posts it once complete (workerPostExcused), and any recheck retries it, so
// neither a failed step nor a crash loses it. The archive is refused
// beforehand when no host is present.
func (a *apiServer) workerRecordExcusedWorktrees(hosts []string, worker, label string, start *workerGitStart, paths map[string]string) {
	if len(paths) == 0 {
		return
	}
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerExcusedLocked(s)
	note := workerExcusedNote{Hosts: hosts, Label: label, Paths: map[string]string{}}
	if start != nil {
		note.StartCwd, note.ExcludeSHA = start.Cwd, start.ExcludeSHA
	}
	for path, why := range paths {
		note.Paths[path] = why
	}
	s.excused[worker] = note
	s.excusedChecked[worker] = time.Now()
	s.excusedArchiving[worker] = true // not posted until the archive ends
	a.saveWorkerExcusedLocked(s)
}

// workerForgetExcused drops the record, and closes the notice, of a worker
// that is open again (its archive was undone, or it was reopened): its own
// retention covers the worktrees now.
func (a *apiServer) workerForgetExcused(worker string) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerExcusedLocked(s)
	delete(s.excusedArchiving, worker)
	if a.asks != nil {
		key := workerExcusedAskKey(worker)
		if x, open := a.workerExcusedAsk(key); open {
			if err := a.asks.closeWorkerKey(x.SourcePane, key, "worker_reopened"); err != nil {
				log.Printf("worker excused worktrees notice %s: %v", key, err)
			}
		}
	}
	if _, ok := s.excused[worker]; !ok {
		return
	}
	delete(s.excused, worker)
	delete(s.excusedChecked, worker)
	a.saveWorkerExcusedLocked(s)
}

// workerExcusedArchiveEnded ends a record's wait on its archive, so a
// recheck may post it (a no-op once posted or forgotten).
func (a *apiServer) workerExcusedArchiveEnded(worker string) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerExcusedLocked(s)
	delete(s.excusedArchiving, worker)
}

// workerPostExcused posts a recorded notice once its worker is archived.
func (a *apiServer) workerPostExcused(worker string) {
	if a.asks == nil {
		return
	}
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerExcusedLocked(s)
	delete(s.excusedArchiving, worker)
	if _, changed := a.ensureWorkerExcusedLocked(s, worker); changed {
		a.saveWorkerExcusedLocked(s)
	}
}

// ensureWorkerExcusedLocked keeps a record's notice open on its nearest
// present host: it posts one not yet saved, moves one whose pane is gone,
// and refreshes its text. A posted notice that is no longer open was
// dismissed or closed: the record is forgotten. It reports whether the
// record is kept, and whether the record map changed (to be saved).
func (a *apiServer) ensureWorkerExcusedLocked(s *workerArchiveState, worker string) (kept, changed bool) {
	note, ok := s.excused[worker]
	if !ok {
		return false, false
	}
	if s.excusedArchiving[worker] {
		// Its archive is still under way (in memory only: after a crash the
		// set is empty, and the record is posted).
		return true, false
	}
	key := workerExcusedAskKey(worker)
	x, open := a.workerExcusedAsk(key)
	if note.Posted && !open {
		delete(s.excused, worker)
		delete(s.excusedChecked, worker)
		return false, true
	}
	host := a.workerExcusedHost(note.Hosts)
	update := ""
	if open {
		update = x.ID
		if host == "" || a.workerLeadPresent(x.SourcePane) {
			host = x.SourcePane // stays where it is while that pane is present
		}
	}
	p, live := a.mgr.Get(host)
	if host == "" || !live {
		return true, false
	}
	// An unchanged repeat is a no-op in the ask store (no write).
	if _, _, err := a.asks.post(askPost{Pane: host, Source: a.paneCallerLabel(p), Kind: askKindWorker, Key: key, Text: workerExcusedText(note), WorkerPane: worker, UpdateID: update}); err != nil {
		log.Printf("worker excused worktrees notice for %s on %s: %v", worker, host, err)
		return true, false
	}
	if !note.Posted {
		note.Posted = true
		s.excused[worker] = note
		return true, true
	}
	return true, false
}

// workerExcusedSaved reports whether the worktree at path no longer holds
// unsaved work: clean and pushed by the archive check's own inspection
// (nested linked worktrees and the unchanged exclude file are honoured), or
// its folder removed while the folder above it is still there (an unmounted
// drive or share is not "removed").
func workerExcusedSaved(ctx context.Context, path string, note workerExcusedNote) bool {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		st, err := os.Stat(filepath.Dir(path))
		return err == nil && st.IsDir()
	}
	c, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	opts := workerGitInspectOptions{}
	if note.StartCwd != "" {
		opts = workerGitArchiveOptions(c, path, &workerGitStart{Cwd: note.StartCwd, ExcludeSHA: note.ExcludeSHA})
	}
	_, why := workerGitCleanPushedWith(c, path, opts)
	return why == ""
}

// recheckWorkerExcused keeps each record's notice where it can be seen and
// inspects its worktrees again, outside the notice lock, at most every
// workerExcusedRecheck: a saved worktree drops out, and the notice closes
// when none is left.
func (a *apiServer) recheckWorkerExcused(ctx context.Context, now time.Time) {
	if a.asks == nil {
		return
	}
	s := a.workerArchives()
	s.noticeMu.Lock()
	a.loadWorkerExcusedLocked(s)
	due := map[string]workerExcusedNote{}
	changed := false
	for worker := range s.excused {
		kept, c := a.ensureWorkerExcusedLocked(s, worker)
		changed = changed || c
		if !kept {
			continue
		}
		if now.Sub(s.excusedChecked[worker]) >= workerExcusedRecheck {
			s.excusedChecked[worker] = now
			due[worker] = s.excused[worker]
		}
	}
	// Save only a changed record map: this runs on every archive pass.
	if changed {
		a.saveWorkerExcusedLocked(s)
	}
	s.noticeMu.Unlock()
	for worker, note := range due {
		saved := map[string]bool{}
		for path := range note.Paths {
			if ctx.Err() != nil {
				return
			}
			if workerExcusedSaved(ctx, path, note) {
				saved[path] = true
			}
		}
		if len(saved) == 0 {
			continue
		}
		s.noticeMu.Lock()
		if cur, ok := s.excused[worker]; ok {
			for path := range saved {
				delete(cur.Paths, path)
			}
			s.excused[worker] = cur
			key := workerExcusedAskKey(worker)
			if len(cur.Paths) > 0 {
				a.ensureWorkerExcusedLocked(s, worker)
			} else {
				if x, open := a.workerExcusedAsk(key); open {
					if err := a.asks.closeWorkerKey(x.SourcePane, key, "worktree_saved"); err != nil {
						log.Printf("worker excused worktrees notice %s: %v", key, err)
					}
				}
				delete(s.excused, worker)
				delete(s.excusedChecked, worker)
			}
			a.saveWorkerExcusedLocked(s)
		}
		s.noticeMu.Unlock()
	}
}
