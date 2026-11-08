package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateSettingsDefaultsToWhenIdle(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	t.Setenv("SOLU_AUTO_UPDATE_SHELL", "")
	st, err := newUpdateSettingsStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUpdateSettingsStore: %v", err)
	}
	if got := st.Snapshot().AutoApply; got != updateAutoApplyWhenIdle {
		t.Fatalf("default autoApply = %q, want %q", got, updateAutoApplyWhenIdle)
	}
	if !st.AutoApplyWhenIdle() {
		t.Fatal("AutoApplyWhenIdle false for the when-idle default")
	}
	if got := st.Snapshot().ShellAutoApply; got != updateAutoApplyWhenIdle {
		t.Fatalf("default shellAutoApply = %q, want %q", got, updateAutoApplyWhenIdle)
	}
}

func TestUpdateSettingsShellEnvDisablesDefault(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE_SHELL", "0")
	st, err := newUpdateSettingsStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUpdateSettingsStore: %v", err)
	}
	if got := st.Snapshot().ShellAutoApply; got != updateAutoApplyOff {
		t.Fatalf("default shellAutoApply = %q, want %q", got, updateAutoApplyOff)
	}
}

func TestUpdateSettingsEnvSeedsWhenIdle(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "1")
	st, err := newUpdateSettingsStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUpdateSettingsStore: %v", err)
	}
	if got := st.Snapshot().AutoApply; got != updateAutoApplyWhenIdle {
		t.Fatalf("default autoApply = %q, want %q", got, updateAutoApplyWhenIdle)
	}
	if !st.AutoApplyWhenIdle() {
		t.Fatal("AutoApplyWhenIdle false for when-idle default")
	}
}

func TestUpdateSettingsFileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOLU_AUTO_UPDATE", "1")
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatalf("newUpdateSettingsStore: %v", err)
	}
	if _, err := st.Set(updateSettings{AutoApply: updateAutoApplyOff, ShellAutoApply: updateAutoApplyWhenIdle}, true); err != nil {
		t.Fatalf("Set off: %v", err)
	}

	t.Setenv("SOLU_AUTO_UPDATE", "1")
	reloaded, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatalf("reload update settings: %v", err)
	}
	if got := reloaded.Snapshot().AutoApply; got != updateAutoApplyOff {
		t.Fatalf("reloaded autoApply = %q, want %q (file must win over env)", got, updateAutoApplyOff)
	}
}

func TestUpdateSettingsRejectsInvalidMode(t *testing.T) {
	st := defaultUpdateSettingsStore(t.TempDir())
	if _, err := st.Set(updateSettings{AutoApply: "always", ShellAutoApply: updateAutoApplyWhenIdle}, true); !errors.Is(err, errInvalidUpdateSettings) {
		t.Fatalf("Set invalid error = %v, want errInvalidUpdateSettings", err)
	}
}

func TestUpdateSettingsRejectsInvalidShellMode(t *testing.T) {
	st := defaultUpdateSettingsStore(t.TempDir())
	if _, err := st.Set(updateSettings{AutoApply: updateAutoApplyOff, ShellAutoApply: "always"}, true); !errors.Is(err, errInvalidUpdateSettings) {
		t.Fatalf("Set invalid shell error = %v, want errInvalidUpdateSettings", err)
	}
}

func TestUpdateSettingsLegacyFileUsesShellDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOLU_AUTO_UPDATE_SHELL", "")
	if err := os.WriteFile(filepath.Join(dir, "update_settings.json"), []byte(`{"autoApply":"off"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatalf("load legacy settings: %v", err)
	}
	if got := st.Snapshot().ShellAutoApply; got != updateAutoApplyWhenIdle {
		t.Fatalf("legacy shellAutoApply = %q, want %q", got, updateAutoApplyWhenIdle)
	}
}

func TestUpdateSettingsPutPreservesOmittedShellPreference(t *testing.T) {
	st := defaultUpdateSettingsStore(t.TempDir())
	if _, err := st.Set(updateSettings{AutoApply: updateAutoApplyOff, ShellAutoApply: updateAutoApplyOff}, true); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	a := &apiServer{updateSettings: st}
	rec := httptest.NewRecorder()
	a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/update/settings", strings.NewReader(`{"autoApply":"when-idle"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got updateSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ShellAutoApply != updateAutoApplyOff {
		t.Fatalf("shellAutoApply = %q, want preserved off", got.ShellAutoApply)
	}
}

func TestUpdateSettingsPutRejectsInvalidShellPreference(t *testing.T) {
	st := defaultUpdateSettingsStore(t.TempDir())
	a := &apiServer{updateSettings: st}
	rec := httptest.NewRecorder()
	a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/update/settings", strings.NewReader(`{"autoApply":"off","shellAutoApply":"always"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestUpdateSettingsRejectsInvalidFileMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "update_settings.json"), []byte(`{"autoApply":"always"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if _, err := newUpdateSettingsStore(dir); !errors.Is(err, errInvalidUpdateSettings) {
		t.Fatalf("load invalid error = %v, want errInvalidUpdateSettings", err)
	}
}

func TestUpdateSettingsEnvZeroKeepsDefaultOff(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "0")
	st, err := newUpdateSettingsStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUpdateSettingsStore: %v", err)
	}
	if got := st.Snapshot().AutoApply; got != updateAutoApplyOff {
		t.Fatalf("default autoApply = %q, want %q", got, updateAutoApplyOff)
	}
}

// existingInstall marks the data directory as one an earlier release ran in.
func existingInstall(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "workspaces.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSavedSettings(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "update_settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpdateSettingsNewInstallIsOnAndNeverAsked(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	dir := t.TempDir()
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().AutoApply != updateAutoApplyWhenIdle || st.AutoApplyAsk() {
		t.Fatalf("new install = %+v ask=%v, want on and not asked", st.Snapshot(), st.AutoApplyAsk())
	}
	// The next boot finds workspaces.json; the recorded default must keep it
	// a new install's on, not an old install's question.
	existingInstall(t, dir)
	again, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Snapshot().AutoApply != updateAutoApplyWhenIdle || again.AutoApplyAsk() {
		t.Fatalf("new install after a restart = %+v ask=%v", again.Snapshot(), again.AutoApplyAsk())
	}
}

func TestUpdateSettingsExistingInstallWithNothingSavedStaysOffAndAsks(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	dir := t.TempDir()
	existingInstall(t, dir)
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().AutoApply != updateAutoApplyOff || !st.AutoApplyAsk() {
		t.Fatalf("existing install = %+v ask=%v, want off and asked", st.Snapshot(), st.AutoApplyAsk())
	}
	if _, err := os.Stat(filepath.Join(dir, "update_settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading wrote a settings file for an undecided install: %v", err)
	}
}

func TestUpdateSettingsExistingInstallEnvOneStaysOn(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "1")
	dir := t.TempDir()
	existingInstall(t, dir)
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().AutoApply != updateAutoApplyWhenIdle || st.AutoApplyAsk() {
		t.Fatalf("env=1 existing install = %+v ask=%v", st.Snapshot(), st.AutoApplyAsk())
	}
}

// No saved choice changes without the person's answer: an old file saying off
// loads as off, asks, and is not rewritten, on every boot.
func TestUpdateSettingsSavedChoiceNeverChangesWithoutAnAnswer(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	for _, body := range []string{
		`{"autoApply":"off","shellAutoApply":"off"}`,
		`{"autoApply":"off"}`,
		`{"autoApply":"when-idle","shellAutoApply":"off"}`,
	} {
		dir := t.TempDir()
		existingInstall(t, dir)
		path := writeSavedSettings(t, dir, body)
		before, _ := newUpdateSettingsStore(dir)
		for i := 0; i < 3; i++ {
			st, err := newUpdateSettingsStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if st.Snapshot().AutoApply != before.Snapshot().AutoApply {
				t.Fatalf("%s: autoApply changed on boot %d to %q", body, i, st.Snapshot().AutoApply)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != body {
			t.Fatalf("%s: file rewritten to %q (%v)", body, raw, err)
		}
	}
}

func TestUpdateSettingsSavedOffIsAskedAndSavedOnIsNot(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	off := t.TempDir()
	existingInstall(t, off)
	writeSavedSettings(t, off, `{"autoApply":"off"}`)
	st, _ := newUpdateSettingsStore(off)
	if !st.AutoApplyAsk() {
		t.Fatal("a saved Notify me install is not asked")
	}
	on := t.TempDir()
	existingInstall(t, on)
	writeSavedSettings(t, on, `{"autoApply":"when-idle"}`)
	st, _ = newUpdateSettingsStore(on)
	if st.AutoApplyAsk() {
		t.Fatal("a saved Install when idle install is asked")
	}
}

func TestUpdateSettingsEnvZeroSuppressesTheQuestion(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "0")
	dir := t.TempDir()
	existingInstall(t, dir)
	writeSavedSettings(t, dir, `{"autoApply":"off"}`)
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().AutoApply != updateAutoApplyOff || st.AutoApplyAsk() {
		t.Fatalf("env=0 = %+v ask=%v, want off and not asked", st.Snapshot(), st.AutoApplyAsk())
	}
	fresh, err := newUpdateSettingsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Snapshot().AutoApply != updateAutoApplyOff || fresh.AutoApplyAsk() {
		t.Fatalf("env=0 new install = %+v ask=%v", fresh.Snapshot(), fresh.AutoApplyAsk())
	}
}

func TestUpdateSettingsAnswerIsRecordedAndNeverAsksAgain(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	for _, answer := range []updateAutoApplyMode{updateAutoApplyOff, updateAutoApplyWhenIdle} {
		dir := t.TempDir()
		existingInstall(t, dir)
		writeSavedSettings(t, dir, `{"autoApply":"off","shellAutoApply":"off"}`)
		st, err := newUpdateSettingsStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Set(updateSettings{AutoApply: answer, ShellAutoApply: updateAutoApplyOff}, true); err != nil {
			t.Fatal(err)
		}
		again, err := newUpdateSettingsStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if again.Snapshot().AutoApply != answer || again.AutoApplyAsk() {
			t.Fatalf("after answering %q: %+v ask=%v", answer, again.Snapshot(), again.AutoApplyAsk())
		}
	}
}

// Changing the desktop-restart setting is not an answer.
func TestUpdateSettingsOtherChangesDoNotAnswer(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	dir := t.TempDir()
	existingInstall(t, dir)
	writeSavedSettings(t, dir, `{"autoApply":"off"}`)
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{updateSettings: st}
	rec := httptest.NewRecorder()
	a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/update/settings", strings.NewReader(`{"autoApply":"off","shellAutoApply":"off"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"autoApplyAsk":true`) {
		t.Fatalf("a shell-only save answered the question: %s", rec.Body.String())
	}
	again, _ := newUpdateSettingsStore(dir)
	if !again.AutoApplyAsk() || again.Snapshot().AutoApply != updateAutoApplyOff {
		t.Fatalf("after a shell-only save: %+v ask=%v", again.Snapshot(), again.AutoApplyAsk())
	}
}

func TestUpdateSettingsAPIAnswerRoundTrip(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	dir := t.TempDir()
	existingInstall(t, dir)
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{updateSettings: st}
	get := func() string {
		rec := httptest.NewRecorder()
		a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodGet, "/api/update/settings", nil))
		return rec.Body.String()
	}
	if body := get(); !strings.Contains(body, `"autoApplyAsk":true`) || strings.Contains(body, "autoApplyDecided") {
		t.Fatalf("GET before answering = %s", body)
	}
	rec := httptest.NewRecorder()
	a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/update/settings", strings.NewReader(`{"autoApply":"off","autoApplyAnswered":true}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"autoApplyAsk":false`) {
		t.Fatalf("PUT answer = %d %s", rec.Code, rec.Body.String())
	}
	if body := get(); !strings.Contains(body, `"autoApply":"off"`) || !strings.Contains(body, `"autoApplyAsk":false`) {
		t.Fatalf("GET after answering = %s", body)
	}
}

// A damaged settings file must not turn updates on for an install that saved
// off: main falls back to defaultUpdateSettingsStore, which keeps the old
// default for an existing install and asks.
func TestUpdateSettingsUnreadableFileFallbackKeepsExistingInstallOff(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	for _, bad := range []string{`{not json`, `{"autoApply":"always"}`} {
		dir := t.TempDir()
		path := writeSavedSettings(t, dir, bad)
		if _, err := newUpdateSettingsStore(dir); err == nil {
			t.Fatalf("%s loaded without error", bad)
		}
		st := defaultUpdateSettingsStore(dir)
		if st.Snapshot().AutoApply != updateAutoApplyOff || st.AutoApplyWhenIdle() || !st.AutoApplyAsk() {
			t.Fatalf("%s: fallback = %+v ask=%v, want off and asked", bad, st.Snapshot(), st.AutoApplyAsk())
		}
		if raw, _ := os.ReadFile(path); string(raw) != bad {
			t.Fatalf("%s: fallback rewrote the file to %q", bad, raw)
		}
	}
	// With earlier state but no file, the same rule holds.
	dir := t.TempDir()
	existingInstall(t, dir)
	if st := defaultUpdateSettingsStore(dir); st.AutoApplyWhenIdle() {
		t.Fatal("fallback for an existing install is on")
	}
	// A new install's fallback is the new default.
	if st := defaultUpdateSettingsStore(t.TempDir()); !st.AutoApplyWhenIdle() || st.AutoApplyAsk() {
		t.Fatal("fallback for a new install is not on")
	}
}

// SOLU_AUTO_UPDATE=0 only supplies the default: nobody answered, so removing
// it later asks an existing install instead of treating it as decided.
func TestUpdateSettingsEnvZeroNewInstallIsNotDecided(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "0")
	dir := t.TempDir()
	if _, err := newUpdateSettingsStore(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "update_settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env=0 new install wrote a decision: %v", err)
	}
	existingInstall(t, dir)
	t.Setenv("SOLU_AUTO_UPDATE", "")
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().AutoApply != updateAutoApplyOff || !st.AutoApplyAsk() {
		t.Fatalf("after removing env=0: %+v ask=%v", st.Snapshot(), st.AutoApplyAsk())
	}
}

// Only a person saves update settings. An agent that could PUT
// autoApplyAnswered would silence the question for good, or turn updates on.
func TestUpdateSettingsPutRequiresAPerson(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "")
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })

	put := func(a *apiServer, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/update/settings", strings.NewReader(body)))
		return rec
	}
	for _, kind := range []callerKind{callerPane, callerWorker, callerUnverified} {
		dir := t.TempDir()
		existingInstall(t, dir)
		st, err := newUpdateSettingsStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		a := &apiServer{updateSettings: st}
		identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: kind} }
		for _, body := range []string{
			`{"autoApply":"off","autoApplyAnswered":true}`,
			`{"autoApply":"when-idle","autoApplyAnswered":true}`,
		} {
			if rec := put(a, body); rec.Code != http.StatusForbidden {
				t.Fatalf("%v PUT %s = %d, want 403", kind, body, rec.Code)
			}
		}
		again, _ := newUpdateSettingsStore(dir)
		if !again.AutoApplyAsk() || again.Snapshot().AutoApply != updateAutoApplyOff {
			t.Fatalf("%v: a refused PUT changed %+v ask=%v", kind, again.Snapshot(), again.AutoApplyAsk())
		}
		// Reading stays open to every caller.
		rec := httptest.NewRecorder()
		a.handleUpdateSettings(rec, httptest.NewRequest(http.MethodGet, "/api/update/settings", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%v GET = %d, want 200", kind, rec.Code)
		}
	}

	dir := t.TempDir()
	existingInstall(t, dir)
	st, err := newUpdateSettingsStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{updateSettings: st}
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	if rec := put(a, `{"autoApply":"when-idle","autoApplyAnswered":true}`); rec.Code != http.StatusOK {
		t.Fatalf("person PUT = %d %s", rec.Code, rec.Body.String())
	}
	again, _ := newUpdateSettingsStore(dir)
	if again.Snapshot().AutoApply != updateAutoApplyWhenIdle || again.AutoApplyAsk() {
		t.Fatalf("person's answer not saved: %+v ask=%v", again.Snapshot(), again.AutoApplyAsk())
	}
}
