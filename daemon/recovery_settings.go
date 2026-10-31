package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type recoveryResumeMode string

const (
	recoveryResumeManual recoveryResumeMode = "manual"
	recoveryResumeAuto   recoveryResumeMode = "auto"
	recoveryResumePaused recoveryResumeMode = "paused"
)

var errInvalidRecoverySettings = errors.New("invalid recovery settings")

type recoverySettings struct {
	ResumeAfterRestart recoveryResumeMode `json:"resumeAfterRestart"`
}

type recoverySettingsStore struct {
	path string

	mu sync.Mutex
	s  recoverySettings
}

func defaultRecoverySettings() recoverySettings {
	mode := recoveryResumeManual
	if os.Getenv("SOLU_AUTO_RESUME") == "1" {
		mode = recoveryResumeAuto
	}
	return recoverySettings{ResumeAfterRestart: mode}
}

func newRecoverySettingsStore(dataDir string) (*recoverySettingsStore, error) {
	st := defaultRecoverySettingsStore(dataDir)
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

func defaultRecoverySettingsStore(dataDir string) *recoverySettingsStore {
	return &recoverySettingsStore{
		path: filepath.Join(dataDir, "recovery_settings.json"),
		s:    defaultRecoverySettings(),
	}
}

func (s *recoverySettingsStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read recovery settings: %w", err)
	}
	var loaded recoverySettings
	if err := json.Unmarshal(b, &loaded); err != nil {
		return fmt.Errorf("parse recovery settings: %w", err)
	}
	mode, ok := normalizeRecoveryResumeMode(loaded.ResumeAfterRestart)
	if !ok {
		return fmt.Errorf("%w: resumeAfterRestart must be manual, auto, or paused", errInvalidRecoverySettings)
	}
	s.mu.Lock()
	s.s = recoverySettings{ResumeAfterRestart: mode}
	s.mu.Unlock()
	return nil
}

func (s *recoverySettingsStore) Snapshot() recoverySettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s
}

func (s *recoverySettingsStore) AutoResumeAfterRestart() bool {
	return s.Snapshot().ResumeAfterRestart == recoveryResumeAuto
}

// Mode is the current After a restart choice: manual ("Ask me"), auto
// ("Resume automatically") or paused ("Don't resume").
func (s *recoverySettingsStore) Mode() recoveryResumeMode {
	return s.Snapshot().ResumeAfterRestart
}

// suspendAfterRestart parks a dead-but-resumable pane found at daemon start
// (its agent died with the machine) without spawning anything. Under "Ask me"
// the client offers Resume on the pane; under "Don't resume" the pane is
// marked RestartPaused so the client shows it as paused with no offer, and it
// stays that way until the person resumes it. Panes carrying a recovery error
// keep their recovery card either way.
func suspendAfterRestart(mgr *Manager, info *PersistedPane, mode recoveryResumeMode) *Pane {
	sp := mgr.SuspendExisting(info)
	if sp == nil || mode != recoveryResumePaused {
		return sp
	}
	sp.mu.Lock()
	if sp.Status == PaneExited && sp.Resumable && sp.RecoveryError == "" {
		sp.RestartPaused = true
	}
	sp.mu.Unlock()
	return sp
}

func (s *recoverySettingsStore) Set(next recoverySettings) (recoverySettings, error) {
	mode, ok := normalizeRecoveryResumeMode(next.ResumeAfterRestart)
	if !ok {
		return recoverySettings{}, fmt.Errorf("%w: resumeAfterRestart must be manual, auto, or paused", errInvalidRecoverySettings)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.ResumeAfterRestart = mode
	if err := s.saveLocked(); err != nil {
		return recoverySettings{}, err
	}
	return s.s, nil
}

func (s *recoverySettingsStore) saveLocked() error {
	b, err := json.MarshalIndent(s.s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal recovery settings: %w", err)
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return fmt.Errorf("write recovery settings: %w", err)
	}
	return nil
}

func normalizeRecoveryResumeMode(mode recoveryResumeMode) (recoveryResumeMode, bool) {
	switch recoveryResumeMode(strings.TrimSpace(string(mode))) {
	case recoveryResumeManual:
		return recoveryResumeManual, true
	case recoveryResumeAuto:
		return recoveryResumeAuto, true
	case recoveryResumePaused:
		return recoveryResumePaused, true
	default:
		return "", false
	}
}

func (a *apiServer) handleRecoverySettings(w http.ResponseWriter, r *http.Request) {
	if a.recoverySettings == nil {
		http.Error(w, "recovery settings unavailable", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.recoverySettings.Snapshot())
	case http.MethodPut:
		var req struct {
			ResumeAfterRestart recoveryResumeMode `json:"resumeAfterRestart"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		updated, err := a.recoverySettings.Set(recoverySettings{ResumeAfterRestart: req.ResumeAfterRestart})
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errInvalidRecoverySettings) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
