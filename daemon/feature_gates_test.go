package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// teamPlanAPIServer is a team-plan member who is not an operator: the shape a
// customer team plan will have once the cloud grants team without operator.
func teamPlanAPIServer(t *testing.T) *apiServer {
	t.Helper()
	t.Setenv("SOLU_TEAM_FEATURES", "")
	t.Setenv("SOLU_OPERATOR_FEATURES", "")
	dir := t.TempDir()
	writeEntitlementsFile(t, dir, entitlementRecord{Team: true, Operator: boolPtr(false), Tier: tierTeam, Source: "machine", CheckedAt: time.Now()})
	return &apiServer{entitlements: newTeamEntitlements(dir, nil)}
}

func operatorAPIServer(t *testing.T) *apiServer {
	t.Helper()
	t.Setenv("SOLU_TEAM_FEATURES", "")
	t.Setenv("SOLU_OPERATOR_FEATURES", "")
	dir := t.TempDir()
	writeEntitlementsFile(t, dir, entitlementRecord{Team: true, Operator: boolPtr(true), Tier: tierTeam, Source: "machine", CheckedAt: time.Now()})
	a := &apiServer{entitlements: newTeamEntitlements(dir, nil)}
	a.view = newPersonalView(dir)
	return a
}

func TestOperatorFlagBackCompat(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		rec  entitlementRecord
		want bool
	}{
		{"pre-split team record", entitlementRecord{Team: true, Source: "machine", CheckedAt: now}, true},
		{"pre-split account record", entitlementRecord{Team: false, Tier: tierAccount, Source: "account", CheckedAt: now}, false},
		{"team plan, not operator", entitlementRecord{Team: true, Operator: boolPtr(false), Tier: tierTeam, CheckedAt: now}, false},
		{"operator", entitlementRecord{Team: true, Operator: boolPtr(true), Tier: tierTeam, CheckedAt: now}, true},
		{"operator flag on a non-team tier is ignored", entitlementRecord{Team: false, Operator: boolPtr(true), Tier: tierAccount, CheckedAt: now}, false},
	}
	for _, c := range cases {
		if got := c.rec.operatorFlag(); got != c.want {
			t.Errorf("%s: operatorFlag=%t, want %t", c.name, got, c.want)
		}
	}
	// A cloud answer without the field stores nil, so the fallback applies.
	rec := recordFromVerdict(cloudVerdict{Team: true, Tier: tierTeam}, "machine", "", now)
	if rec.Operator != nil || !rec.operatorFlag() {
		t.Fatalf("old cloud answer: operator=%v flag=%t", rec.Operator, rec.operatorFlag())
	}
}

func TestOperatorEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	writeEntitlementsFile(t, dir, entitlementRecord{Team: false, Tier: tierAccount, Source: "account", CheckedAt: time.Now()})
	e := newTeamEntitlements(dir, nil)

	t.Setenv("SOLU_OPERATOR_FEATURES", "")
	t.Setenv("SOLU_TEAM_FEATURES", "1")
	if on, src := e.operatorVerdict(); !on || src != "env" {
		t.Fatalf("SOLU_TEAM_FEATURES=1 must still turn operator on: %t/%s", on, src)
	}
	if team, _ := e.verdict(); !team {
		t.Fatal("SOLU_TEAM_FEATURES=1 must still turn team on")
	}
	t.Setenv("SOLU_OPERATOR_FEATURES", "0")
	if on, _ := e.operatorVerdict(); on {
		t.Fatal("SOLU_OPERATOR_FEATURES=0 must win over SOLU_TEAM_FEATURES")
	}
	if team, _ := e.verdict(); !team {
		t.Fatal("SOLU_OPERATOR_FEATURES must not change the team verdict")
	}
	if on, src := bootOperatorVerdict(dir); on || src != "env" {
		t.Fatalf("boot verdict ignored SOLU_OPERATOR_FEATURES=0: %t/%s", on, src)
	}
}

func TestBootOperatorVerdictTeamPlan(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	t.Setenv("SOLU_OPERATOR_FEATURES", "")
	dir := t.TempDir()
	writeEntitlementsFile(t, dir, entitlementRecord{Team: true, Operator: boolPtr(false), Tier: tierTeam, Source: "machine", CheckedAt: time.Now()})
	if on, _ := bootOperatorVerdict(dir); on {
		t.Fatal("a team-plan member must not boot the operator engines")
	}
}

func TestEntitlementsEndpointReportsOperatorAndView(t *testing.T) {
	a := operatorAPIServer(t)
	read := func() map[string]any {
		rec := httptest.NewRecorder()
		a.handleEntitlements(rec, httptest.NewRequest(http.MethodGet, "/api/entitlements", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := read()
	if out["operator"] != true || out["team"] != true || out["viewPersonal"] != false {
		t.Fatalf("operator install: %v", out)
	}
	if _, err := a.view.set(true); err != nil {
		t.Fatal(err)
	}
	out = read()
	// The daemon reports the real verdict plus the view; the web hides.
	if out["operator"] != true || out["viewPersonal"] != true {
		t.Fatalf("personal view: %v", out)
	}

	team := teamPlanAPIServer(t)
	rec := httptest.NewRecorder()
	team.handleEntitlements(rec, httptest.NewRequest(http.MethodGet, "/api/entitlements", nil))
	var tp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &tp)
	if tp["team"] != true || tp["operator"] != false {
		t.Fatalf("team-plan install: %v", tp)
	}
}

func TestPersonalViewPersistsAndIsLocalOnly(t *testing.T) {
	a := operatorAPIServer(t)
	put := func(body string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/view", strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		a.handlePutView(rec, req)
		return rec
	}
	if rec := put(`{"personal":true}`, map[string]string{"X-Forwarded-Prefix": "/m/abc"}); rec.Code != http.StatusForbidden {
		t.Fatalf("relayed change = %d, want 403", rec.Code)
	}
	if rec := put(`{}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", rec.Code)
	}
	if rec := put(`{"personal":true}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("local change = %d: %s", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(a.view.path), personalViewFile))
	if err != nil || !strings.Contains(string(b), `"personal":true`) {
		t.Fatalf("view.json = %q, %v", b, err)
	}
	reloaded := newPersonalView(filepath.Dir(a.view.path))
	if !reloaded.snapshot().Personal || reloaded.snapshot().Since.IsZero() {
		t.Fatalf("reloaded = %+v", reloaded.snapshot())
	}
}

func TestPersonalViewUnavailableWithoutTeamFeatures(t *testing.T) {
	a := publicAPIServer(t)
	a.view = newPersonalView(t.TempDir())
	rec := httptest.NewRecorder()
	a.handlePutView(rec, httptest.NewRequest(http.MethodPut, "/api/view", strings.NewReader(`{"personal":true}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("free install switch = %d, want 403", rec.Code)
	}
	// Even a stale file can't turn it on for a free install.
	if _, err := a.view.set(true); err != nil {
		t.Fatal(err)
	}
	if a.personalViewOn() {
		t.Fatal("a free install has nothing to hide; the view must stay off")
	}
	get := httptest.NewRecorder()
	a.handleGetView(get, httptest.NewRequest(http.MethodGet, "/api/view", nil))
	if !strings.Contains(get.Body.String(), `"available":false`) || !strings.Contains(get.Body.String(), `"personal":false`) {
		t.Fatalf("GET /api/view = %s", get.Body.String())
	}
}
