package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"
)

// logCorruptState records that a corrupt state file was quarantined.
func logCorruptState(orig, quarantined string, cause error) {
	slog.Warn("corrupt state file quarantined; starting fresh",
		"path", orig, "quarantined", quarantined, "err", cause)
}

// PersistedPane is the disk shape written to ~/.solu/panes.json.
// Stored separately from workspaces (layout) because panes outlive any
// particular layout arrangement.
type PersistedPane struct {
	Credential                     *paneCredentialInfo `json:"credential,omitempty"`
	OwnerRetirementRestoreDisabled bool                `json:"ownerRetirementRestoreDisabled,omitempty"`
	// Exact page identities with potentially unsent input. Missing knowledge in
	// legacy entries stays unknown; a disconnected page cannot be ACKed by another.
	OwnerRetirementViewersKnown   bool     `json:"ownerRetirementViewersKnown,omitempty"`
	OwnerRetirementPendingDevices []string `json:"ownerRetirementPendingDevices,omitempty"`

	RecoveryError   string                `json:"recoveryError,omitempty"`
	IdentityBlocked bool                  `json:"identityBlocked,omitempty"`
	ID              string                `json:"id"`
	Cwd             string                `json:"cwd"`
	Cmd             []string              `json:"cmd"`
	Title           string                `json:"title"`
	CreatedAt       time.Time             `json:"createdAt"`
	Cols            uint16                `json:"cols"`
	Rows            uint16                `json:"rows"`
	SocketPath      string                `json:"socketPath"`
	AgentPid        int                   `json:"agentPid"`
	ProcessScope    *processScopeIdentity `json:"processScope,omitempty"`
	Kind            string                `json:"kind,omitempty"`
	// Agent is omitted for legacy/default Claude and explicit for Codex/Muse.
	Agent         string `json:"agent,omitempty"`
	CodexThreadID string `json:"codexThreadId,omitempty"`
	MuseSessionID string `json:"museSessionId,omitempty"`
	// HarnessSessionID/HarnessProvider carry a managed terminal harness's
	// durable conversation id (the DeepSeek client's DSH session) across a
	// daemon restart, the same way CodexThreadID/MuseSessionID do. Written by
	// the poll's identity discovery from the client's per-pane sidecar;
	// omitempty keeps every legacy panes.json entry byte-identical.
	HarnessSessionID string `json:"harnessSessionId,omitempty"`
	HarnessProvider  string `json:"harnessProvider,omitempty"`
	// CodexTurnInFlight records that a Codex Focus turn was underway and had
	// not reported an end when this entry was last written. Surviving as true
	// across a restart is the daemon's ONLY evidence that the previous process
	// died mid-turn: Codex resumes a crash-orphaned turn and a user-interrupted
	// one identically (see Pane.codexTurnInFlight). Read at resume to stamp the
	// "a restart interrupted the previous response" notice. omitempty keeps
	// every existing entry byte-identical.
	CodexTurnInFlight bool `json:"codexTurnInFlight,omitempty"`
	// Provider routes the resumed pane's claude to "glm" (Z.ai) vs Anthropic
	// (""). omitempty keeps existing panes.json entries byte-identical and
	// unmarshalling to "" == Anthropic (zero-risk migration). Re-derived into
	// env on every respawn, so it MUST survive here across a daemon restart.
	Provider string `json:"provider,omitempty"`
	// Effort is the per-pane reasoning-effort level (--effort), re-baked into
	// the native argv on every respawn, so like Provider it MUST survive here
	// across a daemon restart. omitempty keeps legacy entries byte-identical.
	Effort string `json:"effort,omitempty"`
	// Model mirrors Pane.Model (--model id) so a restart/resume respawn keeps
	// the user's explicit model choice (resume alone would only keep the
	// session-stored one — same outcome, but this survives session rotation).
	Model string `json:"model,omitempty"`
	// Role mirrors Pane.Role ("" | "orchestrator"). Like Provider/Effort the
	// brief it drives is re-derived into the argv on every respawn, so it MUST
	// survive here across a daemon restart. omitempty keeps legacy panes.json
	// entries byte-identical; an absent field unmarshals to "" (a plain pane).
	Role string `json:"role,omitempty"`
	// ContextPack is path-only pane configuration. Its adapter is re-derived on
	// every respawn, so the path and last status must survive daemon restarts.
	ContextPack       string `json:"contextPack,omitempty"`
	ContextPackStatus string `json:"contextPackStatus,omitempty"`
	AutoMode          string `json:"autoMode,omitempty"`
	NeverAutoAnswer   bool   `json:"neverAutoAnswer,omitempty"`
	// AnnounceDelegationEvents mirrors Pane.AnnounceDelegationEvents: the
	// user's EXPLICIT delegation-announcement override, or absent for "follow
	// the role default". A pointer (not a bool) so an explicit `false` on an
	// orchestrator pane is distinguishable from "never chose" — omitempty on a
	// plain bool would erase exactly that choice. Absent unmarshals to nil, so
	// every legacy panes.json entry stays byte-identical.
	AnnounceDelegationEvents *bool  `json:"announceDelegationEvents,omitempty"`
	Narrating                bool   `json:"narrating,omitempty"`
	Flagged                  bool   `json:"flagged,omitempty"`
	FlagMemo                 string `json:"flagMemo,omitempty"`
	FlagOwner                string `json:"flagOwner,omitempty"`
	ClaudeSessionID          string `json:"claudeSessionId,omitempty"`
	ClaudeTranscriptPath     string `json:"claudeTranscriptPath,omitempty"`
	// TitleOverride persists the user's per-pane title override so it survives a
	// daemon restart + resume, exactly like ClaudeTranscriptPath above.
	TitleOverride string `json:"titleOverride,omitempty"`
	// Origin persists who or what started the pane and why (pane_origin.go).
	Origin   *PaneOrigin   `json:"origin,omitempty"`
	Review   *PaneReview   `json:"review,omitempty"`
	Identity *PaneIdentity `json:"identity,omitempty"`
	// NativeAllowlist persists the native pane's "always allow this tool" set
	// (sorted tool names) so the user's per-pane opt-ins survive a daemon
	// restart + resume, mirroring how ClaudeSessionID is persisted.
	NativeAllowlist []string `json:"nativeAllowlist,omitempty"`
	// LastPrompts persists the pane's recent submitted-input history
	// (newest first, capped) so the pane-header strip survives daemon
	// restarts the same way titles and flags do.
	LastPrompts []string `json:"lastPrompts,omitempty"`
	// PendingSwitch persists an ARMED deferred mode switch ("switch to X when
	// Claude finishes") so a daemon restart — most often the update flow itself,
	// exactly when users arm one — re-arms it instead of silently dropping the
	// user's intent. RequestedAt preserves the original arm time so the watcher's
	// max-wait cap is honored across the restart. omitempty keeps legacy entries
	// byte-identical; only restored for a live reattach (a dead agent's pending
	// switch is moot and SuspendExisting drops it).
	PendingSwitch *PersistedPendingSwitch `json:"pendingSwitch,omitempty"`
}

// PersistedPendingSwitch is the on-disk shape of an armed deferred switch.
type PersistedPendingSwitch struct {
	Target      string `json:"target"` // "" (terminal) | "native"
	RequestedAt int64  `json:"requestedAt"`
}

type paneStore interface {
	List() []PersistedPane
	Put(*Pane) error
	Remove(string) error
}

type PaneStore struct {
	path string
	mu   sync.Mutex
	list []PersistedPane

	// bootProblem is why panes.json could not be used when this daemon
	// started ("" when it loaded normally or this is a fresh install).
	// bootRestored is how many panes came back from the last-good copy.
	bootProblem  string
	bootRestored int
	// savedAt is when this process last replaced panes.json; zero until its
	// first save. lastGoodAt is when the last-good copy was last checked.
	savedAt    time.Time
	lastGoodAt time.Time
	now        func() time.Time
}

const (
	// A panes.json version becomes the last-good copy once it has been the
	// current file for this long, so the copy and the live file are not both
	// written inside one short window.
	paneLastGoodMinAge = 30 * time.Second
	// Under constant saves the copy is still refreshed this often.
	paneLastGoodMaxStale = 10 * time.Minute
)

var errPanesFileEmpty = errors.New("panes.json is empty")

func NewPaneStore(dataDir string) (*PaneStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &PaneStore{path: filepath.Join(dataDir, "panes.json"), now: time.Now}
	if err := s.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *PaneStore) lastGoodPath() string { return s.path + ".last-good" }

// BootProblem reports why panes.json was unusable when this daemon started
// (a zero-length file, invalid JSON, or a missing file although a last-good
// copy exists), and how many panes were restored from the last-good copy. An
// empty reason means the file loaded normally or this is a fresh install.
//
// The list of such a boot is not a complete record of this daemon's
// sessions: it is either empty or an older copy. Startup must therefore not
// treat "no pane owns it" as proof that a socket or agent process is an
// orphan (see cleanOrphansAtBoot).
func (s *PaneStore) BootProblem() (reason string, restored int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootProblem, s.bootRestored
}

// decodePaneList parses panes.json content. A zero-length or whitespace-only
// file is an error, not "no panes": the store always writes at least "null"
// or "[]", so an empty file is a torn or truncated write.
func decodePaneList(b []byte) ([]PersistedPane, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errPanesFileEmpty
	}
	var list []PersistedPane
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *PaneStore) load() error {
	restored := false
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, statErr := os.Stat(s.lastGoodPath()); statErr != nil {
			return err // fresh install: no file and no last-good copy
		}
		// panes.json never disappears in normal operation (every save is an
		// atomic rename), so a missing file next to a last-good copy is a loss.
		restored = s.restoreFromLastGood("missing", err)
	case err != nil:
		return err
	default:
		list, derr := decodePaneList(b)
		if derr == nil {
			s.list = list
			s.keepLastGood()
			break
		}
		reason := "invalid"
		if errors.Is(derr, errPanesFileEmpty) {
			reason = "empty"
		} else {
			// A corrupt/torn state file must not brick the daemon at startup
			// (NewPaneStore's error is fatal in main.go). Quarantine the bad
			// file for forensics.
			quarantineCorruptState(s.path, derr)
		}
		restored = s.restoreFromLastGood(reason, derr)
	}
	// A restored list is written back at once, so the next start reads a
	// normal panes.json again.
	changed := restored
	for i := range s.list {
		entry := &s.list[i]
		// Retire all Warden bookmarks before any saved pane is reattached.
		if entry.FlagOwner == "warden" {
			entry.Flagged, entry.FlagMemo, entry.FlagOwner = false, "", ""
			changed = true
		}
		mode, locked := tightenPaneAnswerSettings(entry.Cwd, entry.AutoMode, entry.NeverAutoAnswer)
		if mode != entry.AutoMode || locked != entry.NeverAutoAnswer {
			entry.AutoMode, entry.NeverAutoAnswer = mode, locked
			changed = true
		}
	}
	if changed {
		err := s.saveList(s.list)
		if err != nil && restored {
			// The restored list is in memory and sessions may be live: a
			// failed write-back must not stop the daemon (NewPaneStore's error
			// is fatal). The next pane change retries the save.
			slog.Warn("could not write the restored pane list back to panes.json", "path", s.path, "err", err)
			return nil
		}
		return err
	}
	return nil
}

// restoreFromLastGood records that panes.json was unusable at startup and
// loads the last-good copy in its place. Without a usable copy the daemon
// starts with no panes, exactly as before; BootProblem still tells startup
// to leave sockets and agent processes alone.
func (s *PaneStore) restoreFromLastGood(reason string, cause error) bool {
	s.bootProblem, s.list = reason, nil
	b, err := os.ReadFile(s.lastGoodPath())
	if err == nil {
		s.list, err = decodePaneList(b)
	}
	if err != nil {
		s.list = nil
		slog.Warn("panes.json unusable at startup and no last-good copy could be read; starting with no panes",
			"path", s.path, "problem", reason, "err", cause, "lastGoodErr", err)
		return false
	}
	s.bootRestored = len(s.list)
	slog.Warn("panes.json unusable at startup; restored panes from the last-good copy",
		"path", s.path, "problem", reason, "err", cause, "restored", s.bootRestored)
	return true
}

// clock is nil-safe so a zero-value store in tests still works.
func (s *PaneStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// keepLastGood copies the current panes.json to panes.json.last-good before
// a save replaces it, and once at startup. Only content that parses is ever
// copied, so a damaged file cannot overwrite a good copy. It is a separate
// file rather than a hard link, so truncating panes.json in place cannot
// empty both. Best-effort: a failure here never fails the save.
func (s *PaneStore) keepLastGood() {
	now := s.clock()
	settled := s.savedAt.IsZero() || now.Sub(s.savedAt) >= paneLastGoodMinAge
	if !settled && now.Sub(s.lastGoodAt) < paneLastGoodMaxStale {
		return
	}
	s.lastGoodAt = now
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	if _, err := decodePaneList(b); err != nil {
		return
	}
	if old, err := os.ReadFile(s.lastGoodPath()); err == nil && bytes.Equal(old, b) {
		return
	}
	if err := writeFileAtomicSynced(s.lastGoodPath(), b, 0o600); err != nil {
		slog.Warn("could not refresh the last-good copy of panes.json", "path", s.lastGoodPath(), "err", err)
	}
}

func (s *PaneStore) saveList(list []PersistedPane) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	s.keepLastGood()
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return err
	}
	s.savedAt = s.clock()
	return nil
}

// writeFileAtomicSynced writes data to a sibling temp file, fsyncs it so
// the bytes actually reach disk, then atomically renames it over path and
// fsyncs the directory so the rename itself is durable. Plain
// WriteFile+Rename (no fsync) can leave a zero-length or partially-written
// file after a crash/power loss even though the rename succeeded — exactly
// the torn-write that bricks the daemon at the next startup.
func writeFileAtomicSynced(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	// Best-effort directory fsync so the rename survives a crash. Failure
	// here is non-fatal — the data file is already written and renamed.
	_ = syncDir(dir)
	return nil
}

// quarantineCorruptState renames a corrupt state file aside (best-effort)
// so it isn't silently overwritten on the next save, leaving a copy to
// inspect after the fact.
func quarantineCorruptState(path string, cause error) {
	bad := path + ".corrupt"
	_ = os.Remove(bad)
	if rerr := os.Rename(path, bad); rerr == nil {
		logCorruptState(path, bad, cause)
	}
}

func (s *PaneStore) save() error {
	return s.saveList(s.list)
}

// List returns all persisted panes, including dead ones retained for recovery.
func (s *PaneStore) List() []PersistedPane {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PersistedPane, len(s.list))
	for i := range s.list {
		out[i] = clonePersistedPane(s.list[i])
	}
	return out
}

func (s *PaneStore) FromPane(p *Pane) PersistedPane {
	entry := persistedFromSnapshot(p.Snapshot())
	p.subsMu.Lock()
	entry.OwnerRetirementViewersKnown = p.ownerViewerKnowledgeKnown
	for key, pending := range p.ownerViewerPendingDevices {
		if pending {
			entry.OwnerRetirementPendingDevices = append(entry.OwnerRetirementPendingDevices, key)
		}
	}
	p.subsMu.Unlock()
	sort.Strings(entry.OwnerRetirementPendingDevices)
	// Composed ALONGSIDE the snapshot, not inside it: pendingSwitchMu is a LEAF
	// lock (never taken while holding p.mu), and Snapshot holds p.mu.
	if pend := p.pendingSwitchSnapshot(); pend != nil {
		entry.PendingSwitch = &PersistedPendingSwitch{Target: pend.Target, RequestedAt: pend.RequestedAt}
	}
	return entry
}

// persistedFromSnapshot maps a coherent pane snapshot to the on-disk shape. Used
// both by PaneStore.Put (via FromPane) and by the live-resume path, which needs a
// PersistedPane to feed ResumeClaudePane but only has a snapshot of the exited
// pane (already dropped from the store).
func persistedFromSnapshot(snap PaneSnapshot) PersistedPane {
	return PersistedPane{
		Credential:               clonePaneCredential(snap.Credential),
		ID:                       snap.ID,
		RecoveryError:            snap.RecoveryError,
		IdentityBlocked:          snap.IdentityBlocked,
		Cwd:                      snap.Cwd,
		Cmd:                      paneRecoveryCommand(snap),
		Title:                    snap.Title,
		CreatedAt:                snap.CreatedAt,
		Cols:                     snap.Cols,
		Rows:                     snap.Rows,
		SocketPath:               snap.SocketPath,
		AgentPid:                 snap.Pid,
		ProcessScope:             cloneValuePtr(snap.ProcessScope),
		Kind:                     snap.Kind,
		Agent:                    snap.Agent,
		CodexThreadID:            snap.CodexThreadID,
		MuseSessionID:            snap.MuseSessionID,
		HarnessSessionID:         snap.HarnessSessionID,
		HarnessProvider:          snap.HarnessProvider,
		CodexTurnInFlight:        snap.CodexTurnInFlight,
		Provider:                 snap.Provider,
		Effort:                   snap.Effort,
		Model:                    snap.Model,
		Role:                     snap.Role,
		ContextPack:              snap.ContextPack,
		ContextPackStatus:        snap.ContextPackStatus,
		AutoMode:                 snap.AutoMode,
		NeverAutoAnswer:          snap.NeverAutoAnswer,
		AnnounceDelegationEvents: snap.AnnounceDelegationEvents,
		Narrating:                snap.Narrating,
		Flagged:                  snap.Flagged,
		FlagMemo:                 snap.FlagMemo,
		FlagOwner:                snap.FlagOwner,
		ClaudeSessionID:          snap.ClaudeSessionID,
		ClaudeTranscriptPath:     snap.ClaudeTranscriptPath,
		TitleOverride:            snap.TitleOverride,
		Origin:                   snap.Origin.clone(),
		Review:                   snap.Review.clone(),
		Identity:                 snap.Identity.clone(),
		NativeAllowlist:          snap.NativeAllowlist,
		LastPrompts:              snap.LastPrompts,
	}
}

// Put inserts/updates a pane.
func (s *PaneStore) Put(p *Pane) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.FromPane(p)
	if retained, err := persistOwnerReopenGeneration(filepath.Dir(s.path), entry); err != nil {
		return err
	} else if retained {
		p.ownerRetirementRestoreDisabled.Store(true)
		return nil
	}
	// Retain sets this before Remove. A stale status/dims snapshot must never
	// recreate an ordinary boot recipe after the durable closing boundary.
	if p.ownerRetirementRestoreDisabled.Load() {
		return writeOwnerViewerHistory(filepath.Dir(s.path), entry)
	}
	next := make([]PersistedPane, len(s.list), len(s.list)+1)
	for i := range s.list {
		next[i] = clonePersistedPane(s.list[i])
	}
	for i, existing := range s.list {
		if existing.ID == entry.ID {
			if reflect.DeepEqual(existing, entry) {
				return nil
			}
			next[i] = entry
			if err := s.saveList(next); err != nil {
				return err
			}
			s.list = next
			return nil
		}
	}
	next = append(next, entry)
	if err := s.saveList(next); err != nil {
		return err
	}
	s.list = next
	return nil
}

func clonePersistedPane(p PersistedPane) PersistedPane {
	p.ProcessScope = cloneValuePtr(p.ProcessScope)
	p.Cmd = cloneStringSlice(p.Cmd)
	p.LastPrompts = cloneStringSlice(p.LastPrompts)
	p.NativeAllowlist = cloneStringSlice(p.NativeAllowlist)
	p.OwnerRetirementPendingDevices = cloneStringSlice(p.OwnerRetirementPendingDevices)
	if p.PendingSwitch != nil {
		cp := *p.PendingSwitch
		p.PendingSwitch = &cp
	}
	return p
}

// Remove drops a pane by ID.
func (s *PaneStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list[:0]
	for _, e := range s.list {
		if e.ID != id {
			out = append(out, e)
		}
	}
	s.list = out
	return s.save()
}

// RemoveMany drops multiple panes with a single persisted rewrite.
func (s *PaneStore) RemoveMany(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	drop := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list[:0]
	changed := false
	for _, e := range s.list {
		if _, ok := drop[e.ID]; ok {
			changed = true
			continue
		}
		out = append(out, e)
	}
	if !changed {
		return nil
	}
	s.list = out
	return s.save()
}
