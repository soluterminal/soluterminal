package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Privacy settings for the two uploads that leave a paired machine besides
// the anonymous check-in (install_checkin.go):
//
//   - Usage statistics: per-turn Claude Code / Codex usage rows (model, tokens,
//     working folder, git remote and branch, session id) uploaded by the
//     cc-usage and codex-usage scanners. Off by default for the public; team
//     installs keep today's behaviour (on) unless they turn it off.
//   - Crash reports: redacted panic reports. On by default; can be switched off.
//
// Both settings are changed on the computer itself only (PUT is local-only),
// like the check-in. SOLU_DISABLE_USAGE_UPLOAD=1 and SOLU_DISABLE_CRASH_REPORTS=1
// in ~/.solu/env force them off.

const (
	privacySettingsFile           = "privacy.json"
	usageUploadDisabledEnv        = "SOLU_DISABLE_USAGE_UPLOAD"
	crashReportsDisabledEnv       = "SOLU_DISABLE_CRASH_REPORTS"
	privacySettingsMaxRequestBody = 1 << 10
)

// privacySettingsRecord is privacy.json. A nil field means "never chosen", so
// the tier default applies.
type privacySettingsRecord struct {
	UsageUpload  *bool `json:"usageUpload,omitempty"`
	CrashReports *bool `json:"crashReports,omitempty"`
}

type privacySettings struct {
	mu                sync.Mutex
	toggleMu          sync.Mutex
	usageMu           sync.Mutex
	usageParticipants map[*usagePrivacyParticipant]struct{}
	path              string
	record            privacySettingsRecord
	// team reports whether this install has team features. It decides the
	// usage default and whether crash reports carry the recent log tail.
	team func() bool
}

func newPrivacySettings(dataDir string, team func() bool) *privacySettings {
	p := &privacySettings{path: filepath.Join(dataDir, privacySettingsFile), team: team}
	if b, err := os.ReadFile(p.path); err == nil {
		var rec privacySettingsRecord
		if json.Unmarshal(b, &rec) == nil {
			p.record = rec
		}
	}
	return p
}

func envFlagOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (p *privacySettings) isTeam() bool {
	return p != nil && p.team != nil && p.team()
}

// usageUploadAllowed: an explicit choice wins; otherwise team installs upload
// and everyone else does not.
func (p *privacySettings) usageUploadAllowed() bool {
	if envFlagOn(usageUploadDisabledEnv) || p == nil {
		return false
	}
	p.mu.Lock()
	choice := p.record.UsageUpload
	p.mu.Unlock()
	if choice != nil {
		return *choice
	}
	return p.isTeam()
}

func (p *privacySettings) crashReportsAllowed() bool {
	if envFlagOn(crashReportsDisabledEnv) {
		return false
	}
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.record.CrashReports == nil || *p.record.CrashReports
}

func (p *privacySettings) update(usage, crash *bool) error {
	if usage == nil {
		return p.persistUpdate(usage, crash)
	}
	return p.updateUsage(usage, crash)
}

func (p *privacySettings) persistUpdate(usage, crash *bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.record
	if usage != nil {
		v := *usage
		p.record.UsageUpload = &v
	}
	if crash != nil {
		v := *crash
		p.record.CrashReports = &v
	}
	b, err := json.Marshal(p.record)
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(p.path, b, 0o600); err != nil {
		p.record = previous
		return err
	}
	return nil
}

// activePrivacy is read by the crash sink and the usage scanners, which run
// outside the API server.
var activePrivacy atomic.Pointer[privacySettings]

func usageUploadAllowedNow() bool {
	return activePrivacy.Load().usageUploadAllowed()
}

func crashReportsAllowedNow() bool {
	return activePrivacy.Load().crashReportsAllowed()
}

// crashLogTailAllowedNow: the recent log tail can mention project folders and
// pane titles, so only team installs attach it.
func crashLogTailAllowedNow() bool {
	return activePrivacy.Load().isTeam()
}

type privacySettingsResponse struct {
	UsageUpload            bool `json:"usageUpload"`
	UsageUploadEnvDisabled bool `json:"usageUploadEnvDisabled"`
	CrashReports           bool `json:"crashReports"`
	CrashEnvDisabled       bool `json:"crashReportsEnvDisabled"`
}

func (a *apiServer) privacySnapshot() privacySettingsResponse {
	return privacySettingsResponse{
		UsageUpload:            a.privacy.usageUploadAllowed(),
		UsageUploadEnvDisabled: envFlagOn(usageUploadDisabledEnv),
		CrashReports:           a.privacy.crashReportsAllowed(),
		CrashEnvDisabled:       envFlagOn(crashReportsDisabledEnv),
	}
}

func (a *apiServer) handleGetPrivacySettings(w http.ResponseWriter, _ *http.Request) {
	if a.privacy == nil {
		http.Error(w, "privacy settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.privacySnapshot())
}

// handlePutPrivacySettings is LOCAL-ONLY, like the check-in setting: privacy
// choices are made on the computer itself, never over the cloud relay.
func (a *apiServer) handlePutPrivacySettings(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "this setting can only be changed on the computer itself", http.StatusForbidden)
		return
	}
	if a.privacy == nil {
		http.Error(w, "privacy settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		UsageUpload  *bool `json:"usageUpload"`
		CrashReports *bool `json:"crashReports"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, privacySettingsMaxRequestBody)).Decode(&body); err != nil ||
		(body.UsageUpload == nil && body.CrashReports == nil) {
		http.Error(w, `body must set "usageUpload" and/or "crashReports" to true or false`, http.StatusBadRequest)
		return
	}
	if err := a.privacy.update(body.UsageUpload, body.CrashReports); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.privacySnapshot())
}
