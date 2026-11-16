//go:build !windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func standInReleaseBuild(t *testing.T, release bool) {
	t.Helper()
	old := releaseBuild
	releaseBuild = func() bool { return release }
	t.Cleanup(func() { releaseBuild = old })
}

func standInCaller(t *testing.T, id callerIdentity) {
	t.Helper()
	old := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return id }
	t.Cleanup(func() { identifyCallerHook = old })
}

// In a release build apply-local is refused, with where to turn it on, until
// the person has Developer overrides on; a web page can never drive it.
func TestApplyLocalNeedsDeveloperOverridesInReleaseBuilds(t *testing.T) {
	serveUnpausedControl(t)
	standInReleaseBuild(t, true)
	data := t.TempDir()
	path, sum := writeLocalBuild(t, data, 0o755)
	gate := new(atomic.Bool)
	gate.Store(true) // a held restart reservation: a request that got through answers busy
	u := NewUpdater("0.0.1168", "", "", nil, nil)
	u.restartGate = gate
	a := &apiServer{updater: u, dataDir: data, devOverrides: newDeveloperOverrides(data), uiBus: newUIBus()}
	good := `{"path":"` + path + `","sha256":"` + sum + `","version":"0.0.1168.3"}`

	rec := applyLocalRequest(t, a, good, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Developer overrides") {
		t.Fatalf("apply-local with the setting off = %d %q", rec.Code, rec.Body.String())
	}

	if err := a.devOverrides.set(true, "settings"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*http.Request){
		"cross-site fetch":   func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site fetch":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"another origin":     func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"another local port": func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:3000") },
		"opaque origin":      func(r *http.Request) { r.Header.Set("Origin", "null") },
		"two origins": func(r *http.Request) {
			r.Header.Add("Origin", "http://127.0.0.1:8787")
			r.Header.Add("Origin", "https://evil.example")
		},
		"over the cloud relay": func(r *http.Request) { r.Header.Set("X-Forwarded-Prefix", "/m/mach_1") },
	} {
		if rec := applyLocalRequest(t, a, good, mutate); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	// The daemon's own page (same origin) and a script (no browser headers)
	// get through to the guarded updater.
	for name, mutate := range map[string]func(*http.Request){
		"script": nil,
		"own origin": func(r *http.Request) {
			r.Header.Set("Origin", "http://127.0.0.1:8787")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		},
		"direct load": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") },
	} {
		if rec := applyLocalRequest(t, a, good, mutate); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"busy":true`) {
			t.Errorf("%s with the setting on did not reach the updater: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	// A dev build keeps the developer paths without the setting.
	standInReleaseBuild(t, false)
	if err := a.devOverrides.set(false, ""); err != nil {
		t.Fatal(err)
	}
	if rec := applyLocalRequest(t, a, good, nil); rec.Code != http.StatusConflict {
		t.Fatalf("dev build apply-local = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeveloperOverridesSettingIsTheirsToTurnOn(t *testing.T) {
	standInReleaseBuild(t, true)
	data := t.TempDir()
	a := &apiServer{devOverrides: newDeveloperOverrides(data), uiBus: newUIBus()}
	put := func(body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/settings/developer-overrides", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:50123"
		r.Host = "127.0.0.1:8787"
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		a.handlePutDeveloperOverrides(w, r)
		return w
	}

	standInCaller(t, callerIdentity{Kind: callerPane, PaneID: "pane_agent"})
	if w := put(`{"on":true}`, nil); w.Code == http.StatusOK || a.devOverrides.on() {
		t.Fatalf("an agent turned developer overrides on: %d", w.Code)
	}
	standInCaller(t, callerIdentity{Kind: callerHuman})
	if w := put(`{"on":true}`, func(r *http.Request) { r.Header.Set("X-Forwarded-Prefix", "/m/mach_1") }); w.Code != http.StatusForbidden || a.devOverrides.on() {
		t.Fatalf("turned on over the relay: %d", w.Code)
	}
	for _, bad := range []string{`{}`, `{"on":"yes"}`, `{"on":true,"by":"x"}`} {
		if w := put(bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400", bad, w.Code)
		}
	}
	if w := put(`{"on":true}`, nil); w.Code != http.StatusOK || !a.devOverrides.on() {
		t.Fatalf("the person could not turn it on: %d %s", w.Code, w.Body.String())
	}
	// It survives a restart (and so an update).
	if !newDeveloperOverrides(data).on() {
		t.Fatal("the setting was not kept on disk")
	}
	// Anyone may turn it off.
	standInCaller(t, callerIdentity{Kind: callerPane, PaneID: "pane_agent"})
	if w := put(`{"on":false}`, nil); w.Code != http.StatusOK || a.devOverrides.on() {
		t.Fatalf("turning it off = %d", w.Code)
	}
}

func TestChannelSwitchTurnsOnDeveloperOverridesOnlyForThePerson(t *testing.T) {
	standInReleaseBuild(t, true)
	req := func(mutate func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/update/preview/install", nil)
		r.RemoteAddr = "127.0.0.1:50123"
		r.Host = "127.0.0.1:8787"
		if mutate != nil {
			mutate(r)
		}
		return r
	}
	for name, tc := range map[string]struct {
		id     callerIdentity
		mutate func(*http.Request)
		want   bool
	}{
		"person at the computer": {callerIdentity{Kind: callerHuman}, nil, true},
		"person over the relay":  {callerIdentity{Kind: callerHuman, Relay: true}, nil, false},
		"forwarded prefix":       {callerIdentity{Kind: callerHuman}, func(r *http.Request) { r.Header.Set("X-Forwarded-Prefix", "/m/mach_1") }, false},
		"agent in a pane":        {callerIdentity{Kind: callerPane, PaneID: "p"}, nil, false},
		"untraced program":       {callerIdentity{Kind: callerUnverified}, nil, false},
	} {
		a := &apiServer{devOverrides: newDeveloperOverrides(t.TempDir()), uiBus: newUIBus()}
		standInCaller(t, tc.id)
		_ = a.turnOnDeveloperOverridesForChannelSwitch(req(tc.mutate))
		if got := a.devOverrides.on(); got != tc.want {
			t.Errorf("%s: developer overrides on = %t, want %t", name, got, tc.want)
		}
	}
}

func TestDevWebMarkerNeedsDeveloperOverridesInReleaseBuilds(t *testing.T) {
	a := &apiServer{devOverrides: newDeveloperOverrides(t.TempDir())}
	serve := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	h := a.devWebGate(serve("dev"), serve("embedded"))
	get := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		return w.Body.String()
	}
	standInReleaseBuild(t, true)
	if got := get(); got != "embedded" {
		t.Fatalf("release build, setting off: served %s", got)
	}
	if err := a.devOverrides.set(true, "settings"); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "dev" {
		t.Fatalf("release build, setting on: served %s", got)
	}
	if err := a.devOverrides.set(false, ""); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "embedded" {
		t.Fatalf("setting turned off again: served %s", got)
	}
	standInReleaseBuild(t, false)
	if got := get(); got != "dev" {
		t.Fatalf("dev build: served %s", got)
	}
}

// A switch back to Stable turns developer overrides off once its install is
// accepted, but only when a channel switch turned them on.
func TestStableSwitchTurnsChannelOverridesBackOff(t *testing.T) {
	standInReleaseBuild(t, true)
	for name, tc := range map[string]struct {
		target, by string
		alive      bool
		wantOn     bool
	}{
		"stable, turned on by the channel switch":   {"stable", "channel", true, false},
		"stable, turned on in Settings":             {"stable", "settings", true, true},
		"preview, turned on by the channel switch":  {"preview", "channel", true, true},
		"stable job no longer running (stale file)": {"stable", "channel", false, true},
	} {
		data := t.TempDir()
		p := newPreviewChannel(data)
		p.alive = func(int) bool { return tc.alive }
		job, _ := json.Marshal(previewJob{Target: tc.target, PID: 4242})
		if err := os.WriteFile(p.jobPath(), job, 0o600); err != nil {
			t.Fatal(err)
		}
		a := &apiServer{preview: p, devOverrides: newDeveloperOverrides(data), uiBus: newUIBus()}
		if err := a.devOverrides.set(true, tc.by); err != nil {
			t.Fatal(err)
		}
		restore := a.closeChannelOverridesForStableInstall()
		if got := a.devOverrides.on(); got != tc.wantOn {
			t.Errorf("%s: on = %t, want %t", name, got, tc.wantOn)
		}
		if (restore != nil) != !tc.wantOn {
			t.Errorf("%s: restore = %v, want one only when it was cleared", name, restore != nil)
		}
		if restore != nil {
			restore()
			if !a.devOverrides.on() {
				t.Errorf("%s: restore did not turn it back on", name)
			}
		}
	}
}

// A switch back to Stable clears a channel-set override before the swap; when
// the install then fails, it is restored.
func TestStableInstallFailureRestoresChannelOverrides(t *testing.T) {
	serveUnpausedControl(t)
	standInReleaseBuild(t, true)
	data := t.TempDir()
	path, sum := writeLocalBuild(t, data, 0o755)
	gate := new(atomic.Bool)
	gate.Store(true) // held: ApplyLocal answers busy
	u := NewUpdater("0.0.1168", "", "", nil, nil)
	u.restartGate = gate
	p := newPreviewChannel(data)
	p.alive = func(int) bool { return true }
	job, _ := json.Marshal(previewJob{Target: "stable", PID: 4242})
	if err := os.WriteFile(p.jobPath(), job, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{updater: u, dataDir: data, preview: p, devOverrides: newDeveloperOverrides(data), uiBus: newUIBus()}
	if err := a.devOverrides.set(true, "channel"); err != nil {
		t.Fatal(err)
	}
	good := `{"path":"` + path + `","sha256":"` + sum + `","version":"0.0.1168.3"}`
	if rec := applyLocalRequest(t, a, good, nil); rec.Code != http.StatusConflict {
		t.Fatalf("install with a held gate = %d %s", rec.Code, rec.Body.String())
	}
	if !a.devOverrides.on() {
		t.Fatal("a failed Stable install left developer overrides off")
	}
}

// With developer overrides off in a release build, a channel switch from a
// caller that can't turn them on is refused before any build starts.
func TestChannelSwitchRefusedUpFrontWhenOverridesCantBeEnabled(t *testing.T) {
	standInReleaseBuild(t, true)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/update/preview/install", nil)
		r.RemoteAddr = "127.0.0.1:50123"
		r.Host = "127.0.0.1:8787"
		return r
	}
	for name, tc := range map[string]struct {
		id      callerIdentity
		on      bool
		refused bool
	}{
		"untraced program, overrides off": {callerIdentity{Kind: callerUnverified}, false, true},
		"agent in a pane, overrides off":  {callerIdentity{Kind: callerPane, PaneID: "p"}, false, true},
		"person over the relay, off":      {callerIdentity{Kind: callerHuman, Relay: true}, false, true},
		"person at the computer, off":     {callerIdentity{Kind: callerHuman}, false, false},
		"untraced program, overrides on":  {callerIdentity{Kind: callerUnverified}, true, false},
	} {
		a := &apiServer{devOverrides: newDeveloperOverrides(t.TempDir())}
		if tc.on {
			if err := a.devOverrides.set(true, "settings"); err != nil {
				t.Fatal(err)
			}
		}
		standInCaller(t, tc.id)
		if got := a.channelSwitchWouldBeRefused(req()); got != tc.refused {
			t.Errorf("%s: refused = %t, want %t", name, got, tc.refused)
		}
	}
	// A dev build never refuses.
	standInReleaseBuild(t, false)
	a := &apiServer{devOverrides: newDeveloperOverrides(t.TempDir())}
	standInCaller(t, callerIdentity{Kind: callerUnverified})
	if a.channelSwitchWouldBeRefused(req()) {
		t.Fatal("dev build refused a channel switch")
	}
}
