package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

const uiPrefsMaxBody = 16 << 10

// UI layout belongs to this OS user's daemon/data directory, so both direct
// and relayed clients see the same choices regardless of their page origin.
// Optional fields preserve shipped defaults for a never-customized sidebar.
type sidebarUIPrefs struct {
	Order               *[]string `json:"order,omitempty"`
	Hidden              *[]string `json:"hidden,omitempty"`
	Open                *[]string `json:"open,omitempty"`
	WorkspacesCollapsed *bool     `json:"workspacesCollapsed,omitempty"`
}

type uiPrefs struct {
	Sidebar *sidebarUIPrefs `json:"sidebar,omitempty"`
}

type uiPrefsStore struct {
	mu   sync.Mutex
	path string
}

func newUIPrefsStore(dataDir string) *uiPrefsStore {
	return &uiPrefsStore{path: filepath.Join(dataDir, "ui-prefs.json")}
}

func decodeUIPrefs(data []byte) (uiPrefs, error) {
	var prefs uiPrefs
	// Reject null, arrays, unknown fields, trailing JSON and wrongly typed values.
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return prefs, errors.New("prefs must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&prefs); err != nil {
		return prefs, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return prefs, errors.New("trailing JSON")
	}
	if s := prefs.Sidebar; s != nil {
		lists := [][]string{}
		if s.Order != nil {
			lists = append(lists, *s.Order)
		}
		if s.Hidden != nil {
			lists = append(lists, *s.Hidden)
		}
		if s.Open != nil {
			lists = append(lists, *s.Open)
		}
		// Allow bounded future/retired section identifiers: the web reconciles
		// these against its registry so client and daemon versions can differ.
		for _, list := range lists {
			if len(list) > 64 {
				return prefs, errors.New("too many sections")
			}
			for _, key := range list {
				if len(key) == 0 || len(key) > 64 {
					return prefs, errors.New("invalid section key")
				}
				for _, c := range key {
					if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
						return prefs, errors.New("invalid section key")
					}
				}
			}
		}
	}
	// JSON null is not a valid value for an optional field; omission is the
	// explicit way to request defaults.
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(data, &obj)
	for key := range obj {
		if key != "sidebar" {
			return prefs, errors.New("unknown preference field")
		}
	}
	if raw, ok := obj["sidebar"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return prefs, errors.New("sidebar must be an object")
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		for key, v := range fields {
			switch key {
			case "order", "hidden", "open", "workspacesCollapsed":
			default:
				return prefs, errors.New("unknown sidebar field")
			}
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return prefs, errors.New("null sidebar field")
			}
		}
	}
	return prefs, nil
}

func (a *apiServer) handleUIPrefs(w http.ResponseWriter, r *http.Request) {
	if a.personalViewOn() {
		http.Error(w, "layout sync is paused in personal view", http.StatusConflict)
		return
	}
	if a.uiPrefs == nil {
		http.Error(w, "UI preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	p := a.uiPrefs
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		data, err := os.ReadFile(p.path)
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"prefs": nil})
			return
		}
		if err != nil || len(data) > uiPrefsMaxBody {
			http.Error(w, "could not load UI preferences", http.StatusInternalServerError)
			return
		}
		prefs, err := decodeUIPrefs(data)
		if err != nil {
			http.Error(w, "invalid stored UI preferences", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"prefs": prefs})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, uiPrefsMaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "invalid UI preferences body", status)
		return
	}
	prefs, err := decodeUIPrefs(data)
	if err != nil {
		http.Error(w, "invalid UI preferences: "+err.Error(), http.StatusBadRequest)
		return
	}
	data, err = json.Marshal(prefs)
	if err == nil {
		err = writeFileAtomicSynced(p.path, data, 0o600)
	}
	if err != nil {
		http.Error(w, "could not save UI preferences", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prefs": prefs})
}
