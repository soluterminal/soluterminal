package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Anonymous daily install check-in. Once a day the daemon tells the cloud
// (POST /api/checkin) that a random install id is alive, with its version, OS
// family, whether it is signed in, and two yes/no activation flags (a Claude
// Code or Codex pane was started in the last 24 h; one pane's agent used
// SoluTerminal's tools on another pane in the last 24 h, see
// install_checkin_activity.go) — nothing else. The id
// (<dataDir>/checkin_install_id) is minted here
// and never tied to an account, machine or IP. The user can turn it off in
// Settings (PUT /api/settings/checkin, local-only) or with
// SOLU_DISABLE_CHECKIN=1 in ~/.solu/env. Development builds (version "dev")
// never send.

const (
	checkinInstallIDFile = "checkin_install_id"
	checkinSettingsFile  = "checkin.json"
	checkinFirstDelay    = time.Minute
	checkinFirstJitter   = 30 * time.Second
	checkinPeriodMin     = 23 * time.Hour
	checkinPeriodJitter  = 2 * time.Hour
	checkinHTTPTimeout   = 10 * time.Second
	checkinDisabledEnv   = "SOLU_DISABLE_CHECKIN"
	checkinUTCDateLayout = "2006-01-02"
)

var checkinInstallIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// checkinSettings is checkin.json: the user's opt-out plus the last day a
// check-in was accepted, so restarts on the same day do not resend.
type checkinSettings struct {
	Enabled       *bool  `json:"enabled,omitempty"`
	LastSentDay   string `json:"lastSentDay,omitempty"`
	LastSignature string `json:"lastSignature,omitempty"`
	// When the two activation flags were last true, in Unix seconds, kept
	// locally so a daemon restart doesn't forget the day's activity. Only the
	// yes/no derived from them is ever sent.
	AgentPaneAt      int64 `json:"agentPaneAt,omitempty"`
	AgentsTogetherAt int64 `json:"agentsTogetherAt,omitempty"`
}

type checkinPayload struct {
	InstallID string `json:"installId"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	SignedIn  bool   `json:"signedIn"`
	// Older clouds ignore these two; the current cloud treats them as optional.
	AgentPaneDay      bool `json:"agentPaneDay"`
	AgentsTogetherDay bool `json:"agentsTogetherDay"`
}

type installCheckin struct {
	mu       sync.Mutex
	dataDir  string
	settings checkinSettings
	version  string

	base     func() string
	signedIn func() bool
	client   *http.Client
	now      func() time.Time
}

func newInstallCheckin(dataDir, version string, base func() string, signedIn func() bool) *installCheckin {
	requireNormalBootEngine("install_checkin.newInstallCheckin")
	c := &installCheckin{
		dataDir:  dataDir,
		version:  version,
		base:     base,
		signedIn: signedIn,
		client:   &http.Client{Timeout: checkinHTTPTimeout},
		now:      time.Now,
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, checkinSettingsFile)); err == nil {
		var s checkinSettings
		if json.Unmarshal(b, &s) == nil {
			c.settings = s
		}
	}
	return c
}

func checkinEnvDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(checkinDisabledEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// userEnabled is the persisted setting (default on).
func (c *installCheckin) userEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.Enabled == nil || *c.settings.Enabled
}

func (c *installCheckin) enabled() bool {
	return !checkinEnvDisabled() && c.userEnabled()
}

func (c *installCheckin) persistLocked() error {
	b, err := json.Marshal(c.settings)
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(filepath.Join(c.dataDir, checkinSettingsFile), b, 0o600)
}

func (c *installCheckin) setEnabled(on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings.Enabled = &on
	return c.persistLocked()
}

// loadOrCreateCheckinInstallID returns the persisted random check-in id,
// minting a fresh UUID v4 when the file is missing or malformed. It is
// deliberately separate from installid.go's install-id (updater cohort and
// browser-window ownership) so check-ins cannot be joined to anything else.
func loadOrCreateCheckinInstallID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, checkinInstallIDFile)
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.ToLower(strings.TrimSpace(string(b))); checkinInstallIDPattern.MatchString(id) {
			return id, nil
		}
	}
	id, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomicSynced(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func checkinOSFamily(goos string) string {
	switch goos {
	case "windows", "linux", "darwin":
		return goos
	}
	return "other"
}

func (c *installCheckin) payload() (checkinPayload, error) {
	id, err := loadOrCreateCheckinInstallID(c.dataDir)
	if err != nil {
		return checkinPayload{}, err
	}
	signedIn := false
	if c.signedIn != nil {
		signedIn = c.signedIn()
	}
	agentPane, together := c.activityFlags()
	return checkinPayload{
		InstallID:         id,
		Version:           c.version,
		OS:                checkinOSFamily(runtime.GOOS),
		SignedIn:          signedIn,
		AgentPaneDay:      agentPane,
		AgentsTogetherDay: together,
	}, nil
}

var errCheckinRejected = errors.New("checkin: cloud rejected the check-in")

// sendOnce posts today's check-in unless it is off, a development build, or
// an identical check-in was already accepted today. It reports whether a
// request was sent.
func (c *installCheckin) sendOnce(ctx context.Context) (bool, error) {
	if !c.enabled() || c.version == "" || c.version == "dev" {
		return false, nil
	}
	p, err := c.payload()
	if err != nil {
		return false, err
	}
	day := c.now().UTC().Format(checkinUTCDateLayout)
	// A flag turning on later the same day changes the signature, so it is
	// reported; the cloud keeps each flag sticky within the day.
	yesNo := map[bool]string{true: "1", false: "0"}
	signature := p.Version + "|" + p.OS + "|" + map[bool]string{true: "in", false: "out"}[p.SignedIn] +
		"|" + yesNo[p.AgentPaneDay] + yesNo[p.AgentsTogetherDay]
	c.mu.Lock()
	already := c.settings.LastSentDay == day && c.settings.LastSignature == signature
	c.mu.Unlock()
	if already {
		return false, nil
	}
	body, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	base := defaultCloudHTTPBase
	if c.base != nil {
		if b := strings.TrimRight(c.base(), "/"); b != "" {
			base = b
		}
	}
	if base == "" {
		return false, nil // no cloud configured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/checkin", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return true, err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// 404 is an older cloud without the endpoint; nothing to retry today.
		return true, errCheckinRejected
	}
	c.mu.Lock()
	c.settings.LastSentDay = day
	c.settings.LastSignature = signature
	err = c.persistLocked()
	c.mu.Unlock()
	return true, err
}

func checkinJitter(max time.Duration) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}

// run sends the first check-in about a minute after boot, then roughly once a
// day. It never blocks boot and gives up silently on errors until the next
// round.
func (c *installCheckin) run(ctx context.Context) {
	requireNormalBootEngine("install_checkin.run")
	if c == nil {
		return
	}
	wait := checkinFirstDelay + checkinJitter(checkinFirstJitter)
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		sendCtx, cancel := context.WithTimeout(ctx, 2*checkinHTTPTimeout)
		if _, err := c.sendOnce(sendCtx); err != nil {
			log.Printf("checkin: %v", err)
		}
		cancel()
		wait = checkinPeriodMin + checkinJitter(checkinPeriodJitter)
	}
}

// checkinCloudBase prefers the paired cloud, else SOLU_CLOUD_URL, else the
// production cloud.
func (a *apiServer) checkinCloudBase() string {
	if base, _ := a.cloudCreds(); base != "" {
		return base
	}
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("SOLU_CLOUD_URL")), "/"); v != "" {
		return v
	}
	return defaultCloudHTTPBase
}

// checkinSignedIn: the install is paired with the cloud, or the last cloud
// verdict says it belongs to an account.
func (a *apiServer) checkinSignedIn() bool {
	if _, token := a.cloudCreds(); token != "" {
		return true
	}
	return a.entitlements.cachedTier() != tierNone
}

type checkinSettingsResponse struct {
	Enabled     bool `json:"enabled"`
	EnvDisabled bool `json:"envDisabled"`
}

func (a *apiServer) checkinSettingsSnapshot() checkinSettingsResponse {
	return checkinSettingsResponse{
		Enabled:     a.checkin.userEnabled() && !checkinEnvDisabled(),
		EnvDisabled: checkinEnvDisabled(),
	}
}

func (a *apiServer) handleGetCheckinSettings(w http.ResponseWriter, _ *http.Request) {
	if a.checkin == nil {
		http.Error(w, "check-in is unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.checkinSettingsSnapshot())
}

// handlePutCheckinSettings is LOCAL-ONLY: a privacy setting is changed from
// the machine itself, never over the cloud relay.
func (a *apiServer) handlePutCheckinSettings(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "this setting can only be changed on the computer itself", http.StatusForbidden)
		return
	}
	if a.checkin == nil {
		http.Error(w, "check-in is unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Enabled == nil {
		http.Error(w, `body must be {"enabled": true|false}`, http.StatusBadRequest)
		return
	}
	if err := a.checkin.setEnabled(*body.Enabled); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.checkinSettingsSnapshot())
}
