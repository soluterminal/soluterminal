package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeEntitlementsCloud(t *testing.T, teamBearers map[string]bool, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/entitlements" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if teamBearers[bearer] {
			_, _ = w.Write([]byte(`{"team":true}`))
		} else {
			_, _ = w.Write([]byte(`{"team":false}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEntitlementsVerdictOrder(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	dir := t.TempDir()
	e := newTeamEntitlements(dir, nil)
	if team, src := e.verdict(); team || src != "none" {
		t.Fatalf("fresh install = %t/%s, want off", team, src)
	}

	t.Setenv("SOLU_TEAM_FEATURES", "0")
	if team, src := e.verdict(); team || src != "env" {
		t.Fatalf("env off = %t/%s", team, src)
	}
	t.Setenv("SOLU_TEAM_FEATURES", "1")
	fresh := newTeamEntitlements(t.TempDir(), nil)
	if team, _ := fresh.verdict(); !team {
		t.Fatal("env on must enable a fresh install")
	}
}

func TestEntitlementsMachineVerdictPersistsAndOverridesLegacy(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	cloud := fakeEntitlementsCloud(t, map[string]bool{"team-machine": true}, http.StatusOK)
	dir := t.TempDir()
	creds := func() (string, string) { return cloud.URL, "stranger-machine" }
	e := newTeamEntitlements(dir, creds)
	e.refresh(context.Background(), "")
	if team, src := e.verdict(); team || src != "machine" {
		t.Fatalf("cloud said stranger: got %t/%s", team, src)
	}

	creds = func() (string, string) { return cloud.URL, "team-machine" }
	e = newTeamEntitlements(dir, creds)
	if team, _ := e.verdict(); team {
		t.Fatal("persisted stranger verdict must load on restart")
	}
	e.refresh(context.Background(), "")
	reloaded := newTeamEntitlements(dir, nil)
	if team, src := reloaded.verdict(); !team || src != "machine" {
		t.Fatalf("persisted team verdict = %t/%s", team, src)
	}
	info, err := os.Stat(filepath.Join(dir, entitlementsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("entitlements file mode = %v, err %v", info, err)
	}
}

func TestEntitlementsOldCloudKeepsVerdict(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	cloud := fakeEntitlementsCloud(t, nil, http.StatusNotFound)
	dir := t.TempDir()
	cached := newTeamEntitlements(dir, nil)
	cached.store(entitlementRecord{Team: true, Tier: tierTeam, Source: "machine", CheckedAt: time.Now()})
	e := newTeamEntitlements(dir, func() (string, string) { return cloud.URL, "tok" })
	e.refresh(context.Background(), "")
	if team, src := e.verdict(); !team || src != "machine" {
		t.Fatalf("404 from an older cloud must not change the verdict: %t/%s", team, src)
	}
}

func testJWT(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
}

func TestJevCloudEntitledFromCache(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	prev := jevEntitlementsRef.Load()
	t.Cleanup(func() { jevEntitlementsRef.Store(prev) })

	jevEntitlementsRef.Store(nil)
	if allowed, known := jevCloudEntitledFromCache(); allowed || known {
		t.Fatalf("no cache: allowed=%t known=%t, want unknown", allowed, known)
	}
	e := newTeamEntitlements(t.TempDir(), nil)
	jevEntitlementsRef.Store(e)
	if _, known := jevCloudEntitledFromCache(); known {
		t.Fatal("no cloud verdict yet must be unknown")
	}
	e.store(entitlementRecord{Tier: tierAccount, SmartApprovals: true, Source: "machine", CheckedAt: time.Now()})
	if allowed, known := jevCloudEntitledFromCache(); !allowed || !known {
		t.Fatalf("account with smart approvals: allowed=%t known=%t", allowed, known)
	}
	e.store(entitlementRecord{Tier: tierAccount, SmartApprovals: false, Source: "machine", CheckedAt: time.Now()})
	if allowed, known := jevCloudEntitledFromCache(); allowed || !known {
		t.Fatalf("account without smart approvals: allowed=%t known=%t", allowed, known)
	}
}
