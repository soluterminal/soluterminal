package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Device proof for callers the daemon can't trace.
//
// caller_authority.go knows who is calling when it can trace the connection
// to a process. When it can't (a Windows program reaching a WSL daemon,
// WSL's localhost relay, the desktop app's Dual-mode router), only
// what the request says about itself is left, and browser fetch headers are
// easy to imitate. So an untraceable caller counts as the person when its
// request is signed by a device the person approved (trusted_devices.go):
// the browser or desktop window keeps a non-extractable key and signs for a
// target naming this daemon, "local/<daemon key id>".
//
// The mode decides what happens to an untraceable caller that only has the
// browser headers: "off" accepts it as before, "report" accepts it and logs
// the act, "require" refuses it. Require is only switched on from an
// approved device, needs at least one approved device to take effect, and
// SOLU_HUMAN_PROOF=off at daemon start turns it off whatever the file says,
// so the person can always get back in.
//
// This raises the bar from "send a header" to stealing a browser's key
// material or driving the person's own window. It is not a sandbox: a program
// that is already hostile and runs as the same OS user can still edit this
// daemon's files, as caller_authority.go says.

const (
	humanProofFile       = "human-proof.json"
	humanProofOff        = "off"
	humanProofReport     = "report"
	humanProofRequire    = "require"
	humanProofEnv        = "SOLU_HUMAN_PROOF"
	humanProofMaxBody    = 1 << 20
	humanProofHeader     = "X-Solu-Human-Proof"
	localDeviceTarget    = "local/"
	humanProofMaxRequest = 1 << 12
)

func humanProofRank(mode string) int {
	switch mode {
	case humanProofReport:
		return 1
	case humanProofRequire:
		return 2
	}
	return 0
}

func validHumanProofMode(mode string) bool {
	return mode == humanProofOff || mode == humanProofReport || mode == humanProofRequire
}

// humanProofRecord is human-proof.json.
type humanProofRecord struct {
	Mode string `json:"mode"`
	// Confirmed is the mode last set in the app. A Mode below it was lowered
	// by editing the file, which is announced. (Editing both is a same-user
	// file edit this cannot see.)
	Confirmed string    `json:"confirmed,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Notices are devices approved without an approved device's proof (the
	// computer's first device, or one approved while SOLU_HUMAN_PROOF=off),
	// and removals made by such a device or one it approved. They are shown
	// on every screen until acknowledged (acknowledgeNoticesAs), and outlive
	// the devices they name.
	Notices []humanProofNotice `json:"notices,omitempty"`
	// LegacyFirstDevice is the single notice earlier builds wrote; it is
	// moved into Notices when the file is read.
	LegacyFirstDevice *humanProofNotice `json:"firstDevice,omitempty"`
}

type humanProofNotice struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
	// Via is "first-device", "env-recovery" or "removed" (ID removed the
	// device named Removed).
	Via     string `json:"via"`
	Removed string `json:"removed,omitempty"`
}

type humanProof struct {
	mu     sync.Mutex
	path   string
	record humanProofRecord
	// stored: the file existed with a valid mode.
	stored bool
	// defaultMode applies until a mode is chosen: report on a Preview
	// machine or under WSL, off elsewhere.
	defaultMode string
	// envOff: SOLU_HUMAN_PROOF=off at start.
	envOff bool
	// lowered describes a drop below the confirmed mode made outside the
	// app, seen at start ("require → off").
	lowered string

	unprovenCount  int
	unprovenAt     time.Time
	unprovenAction string
}

// humanProofWSLHost reports a daemon running under WSL, where Windows
// programs and relayed loopback connections can't be traced: report is the
// default there, so those acts are counted before anything is required.
// A variable so tests can choose.
var humanProofWSLHost = isInsideWSL

func newHumanProof(dataDir string) *humanProof {
	h := &humanProof{path: filepath.Join(dataDir, humanProofFile), defaultMode: humanProofOff}
	if os.Getenv(previewOwnerEnv) == "1" || humanProofWSLHost() {
		h.defaultMode = humanProofReport
	}
	if b, err := os.ReadFile(h.path); err == nil {
		var rec humanProofRecord
		if json.Unmarshal(b, &rec) == nil && (rec.Mode == "" || validHumanProofMode(rec.Mode)) {
			if old := rec.LegacyFirstDevice; old != nil {
				old.Via = "first-device"
				rec.Notices = append([]humanProofNotice{*old}, rec.Notices...)
				rec.LegacyFirstDevice = nil
			}
			h.record, h.stored = rec, rec.Mode != ""
		} else {
			log.Printf("human-proof: %s is unreadable; using %s", h.path, h.defaultMode)
		}
	}
	if h.stored && validHumanProofMode(h.record.Confirmed) && humanProofRank(h.record.Mode) < humanProofRank(h.record.Confirmed) {
		h.lowered = h.record.Confirmed + " → " + h.record.Mode
		log.Printf("human-proof: device check was lowered outside the app (%s)", h.lowered)
	}
	switch v := strings.TrimSpace(strings.ToLower(os.Getenv(humanProofEnv))); v {
	case "":
	case humanProofOff:
		h.envOff = true
		log.Printf("human-proof: %s=off: device check is off at this computer", humanProofEnv)
	default:
		log.Printf("human-proof: ignoring %s=%q (only \"off\" is read)", humanProofEnv, v)
	}
	log.Printf("human-proof: mode %s", h.configuredMode())
	return h
}

// configuredMode is the chosen mode before the env override and the
// approved-device check.
func (h *humanProof) configuredMode() string {
	if h.stored {
		return h.record.Mode
	}
	return h.defaultMode
}

// mode is the mode in force. Require without an approved device would leave
// nobody able to act, so it works as report until one exists.
func (a *apiServer) humanProofMode() string {
	h := a.humanProof
	if h == nil {
		return humanProofOff
	}
	h.mu.Lock()
	mode, envOff := h.configuredMode(), h.envOff
	h.mu.Unlock()
	if envOff {
		return humanProofOff
	}
	if mode == humanProofRequire && a.approvedDeviceCount() == 0 {
		return humanProofReport
	}
	return mode
}

func (a *apiServer) approvedDeviceCount() int {
	if a.trustedDevices == nil {
		return 0
	}
	s := a.trustedDevices
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.record.Devices {
		if d.Status == deviceStatusApproved {
			n++
		}
	}
	return n
}

func (h *humanProof) set(mode string, now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.record
	rec.Mode, rec.Confirmed, rec.UpdatedAt = mode, mode, now
	if err := h.saveLocked(rec); err != nil {
		return err
	}
	h.lowered = ""
	return nil
}

// saveLocked writes rec. A record whose mode was never chosen keeps an empty
// mode, so the platform default still applies (and follows later default
// changes) rather than being saved as if someone had picked it.
func (h *humanProof) saveLocked(rec humanProofRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(h.path, b, 0o600); err != nil {
		return err
	}
	h.record, h.stored = rec, validHumanProofMode(rec.Mode)
	return nil
}

// addNotice records n, to be shown until acknowledged.
func (h *humanProof) addNotice(n humanProofNotice) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.record
	rec.Notices = append(append([]humanProofNotice(nil), rec.Notices...), n)
	return h.saveLocked(rec)
}

// noteApprovalNotice records a device approved without an approved
// device's proof (via "first-device" or "env-recovery").
func (h *humanProof) noteApprovalNotice(d *trustedDevice, via string, now time.Time) error {
	return h.addNotice(humanProofNotice{ID: d.ID, Name: d.Name, At: now, Via: via})
}

// dropNotice withdraws a notice recorded ahead of an approval that then
// failed.
func (h *humanProof) dropNotice(id string, at time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var kept []humanProofNotice
	for _, n := range h.record.Notices {
		if n.ID != id || !n.At.Equal(at) {
			kept = append(kept, n)
		}
	}
	rec := h.record
	rec.Notices = kept
	if err := h.saveLocked(rec); err != nil {
		log.Printf("human-proof: withdraw notice for %s: %v", id, err)
	}
}

// approvalNotMade reports an approval refused before anything changed, so
// the notice written ahead of it is withdrawn. After a failed save the
// device may be approved in memory, so the caller checks that instead.
func approvalNotMade(err error) bool {
	return errors.Is(err, errDeviceAlreadyBootstrapped) || errors.Is(err, errDeviceEnrolment) || errors.Is(err, os.ErrNotExist) || errors.Is(err, errApproverGone)
}

// deviceFlaggedLocked reports whether device has a notice of its own or was
// approved, directly or further up its chain, by a device that has one.
// Called with the device and notice locks held.
func (a *apiServer) deviceFlaggedLocked(device string) bool {
	noticed := map[string]bool{}
	for _, n := range a.humanProof.record.Notices {
		noticed[n.ID] = true
	}
	if noticed[device] {
		return true
	}
	if d := a.trustedDevices.find(device); d != nil {
		for _, up := range d.Lineage {
			if noticed[up] {
				return true
			}
		}
	}
	return false
}

// deviceFlagged is deviceFlaggedLocked taking the locks.
func (a *apiServer) deviceFlagged(device string) bool {
	if a.humanProof == nil || a.trustedDevices == nil {
		return false
	}
	a.trustedDevices.mu.Lock()
	defer a.trustedDevices.mu.Unlock()
	a.humanProof.mu.Lock()
	defer a.humanProof.mu.Unlock()
	return a.deviceFlaggedLocked(device)
}

type noticeAck int

const (
	noticeAckCleared noticeAck = iota
	// noticeAckOwn: the device has a notice of its own, outside the
	// bootstrap case.
	noticeAckOwn
	// noticeAckTainted: a device above it in its approval chain has one.
	noticeAckTainted
	// noticeAckNotApproved: the device is no longer approved.
	noticeAckNotApproved
)

// acknowledgeNoticesAs clears the notices, decided under the device and
// notice locks together so a notice or approval made meanwhile is judged
// too. A person traced at this computer (tracedPerson) clears them all.
// Otherwise device must be approved and clean: no notice of its own, and
// none on any device above it in its approval chain, since a device approved
// by a flagged one could be the same program's second key. The one
// exception is the bootstrap case: the computer's only device ever
// approved, with none removed since its notice, may clear its own.
func (a *apiServer) acknowledgeNoticesAs(device string, tracedPerson bool) (noticeAck, error) {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	s, h := a.trustedDevices, a.humanProof
	s.mu.Lock()
	defer s.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !tracedPerson {
		d := s.find(device)
		if d == nil || d.Status != deviceStatusApproved {
			return noticeAckNotApproved, nil
		}
		for _, up := range d.Lineage {
			for _, n := range h.record.Notices {
				if n.ID == up {
					return noticeAckTainted, nil
				}
			}
		}
		own, others := false, false
		for _, n := range h.record.Notices {
			if n.ID != device {
				others = true
				continue
			}
			own = true
			if s.record.LastApprovedRemoval != nil && !s.record.LastApprovedRemoval.Before(n.At) {
				return noticeAckOwn, nil
			}
		}
		// Its own notice is cleared only in the bootstrap case, and then
		// only when every notice is its own.
		if own && (s.approvedEverLocked("") != 1 || others) {
			return noticeAckOwn, nil
		}
	}
	if len(h.record.Notices) == 0 {
		return noticeAckCleared, nil
	}
	rec := h.record
	rec.Notices = nil
	return noticeAckCleared, h.saveLocked(rec)
}

func (h *humanProof) noteUnproven(action string, now time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.unprovenCount++
	h.unprovenAt, h.unprovenAction = now, action
	h.mu.Unlock()
}

// localDeviceTargetFor names this daemon in a local signature. Each daemon
// has its own identity key, so a signature for one daemon behind the
// desktop router can't be used on the other.
func localDeviceTargetFor(daemonSPKI string) string {
	if daemonSPKI == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(daemonSPKI)))
	return localDeviceTarget + "dmn_" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

func (s *trustedDevices) localTarget() string {
	return localDeviceTargetFor(s.daemonSPKI())
}

func (s *trustedDevices) localTargetCheck() func(string) bool {
	want := s.localTarget()
	return func(t string) bool { return want != "" && t == want }
}

type localVerifiedDeviceKey struct{}

// localVerifiedDevice is the approved device whose local signature
// localDeviceProof verified on this request, or "".
func localVerifiedDevice(r *http.Request) string {
	v, _ := r.Context().Value(localVerifiedDeviceKey{}).(string)
	return v
}

// localDeviceProof verifies a local device signature once, before the API,
// and records the device on the request. It never refuses anything itself:
// most requests don't need the person, and an unproven request is simply
// not one the person made. Verifying here rather than in identifyCaller
// matters because a handler may ask who is calling more than once, and the
// nonce is spent on the first check.
func (a *apiServer) localDeviceProof(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := deviceSigFieldsFrom(r)
		if a.trustedDevices == nil || f.id == "" || !strings.HasPrefix(f.target, localDeviceTarget) {
			next.ServeHTTP(w, r)
			return
		}
		d, err := a.trustedDevices.verifySigned(r, deviceSigCheck{
			targetOK:     a.trustedDevices.localTargetCheck(),
			maxBody:      humanProofMaxBody,
			keepOversize: true,
		})
		if err != nil {
			w.Header().Set(humanProofHeader, humanProofFailure(err))
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), localVerifiedDeviceKey{}, d.ID)))
	})
}

// humanProofFailure is the short reason sent back with an unproven request,
// so the app can correct its clock or ask to be approved.
func humanProofFailure(err error) string {
	switch {
	case errors.Is(err, errDeviceStale):
		return "stale"
	case errors.Is(err, errDeviceUnknown):
		return "not-approved"
	case errors.Is(err, errDeviceReplay):
		return "replay"
	case errors.Is(err, errDeviceWrongTarget):
		return "wrong-target"
	case errors.Is(err, errDeviceBodyTooLong):
		return "too-large"
	}
	return "invalid"
}

// tracedBrowserCaller classifies a traced browser making a same-origin
// request. A browser is recognised by its executable's name, which any
// program can take (a copy of curl named "chrome"), so under require only an
// approved device's signature makes it the person, as for any other caller.
// In off and report a browser counts on its name, except one run by
// automation (solubrowse, Playwright), which is what an agent drives.
func (a *apiServer) tracedBrowserCaller(r *http.Request, pid int) callerIdentity {
	if dev := localVerifiedDevice(r); dev != "" {
		return callerIdentity{Kind: callerHuman, PID: pid, Device: dev}
	}
	automation := peerIsAutomationBrowser(pid)
	if a.humanProofMode() == humanProofRequire {
		log.Printf("authority: refused unsigned browser %s %s from %s (pid %d)", r.Method, r.URL.Path, r.RemoteAddr, pid)
		return callerIdentity{Kind: callerUnverified, PID: pid, Unproven: true, Automation: automation}
	}
	if automation {
		return callerIdentity{Kind: callerHuman, PID: pid, Unproven: true, Automation: true}
	}
	return callerIdentity{Kind: callerHuman, PID: pid}
}

// automationBrowserNames are browsers that exist to be driven by programs.
var automationBrowserNames = map[string]bool{"solubrowse": true}

// automationLaunchers are the runtimes that start a browser for automation
// (Playwright, Puppeteer, Selenium drivers).
var automationLaunchers = map[string]bool{
	"node": true, "python": true, "python3": true, "deno": true, "bun": true,
	"chromedriver": true, "msedgedriver": true, "geckodriver": true, "playwright": true,
}

// peerIsAutomationBrowser reports whether pid is a browser that a program
// drives: solubrowse, a browser started with automation or remote-debugging
// switches, or one launched directly by an automation runtime. Only the
// browser's own processes and the program that launched them are looked at,
// so a browser with some node tool further up its ancestry still counts as
// the person's. A variable so tests can stand one in.
var peerIsAutomationBrowser = func(pid int) bool {
	cur := pid
	for depth := 0; depth < 8 && cur > 1; depth++ {
		name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(readComm(cur))), ".exe")
		if automationBrowserNames[name] {
			return true
		}
		if !isBrowserProcessName(name) {
			// The first process that isn't part of the browser launched it.
			return depth > 0 && automationLaunchers[name]
		}
		cmd := browserCommandLine(cur)
		for _, flag := range []string{"--enable-automation", "--remote-debugging-pipe", "--remote-debugging-port", "--headless", "-marionette", "--marionette"} {
			if strings.Contains(cmd, flag) {
				return true
			}
		}
		next := readPpid(cur)
		if next == cur {
			break
		}
		cur = next
	}
	return false
}

// ---- HTTP ------------------------------------------------------------------

type humanProofView struct {
	// Mode is the mode chosen in the app (or the default); Effective is the
	// one in force after the env override and the approved-device check.
	Mode      string `json:"mode"`
	Effective string `json:"effective"`
	// Target is what this daemon's local signatures must name.
	Target     string `json:"target"`
	ServerTime int64  `json:"serverTime"`
	// EnvOff: SOLU_HUMAN_PROOF=off at this computer.
	EnvOff bool `json:"envOff"`
	// Lowered: the mode was lowered outside the app ("require → off").
	Lowered         string             `json:"lowered,omitempty"`
	Notices         []humanProofNotice `json:"notices,omitempty"`
	ApprovedDevices int                `json:"approvedDevices"`
	// CanBootstrap: no device is approved yet, so the first one can approve
	// itself.
	CanBootstrap bool `json:"canBootstrap"`
	// Device is the asking device's status ("approved", "pending", "") when
	// ?device=<id> is given.
	Device   string              `json:"device,omitempty"`
	Unproven *humanProofUnproven `json:"unproven,omitempty"`
}

type humanProofUnproven struct {
	Count  int       `json:"count"`
	At     time.Time `json:"at"`
	Action string    `json:"action"`
}

func (a *apiServer) humanProofView(device string) humanProofView {
	h := a.humanProof
	h.mu.Lock()
	v := humanProofView{Mode: h.configuredMode(), EnvOff: h.envOff, Lowered: h.lowered, Notices: h.record.Notices}
	if h.unprovenCount > 0 {
		v.Unproven = &humanProofUnproven{Count: h.unprovenCount, At: h.unprovenAt, Action: h.unprovenAction}
	}
	h.mu.Unlock()
	v.Effective = a.humanProofMode()
	v.Target = a.trustedDevices.localTarget()
	v.ServerTime = time.Now().UnixMilli()
	v.ApprovedDevices = a.approvedDeviceCount()
	v.CanBootstrap = v.ApprovedDevices == 0
	if device != "" {
		s := a.trustedDevices
		s.mu.Lock()
		if d := s.find(device); d != nil {
			v.Device = d.Status
		}
		s.mu.Unlock()
	}
	return v
}

// handleGetHumanProof is for screens at this computer: over the relay it
// would tell a remote caller the mode, the approved-device count and any
// device's standing.
func (a *apiServer) handleGetHumanProof(w http.ResponseWriter, r *http.Request) {
	if a.humanProof == nil || a.trustedDevices == nil {
		http.Error(w, "device check is unavailable", http.StatusServiceUnavailable)
		return
	}
	if requestViaRelay(r) {
		http.Error(w, "the device check is only shown at the computer itself", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, a.humanProofView(r.URL.Query().Get("device")))
}

// handlePutHumanProof changes the mode, at the computer only. Raising it to
// require needs an approved device's signature, so the screen doing it can
// still act afterwards. Lowering it needs the same or, from off or report, a
// traced browser, so an agent can't switch it off by API. Lowering require
// always needs the signature: a browser is traced by its name alone. Without
// an approved device left, the way down is SOLU_HUMAN_PROOF=off at the
// computer.
func (a *apiServer) handlePutHumanProof(w http.ResponseWriter, r *http.Request) {
	if a.humanProof == nil || a.trustedDevices == nil {
		http.Error(w, "device check is unavailable", http.StatusServiceUnavailable)
		return
	}
	if requestViaRelay(r) {
		http.Error(w, "the device check can only be changed at the computer itself", http.StatusForbidden)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, humanProofMaxRequest)).Decode(&body); err != nil || !validHumanProofMode(body.Mode) {
		http.Error(w, `mode must be "off", "report" or "require"`, http.StatusBadRequest)
		return
	}
	id := a.identifyCaller(r)
	if id.Kind != callerHuman {
		a.refuseCaller(w, id, "change the device check", false)
		return
	}
	a.humanProof.mu.Lock()
	current := a.humanProof.configuredMode()
	a.humanProof.mu.Unlock()
	switch {
	case body.Mode == humanProofRequire && id.Device == "":
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "device_required",
			"message": "Approve this browser first: only an approved browser or app can require approved devices.",
		})
		return
	case body.Mode == humanProofRequire && a.approvedDeviceCount() == 0:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no_devices", "message": "Approve a browser or app first."})
		return
	case humanProofRank(body.Mode) < humanProofRank(current) && id.Device == "" && (id.Unproven || id.PID == 0 || current == humanProofRequire):
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "device_required",
			"message": "Lower the device check from an approved browser or app. If none is left, add SOLU_HUMAN_PROOF=off to .solu/env in your home folder (in Linux if SoluTerminal runs in WSL), then restart SoluTerminal's background service: if it runs in WSL, run wsl --shutdown in Windows and reopen SoluTerminal (this stops everything running in WSL); if it runs on Windows without WSL, quit SoluTerminal from the tray and reopen it; with a Linux command-line install, run systemctl --user restart soluterminal.",
		})
		return
	}
	if err := a.humanProof.set(body.Mode, time.Now().UTC()); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	log.Printf("human-proof: mode %s → %s (device %q, pid %d)", current, body.Mode, id.Device, id.PID)
	writeJSON(w, http.StatusOK, a.humanProofView(""))
}

// handleBootstrapHumanProof approves the browser sending it as this
// computer's first device. It is open only while no device at all is
// approved (relayed or local): after that, a new device is approved from one
// that already is. The request must be signed by the key it enrols.
func (a *apiServer) handleBootstrapHumanProof(w http.ResponseWriter, r *http.Request) {
	if a.humanProof == nil || a.trustedDevices == nil {
		http.Error(w, "device check is unavailable", http.StatusServiceUnavailable)
		return
	}
	if requestViaRelay(r) {
		http.Error(w, "the first device is approved at the computer itself", http.StatusForbidden)
		return
	}
	if a.approvedDeviceCount() > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "already_bootstrapped",
			"message": "A device is already approved here. Approve this one from it: Settings → Privacy & security.",
		})
		return
	}
	if pid, ok := lookupPeerPID(r); ok && pid > 0 && peerIsAutomationBrowser(pid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "automation_browser",
			"message": "A browser run by automation can't be approved as your device.",
		})
		return
	}
	var body struct {
		Name      string `json:"name"`
		PublicKey string `json:"publicKey"`
	}
	// Possession is checked over the raw body, so read it first and restore.
	raw, err := readLimitedBody(w, r, humanProofMaxRequest)
	if err != nil || json.Unmarshal(raw, &body) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	key, err := parseDevicePublicKey(body.PublicKey)
	if err != nil {
		http.Error(w, "bad public key", http.StatusBadRequest)
		return
	}
	if f := deviceSigFieldsFrom(r); f.id != deviceID(body.PublicKey) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsigned", "message": "The request must be signed by the key it approves."})
		return
	}
	if _, err := a.trustedDevices.verifySigned(r, deviceSigCheck{
		targetOK:   a.trustedDevices.localTargetCheck(),
		maxBody:    humanProofMaxRequest,
		possession: key,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsigned", "message": "The request must be signed by the key it approves.", "reason": humanProofFailure(err)})
		return
	}
	id := a.identifyCaller(r)
	if id.Kind != callerHuman {
		a.refuseCaller(w, id, "approve this browser", false)
		return
	}
	// The notice goes in first, so the new device is never approved without
	// it, not even for the moment between the two saves; noticeMu keeps an
	// acknowledgement from landing in between.
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	now := time.Now().UTC()
	if err := a.humanProof.noteApprovalNotice(&trustedDevice{ID: deviceID(body.PublicKey), Name: deviceDisplayName(body.Name)}, "first-device", now); err != nil {
		log.Printf("human-proof: record first device: %v", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	d, err := a.trustedDevices.bootstrapLocal(body.Name, body.PublicKey, deviceSurface(r))
	if approvalNotMade(err) || (err != nil && !a.trustedDevices.isApproved(deviceID(body.PublicKey))) {
		a.humanProof.dropNotice(deviceID(body.PublicKey), now)
	}
	if errors.Is(err, errDeviceAlreadyBootstrapped) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already_bootstrapped", "message": "A device is already approved here."})
		return
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	log.Printf("human-proof: first device approved: %s (%s), pid %d", d.ID, d.Name, id.PID)
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "human-proof.first-device", Text: d.Name})
	}
	a.trustedDevices.mu.Lock()
	v := viewOfDevice(d)
	a.trustedDevices.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// handleAcknowledgeHumanProof clears the notices, for an approved device
// that may vouch for them (acknowledgeNoticesAs) or, outside require, a
// person traced at this computer. A refusal never blocks anything else: the
// banner stays up.
func (a *apiServer) handleAcknowledgeHumanProof(w http.ResponseWriter, r *http.Request) {
	if a.humanProof == nil {
		http.Error(w, "device check is unavailable", http.StatusServiceUnavailable)
		return
	}
	id := a.identifyCaller(r)
	dev := id.Device
	if dev == "" {
		dev = remoteVerifiedDevice(r)
	}
	// A signed request is judged as its device, wherever it came from (a
	// signed request through a proxy has the proxy's pid; a signed
	// automation browser isn't marked as one). Only an unsigned browser
	// the daemon traced at this computer is the person in their own right,
	// and not under require, where a browser known only by name isn't.
	traced := dev == "" && id.Kind == callerHuman && id.PID > 0 && !id.Unproven && !id.Automation && !id.Relay &&
		a.humanProofMode() != humanProofRequire
	if dev == "" && !traced {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "device_required", "message": "Acknowledge this from an approved browser or app."})
		return
	}
	got, err := a.acknowledgeNoticesAs(dev, traced)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	switch got {
	case noticeAckOwn:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "other_device_required",
			"message": "This browser or app is one the notice names. Acknowledge it from another approved browser or app that was approved another way.",
		})
		return
	case noticeAckTainted:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "clean_device_required",
			"message": "This browser or app was approved from a device the notice names, so it can't vouch for it. Acknowledge it from a device approved another way, or from a browser on this computer.",
		})
		return
	case noticeAckNotApproved:
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "device_required", "message": "Acknowledge this from an approved browser or app."})
		return
	}
	log.Printf("human-proof: notices acknowledged (device %q, traced %t, pid %d)", dev, traced, id.PID)
	writeJSON(w, http.StatusOK, a.humanProofView(""))
}

func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("no body")
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// deviceSurface says where a device enrolled from: over the relay or at this
// computer.
func deviceSurface(r *http.Request) string {
	if requestViaRelay(r) {
		return deviceSurfaceRelay
	}
	return deviceSurfaceLocal
}
