package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tierCloud(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/entitlements" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func entitlementsResponse(t *testing.T, a *apiServer) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleEntitlements(rec, httptest.NewRequest(http.MethodGet, "/api/entitlements", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDaemonEntitlementsTierFromCloud(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	cases := []struct {
		name, body, tier string
		team, account    bool
		smart            bool
	}{
		{"account", `{"tier":"account","team":false,"account":true,"smartApprovals":true}`, "account", false, true, true},
		{"team", `{"tier":"team","team":true,"account":true,"smartApprovals":false}`, "team", true, true, false},
		{"anonymous", `{"tier":"none","team":false,"account":false}`, "none", false, false, false},
		{"older cloud stranger", `{"team":false}`, "account", false, true, false},
		{"older cloud member", `{"team":true}`, "team", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := tierCloud(t, tc.body)
			a := &apiServer{}
			dir := t.TempDir()
			a.entitlements = newTeamEntitlements(dir, func() (string, string) { return cloud.URL, "machine" })
			a.entitlements.refresh(context.Background(), "")
			got := entitlementsResponse(t, a)
			if got["tier"] != tc.tier || got["team"] != tc.team || got["account"] != tc.account {
				t.Fatalf("response = %v", got)
			}
			for _, key := range []string{"remoteAccess", "push", "voice"} {
				want := tc.account
				if key == "voice" {
					want = tc.account && voiceBuiltIn // a build without voice never offers it
				}
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
			if got["smartApprovals"] != tc.smart {
				t.Fatalf("response = %v", got)
			}
			if got["source"] != "machine" {
				t.Fatalf("source = %v", got["source"])
			}
			if team, _ := a.entitlements.verdict(); team != tc.team {
				t.Fatalf("team gate verdict = %v, want %v", team, tc.team)
			}
			// The tier survives a restart.
			reloaded := newTeamEntitlements(dir, nil)
			if tier := reloaded.cachedTier(); tier != tc.tier {
				t.Fatalf("persisted tier = %q, want %q", tier, tc.tier)
			}
		})
	}
}

func TestDaemonEntitlementsLegacyFileAndNoVerdict(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	// A file written by a pre-tier daemon only has team.
	dir := t.TempDir()
	old := `{"team":false,"source":"machine","checkedAt":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, entitlementsFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{entitlements: newTeamEntitlements(dir, nil)}
	got := entitlementsResponse(t, a)
	if got["tier"] != "account" || got["team"] != false || got["account"] != true {
		t.Fatalf("legacy non-team record = %v", got)
	}

	// No verdict at all: no account.
	fresh := &apiServer{entitlements: newTeamEntitlements(t.TempDir(), nil)}
	got = entitlementsResponse(t, fresh)
	if got["tier"] != "none" || got["account"] != false || got["remoteAccess"] != false || got["source"] != "none" {
		t.Fatalf("fresh install = %v", got)
	}

	// Nothing on the machine grants team before the cloud answers.
	if fresh.entitlements.cachedTier() != tierNone {
		t.Fatal("fresh install has a cached tier")
	}
}

func TestDaemonEntitlementsEnvOverrideKeepsAccount(t *testing.T) {
	cloud := tierCloud(t, `{"tier":"team","team":true,"account":true}`)
	a := &apiServer{}
	a.entitlements = newTeamEntitlements(t.TempDir(), func() (string, string) { return cloud.URL, "machine" })
	t.Setenv("SOLU_TEAM_FEATURES", "")
	a.entitlements.refresh(context.Background(), "")

	t.Setenv("SOLU_TEAM_FEATURES", "0")
	got := entitlementsResponse(t, a)
	if got["tier"] != "account" || got["team"] != false || got["source"] != "env" {
		t.Fatalf("env off on a team install = %v", got)
	}
	t.Setenv("SOLU_TEAM_FEATURES", "1")
	fresh := &apiServer{entitlements: newTeamEntitlements(t.TempDir(), nil)}
	if got := entitlementsResponse(t, fresh); got["tier"] != "team" {
		t.Fatalf("env on = %v", got)
	}
}
