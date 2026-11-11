package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Per-install self-update preferences. `off` keeps the notify-then-click
// flow; `when-idle` applies the daemon swap through a bounded collaboration
// hold, temporary Focus compatibility, and the other server safety gates — see the client's maybeAutoApplyUpdate +
// applyUpdate({auto}) and the daemon poll's runAutoApplyIfEligible.
// Daemon-stored (not localStorage) so it's correctly scoped to the install and
// survives a webview cache clear.
//
// Defaults: an install with no saved choice that is new (no earlier state in
// the data directory) starts on `when-idle`, and that is recorded as decided.
// An install that predates the change keeps what it had — `off`, the old
// default, when nothing was saved — and is asked once (autoApplyAsk) instead of
// being switched. Nothing here changes a saved choice; only a person's answer
// (Set with answered=true) does. SOLU_AUTO_UPDATE=0 supplies the `off` default
// and suppresses the question.
type updateAutoApplyMode string

const (
	updateAutoApplyOff      updateAutoApplyMode = "off"
	updateAutoApplyWhenIdle updateAutoApplyMode = "when-idle"
)

var errInvalidUpdateSettings = errors.New("invalid update settings")

type updateSettings struct {
	AutoApply      updateAutoApplyMode `json:"autoApply"`
	ShellAutoApply updateAutoApplyMode `json:"shellAutoApply"`
}

// updateSettingsFile is the on-disk form: the settings plus whether the
// automatic-updates choice has been decided (a person's answer, or the
// new-install default). The file never shows the flag through the API; the
// API shows autoApplyAsk instead.
type updateSettingsFile struct {
	updateSettings
	AutoApplyDecided bool `json:"autoApplyDecided,omitempty"`
}

// updateSettingsView is what the API returns: the settings plus whether the
// client should ask the person about automatic updates.
type updateSettingsView struct {
	updateSettings
	AutoApplyAsk bool `json:"autoApplyAsk"`
}

func autoUpdateEnvDisabled() bool { return os.Getenv("SOLU_AUTO_UPDATE") == "0" }

// updateStateExists reports whether the data directory held daemon state
// before this boot. workspaces.json is written on the first start of every
// release, so it marks an install that predates the new default.
func updateStateExists(dataDir string) bool {
	for _, name := range []string{"workspaces.json", "update_settings.json"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			return true
		}
	}
	return false
}

type updateSettingsStore struct {
	path string

	mu      sync.Mutex
	s       updateSettings
	decided bool
}

func defaultUpdateSettings() updateSettings {
	mode := updateAutoApplyWhenIdle
	if autoUpdateEnvDisabled() {
		mode = updateAutoApplyOff
	}
	shellMode := updateAutoApplyWhenIdle
	if os.Getenv("SOLU_AUTO_UPDATE_SHELL") == "0" {
		shellMode = updateAutoApplyOff
	}
	return updateSettings{AutoApply: mode, ShellAutoApply: shellMode}
}

func newUpdateSettingsStore(dataDir string) (*updateSettingsStore, error) {
	st := defaultUpdateSettingsStore(dataDir)
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

// defaultUpdateSettingsStore is the store used when the file is unreadable or
// invalid. It applies the same rule as an install with nothing saved, and never
// writes: a new install is on, an existing one stays off and undecided, so a
// damaged file can't turn automatic updates on for someone who saved off.
func defaultUpdateSettingsStore(dataDir string) *updateSettingsStore {
	st := &updateSettingsStore{path: filepath.Join(dataDir, "update_settings.json")}
	st.s, st.decided = noFileSettings(dataDir)
	return st
}

// noFileSettings is the setting for an install with nothing usable saved, and
// whether it counts as decided. An install that predates the on-by-default
// change (earlier state in the data directory, or a settings file we can't
// read) keeps the old default: off, or on under SOLU_AUTO_UPDATE=1 as before.
func noFileSettings(dataDir string) (updateSettings, bool) {
	cur := defaultUpdateSettings()
	if !updateStateExists(dataDir) {
		// New install. Under SOLU_AUTO_UPDATE=0 nobody has decided; the env var
		// only supplies the default.
		return cur, !autoUpdateEnvDisabled()
	}
	cur.AutoApply = updateAutoApplyOff
	if os.Getenv("SOLU_AUTO_UPDATE") == "1" {
		cur.AutoApply = updateAutoApplyWhenIdle
		return cur, true
	}
	return cur, false
}

func (s *updateSettingsStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read update settings: %w", err)
		}
		s.loadNoFile()
		return nil
	}
	// Seed from defaults so files written before shellAutoApply existed opt in
	// to the new zero-click shell behavior instead of failing validation.
	file := updateSettingsFile{updateSettings: defaultUpdateSettings()}
	if err := json.Unmarshal(b, &file); err != nil {
		return fmt.Errorf("parse update settings: %w", err)
	}
	loaded := file.updateSettings
	mode, ok := normalizeUpdateAutoApplyMode(loaded.AutoApply)
	if !ok {
		return fmt.Errorf("%w: autoApply must be off or when-idle", errInvalidUpdateSettings)
	}
	shellMode, ok := normalizeUpdateAutoApplyMode(loaded.ShellAutoApply)
	if !ok {
		return fmt.Errorf("%w: shellAutoApply must be off or when-idle", errInvalidUpdateSettings)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A saved choice is kept exactly as written. A file without the decided
	// flag was saved by an older release: when-idle was chosen (it was never
	// the default), off may be the old default and is still asked about.
	s.s = updateSettings{AutoApply: mode, ShellAutoApply: shellMode}
	s.decided = file.AutoApplyDecided || mode == updateAutoApplyWhenIdle
	return nil
}

// loadNoFile settles an install with nothing saved (see noFileSettings). A new
// install records its on-by-default so a later boot, which finds
// workspaces.json, still sees it as decided.
func (s *updateSettingsStore) loadNoFile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Dir(s.path)
	fresh := !updateStateExists(dir)
	s.s, s.decided = noFileSettings(dir)
	if !fresh || !s.decided {
		return
	}
	// Best effort: an unsaved new-install default is simply decided again next
	// start (the data directory then holds state, so the install would be asked
	// instead — the only cost is one question).
	if err := s.saveLocked(); err != nil {
		log.Printf("update settings: %v", err)
	}
}

func (s *updateSettingsStore) Snapshot() updateSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s
}

// AutoApplyWhenIdle reports whether the install opted into unattended
// swap-when-safe. Wired for a future daemon-driven headless apply; the v1
// trigger lives in the client.
func (s *updateSettingsStore) AutoApplyWhenIdle() bool {
	return s.Snapshot().AutoApply == updateAutoApplyWhenIdle
}

// AutoApplyAsk reports whether the person should be asked about automatic
// updates: the install predates the new default, still says off, and nobody has
// answered. SOLU_AUTO_UPDATE=0 suppresses the question.
func (s *updateSettingsStore) AutoApplyAsk() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.decided && s.s.AutoApply == updateAutoApplyOff && !autoUpdateEnvDisabled()
}

// Set saves a change. answered marks the automatic-updates choice as decided so
// the question is never asked again; a change to another setting passes false
// and leaves both the choice and the question as they were.
func (s *updateSettingsStore) Set(next updateSettings, answered bool) (updateSettings, error) {
	mode, ok := normalizeUpdateAutoApplyMode(next.AutoApply)
	if !ok {
		return updateSettings{}, fmt.Errorf("%w: autoApply must be off or when-idle", errInvalidUpdateSettings)
	}
	shellMode, ok := normalizeUpdateAutoApplyMode(next.ShellAutoApply)
	if !ok {
		return updateSettings{}, fmt.Errorf("%w: shellAutoApply must be off or when-idle", errInvalidUpdateSettings)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, prevDecided := s.s, s.decided
	s.s.AutoApply = mode
	s.s.ShellAutoApply = shellMode
	if answered {
		s.decided = true
	}
	if err := s.saveLocked(); err != nil {
		s.s, s.decided = prev, prevDecided
		return updateSettings{}, err
	}
	return s.s, nil
}

func (s *updateSettingsStore) saveLocked() error {
	b, err := json.MarshalIndent(updateSettingsFile{updateSettings: s.s, AutoApplyDecided: s.decided}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal update settings: %w", err)
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return fmt.Errorf("write update settings: %w", err)
	}
	return nil
}

func normalizeUpdateAutoApplyMode(mode updateAutoApplyMode) (updateAutoApplyMode, bool) {
	switch updateAutoApplyMode(strings.TrimSpace(string(mode))) {
	case updateAutoApplyOff:
		return updateAutoApplyOff, true
	case updateAutoApplyWhenIdle:
		return updateAutoApplyWhenIdle, true
	default:
		return "", false
	}
}

func (a *apiServer) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if a.updateSettings == nil {
		http.Error(w, "update settings unavailable", http.StatusServiceUnavailable)
		return
	}
	view := func(u updateSettings) updateSettingsView {
		return updateSettingsView{updateSettings: u, AutoApplyAsk: a.updateSettings.AutoApplyAsk()}
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, view(a.updateSettings.Snapshot()))
	case http.MethodPut:
		// Only a person changes this: autoApplyAnswered would otherwise let an
		// agent silence the question for good or switch updates on or off.
		if !a.requireHuman(w, r, "change automatic updates") {
			return
		}
		// Start from the snapshot so an older web bundle that PUTs only
		// autoApply preserves the shell preference. Only a request that says
		// autoApplyAnswered records the person's answer; saving another
		// setting (the desktop restart) never does.
		req := struct {
			updateSettings
			AutoApplyAnswered bool `json:"autoApplyAnswered"`
		}{updateSettings: a.updateSettings.Snapshot()}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		updated, err := a.updateSettings.Set(req.updateSettings, req.AutoApplyAnswered)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errInvalidUpdateSettings) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, http.StatusOK, view(updated))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
