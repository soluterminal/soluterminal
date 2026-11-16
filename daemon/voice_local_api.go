package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"soluterminal-shared/voiceclean"
)

// Where dictation is cleaned (ST-181, @secure's ruling of 14 Nov):
//   - "Clean up dictation with my Claude plan" (Enabled): only when the person
//     ticked it. Unset means off; it used to mean on.
//   - "Use my Anthropic API key" (APIKey): only when the person picked it in
//     Settings. An exported ANTHROPIC_API_KEY alone is never used: env keys are
//     often set for other tools, and using one silently both bills it and
//     changes where the dictation goes.
//   - Otherwise SoluTerminal's cloud.
//
// Stored in ~/.solu/voice-local.json.

const voiceLocalSettingsFile = "voice-local.json"

type voiceLocalSettings struct {
	mu      sync.Mutex
	path    string
	Enabled *bool `json:"enabled,omitempty"`
	APIKey  *bool `json:"apiKey,omitempty"`
	// NoticeSeen: the one-time "clean-up now runs in the cloud by default"
	// notice was shown for this computer.
	NoticeSeen bool `json:"noticeSeen,omitempty"`
}

func newVoiceLocalSettings(dataDir string) *voiceLocalSettings {
	s := &voiceLocalSettings{path: filepath.Join(dataDir, voiceLocalSettingsFile)}
	if b, err := os.ReadFile(s.path); err == nil {
		var rec voiceLocalSettings
		if json.Unmarshal(b, &rec) == nil {
			s.Enabled, s.APIKey, s.NoticeSeen = rec.Enabled, rec.APIKey, rec.NoticeSeen
		}
	}
	return s
}

// enabled reports the person's explicit opt-in to clean-up on their Claude
// plan. Unset means off: the plan is used only when they chose it (ST-181).
func (s *voiceLocalSettings) enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Enabled != nil && *s.Enabled
}

// apiKeyChosen reports the person's explicit choice of their own API key.
func (s *voiceLocalSettings) apiKeyChosen() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.APIKey != nil && *s.APIKey
}

// planChoiceSet reports whether the person ever chose either way on the
// Claude-plan setting (before ST-181 an unset choice meant on).
func (s *voiceLocalSettings) planChoiceSet() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Enabled != nil
}

func (s *voiceLocalSettings) noticeSeen() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NoticeSeen
}

// Which login a local clean-up runs on.
const (
	voiceCleanupPlan   = "plan"    // the person's Claude plan, by their opt-in
	voiceCleanupAPIKey = "api_key" // the person's ANTHROPIC_API_KEY, by their choice
	voiceCleanupCloud  = "cloud"   // no local run: SoluTerminal's cloud service
)

// voiceAPIKeyPresent reports whether the daemon has an ANTHROPIC_API_KEY to
// give the clean-up run. Only presence is read. A seam for tests.
var voiceAPIKeyPresent = func() bool { return strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" }

// localCleanupMode is where a dictation would be cleaned: on the plan when
// the person opted in, on their API key when they chose it and it is still
// exported, else the cloud.
func (a *apiServer) localCleanupMode() string {
	if a.voiceLocal().enabled() {
		return voiceCleanupPlan
	}
	if a.voiceLocal().apiKeyChosen() && voiceAPIKeyPresent() {
		return voiceCleanupAPIKey
	}
	return voiceCleanupCloud
}

// set records the Claude-plan choice. Turning it on turns the API-key choice
// off: the two are alternatives.
func (s *voiceLocalSettings) set(on bool) error {
	return s.update(&on, nil, false)
}

// update records any of the three fields given (nil leaves a choice as it
// is) and saves. The plan and the API key exclude each other: choosing one
// clears the other.
func (s *voiceLocalSettings) update(plan, apiKey *bool, noticeSeen bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	off := false
	if plan != nil {
		v := *plan
		s.Enabled = &v
		if v {
			s.APIKey = &off
		}
	}
	if apiKey != nil {
		v := *apiKey
		s.APIKey = &v
		if v {
			s.Enabled = &off
		}
	}
	if noticeSeen {
		s.NoticeSeen = true
	}
	b, err := json.Marshal(struct {
		Enabled    *bool `json:"enabled,omitempty"`
		APIKey     *bool `json:"apiKey,omitempty"`
		NoticeSeen bool  `json:"noticeSeen,omitempty"`
	}{s.Enabled, s.APIKey, s.NoticeSeen})
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, b, 0o600)
}

// localCleanupAvailable reports whether a dictation would be cleaned on the
// user's Claude login, and if not, why (shown in Settings).
func (a *apiServer) localCleanupAvailable() (bool, string) {
	if a.localCleaner() == nil || a.voiceLocal() == nil {
		return false, "unavailable"
	}
	mode := a.localCleanupMode()
	if mode == voiceCleanupCloud {
		return false, "off"
	}
	// Before the first Claude probe finishes we simply try: a failed run
	// falls back to the cloud like any other failure. The API-key run needs
	// Claude Code but not a Claude login.
	if st := a.currentClaudeStatus(); st.checked {
		if !st.installed {
			return false, "not_installed"
		}
		// The plan path needs the plan login, not just "signed in": it strips
		// ANTHROPIC_* and must never bill an API key, including one Claude
		// stored itself (ST-97). The API-key path bills only the key the
		// person chose (ST-181).
		if mode == voiceCleanupPlan && !st.planLogin {
			return false, "not_signed_in"
		}
	}
	if a.localCleaner().paused() {
		return false, "paused"
	}
	return true, ""
}

type voiceLocalResponse struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model"`
	// Profiles lists the cleanup modes /api/voice/cleanup understands, so a
	// prose client (Shinoque) can tell this daemon won't use the terminal
	// prompt on its text.
	Profiles []string `json:"profiles"`
	// WritingContext: a writing cleanup's context (the open conversation)
	// is used only on the user's own Claude login and dropped before any
	// cloud fallback. Clients send a conversation only when this is true;
	// older daemons would forward it to the cloud fallback unchanged.
	WritingContext bool `json:"writingContext"`
	// Using is where clean-up runs now: "plan", "api_key" or "cloud" (ST-181).
	// "plan" or "api_key" may still fall back to the cloud when Available is
	// false or a run fails.
	Using string `json:"using"`
	// APIKey: an ANTHROPIC_API_KEY is exported to SoluTerminal (presence only),
	// so Settings can offer it. APIKeyChosen: the person picked it.
	APIKey       bool `json:"apiKey"`
	APIKeyChosen bool `json:"apiKeyChosen"`
	// PlanChoiceSet: the person chose either way on the plan setting. With it
	// unset and NoticeSeen false, a client that saw plan clean-ups before the
	// update shows the one-time "now runs in the cloud" notice.
	PlanChoiceSet bool `json:"planChoiceSet"`
	NoticeSeen    bool `json:"noticeSeen"`
	// MinWritingBytes is the shortest dictation the writing profile tidies.
	MinWritingBytes int `json:"minWritingBytes"`
	// CloudOnly: this daemon honours "cloudOnly": true on /api/voice/cleanup
	// and /api/voice/prewarm (and ?cloudOnly=1 on /api/transcribe): such a
	// request never runs on the person's Claude login (ST-180). Shinoque asks
	// only a daemon that says this.
	CloudOnly bool `json:"cloudOnly"`
}

func (a *apiServer) voiceLocalSnapshot() voiceLocalResponse {
	ok, reason := a.localCleanupAvailable()
	return voiceLocalResponse{
		Enabled:   a.voiceLocal().enabled(),
		Available: ok,
		Reason:    reason,
		Model:     localCleanupModel,
		Using:     a.localCleanupMode(),
		APIKey:    voiceAPIKeyPresent(),

		APIKeyChosen:  a.voiceLocal().apiKeyChosen(),
		PlanChoiceSet: a.voiceLocal().planChoiceSet(),
		NoticeSeen:    a.voiceLocal().noticeSeen(),
		Profiles:      []string{string(voiceclean.ProfileCode), string(voiceclean.ProfileWriting)},

		WritingContext:  true,
		MinWritingBytes: voiceclean.MinWritingCleanupBytes,
		CloudOnly:       true,
	}
}

func (a *apiServer) handleGetVoiceLocal(w http.ResponseWriter, _ *http.Request) {
	if a.voiceLocal() == nil {
		http.Error(w, "voice settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.voiceLocalSnapshot())
}

func (a *apiServer) handlePutVoiceLocal(w http.ResponseWriter, r *http.Request) {
	if a.voiceLocal() == nil {
		http.Error(w, "voice settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled    *bool `json:"enabled"`
		APIKey     *bool `json:"apiKey"`
		NoticeSeen bool  `json:"noticeSeen"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil ||
		(body.Enabled == nil && body.APIKey == nil && !body.NoticeSeen) {
		http.Error(w, `body must set "enabled" or "apiKey" to true or false, or "noticeSeen" to true`, http.StatusBadRequest)
		return
	}
	if body.Enabled != nil && body.APIKey != nil && *body.Enabled && *body.APIKey {
		http.Error(w, `choose the Claude plan or the API key, not both`, http.StatusBadRequest)
		return
	}
	// The plan and API-key choices decide whose account is billed and where
	// dictation goes, so only the person at this computer makes them: never a
	// relayed device, never an agent (@secure, ST-181). Marking the notice
	// seen is open to any caller that can reach this route.
	if body.Enabled != nil || body.APIKey != nil {
		if requestViaRelay(r) || privateRelayDevice(r) != "" {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "voice_cleanup_choice_local_only",
				"message": "Choose where dictation is cleaned on the computer itself, in Settings › Voice.",
			})
			return
		}
		if !a.requireHuman(w, r, "change where dictation is cleaned") {
			return
		}
	}
	if err := a.voiceLocal().update(body.Enabled, body.APIKey, body.NoticeSeen); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.voiceLocalSnapshot())
}

// handleVoicePrewarm starts a Claude process while the user is still
// talking, so the cleanup that follows skips the start-up time.
func (a *apiServer) handleVoicePrewarm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Context   bool   `json:"context"`
		Mode      string `json:"mode"`
		CloudOnly bool   `json:"cloudOnly"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
	// A cloud-only caller (ST-180) never starts the person's Claude login.
	if ok, _ := a.localCleanupAvailable(); ok && !body.CloudOnly && !a.voiceForcedCloudOnly(r) {
		v := cleanVariant{profile: voiceclean.ParseProfile(body.Mode), withCtx: body.Context}
		go a.localCleaner().Prewarm(v)
	}
	w.WriteHeader(http.StatusNoContent)
}
