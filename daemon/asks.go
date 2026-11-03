package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Asks are the "Waiting on you" list: a decision or an action an agent needs
// from the person, posted once as a record (post_user_ask) instead of being
// restated at the end of every reply. The daemon owns who posted it (the
// traced caller pane, never an argument), closes it on conditions it can
// check (the pane closed, the person answered in the pane), and only the
// person dismisses it. Design: launch/roles/engineering/waiting-on-you-design.md.

const (
	askTextMaxRunes = 240
	// askOpenPerPane bounds one pane's open asks; askNewPerHour and
	// askPostsPerHour bound how fast it adds and rewrites them.
	askOpenPerPane  = 5
	askNewPerHour   = 6
	askPostsPerHour = 30
	// askDismissMemory: after the person dismisses an ask, the same pane may
	// not post the same key with the same text again for this long.
	askDismissMemory = 4 * time.Hour
	// Closed asks are kept this long (the dismiss memory, "recently cleared").
	askClosedKeep = 24 * time.Hour
	askStoreMax   = 300
)

const (
	askOpen      = "open"
	askResolved  = "resolved"
	askDismissed = "dismissed"

	askKindAgent  = "agent"
	askKindWorker = "worker"

	askResolveOnReply = "reply"
)

// Ask is one item on the list. Source is the label the pane had when the ask
// was posted, for display once the pane is gone.
type Ask struct {
	ID         string `json:"id"`
	Key        string `json:"key"`
	SourcePane string `json:"sourcePane"`
	Source     string `json:"source,omitempty"`
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	ResolveOn  string `json:"resolveOn,omitempty"`
	WorkerPane string `json:"workerPane,omitempty"`
	// Worker notices are quiet unless the daemon identifies a person decision.
	NeedsUser bool   `json:"needsUser,omitempty"`
	State     string `json:"state"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	ClosedAt  int64  `json:"closedAt,omitempty"`
	ClosedBy  string `json:"closedBy,omitempty"`
}

type askFile struct {
	Version int   `json:"version"`
	Asks    []Ask `json:"asks"`
}

func askNeedsUser(a Ask) bool { return a.Kind != askKindWorker || a.NeedsUser }

var askKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

var (
	errAskNotFound = errors.New("ask not found")
	errAskClosed   = errors.New("ask is already closed")
)

// askText is ask text as it is stored and shown: one line, no control,
// bidirectional or other format characters (they change what the person
// reads or would act on a terminal), except the joiners emoji and some
// scripts need. Variation selectors are marks, not format characters, and
// stay. Capped at askTextMaxRunes.
func askText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r):
			space = true
			continue
		case r == '‌' || r == '‍':
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), unicode.Is(unicode.Cf, r):
			continue
		case r == unicode.ReplacementChar:
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return clipRunesEllipsis(b.String(), askTextMaxRunes)
}

// askTextKey is ask text as the dismiss memory compares it: case, spacing and
// trailing punctuation don't make a dismissed ask new.
func askTextKey(s string) string {
	return strings.TrimRight(strings.Join(strings.Fields(strings.ToLower(s)), " "), ".!?…")
}

func clipRunesEllipsis(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

// askStore is the list, persisted in ~/.solu/asks.json. Every change is saved
// before it is visible; a failed save leaves the list as it was.
type askStore struct {
	path string
	now  func() time.Time
	// changed runs after a change is saved, outside the lock; created after
	// a new ask is saved or a quiet note becomes a person ask (asks_push.go).
	changed   func()
	onCreated func(Ask)

	mu   sync.Mutex
	asks []Ask
	// posts and created are each pane's post and new-ask times in the last
	// hour, for the rate limits. In memory: a restart resets them.
	posts   map[string][]time.Time
	created map[string][]time.Time
}

func newAskStore(dataDir string, changed func()) *askStore {
	s := &askStore{path: filepath.Join(dataDir, "asks.json"), now: time.Now, changed: changed, posts: map[string][]time.Time{}, created: map[string][]time.Time{}}
	s.load()
	return s
}

func (s *askStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("asks: read %s: %v", s.path, err)
		}
		return
	}
	var f askFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != 1 {
		bad := fmt.Sprintf("%s.corrupt-%d", s.path, s.now().Unix())
		_ = os.Rename(s.path, bad)
		log.Printf("asks: %s unreadable (%v); moved to %s, starting empty", s.path, err, bad)
		return
	}
	for _, a := range f.Asks {
		if a.ID == "" || a.SourcePane == "" || !askKeyRE.MatchString(a.Key) {
			continue
		}
		a.Text = askText(a.Text)
		s.asks = append(s.asks, a)
	}
}

// commitLocked saves next and makes it the list. Caller holds s.mu.
func (s *askStore) commitLocked(next []Ask) error {
	next = s.pruneLocked(next)
	data, err := json.MarshalIndent(askFile{Version: 1, Asks: next}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(s.path, data, 0o600); err != nil {
		return fmt.Errorf("save asks: %w", err)
	}
	s.asks = next
	return nil
}

// pruneLocked drops closed asks past askClosedKeep, then the oldest closed
// ones while the list is over askStoreMax.
func (s *askStore) pruneLocked(list []Ask) []Ask {
	cut := s.now().Add(-askClosedKeep).UnixMilli()
	out := list[:0:0]
	for _, a := range list {
		if a.State != askOpen && a.ClosedAt < cut {
			continue
		}
		out = append(out, a)
	}
	if len(out) <= askStoreMax {
		return out
	}
	var closed []int
	for i, a := range out {
		if a.State != askOpen {
			closed = append(closed, i)
		}
	}
	sort.Slice(closed, func(i, j int) bool { return out[closed[i]].ClosedAt < out[closed[j]].ClosedAt })
	drop := map[int]bool{}
	for _, i := range closed {
		if len(out)-len(drop) <= askStoreMax {
			break
		}
		drop[i] = true
	}
	kept := out[:0:0]
	for i, a := range out {
		if !drop[i] {
			kept = append(kept, a)
		}
	}
	return kept
}

func (s *askStore) clone() []Ask { return append([]Ask(nil), s.asks...) }

func (s *askStore) notify() {
	if s.changed != nil {
		s.changed()
	}
}

// recent keeps the times within the last hour.
func withinHour(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0:0]
	for _, t := range ts {
		if now.Sub(t) < time.Hour {
			out = append(out, t)
		}
	}
	return out
}

// askPost is one post_user_ask (or the worker manager's equivalent).
type askPost struct {
	Pane       string // the traced caller pane
	Source     string // its label now
	Kind       string
	Key        string
	Text       string
	ResolveOn  string
	WorkerPane string
	NeedsUser  bool // worker manager only; never accepted from MCP arguments
	// UpdateID: refresh only this open ask. A dismissal or a newer ask under
	// the same key must not be undone by a stale worker-notice snapshot.
	UpdateID string
	// Internal: the daemon posts it for the pane (a migrated flag), so the
	// agent limits don't apply.
	Internal bool
}

// post adds an ask, or updates the pane's open ask with the same key. It
// reports the ask and whether it is new; an unchanged repeat changes nothing.
func (s *askStore) post(in askPost) (Ask, bool, error) {
	key := strings.ToLower(strings.TrimSpace(in.Key))
	if !askKeyRE.MatchString(key) {
		return Ask{}, false, errors.New("key must be 1–40 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit (e.g. \"deploy-site\")")
	}
	text := askText(in.Text)
	if text == "" {
		return Ask{}, false, errors.New("text is empty: say in one line what you need the person to decide or do")
	}
	if in.ResolveOn != "" && in.ResolveOn != askResolveOnReply {
		return Ask{}, false, errors.New(`resolve_on must be "reply" or left out`)
	}
	if in.Kind == "" {
		in.Kind = askKindAgent
	}
	now := s.now()
	s.mu.Lock()
	posts := withinHour(s.posts[in.Pane], now)
	created := withinHour(s.created[in.Pane], now)
	s.posts[in.Pane], s.created[in.Pane] = posts, created
	open, lastDismissed := 0, -1
	existing := -1
	norm := askTextKey(text)
	for i, a := range s.asks {
		// An agent's asks and the worker manager's asks on the same pane are
		// separate: neither can see, update, close or crowd out the other's.
		// Only the daemon's exact worker-notice refresh may change its host
		// pane (e.g. a lead closed). MCP never accepts these internal fields.
		relocatingWorker := in.Kind == askKindWorker && a.Kind == askKindWorker && in.UpdateID == a.ID && in.WorkerPane == a.WorkerPane && a.Key == key
		if (a.SourcePane != in.Pane && !relocatingWorker) || a.Kind != in.Kind {
			continue
		}
		if a.State == askOpen {
			open++
			if a.Key == key {
				existing = i
			}
		} else if a.State == askDismissed && (a.Key == key || askTextKey(a.Text) == norm) && (lastDismissed < 0 || a.ClosedAt > s.asks[lastDismissed].ClosedAt) {
			// The person dismissed this key, or this text under another key.
			lastDismissed = i
		}
	}
	if in.UpdateID != "" && (existing < 0 || s.asks[existing].ID != in.UpdateID) {
		s.mu.Unlock()
		return Ask{}, false, errAskNotFound
	}
	limited := in.Kind == askKindAgent && !in.Internal
	if existing >= 0 {
		cur := s.asks[existing]
		if cur.SourcePane == in.Pane && cur.Text == text && cur.ResolveOn == in.ResolveOn && cur.WorkerPane == in.WorkerPane && cur.NeedsUser == in.NeedsUser {
			s.mu.Unlock()
			return cur, false, nil
		}
		if limited && len(posts) >= askPostsPerHour {
			s.mu.Unlock()
			return Ask{}, false, fmt.Errorf("you have updated asks %d times in the last hour; wait before changing this one again", askPostsPerHour)
		}
		next := s.clone()
		next[existing].Text = text
		next[existing].ResolveOn = in.ResolveOn
		next[existing].WorkerPane = in.WorkerPane
		next[existing].NeedsUser = in.NeedsUser
		next[existing].Source = in.Source
		next[existing].SourcePane = in.Pane
		next[existing].UpdatedAt = now.UnixMilli()
		// Saving prunes old closed asks, so the index no longer holds after it.
		got := next[existing]
		if err := s.commitLocked(next); err != nil {
			s.mu.Unlock()
			return Ask{}, false, err
		}
		if limited {
			s.posts[in.Pane] = append(posts, now)
		}
		onCreated := s.onCreated
		s.mu.Unlock()
		s.notify()
		if !askNeedsUser(cur) && askNeedsUser(got) && onCreated != nil {
			onCreated(got)
		}
		return got, false, nil
	}
	if limited {
		if lastDismissed >= 0 {
			d := s.asks[lastDismissed]
			if askTextKey(d.Text) == norm && now.Sub(time.UnixMilli(d.ClosedAt)) < askDismissMemory {
				s.mu.Unlock()
				return Ask{}, false, fmt.Errorf("the person dismissed this ask at %s; don't post it again unless something has changed", time.UnixMilli(d.ClosedAt).Format("15:04"))
			}
		}
		if open >= askOpenPerPane {
			s.mu.Unlock()
			return Ask{}, false, fmt.Errorf("you already have %d open asks; resolve one (resolve_user_ask) or update one by its key first", askOpenPerPane)
		}
		if len(created) >= askNewPerHour {
			s.mu.Unlock()
			return Ask{}, false, fmt.Errorf("you have posted %d new asks in the last hour; update an open one by its key, or wait", askNewPerHour)
		}
		if len(posts) >= askPostsPerHour {
			s.mu.Unlock()
			return Ask{}, false, fmt.Errorf("you have posted asks %d times in the last hour; wait before posting again", askPostsPerHour)
		}
	}
	if s.openTotalLocked() >= askStoreMax {
		s.mu.Unlock()
		return Ask{}, false, errors.New("the list of asks is full; try again later")
	}
	id, err := randomID()
	if err != nil {
		s.mu.Unlock()
		return Ask{}, false, err
	}
	a := Ask{ID: "ask_" + id, Key: key, SourcePane: in.Pane, Source: in.Source, Kind: in.Kind, Text: text, ResolveOn: in.ResolveOn, WorkerPane: in.WorkerPane, NeedsUser: in.NeedsUser, State: askOpen, CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}
	if err := s.commitLocked(append(s.clone(), a)); err != nil {
		s.mu.Unlock()
		return Ask{}, false, err
	}
	if limited {
		s.posts[in.Pane] = append(posts, now)
		s.created[in.Pane] = append(created, now)
	}
	onCreated := s.onCreated
	s.mu.Unlock()
	s.notify()
	if onCreated != nil {
		onCreated(a)
	}
	return a, true, nil
}

func (s *askStore) openTotalLocked() int {
	n := 0
	for _, a := range s.asks {
		if a.State == askOpen {
			n++
		}
	}
	return n
}

// closeWhere closes every open ask match accepts. It reports how many closed.
func (s *askStore) closeWhere(state, by string, match func(Ask) bool) (int, error) {
	s.mu.Lock()
	n, err := s.closeWhereLocked(state, by, match)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.notify()
	}
	return n, nil
}

// closeWhereLocked is closeWhere for a caller holding s.mu; it does not notify.
func (s *askStore) closeWhereLocked(state, by string, match func(Ask) bool) (int, error) {
	now := s.now().UnixMilli()
	next := s.clone()
	n := 0
	for i, a := range next {
		if a.State == askOpen && match(a) {
			next[i].State, next[i].ClosedBy, next[i].ClosedAt = state, by, now
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.commitLocked(next)
}

// resolve closes the pane's own open ask with that key (the agent is done
// with it, or no longer needs it).
func (s *askStore) resolve(pane, key string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	n, err := s.closeWhere(askResolved, "agent", func(a Ask) bool { return a.SourcePane == pane && a.Kind == askKindAgent && a.Key == key })
	if err == nil && n == 0 {
		return errAskNotFound
	}
	return err
}

// dismiss is the person's: it closes one open ask by id. The check and the
// close happen under one lock, so an ask closed meanwhile is errAskClosed.
func (s *askStore) dismiss(id string) error {
	s.mu.Lock()
	found, open := false, false
	for _, a := range s.asks {
		if a.ID == id {
			found, open = true, a.State == askOpen
		}
	}
	if !found || !open {
		s.mu.Unlock()
		if !found {
			return errAskNotFound
		}
		return errAskClosed
	}
	_, err := s.closeWhereLocked(askDismissed, "user", func(a Ask) bool { return a.ID == id })
	s.mu.Unlock()
	if err == nil {
		s.notify()
	}
	return err
}

// movePane hands a pane's open asks to the pane that replaces it under a new
// id (migrate-clean): the same agent goes on there.
func (s *askStore) movePane(from, to, label string) {
	s.mu.Lock()
	next := s.clone()
	n := 0
	for i, a := range next {
		if a.State != askOpen {
			continue
		}
		if a.SourcePane == from {
			next[i].SourcePane, next[i].Source = to, label
			n++
		}
		if a.WorkerPane == from {
			next[i].WorkerPane = to
			n++
		}
	}
	var err error
	if n > 0 {
		err = s.commitLocked(next)
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("asks: move asks of pane %s to %s: %v", from, to, err)
	}
	if n > 0 && err == nil {
		s.notify()
	}
}

// paneGone closes the asks a closed or archived pane posted, and the worker
// asks about it. Called only where a pane is gone for good (asksPaneClosed),
// never on a respawn in place.
func (s *askStore) paneGone(id string) {
	if _, err := s.closeWhere(askResolved, "pane_closed", func(a Ask) bool { return a.SourcePane == id && a.Kind != askKindWorker }); err != nil {
		log.Printf("asks: close asks of pane %s: %v", id, err)
	}
	if _, err := s.closeWhere(askResolved, "worker_gone", func(a Ask) bool { return a.WorkerPane == id }); err != nil {
		log.Printf("asks: close worker asks about pane %s: %v", id, err)
	}
}

// closeMissing closes open asks whose pane no longer exists (it closed while
// the daemon was down). Run once the panes are restored.
func (s *askStore) closeMissing(exists func(id string) bool) {
	if _, err := s.closeWhere(askResolved, "pane_closed", func(a Ask) bool { return !exists(a.SourcePane) && a.Kind != askKindWorker }); err != nil {
		log.Printf("asks: close asks of missing panes: %v", err)
	}
	// Worker asks are the worker manager's: a worker whose archive cleanup is
	// still pending is gone from the panes but keeps its notice until the
	// manager's next reconcile (worker_archives.go).
}

// workerAsks is the open worker-manager asks on a lead.
func (s *askStore) workerAsks(lead string) []Ask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ask
	for _, a := range s.asks {
		if a.State == askOpen && a.Kind == askKindWorker && a.SourcePane == lead {
			out = append(out, a)
		}
	}
	return out
}

// workerAskLeads is every pane with an open worker-manager ask.
func (s *askStore) workerAskLeads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, a := range s.asks {
		if a.State == askOpen && a.Kind == askKindWorker && !seen[a.SourcePane] {
			seen[a.SourcePane] = true
			out = append(out, a.SourcePane)
		}
	}
	return out
}

// closeWorkerKey closes the worker manager's open ask with that key on the
// lead, recording by.
func (s *askStore) closeWorkerKey(lead, key, by string) error {
	_, err := s.closeWhere(askResolved, by, func(a Ask) bool { return a.SourcePane == lead && a.Kind == askKindWorker && a.Key == key })
	return err
}

func (s *askStore) closeWorkerNotice(worker string) {
	if _, err := s.closeWhere(askResolved, "worker_cleared", func(a Ask) bool { return a.Kind == askKindWorker && a.WorkerPane == worker }); err != nil {
		log.Printf("worker notice cleanup for %s: %v", worker, err)
	}
}

// openAgentAsks is the pane agent's own open asks (not the worker manager's
// on it), oldest first.
func (s *askStore) openAgentAsks(pane string) []Ask {
	var out []Ask
	for _, a := range s.openFor(pane) {
		if a.Kind == askKindAgent {
			out = append(out, a)
		}
	}
	return out
}

// personReplied closes the pane's resolve_on "reply" asks posted before the
// person submitted input at `at`. Only the person's own terminal input calls
// it (handleWS, a human-classified socket); agent deliveries never do.
func (s *askStore) personReplied(pane string, at time.Time) {
	ms := at.UnixMilli()
	s.mu.Lock()
	hit := false
	for _, a := range s.asks {
		if a.State == askOpen && a.SourcePane == pane && a.ResolveOn == askResolveOnReply && a.UpdatedAt <= ms {
			hit = true
			break
		}
	}
	s.mu.Unlock()
	if !hit {
		return
	}
	if _, err := s.closeWhere(askResolved, "replied", func(a Ask) bool {
		return a.SourcePane == pane && a.ResolveOn == askResolveOnReply && a.UpdatedAt <= ms
	}); err != nil {
		log.Printf("asks: close answered asks of pane %s: %v", pane, err)
	}
}

// list is every open ask (newest first), then the closed ones kept for
// "recently cleared" (most recently closed first).
func (s *askStore) list() []Ask {
	s.mu.Lock()
	out := s.pruneLocked(s.clone())
	s.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := out[i].State == askOpen, out[j].State == askOpen
		if oi != oj {
			return oi
		}
		if oi {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ClosedAt > out[j].ClosedAt
	})
	return out
}

// openFor is the pane's open asks, oldest first.
func (s *askStore) openFor(pane string) []Ask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ask
	for _, a := range s.asks {
		if a.State == askOpen && a.SourcePane == pane {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// openByPane is each pane's open asks, oldest first.
func (s *askStore) openByPane() map[string][]Ask {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]Ask{}
	for _, a := range s.asks {
		if a.State == askOpen {
			out[a.SourcePane] = append(out[a.SourcePane], a)
		}
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	}
	return out
}

// openCounts counts only asks that need the person, never quiet worker notes.
func (s *askStore) openCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, a := range s.asks {
		if a.State == askOpen && askNeedsUser(a) {
			out[a.SourcePane]++
		}
	}
	return out
}

// paneShowsMenu reports a pane at a menu (choice-prompt). A variable so tests
// can stand in for the screen classifier.
var paneShowsMenu = func(p *Pane) bool { return p.computeStateCached() == StateChoicePrompt }

// noteHumanSubmit tells the manager the person pressed Enter in p: the frame
// ends in a carriage return (a keypress, or a composer's text plus Enter). A
// bracketed paste ends in its closing marker, so a pasted newline is not a
// submit. Callers pass only a human-classified socket's input.
func noteHumanSubmit(m *Manager, p *Pane, data []byte) {
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) == 0 || data[len(data)-1] != '\r' {
		return
	}
	// Enter on a menu (a permission prompt) answers the menu, not the agent.
	if paneShowsMenu(p) {
		return
	}
	at := time.Now()
	go m.humanSubmitted(p.ID, at)
}
