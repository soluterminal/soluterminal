package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Developer overrides: in a release build, the developer paths that let local
// files change what the daemon runs or serves are off until the person turns
// this on at the computer itself:
//   - the dev-web marker files and SOLU_WEB_DEV_DIR (the -dev-web flag stays:
//     only the launcher's own arguments can set it);
//   - POST /api/update/apply-local (follow-main's local installs).
//
// A dev build (version "dev") keeps both, as before. Turning it on needs a
// person (requireHuman) and is refused over the relay; anyone may turn it
// off. Switching the update channel (Preview or back to Stable) is the
// person's act too, and turns it on for them, since both run follow-main.
// The record survives updates; while it is on, Settings and the status bar
// show it.

const (
	developerOverridesFile           = "developer-overrides.json"
	developerOverridesMaxRequestBody = 1 << 10
)

type developerOverridesRecord struct {
	On        bool      `json:"on"`
	By        string    `json:"by,omitempty"` // "settings" | "channel"
	UpdatedAt time.Time `json:"updatedAt"`
}

type developerOverrides struct {
	mu     sync.Mutex
	path   string
	record developerOverridesRecord
}

func newDeveloperOverrides(dataDir string) *developerOverrides {
	s := &developerOverrides{path: filepath.Join(dataDir, developerOverridesFile)}
	if b, err := os.ReadFile(s.path); err == nil {
		var rec developerOverridesRecord
		if json.Unmarshal(b, &rec) == nil {
			s.record = rec
		}
	}
	return s
}

func (s *developerOverrides) on() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record.On
}

func (s *developerOverrides) set(on bool, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setLocked(on, by)
}

// clearIfBy turns the setting off only if it is on and was turned on by by,
// checked and changed under one lock so a Settings turn-on in between wins.
func (s *developerOverrides) clearIfBy(by string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.record.On || s.record.By != by {
		return false, nil
	}
	return true, s.setLocked(false, "")
}

func (s *developerOverrides) setLocked(on bool, by string) error {
	rec := developerOverridesRecord{On: on, By: by, UpdatedAt: time.Now().UTC()}
	if !on {
		rec.By = ""
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return err
	}
	s.record = rec
	return nil
}

// releaseBuild: anything but a dev build. A variable so tests can stand in a
// release build.
var releaseBuild = func() bool { return version != "dev" }

// developerOverridesAllowed reports whether the developer paths may be used
// now: always in a dev build, and in a release build only while the person
// has the setting on.
func (a *apiServer) developerOverridesAllowed() bool {
	return !releaseBuild() || a.devOverrides.on()
}

type developerOverridesResponse struct {
	On bool `json:"on"`
	// Required: this is a release build, where the setting gates the paths.
	Required bool   `json:"required"`
	By       string `json:"by,omitempty"`
}

func (a *apiServer) developerOverridesResponse() developerOverridesResponse {
	resp := developerOverridesResponse{Required: releaseBuild()}
	if a.devOverrides != nil {
		a.devOverrides.mu.Lock()
		resp.On, resp.By = a.devOverrides.record.On, a.devOverrides.record.By
		a.devOverrides.mu.Unlock()
	}
	return resp
}

func (a *apiServer) handleGetDeveloperOverrides(w http.ResponseWriter, _ *http.Request) {
	if a.devOverrides == nil {
		http.Error(w, "developer overrides are unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.developerOverridesResponse())
}

// handlePutDeveloperOverrides turns the setting on or off. On is a person's
// act at the computer itself: never over the relay, never by an agent.
func (a *apiServer) handlePutDeveloperOverrides(w http.ResponseWriter, r *http.Request) {
	if a.devOverrides == nil {
		http.Error(w, "developer overrides are unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		On *bool `json:"on"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, developerOverridesMaxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.On == nil {
		http.Error(w, `body must set "on" to true or false`, http.StatusBadRequest)
		return
	}
	if *body.On {
		if requestViaRelay(r) || fileAccessOverRelay(r) {
			http.Error(w, "developer overrides can only be turned on at the computer itself", http.StatusForbidden)
			return
		}
		if !a.requireHuman(w, r, "turn on developer overrides") {
			return
		}
	}
	if err := a.devOverrides.set(*body.On, "settings"); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	log.Printf("developer overrides: set on=%t from Settings", *body.On)
	a.publishDeveloperOverrides()
	writeJSON(w, http.StatusOK, a.developerOverridesResponse())
}

// turnOnDeveloperOverridesForChannelSwitch: the person switching the update
// channel at this computer also turns the setting on, since follow-main
// installs through apply-local. Only a person's own local request counts;
// the switch itself also admits the relay and tokenless loopback programs.
// It reports whether this call turned the setting on, so a switch that then
// fails to start can turn it back off (undoChannelOverrides).
func (a *apiServer) turnOnDeveloperOverridesForChannelSwitch(r *http.Request) bool {
	if a.devOverrides == nil || !releaseBuild() || a.devOverrides.on() {
		return false
	}
	id, ok := a.channelSwitchByPerson(r)
	if !ok {
		return false
	}
	if err := a.devOverrides.set(true, "channel"); err != nil {
		log.Printf("developer overrides: could not turn on for the channel switch: %v", err)
		return false
	}
	a.logPersonAct(id, "turn on developer overrides (update channel switch)")
	a.publishDeveloperOverrides()
	return true
}

// undoChannelOverrides turns the setting back off after a channel switch that
// turned it on failed to start, unless the person has since set it.
func (a *apiServer) undoChannelOverrides() {
	if cleared, err := a.devOverrides.clearIfBy("channel"); err == nil && cleared {
		a.publishDeveloperOverrides()
	}
}

// closeChannelOverridesForStableInstall: a switch back to Stable turns the
// setting back off if a channel switch is what turned it on (By "channel"),
// so someone who tried Preview and returned doesn't keep developer overrides
// on for good. It runs before the swap, so the successor daemon starts with
// it off; the returned func restores it if the swap then fails (nil when
// nothing changed). A Preview install leaves it on, and so does a setting the
// person turned on in Settings.
func (a *apiServer) closeChannelOverridesForStableInstall() (restore func()) {
	if a.devOverrides == nil || a.preview == nil {
		return nil
	}
	job, ok := a.preview.loadJob()
	if !ok || job.Target != "stable" || a.preview.alive == nil || !a.preview.alive(job.PID) {
		return nil
	}
	cleared, err := a.devOverrides.clearIfBy("channel")
	if err != nil {
		log.Printf("developer overrides: could not turn off for the switch back to Stable: %v", err)
		return nil
	}
	if !cleared {
		return nil
	}
	log.Printf("developer overrides: off for the switch back to Stable (the channel switch had turned them on)")
	a.publishDeveloperOverrides()
	return func() {
		if err := a.devOverrides.set(true, "channel"); err != nil {
			log.Printf("developer overrides: could not restore after a failed install: %v", err)
			return
		}
		a.publishDeveloperOverrides()
	}
}

// channelSwitchByPerson: r is the person's own request at this computer, so
// it may turn developer overrides on. The relay, a forwarded request and any
// program are not.
func (a *apiServer) channelSwitchByPerson(r *http.Request) (callerIdentity, bool) {
	if requestViaRelay(r) || fileAccessOverRelay(r) {
		return callerIdentity{}, false
	}
	id := a.identifyCaller(r)
	return id, id.Kind == callerHuman && !id.Relay && !id.Forwarded
}

// channelSwitchWouldBeRefused: in a release build with developer overrides
// off, a switch from a caller that can't turn them on would build for minutes
// and then be refused at install, so it is refused up front.
func (a *apiServer) channelSwitchWouldBeRefused(r *http.Request) bool {
	if a.devOverrides == nil || !releaseBuild() || a.devOverrides.on() {
		return false
	}
	_, ok := a.channelSwitchByPerson(r)
	return !ok
}

func (a *apiServer) publishDeveloperOverrides() {
	if a.uiBus == nil {
		return
	}
	a.uiBus.publish(uiEvent{Type: "developer-overrides", Ts: time.Now().UnixMilli()})
}

// devWebGate serves dev (a dev-web marker or SOLU_WEB_DEV_DIR) while
// developer overrides allow it, and the embedded dist otherwise.
func (a *apiServer) devWebGate(dev, embedded http.Handler) http.Handler {
	var warned sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.developerOverridesAllowed() {
			dev.ServeHTTP(w, r)
			return
		}
		warned.Do(func() {
			log.Printf("web assets: dev-web override ignored: developer overrides are off in this release build; serving the embedded dist")
		})
		embedded.ServeHTTP(w, r)
	})
}

// crossSiteBrowserRequest: a browser request from any page but the daemon's
// own. Sec-Fetch-Site same-site or cross-site, or an Origin other than the
// request's own host, means a web page sent it.
func crossSiteBrowserRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return false
	}
	if len(origins) > 1 {
		return true
	}
	u, err := url.Parse(origins[0])
	return err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host)
}
