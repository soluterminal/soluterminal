package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ST-181 (@secure, 14 Nov): dictation is cleaned on the person's Claude plan
// only when they ticked it, on their API key only when they picked it in
// Settings, and otherwise by the cloud. An exported key alone is never used.

func chooseAPIKey(t *testing.T, a *apiServer, on bool) {
	t.Helper()
	if err := a.voiceLocal().update(nil, &on, false); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceCleanupModeDefaultsAwayFromThePlan(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	if err := a.voiceLocal().set(false); err != nil {
		t.Fatal(err)
	}
	withVoiceAPIKey(t, false)
	if got := a.localCleanupMode(); got != voiceCleanupCloud {
		t.Fatalf("no opt-in, no key: mode %q, want cloud", got)
	}
	if ok, reason := a.localCleanupAvailable(); ok || reason != "off" {
		t.Fatalf("available=%v reason=%q", ok, reason)
	}
	withVoiceAPIKey(t, true)
	if got := a.localCleanupMode(); got != voiceCleanupCloud {
		t.Fatalf("key exported but not chosen: mode %q, want cloud", got)
	}
	if ok, reason := a.localCleanupAvailable(); ok || reason != "off" {
		t.Fatalf("key exported but not chosen: available=%v reason=%q", ok, reason)
	}
	chooseAPIKey(t, a, true)
	if got := a.localCleanupMode(); got != voiceCleanupAPIKey {
		t.Fatalf("key chosen: mode %q, want api_key", got)
	}
	withVoiceAPIKey(t, false)
	if got := a.localCleanupMode(); got != voiceCleanupCloud {
		t.Fatalf("key chosen but no longer exported: mode %q, want cloud", got)
	}
	withVoiceAPIKey(t, true)
	if err := a.voiceLocal().set(true); err != nil {
		t.Fatal(err)
	}
	if got := a.localCleanupMode(); got != voiceCleanupPlan || a.voiceLocal().apiKeyChosen() {
		t.Fatalf("plan opt-in: mode %q apiKeyChosen=%v, want plan and the key choice cleared", got, a.voiceLocal().apiKeyChosen())
	}
}

// An exported key with no choice must not reach a clean-up run: the request
// goes to the cloud.
func TestVoiceCleanupExportedKeyWithoutChoiceUsesTheCloud(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	if err := a.voiceLocal().set(false); err != nil {
		t.Fatal(err)
	}
	withVoiceAPIKey(t, true)
	var hits int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, 200, map[string]string{"text": "Cloud cleaned.", "cleanup": "ok"})
	}))
	defer cloud.Close()
	a.cloudBase, a.cloudToken = cloud.URL, "tok"
	rec := postCleanup(a, rawDictation)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "cloud" || hits != 1 || countFiles(t, dir, "args") != 0 {
		t.Fatalf("%d %v hits=%d spawned=%d", rec.Code, got, hits, countFiles(t, dir, "args"))
	}
}

func TestVoiceLocalSettingsPlanAndKeyExcludeEachOther(t *testing.T) {
	dir := t.TempDir()
	s := newVoiceLocalSettings(dir)
	on := true
	if err := s.update(nil, &on, false); err != nil {
		t.Fatal(err)
	}
	if !s.apiKeyChosen() || s.enabled() || !s.planChoiceSet() {
		t.Fatalf("after choosing the key: key=%v plan=%v set=%v", s.apiKeyChosen(), s.enabled(), s.planChoiceSet())
	}
	if err := s.update(&on, nil, true); err != nil {
		t.Fatal(err)
	}
	re := newVoiceLocalSettings(dir)
	if !re.enabled() || re.apiKeyChosen() || !re.noticeSeen() {
		t.Fatalf("reloaded: plan=%v key=%v notice=%v", re.enabled(), re.apiKeyChosen(), re.noticeSeen())
	}
}

func TestVoiceCleanupUnsetSettingIsNotAnOptIn(t *testing.T) {
	withVoiceAPIKey(t, false)
	c, dir := newFakeCleaner(t, "ok")
	a := withVoice(&apiServer{}, &voiceServerState{localCleaner: c, voiceLocal: newVoiceLocalSettings(t.TempDir())})
	a.claudeStat = claudeStatus{checked: true, installed: true, authed: true}
	rec := postCleanup(a, rawDictation) // unpaired and no key: nothing may clean
	if rec.Code != 503 || countFiles(t, dir, "args") != 0 {
		t.Fatalf("code=%d spawned=%d", rec.Code, countFiles(t, dir, "args"))
	}
	if snap := a.voiceLocalSnapshot(); snap.Enabled || snap.Using != voiceCleanupCloud || snap.PlanChoiceSet || snap.NoticeSeen {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestVoiceCleanupAPIKeyModeRunsWithoutAClaudeLogin(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	if err := a.voiceLocal().set(false); err != nil {
		t.Fatal(err)
	}
	withVoiceAPIKey(t, true)
	chooseAPIKey(t, a, true)
	a.claudeStat = claudeStatus{checked: true, installed: true, authed: false}
	if ok, reason := a.localCleanupAvailable(); !ok {
		t.Fatalf("API-key mode unavailable: %q", reason)
	}
	var gotKey []bool
	c := a.localCleaner()
	prevEnv := c.env
	c.env = func(bin string, apiKey bool) []string {
		gotKey = append(gotKey, apiKey)
		return prevEnv(bin, apiKey)
	}
	out := c.Clean(t.Context(), "", rawDictation, "")
	if out.Status != "ok" || out.Provider != "local/api-key" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(gotKey) == 0 || !gotKey[0] {
		t.Fatalf("run not started on the API key: %v", gotKey)
	}
	if snap := a.voiceLocalSnapshot(); snap.Using != voiceCleanupAPIKey || !snap.APIKey || !snap.APIKeyChosen || snap.Enabled {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestLocalCleanupEnvAPIKeyModeKeepsOnlyTheKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.invalid")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "other")
	joined := strings.Join(localCleanupEnv("/nonexistent/claude", true), "\n")
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-test") {
		t.Fatal("API key dropped in API-key mode")
	}
	if strings.Contains(joined, "ANTHROPIC_BASE_URL") || strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatal("other ANTHROPIC_* leaked into the API-key run")
	}
	if strings.Contains(strings.Join(localCleanupEnv("/nonexistent/claude", false), "\n"), "ANTHROPIC_") {
		t.Fatal("plan run kept an ANTHROPIC_* variable")
	}
}

// A spare started on the plan is never handed to an API-key run.
func TestLocalCleanerSpareIsBoundToItsLogin(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	c := a.localCleaner()
	c.Prewarm(cleanVariant{profile: "code"})
	c.mu.Lock()
	spare := c.spare
	c.mu.Unlock()
	if spare == nil || spare.variant.apiKey {
		t.Fatalf("plan spare = %+v", spare)
	}
	withVoiceAPIKey(t, true)
	chooseAPIKey(t, a, true)
	p, err := c.take(c.withMode(newCleanVariant("code", "")))
	if err != nil {
		t.Fatal(err)
	}
	defer p.retire()
	if p == spare || !p.variant.apiKey {
		t.Fatal("the plan spare was reused for an API-key run")
	}
}

// @secure: the plan and API-key choices are the person's act at the
// computer. An agent and a relayed request are refused; the notice is not.
func TestPutVoiceLocalChoicesArePersonOnly(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	if err := a.voiceLocal().set(false); err != nil {
		t.Fatal(err)
	}
	put := func(h http.Handler, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/voice/local", strings.NewReader(body)))
		return rec
	}
	direct := http.HandlerFunc(a.handlePutVoiceLocal)
	relayed := markRelayTransport(direct)

	asCaller(t, callerPane)
	for _, body := range []string{`{"apiKey":true}`, `{"enabled":true}`, `{"enabled":false}`} {
		if rec := put(direct, body); rec.Code != http.StatusForbidden {
			t.Fatalf("agent %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if a.voiceLocal().apiKeyChosen() || a.voiceLocal().enabled() {
		t.Fatal("an agent changed the clean-up choice")
	}
	if rec := put(direct, `{"noticeSeen":true}`); rec.Code != 200 {
		t.Fatalf("agent noticeSeen: %d", rec.Code)
	}

	asCaller(t, callerHuman)
	for _, body := range []string{`{"apiKey":true}`, `{"enabled":true}`} {
		rec := put(relayed, body)
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != http.StatusForbidden || got["error"] != "voice_cleanup_choice_local_only" {
			t.Fatalf("relayed %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if a.voiceLocal().apiKeyChosen() || a.voiceLocal().enabled() {
		t.Fatal("a relayed request changed the clean-up choice")
	}
	if rec := put(direct, `{"apiKey":true}`); rec.Code != 200 || !a.voiceLocal().apiKeyChosen() {
		t.Fatalf("person at the computer: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPutVoiceLocalChoices(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	asCaller(t, callerHuman)
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handlePutVoiceLocal(rec, httptest.NewRequest("PUT", "/api/voice/local", strings.NewReader(body)))
		return rec
	}
	if rec := put(`{"enabled":true,"apiKey":true}`); rec.Code != 400 {
		t.Fatalf("both chosen: %d", rec.Code)
	}
	if rec := put(`{}`); rec.Code != 400 {
		t.Fatalf("empty body: %d", rec.Code)
	}
	if rec := put(`{"apiKey":true}`); rec.Code != 200 || !a.voiceLocal().apiKeyChosen() || a.voiceLocal().enabled() {
		t.Fatalf("choose key: %d %s", rec.Code, rec.Body.String())
	}
	rec := put(`{"noticeSeen":true}`)
	var snap voiceLocalResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	if rec.Code != 200 || !snap.NoticeSeen || !snap.APIKeyChosen {
		t.Fatalf("notice seen: %d %+v", rec.Code, snap)
	}
}
