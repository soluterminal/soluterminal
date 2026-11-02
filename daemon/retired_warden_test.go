package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredWardenSettingsMigration(t *testing.T) {
	for _, kind := range []string{"legacy", "unknown", "corrupt", "missing"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "warden_settings.json")
			raw := `{"enabled":true,"autoFlags":true,"model":"expensive","triageAfterMin":10,"maxRunsPerHour":6}`
			if kind == "unknown" {
				raw = raw[:len(raw)-1] + `,"other":{"keep":true}}`
			}
			if kind == "corrupt" {
				raw = `{"enabled":`
			}
			if kind != "missing" {
				putCapabilityFile(t, path, raw)
			}
			log := filepath.Join(dir, "warden-runs", "old.log")
			putCapabilityFile(t, log, "historical run")
			err := retireWardenSettings(dir)
			if (err != nil) != (kind == "corrupt") {
				t.Fatalf("migration: %v", err)
			}
			out, readErr := os.ReadFile(path)
			switch kind {
			case "legacy", "missing":
				if !os.IsNotExist(readErr) {
					t.Fatalf("retired settings remain: %s %v", out, readErr)
				}
			case "unknown":
				if string(out) != "{\n  \"other\": {\n    \"keep\": true\n  }\n}" {
					t.Fatalf("unknown keys changed: %s", out)
				}
			case "corrupt":
				if string(out) != raw {
					t.Fatal("corrupt file changed")
				}
			}
			if out, err := os.ReadFile(log); err != nil || string(out) != "historical run" {
				t.Fatal("run history changed")
			}
			if kind != "corrupt" {
				if err := retireWardenSettings(dir); err != nil {
					t.Fatal("repeat migration", err)
				}
			}
		})
	}
}

func TestRetiredWardenFlagsClearedBeforeRestore(t *testing.T) {
	dir := t.TempDir()
	raw := `[{"id":"old","flagged":true,"flagMemo":"stale","flagOwner":"warden","flagEpisode":"episode","flagRevision":"revision"},{"id":"user","flagged":true,"flagMemo":"bookmark","flagOwner":"user"},{"id":"agent","flagged":true,"flagMemo":"agent note","flagOwner":"agent:caller"}]`
	putCapabilityFile(t, filepath.Join(dir, "panes.json"), raw)
	store, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	panes := store.List()
	if len(panes) != 3 || panes[0].Flagged || panes[0].FlagOwner != "" || panes[0].FlagMemo != "" {
		t.Fatalf("stale flag restored: %+v", panes)
	}
	if !panes[1].Flagged || panes[1].FlagMemo != "bookmark" || !panes[2].Flagged {
		t.Fatal("other flags changed")
	}
	out, err := os.ReadFile(filepath.Join(dir, "panes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "flagEpisode") || strings.Contains(string(out), "flagRevision") || strings.Contains(string(out), "warden") {
		t.Fatal("retired fields remain on disk")
	}
	var checked []PersistedPane
	if err = json.Unmarshal(out, &checked); err != nil {
		t.Fatal(err)
	}
	again, err := NewPaneStore(dir)
	if err != nil || again.List()[0].Flagged {
		t.Fatal("cleanup did not persist", err)
	}
}

func TestRetiredWardenRoutesUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	(&apiServer{metrics: NewMetrics(), mgr: NewManager(""), dataDir: t.TempDir()}).routes(mux)
	for _, tc := range []struct{ method, path string }{{"GET", "/api/warden/settings"}, {"PUT", "/api/warden/settings"}, {"GET", "/api/warden/status"}, {"POST", "/api/warden/run-now"}, {"GET", "/api/warden/runs/old.log"}} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		_, pattern := mux.Handler(req)
		if strings.Contains(pattern, "warden") {
			t.Fatalf("retired endpoint still registered: %s", pattern)
		}
	}
}
