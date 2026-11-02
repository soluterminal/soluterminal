package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// LayoutNode is a recursive tree: either a split or a pane reference.
// Stored verbatim from the browser — the daemon doesn't interpret it.
type LayoutNode map[string]any

// PaneDef is a minimal serializable pane description, saved inline in a
// workspace so loading the workspace can respawn panes that have since died.
type PaneDef struct {
	NeverAutoAnswer bool     `json:"neverAutoAnswer,omitempty"`
	IdentityBlocked bool     `json:"identityBlocked,omitempty"`
	ID              string   `json:"id"`
	Cwd             string   `json:"cwd"`
	Cmd             []string `json:"cmd"`
	Title           string   `json:"title"`
	Cols            uint16   `json:"cols,omitempty"`
	Rows            uint16   `json:"rows,omitempty"`
	Flagged         bool     `json:"flagged,omitempty"`
	FlagMemo        string   `json:"flagMemo,omitempty"`
	ClaudeSessionID string   `json:"claudeSessionId,omitempty"`
	// Kind selects the pane's rendering model ("native" for a Focus pane,
	// "" for a terminal). Persisted so a workspace restore (and the
	// cached-first switch's optimistic seed) recreates a native pane AS
	// native instead of a terminal showing raw JSONL. Omitted for terminals.
	Kind string `json:"kind,omitempty"`
	// Agent is independent from the renderer Kind. Omitted means legacy Claude.
	Agent         string `json:"agent,omitempty"`
	CodexThreadID string `json:"codexThreadId,omitempty"`
	MuseSessionID string `json:"museSessionId,omitempty"`
	// Provider mirrors Pane.Provider ("glm" vs "" for Anthropic) so a
	// workspace inline-restore recreates a GLM pane against Z.ai instead of
	// silently downgrading it to Anthropic. Omitted for Anthropic panes.
	Provider string `json:"provider,omitempty"`
	// Effort mirrors Pane.Effort (--effort level) so a workspace inline-restore
	// recreates a native pane at the same reasoning effort. Omitted when unset.
	Effort string `json:"effort,omitempty"`
	// Model mirrors Pane.Model (--model id) so a workspace inline-restore keeps
	// the user's explicit model choice. Omitted when unset.
	Model string `json:"model,omitempty"`
	// Role mirrors Pane.Role ("orchestrator") so a workspace inline-restore
	// recreates an orchestrator pane AS orchestrator — with its chief-of-staff
	// brief — instead of silently downgrading it to a plain claude pane. Omitted
	// for plain panes.
	Role string `json:"role,omitempty"`
	// ContextPack mirrors Pane.ContextPack so an inline restore recreates the
	// pane with the same local briefing adapter. Path only; status is re-derived.
	ContextPack string `json:"contextPack,omitempty"`
	// TitleOverride and Origin keep a pane's name and "how it was started"
	// record across a workspace restore (pane_origin.go).
	TitleOverride string      `json:"titleOverride,omitempty"`
	Origin        *PaneOrigin `json:"origin,omitempty"`
	// Identity keeps the handle and working-on line (pane_identity.go).
	Identity *PaneIdentity `json:"identity,omitempty"`
}

// BrowserInfoDef mirrors the client-side BrowserInfo shape so browser
// tiles persist across reloads alongside terminal panes. Everything
// lives client-side at runtime (the daemon doesn't drive browser
// navigation), but the daemon has to round-trip these fields through
// workspaces.json so a tile with its last-known URL comes back after
// a restart / auto-update.
type BrowserInfoDef struct {
	ID        string                   `json:"id"`
	URL       string                   `json:"url"`
	Title     string                   `json:"title,omitempty"`
	CreatedAt string                   `json:"createdAt"`
	Service   *BrowserServiceTargetDef `json:"service,omitempty"`
}

type BrowserServiceTargetDef struct {
	URL           string `json:"url"`
	PreviewPath   string `json:"previewPath,omitempty"`
	Backend       string `json:"backend,omitempty"`
	MachinePrefix string `json:"machinePrefix"`
	PaneID        string `json:"paneId,omitempty"`
	Port          uint16 `json:"port,omitempty"`
	Addr          string `json:"addr,omitempty"`
	Proto         string `json:"proto,omitempty"`
}

// FileTileInfoDef — same story as BrowserInfoDef, for file-viewer
// tiles. Path only; contents are re-fetched on open so we never
// persist potentially-sensitive file bodies.
type FileTileInfoDef struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt"`
}

type Workspace struct {
	// Missing mode is auto for older saved workspaces. Geometry is shared,
	// rather than a browser preference; capacity/aspect drive server spawns.
	LayoutMode    string  `json:"layoutMode,omitempty"`
	LayoutColumns int     `json:"layoutColumns,omitempty"`
	LayoutAspect  float64 `json:"layoutAspect,omitempty"`
	// Revision of committed client layout decisions. Membership additions
	// still merge through KnownPaneIDs, so a server spawn need not conflict.
	LayoutVersion     uint64  `json:"layoutVersion,omitempty"`
	LayoutBaseVersion *uint64 `json:"layoutBaseVersion,omitempty"`

	// Request-only: the panes this view has observed, including ones it has
	// intentionally removed. Unknown server additions must survive its save.
	KnownPaneIDs *[]string `json:"knownPaneIds,omitempty"`
	// Server-owned tombstones for explicitly closed panes. IDs are never reused;
	// retain these across saves/restarts so an old window cannot restore a closed
	// pane's layout leaf or launch recipe. Absence from PaneIds alone is ambiguous:
	// a client may legitimately be saving a newly created local pane.
	RemovedPaneIDs []string `json:"removedPaneIds,omitempty"`

	Name     string            `json:"name"`
	Layout   LayoutNode        `json:"layout"`
	PaneIds  []string          `json:"paneIds"`
	Panes    []PaneDef         `json:"panes,omitempty"`    // inline defs for restore-on-load
	Browsers []BrowserInfoDef  `json:"browsers,omitempty"` // browser-tile snapshot
	Files    []FileTileInfoDef `json:"files,omitempty"`    // file-tile snapshot (path only)
	ActiveAt time.Time         `json:"activeAt"`
	// Protected workspaces can't be deleted. Set on the auto-created
	// "Default" workspace that EnsureDefault adds on first run. Rename
	// preserves the flag so the user can pick a better label without
	// losing the delete-protection.
	Protected bool `json:"protected,omitempty"`
	// ContextPack is the default local pack path for NEW panes in this workspace.
	// Existing panes are not respawned when it changes.
	ContextPack string `json:"contextPack,omitempty"`
	// Incremented by pane moves. Old views cannot overwrite either layout
	// after reassignment, including after a daemon restart.
	PaneMoveVersion uint64 `json:"paneMoveVersion,omitempty"`
}

type WorkspaceStore struct {
	path          string
	mu            sync.Mutex
	data          map[string]*Workspace
	recentAdds    map[string]map[string]time.Time
	bindingMu     sync.RWMutex
	ownerBindings func() map[string]string
}

const recentWorkspaceAddTTL = 30 * time.Second

func NewWorkspaceStore(dir string) (*WorkspaceStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &WorkspaceStore{
		path:       filepath.Join(dir, "workspaces.json"),
		data:       map[string]*Workspace{},
		recentAdds: map[string]map[string]time.Time{},
	}
	if err := s.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *WorkspaceStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var list []*Workspace
	if err := json.Unmarshal(b, &list); err != nil {
		// Don't brick startup on a torn/corrupt file — quarantine and
		// start fresh (EnsureDefault re-creates a usable workspace).
		quarantineCorruptState(s.path, err)
		return nil
	}
	for _, w := range list {
		s.data[w.Name] = w
	}
	return nil
}

var errWorkspacePersistence = errors.New("workspace storage unavailable")
var errWorkspaceInvalidLayout = errors.New("invalid workspace layout")
var errWorkspaceLayoutConflict = errors.New("workspace layout changed in another view; reload the workspace and retry")
var errWorkspaceMoveConflict = errors.New("workspace changed after a pane move; reload the workspace and retry")

// commit publishes a candidate only after persistence succeeds. Callers hold
// mu and clone any workspace they change so existing readers keep the old state.
func (s *WorkspaceStore) commit(next map[string]*Workspace) error {
	list := make([]*Workspace, 0, len(next))
	for _, w := range next {
		list = append(list, w)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return fmt.Errorf("%w: %w", errWorkspacePersistence, err)
	}
	s.data = next
	return nil
}

func (s *WorkspaceStore) candidate() map[string]*Workspace {
	next := make(map[string]*Workspace, len(s.data))
	for name, w := range s.data {
		next[name] = w
	}
	return next
}

func (s *WorkspaceStore) List() []*Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Workspace, 0, len(s.data))
	for _, w := range s.data {
		out = append(out, cloneWorkspace(w))
	}
	return out
}

func (s *WorkspaceStore) Get(name string) (*Workspace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.data[name]
	if !ok {
		return nil, false
	}
	return cloneWorkspace(w), true
}

// Ensure creates a named workspace without replacing another owner's layout.
func (s *WorkspaceStore) Ensure(name string) error {
	if name == "" || name == "_current" {
		return fmt.Errorf("named workspace required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; ok {
		return nil
	}
	next := s.candidate()
	next[name] = &Workspace{Name: name, ActiveAt: time.Now().UTC()}
	return s.commit(next)
}

func (s *WorkspaceStore) AddPane(name string, pane PaneDef, direction, targetPaneID string) (*Workspace, error) {
	if name == "" || name == "_current" {
		return nil, fmt.Errorf("named workspace required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.data[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q not found", name)
	}
	w = cloneWorkspace(w)
	migrated := false
	if pane.Role == paneRoleTaskOwner {
		w.Layout, migrated = migrateOwnerTerminalLayout(w.Layout, pane.ID)
	}
	present := false
	for _, id := range w.PaneIds {
		if id == pane.ID {
			present = true
			break
		}
	}
	if present && !migrated {
		return cloneWorkspace(w), nil
	}
	if !present {
		w.PaneIds = append(w.PaneIds, pane.ID)
	}
	w.Panes = appendPaneDef(w.Panes, pane)
	if !migrated {
		addWorkspaceLayoutPane(w, pane.ID, direction, targetPaneID)
	}
	w.ActiveAt = time.Now().UTC()
	next := s.candidate()
	next[name] = w
	if err := s.commit(next); err != nil {
		return nil, err
	}
	s.markRecentAddLocked(name, pane.ID, w.ActiveAt)
	return cloneWorkspace(w), nil
}

// MovePane atomically reassigns an existing pane from one named workspace to
// another. The pane itself is owned by the pane store and keeps running; only
// the durable workspace references and layouts change.
func (s *WorkspaceStore) MovePane(source, destination, paneID, direction, targetPaneID string, expectedVersion uint64) (*Workspace, *Workspace, error) {
	if source == "" || destination == "" || source == "_current" || destination == "_current" {
		return nil, nil, fmt.Errorf("named source and destination workspaces required")
	}
	if source == destination {
		return nil, nil, fmt.Errorf("source and destination workspaces must differ")
	}
	if paneID == "" {
		return nil, nil, fmt.Errorf("pane id required")
	}
	bindings := s.managedOwnerBindings()
	if _, managed := bindings[paneID]; managed {
		return nil, nil, fmt.Errorf("managed task-owner panes cannot be moved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.data[source]
	if !ok {
		return nil, nil, fmt.Errorf("workspace %q not found", source)
	}
	if src.PaneMoveVersion != expectedVersion {
		return nil, nil, errWorkspaceMoveConflict
	}
	dst, ok := s.data[destination]
	if !ok {
		return nil, nil, fmt.Errorf("workspace %q not found", destination)
	}
	for name, workspace := range s.data {
		if name == source || name == "_current" {
			continue
		}
		for _, id := range workspace.PaneIds {
			if id == paneID {
				return nil, nil, fmt.Errorf("pane %q is also claimed by workspace %q", paneID, name)
			}
		}
	}

	var def PaneDef
	found := false
	for _, id := range src.PaneIds {
		if id == paneID {
			found = true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("pane %q is not in workspace %q", paneID, source)
	}
	for _, id := range dst.PaneIds {
		if id == paneID {
			return nil, nil, fmt.Errorf("pane %q is already in workspace %q", paneID, destination)
		}
	}
	for _, candidate := range src.Panes {
		if candidate.ID == paneID {
			def = candidate
			break
		}
	}
	if def.ID == "" {
		return nil, nil, fmt.Errorf("pane %q has no saved definition in workspace %q", paneID, source)
	}
	if def.Role == paneRoleTaskOwner {
		return nil, nil, fmt.Errorf("task-owner panes cannot be moved")
	}

	srcNext := cloneWorkspace(src)
	dstNext := cloneWorkspace(dst)
	srcNext.PaneIds = removePaneID(srcNext.PaneIds, paneID)
	srcNext.Panes = removePaneDef(srcNext.Panes, paneID)
	var removed bool
	removed = removeWorkspaceLayoutPane(srcNext, paneID)
	if !removed {
		return nil, nil, fmt.Errorf("pane %q is not in the source layout", paneID)
	}
	dstNext.PaneIds = append(dstNext.PaneIds, paneID)
	dstNext.Panes = appendPaneDef(dstNext.Panes, def)
	addWorkspaceLayoutPane(dstNext, paneID, direction, targetPaneID)
	now := time.Now().UTC()
	srcNext.ActiveAt = now
	dstNext.ActiveAt = now
	srcNext.PaneMoveVersion++
	dstNext.PaneMoveVersion++
	srcNext.LayoutVersion++
	dstNext.LayoutVersion++
	next := s.candidate()
	next[source] = srcNext
	next[destination] = dstNext
	if err := s.commit(next); err != nil {
		return nil, nil, err
	}
	s.clearRecentAddLocked(source, paneID)
	s.markRecentAddLocked(destination, paneID, now)
	return cloneWorkspace(srcNext), cloneWorkspace(dstNext), nil
}

func (s *WorkspaceStore) RemovePane(paneID string) error {
	if paneID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	next := s.candidate()
	for name, original := range s.data {
		w := cloneWorkspace(original)
		nextPaneIDs := removePaneID(w.PaneIds, paneID)
		nextPanes := removePaneDef(w.Panes, paneID)
		layoutChanged := removeWorkspaceLayoutPane(w, paneID)
		nextLayout := w.Layout
		if len(nextPaneIDs) == len(w.PaneIds) && len(nextPanes) == len(w.Panes) && !layoutChanged {
			continue
		}
		w.PaneIds = nextPaneIDs
		w.Panes = nextPanes
		w.Layout = nextLayout
		w.RemovedPaneIDs = append(w.RemovedPaneIDs, paneID)
		if name != "_current" {
			w.LayoutVersion++
		}
		w.ActiveAt = time.Now().UTC()
		next[name] = w
		changed = true
	}
	if !changed {
		return nil
	}
	if err := s.commit(next); err != nil {
		return err
	}
	for name := range next {
		s.clearRecentAddLocked(name, paneID)
	}
	return nil
}

// NameForPane returns the name of the first NAMED workspace that
// contains the given paneID, or "" if none does. `_current` is
// intentionally skipped — it's the auto-save scratchpad, not a
// user-facing workspace label. Used by the pusher to enrich phone
// notifications so users can tell which workspace's pane triggered
// the alert when multiple panes share the same default title.
func (s *WorkspaceStore) NameForPane(paneID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.data {
		if w.Name == "_current" {
			continue
		}
		for _, id := range w.PaneIds {
			if id == paneID {
				return w.Name
			}
		}
	}
	return ""
}

// OrdinalForPane returns the pane's 1-based position within its workspace — the
// "#x" number the desktop's Alt+N and the mobile pane list show — or 0 if the
// pane isn't in any workspace. Mirrors the client's buildPaneOrdinals (index
// within WorkspaceSummary.paneIds), so a push notification's "#x" agrees with
// what the pane list shows. First workspace that contains it wins (a pane is
// normally in exactly one); _current is NOT skipped here, matching the client
// which numbers over every workspace summary it receives.
func (s *WorkspaceStore) OrdinalForPane(paneID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.data {
		for i, id := range w.PaneIds {
			if id == paneID {
				return i + 1
			}
		}
	}
	return 0
}

func (s *WorkspaceStore) Put(w *Workspace) error {
	if w == nil || w.Name == "" {
		return fmt.Errorf("workspace name required")
	}
	// Supplier can acquire workflow locks. It MUST run before WorkspaceStore.mu.
	bindings := s.managedOwnerBindings()
	next := sanitizeManagedOwnerWorkspace(cloneWorkspace(w), bindings)
	baseVersion := next.LayoutBaseVersion
	next.LayoutBaseVersion = nil
	known := next.KnownPaneIDs
	next.KnownPaneIDs = nil   // observation belongs to a view, never the saved workspace
	next.RemovedPaneIDs = nil // only RemovePane/ReplacePane may author closed-pane tombstones
	if next.LayoutMode != "" && next.LayoutMode != "auto" && next.LayoutMode != "free" {
		return fmt.Errorf("%w: mode %q", errWorkspaceInvalidLayout, next.LayoutMode)
	}
	if baseVersion != nil {
		if next.LayoutMode == "" {
			next.LayoutMode = "auto"
		}
		next.LayoutColumns, next.LayoutAspect = workspaceLayoutConfig(next)
	}
	next.ActiveAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Use an isolated bookkeeping candidate; even expiry pruning is transactional.
	adds := cloneWorkspaceRecentAdds(s.recentAdds)
	merger := &WorkspaceStore{recentAdds: adds}
	if original, ok := s.data[next.Name]; ok {
		if next.PaneMoveVersion != original.PaneMoveVersion {
			return errWorkspaceMoveConflict
		}
		prev := sanitizeManagedOwnerWorkspace(cloneWorkspace(original), bindings)
		if next.Name != "_current" {
			if (baseVersion != nil && *baseVersion != prev.LayoutVersion && !sameWorkspaceLayoutDecision(next, prev)) || (baseVersion == nil && prev.LayoutMode != "") {
				return errWorkspaceLayoutConflict
			}
		}
		// The revision and shared mode are daemon-owned for legacy writes.
		next.LayoutVersion = prev.LayoutVersion
		if baseVersion == nil {
			next.LayoutMode, next.LayoutColumns, next.LayoutAspect = prev.LayoutMode, prev.LayoutColumns, prev.LayoutAspect
		}
		next.RemovedPaneIDs = cloneStringSlice(prev.RemovedPaneIDs)
		for _, id := range next.RemovedPaneIDs {
			removeWorkspaceLayoutPane(next, id)
			next.PaneIds = removePaneID(next.PaneIds, id)
			next.Panes = removePaneDef(next.Panes, id)
		}
		next.ContextPack = prev.ContextPack
		next.Protected = prev.Protected
		mergeUnseenWorkspacePanes(next, prev, known)
		if known == nil {
			merger.mergeRecentAddsLocked(next, prev, next.ActiveAt)
		}
		next = sanitizeManagedOwnerWorkspace(next, bindings)
		if baseVersion != nil && next.Name != "_current" && (!reflect.DeepEqual(next.Layout, prev.Layout) || next.LayoutMode != prev.LayoutMode || next.LayoutColumns != prev.LayoutColumns || next.LayoutAspect != prev.LayoutAspect) {
			next.LayoutVersion++
		}
		if next.Name != "_current" && len(prev.PaneIds) > 0 && (next.Layout == nil || len(next.PaneIds) == 0) {
			return fmt.Errorf("refusing to clear non-empty workspace %q without delete/clear intent", next.Name)
		}
	} else {
		next.LayoutVersion = 0
	}
	pruneManagedOwnerAdds(adds, bindings)
	candidate := s.candidate()
	candidate[next.Name] = next
	if err := s.commit(candidate); err != nil {
		return err
	}
	s.recentAdds = adds
	// API callers receive the ACTUAL committed representation. Failure does not
	// modify input, data, timestamps, context or recent-add bookkeeping.
	*w = *cloneWorkspace(next)
	return nil
}

func cloneWorkspace(w *Workspace) *Workspace {
	if w == nil {
		return nil
	}
	next := *w
	next.Layout = cloneLayoutDeep(w.Layout)
	next.PaneIds = cloneStringSlice(w.PaneIds)
	next.RemovedPaneIDs = cloneStringSlice(w.RemovedPaneIDs)
	next.Panes = clonePaneDefs(w.Panes)
	next.Browsers = cloneBrowserInfoDefs(w.Browsers)
	next.Files = cloneFileTileInfoDefs(w.Files)
	return &next
}

func clonePaneDefs(defs []PaneDef) []PaneDef {
	if defs == nil {
		return nil
	}
	next := make([]PaneDef, len(defs))
	copy(next, defs)
	for i := range next {
		next[i].Cmd = cloneStringSlice(defs[i].Cmd)
	}
	return next
}

func cloneBrowserInfoDefs(defs []BrowserInfoDef) []BrowserInfoDef {
	if defs == nil {
		return nil
	}
	next := make([]BrowserInfoDef, len(defs))
	copy(next, defs)
	for i := range next {
		if next[i].Service != nil {
			cp := *next[i].Service
			next[i].Service = &cp
		}
	}
	return next
}

func cloneFileTileInfoDefs(defs []FileTileInfoDef) []FileTileInfoDef {
	if defs == nil {
		return nil
	}
	next := make([]FileTileInfoDef, len(defs))
	copy(next, defs)
	return next
}

func cloneStringSlice(items []string) []string {
	if items == nil {
		return nil
	}
	next := make([]string, len(items))
	copy(next, items)
	return next
}

func cloneLayoutDeep(node LayoutNode) LayoutNode {
	if node == nil {
		return nil
	}
	next := make(LayoutNode, len(node))
	for k, v := range node {
		next[k] = cloneLayoutValue(v)
	}
	return next
}

func cloneLayoutValue(v any) any {
	switch value := v.(type) {
	case LayoutNode:
		return cloneLayoutDeep(value)
	case map[string]any:
		return cloneLayoutDeep(LayoutNode(value))
	case []any:
		next := make([]any, len(value))
		for i := range value {
			next[i] = cloneLayoutValue(value[i])
		}
		return next
	case []string:
		return cloneStringSlice(value)
	default:
		return value
	}
}

func appendPaneDef(defs []PaneDef, pane PaneDef) []PaneDef {
	pane = clonePaneDef(pane)
	for i := range defs {
		if defs[i].ID == pane.ID {
			defs[i] = pane
			return defs
		}
	}
	return append(defs, pane)
}

func clonePaneDef(def PaneDef) PaneDef {
	def.Cmd = cloneStringSlice(def.Cmd)
	return def
}

func (s *WorkspaceStore) markRecentAddLocked(workspaceName, paneID string, at time.Time) {
	if workspaceName == "" || paneID == "" {
		return
	}
	if s.recentAdds == nil {
		s.recentAdds = map[string]map[string]time.Time{}
	}
	if s.recentAdds[workspaceName] == nil {
		s.recentAdds[workspaceName] = map[string]time.Time{}
	}
	s.recentAdds[workspaceName][paneID] = at
}

func (s *WorkspaceStore) clearRecentAddLocked(workspaceName, paneID string) {
	if s.recentAdds == nil || workspaceName == "" || paneID == "" {
		return
	}
	delete(s.recentAdds[workspaceName], paneID)
	if len(s.recentAdds[workspaceName]) == 0 {
		delete(s.recentAdds, workspaceName)
	}
}

func (s *WorkspaceStore) mergeRecentAddsLocked(next *Workspace, prev *Workspace, now time.Time) {
	if next == nil || prev == nil || s.recentAdds == nil {
		return
	}
	adds := s.recentAdds[next.Name]
	if len(adds) == 0 {
		return
	}
	nextIDs := map[string]bool{}
	for _, id := range next.PaneIds {
		nextIDs[id] = true
	}
	prevDefs := map[string]PaneDef{}
	for _, def := range prev.Panes {
		prevDefs[def.ID] = def
	}
	for paneID, addedAt := range adds {
		if now.Sub(addedAt) > recentWorkspaceAddTTL {
			delete(adds, paneID)
			continue
		}
		if nextIDs[paneID] {
			continue
		}
		def, ok := prevDefs[paneID]
		if !ok {
			continue
		}
		next.PaneIds = append(next.PaneIds, paneID)
		next.Panes = appendPaneDef(next.Panes, def)
		addWorkspaceLayoutPane(next, paneID, "horizontal", "")
		nextIDs[paneID] = true
		log.Printf("workspace.Put: preserved recently-created pane %s in %q from stale save", paneID, next.Name)
	}
	if len(adds) == 0 {
		delete(s.recentAdds, next.Name)
	}
}

// Merge against the committed membership under the store lock. Unlike the
// legacy recent-add grace period this works after any delay or daemon restart.
func mergeUnseenWorkspacePanes(next, prev *Workspace, known *[]string) {
	// _current is the replaceable client scratchpad, including an explicit
	// Clear layout. Agent spawns only attach to visible named workspaces.
	if next.Name == "_current" {
		return
	}
	seen := map[string]bool{}
	if known != nil {
		for _, id := range *known {
			seen[id] = true
		}
	}
	defs := map[string]PaneDef{}
	for _, def := range prev.Panes {
		defs[def.ID] = def
	}
	present := map[string]bool{}
	var order []string
	layoutPaneOrder(next.Layout, &order)
	for _, id := range order {
		present[id] = true
	}
	for _, id := range prev.PaneIds {
		def, hasDef := defs[id]
		// Older clients have no observation set. Conservatively retain agent
		// additions; the updated client can explicitly remove observed panes.
		if seen[id] || (known == nil && (!hasDef || def.Origin == nil || def.Origin.Source != "agent")) {
			continue
		}
		if !present[id] {
			target := ""
			if def.Origin != nil {
				target = def.Origin.ByPaneID
			}
			addWorkspaceLayoutPane(next, id, "horizontal", target)
			present[id] = true
		}
		found := false
		for _, candidate := range next.PaneIds {
			if candidate == id {
				found = true
				break
			}
		}
		if !found {
			next.PaneIds = append(next.PaneIds, id)
		}
		if hasDef {
			next.Panes = appendPaneDef(next.Panes, def)
		}
	}
}

func removePaneID(items []string, value string) []string {
	next := items[:0]
	for _, item := range items {
		if item != value {
			next = append(next, item)
		}
	}
	return next
}

func removePaneDef(defs []PaneDef, paneID string) []PaneDef {
	next := defs[:0]
	for _, def := range defs {
		if def.ID != paneID {
			next = append(next, def)
		}
	}
	return next
}

func insertPaneIntoLayout(existing LayoutNode, paneID, direction, targetPaneID string) LayoutNode {
	if direction != "horizontal" && direction != "vertical" {
		direction = "horizontal"
	}
	leaf := LayoutNode{"type": "pane", "paneId": paneID}
	if existing == nil {
		return leaf
	}
	if targetPaneID != "" {
		if next, ok := insertPaneAtTarget(existing, leaf, direction, targetPaneID); ok {
			return next
		}
	}
	return LayoutNode{
		"type":      "split",
		"direction": direction,
		"ratio":     float64(layoutLeafCount(existing)) / float64(layoutLeafCount(existing)+1),
		"a":         existing,
		"b":         leaf,
	}
}

func layoutLeafCount(node LayoutNode) int {
	if node == nil {
		return 0
	}
	if node["type"] != "split" {
		return 1
	}
	a, _ := layoutChild(node["a"])
	b, _ := layoutChild(node["b"])
	return layoutLeafCount(a) + layoutLeafCount(b)
}

func insertPaneAtTarget(node LayoutNode, leaf LayoutNode, direction, targetPaneID string) (LayoutNode, bool) {
	if node == nil {
		return nil, false
	}
	if typ, _ := node["type"].(string); typ == "pane" {
		if id, _ := node["paneId"].(string); id == targetPaneID {
			return LayoutNode{
				"type":      "split",
				"direction": direction,
				"ratio":     0.5,
				"a":         node,
				"b":         leaf,
			}, true
		}
		return node, false
	}
	if typ, _ := node["type"].(string); typ != "split" {
		return node, false
	}
	a, aOK := layoutChild(node["a"])
	b, bOK := layoutChild(node["b"])
	if aOK {
		if nextA, ok := insertPaneAtTarget(a, leaf, direction, targetPaneID); ok {
			next := cloneLayoutNode(node)
			next["a"] = nextA
			return next, true
		}
	}
	if bOK {
		if nextB, ok := insertPaneAtTarget(b, leaf, direction, targetPaneID); ok {
			next := cloneLayoutNode(node)
			next["b"] = nextB
			return next, true
		}
	}
	return node, false
}

func removePaneFromLayout(node LayoutNode, paneID string) (LayoutNode, bool) {
	if node == nil {
		return nil, false
	}
	if typ, _ := node["type"].(string); typ == "pane" {
		id, _ := node["paneId"].(string)
		if id == paneID {
			return nil, true
		}
		return node, false
	}
	if typ, _ := node["type"].(string); typ != "split" {
		return node, false
	}
	a, aOK := layoutChild(node["a"])
	b, bOK := layoutChild(node["b"])
	nextA, aChanged := removePaneFromLayout(a, paneID)
	nextB, bChanged := removePaneFromLayout(b, paneID)
	if !aOK {
		nextA = nil
	}
	if !bOK {
		nextB = nil
	}
	if !aChanged && !bChanged {
		return node, false
	}
	if nextA == nil && nextB == nil {
		return nil, true
	}
	if nextA == nil {
		return nextB, true
	}
	if nextB == nil {
		return nextA, true
	}
	next := cloneLayoutNode(node)
	next["a"] = nextA
	next["b"] = nextB
	return next, true
}

func layoutChild(v any) (LayoutNode, bool) {
	switch n := v.(type) {
	case LayoutNode:
		return n, true
	case map[string]any:
		return LayoutNode(n), true
	default:
		return nil, false
	}
}

func cloneLayoutNode(node LayoutNode) LayoutNode {
	next := make(LayoutNode, len(node))
	for k, v := range node {
		next[k] = v
	}
	return next
}

func (s *WorkspaceStore) Delete(name string) error { return s.deleteWorkspace(name, false) }

// DeleteEmpty checks membership under the same lock as the durable deletion,
// including panes added while an agent was waiting for the person's Allow.
func (s *WorkspaceStore) DeleteEmpty(name string) error { return s.deleteWorkspace(name, true) }

func (s *WorkspaceStore) deleteWorkspace(name string, emptyOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if emptyOnly {
		if err := s.validateEmptyWorkspaceLocked(name); err != nil {
			return err
		}
	}
	if w, ok := s.data[name]; ok && w.Protected {
		return fmt.Errorf("workspace %q is protected and cannot be deleted", name)
	}
	next := s.candidate()
	delete(next, name)
	return s.commit(next)
}

// validateEmptyWorkspaceLocked is shared by preflight and commit; callers hold mu.
func (s *WorkspaceStore) validateEmptyWorkspaceLocked(name string) error {
	if name == "" || strings.EqualFold(name, "_current") {
		return fmt.Errorf("choose a visible named workspace; _current is reserved")
	}
	w, ok := s.data[name]
	if !ok {
		return fmt.Errorf("workspace %q not found", name)
	}
	if w.Protected {
		return fmt.Errorf("workspace %q is protected and cannot be deleted", name)
	}
	var layoutPanes []string
	layoutPaneOrder(w.Layout, &layoutPanes)
	if len(w.PaneIds) != 0 || len(w.Panes) != 0 || len(layoutPanes) != 0 {
		return fmt.Errorf("workspace %q is not empty; move or close its panes first", name)
	}
	return nil
}

func (s *WorkspaceStore) CheckDeleteEmpty(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateEmptyWorkspaceLocked(name)
}

// EnsureDefault makes sure there's always at least one named workspace
// visible to the user. On first run (no named workspaces yet, ignoring
// the auto-saved `_current` fallback) it creates an empty "Default"
// workspace with Protected=true. Delete is blocked; rename preserves
// the flag so users can relabel to their preference.
//
// Called once from daemon main() after the store is loaded.
func (s *WorkspaceStore) EnsureDefault() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Skip if any user-facing workspace exists. `_current` is an
	// auto-save buffer, not a user-visible entry — ignore it.
	for name := range s.data {
		if name != "_current" {
			return nil
		}
	}

	name := "Default"
	// Collision defence: if a prior failed attempt left something
	// named Default around without Protected=true, leave it alone.
	if _, exists := s.data[name]; exists {
		return nil
	}
	next := s.candidate()
	next[name] = &Workspace{
		Name:      name,
		Layout:    nil,
		PaneIds:   []string{},
		ActiveAt:  time.Now().UTC(),
		Protected: true,
	}
	return s.commit(next)
}

// Rename moves a workspace from oldName to newName. Fails if newName is
// already taken or oldName doesn't exist.
func (s *WorkspaceStore) Rename(oldName, newName string) error {
	if oldName == "" || newName == "" {
		return fmt.Errorf("names cannot be empty")
	}
	if strings.EqualFold(oldName, "_current") || strings.EqualFold(newName, "_current") {
		return fmt.Errorf("_current is reserved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.data[oldName]
	if !ok {
		return fmt.Errorf("workspace %q not found", oldName)
	}
	for name := range s.data {
		if name != oldName && strings.EqualFold(name, newName) {
			return fmt.Errorf("workspace %q already exists", newName)
		}
	}
	if oldName == newName {
		return nil
	}
	next := s.candidate()
	delete(next, oldName)
	w = cloneWorkspace(w)
	w.Name = newName
	w.ActiveAt = time.Now().UTC()
	next[newName] = w
	if err := s.commit(next); err != nil {
		return err
	}
	if recent, ok := s.recentAdds[oldName]; ok {
		s.recentAdds[newName] = recent
		delete(s.recentAdds, oldName)
	}
	return nil
}

// SetContextPack is the only writer for a workspace's context-pack default.
// dir is already validated and normalized by the API; "" clears the default.
func (s *WorkspaceStore) SetContextPack(name, dir string) (*Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.data[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q not found", name)
	}
	w = cloneWorkspace(w)
	w.ContextPack = dir
	w.ActiveAt = time.Now().UTC()
	next := s.candidate()
	next[name] = w
	if err := s.commit(next); err != nil {
		return nil, err
	}
	return cloneWorkspace(w), nil
}

// Migrate only the exact historical owner dashboard leaf; generic task tiles
// and intentionally hidden terminals are left alone.
func migrateOwnerTerminalLayout(node LayoutNode, paneID string) (LayoutNode, bool) {
	if node == nil {
		return node, false
	}
	if node["type"] == "tasks" && node["tasksId"] == paneID {
		if task, ok := node["ownerTaskId"].(string); ok && task != "" && paneID == "owner_"+task {
			return LayoutNode{"type": "pane", "paneId": paneID}, true
		}
	}
	if node["type"] != "split" {
		return node, false
	}
	next := cloneLayoutDeep(node)
	changed := false
	for _, key := range []string{"a", "b"} {
		if child, ok := layoutChild(node[key]); ok {
			replacement, did := migrateOwnerTerminalLayout(child, paneID)
			next[key] = replacement
			changed = changed || did
		}
	}
	return next, changed
}
