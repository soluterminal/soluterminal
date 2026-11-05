package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Notifications-handler lease: another app on an approved device takes over
// the person's Code alerts, and while the lease is live the daemon asks the
// cloud not to send its own FCM push for them. The usual holder is a desktop
// window signing at this computer as a scoped "alerts" device
// (trustedDevice.Scope), which may do nothing else; the person's own devices
// (signed here, signed over the relay, or on their private channel) may hold
// it too.
//
// Whoever holds the lease silences the person's alerts, so only an approved
// device may take it: never a pane (Agent permissions don't delegate it), a
// worker, an unsigned program or browser headers alone. The lease belongs to
// the device; the client label is display text. It lives in memory only, and
// at every push decision it counts only while unexpired and while its device
// is still approved: anything else sends the alert as before.
//
// Releasing only brings alerts back, so the holder or the person at this
// computer may release it, and releasing what isn't held is a no-op. No MCP
// tool or agent path reaches it.

const (
	alertsLeaseMinTTL     = 60 * time.Second
	alertsLeaseMaxTTL     = 600 * time.Second
	alertsLeaseClientMax  = 64
	alertsLeaseEventType  = "alerts-handler"
	alertsLeaseBodyMaxLen = 4 << 10
)

// Lease events on the event stream (uiEvent.Lease.Event). A renewal by the
// holder is not announced.
const (
	alertsLeaseAcquired  = "acquired"
	alertsLeaseTakenOver = "taken_over"
	alertsLeaseRevoked   = "revoked"
	alertsLeaseExpired   = "expired"
)

// Why a lease was revoked (uiEvent.Lease.Cause).
const (
	alertsLeaseCauseReleased   = "released"   // the holder's DELETE
	alertsLeaseCauseTakenBack  = "taken_back" // the person at this computer
	alertsLeaseCauseUnapproved = "unapproved" // the holder device is no longer approved
)

// alertsLeaseEvent is the lease's part of a uiEvent. It carries the holder's
// id and name and times only: no key, signature or alert content.
type alertsLeaseEvent struct {
	Event            string `json:"event"`
	DeviceID         string `json:"deviceId"`
	DeviceName       string `json:"deviceName"`
	ExpiresAtMs      int64  `json:"expiresAtMs"`
	PreviousDeviceID string `json:"previousDeviceId,omitempty"`
	Cause            string `json:"cause,omitempty"`
}

type alertsLease struct {
	holder  string // approved device id
	name    string // the holder device's name when it took the lease
	client  string // display label only
	expires time.Time
	gen     uint64
	kick    chan struct{} // wakes this lease's watcher
}

type alertsLeaseStore struct {
	mu    sync.Mutex
	lease *alertsLease
	gen   uint64

	now      func() time.Time
	after    func(time.Duration) <-chan time.Time
	approved func(device string) bool
	name     func(device string) (string, bool)
	changes  func() <-chan struct{} // closed when a device's access may have ended
	publish  func(uiEvent)
}

func newAlertsLeaseStore(devices *trustedDevices, bus *uiBus) *alertsLeaseStore {
	s := &alertsLeaseStore{now: time.Now, after: time.After}
	if devices != nil {
		s.approved = func(id string) bool { _, ok := devices.alertsDevice(id); return ok }
		s.name = devices.alertsDevice
		s.changes = devices.changes
	}
	if bus != nil {
		s.publish = bus.publish
	}
	return s
}

// alertsLease returns the daemon's lease store, made on first use.
func (a *apiServer) alertsLease() *alertsLeaseStore {
	a.alertsLeaseOnce.Do(func() {
		s := newAlertsLeaseStore(nil, nil)
		// Read at each use: the device store and bus are set as the daemon
		// starts, possibly after this.
		s.approved = func(id string) bool {
			if a.trustedDevices == nil {
				return false
			}
			_, ok := a.trustedDevices.alertsDevice(id)
			return ok
		}
		s.name = func(id string) (string, bool) {
			if a.trustedDevices == nil {
				return "", false
			}
			return a.trustedDevices.alertsDevice(id)
		}
		s.changes = func() <-chan struct{} {
			if a.trustedDevices == nil {
				return nil
			}
			return a.trustedDevices.changes()
		}
		s.publish = func(e uiEvent) {
			if a.uiBus != nil {
				a.uiBus.publish(e)
			}
		}
		a.alertsLeaseV = s
	})
	return a.alertsLeaseV
}

// liveLocked is the current lease while it is unexpired and its device is
// still approved, else nil.
func (s *alertsLeaseStore) liveLocked() *alertsLease {
	l := s.lease
	if l == nil || !s.now().Before(l.expires) || s.approved == nil || !s.approved(l.holder) {
		return nil
	}
	return l
}

// skipsFCM is the push decision: true only while a live lease is held by a
// still-approved device. A panic on this path counts as no lease.
func (s *alertsLeaseStore) skipsFCM() (skip bool) {
	if s == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			skip = false
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveLocked() != nil
}

type alertsLeaseView struct {
	DeviceID  string
	Name      string
	Client    string
	Expires   time.Time
	TakenOver bool
	TTL       time.Duration
}

// take gives the lease to device for ttl, or renews it when device already
// holds it. The device is checked as approved again under the lock.
func (s *alertsLeaseStore) take(device, client string, ttl time.Duration) (alertsLeaseView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approved == nil || !s.approved(device) {
		return alertsLeaseView{}, false
	}
	name := ""
	if s.name != nil {
		name, _ = s.name(device)
		name = sanitizeAlertsClient(name) // shown on the event stream and in Settings
	}
	now := s.now()
	expires := now.Add(ttl)
	cur := s.liveLocked()
	if cur == nil {
		s.dropStaleLocked() // announce the end of a lease that lapsed unseen
	}
	view := alertsLeaseView{DeviceID: device, Name: name, Client: client, Expires: expires, TTL: ttl}
	if cur != nil && cur.holder == device {
		cur.name, cur.client, cur.expires = name, client, expires
		wake(cur.kick)
		return view, true
	}
	if s.lease != nil {
		wake(s.lease.kick) // its watcher stops: the generation moved on
	}
	s.gen++
	s.lease = &alertsLease{holder: device, name: name, client: client, expires: expires, gen: s.gen, kick: make(chan struct{}, 1)}
	go s.watch(s.lease.gen)
	if cur != nil {
		view.TakenOver = true
		log.Printf("notifications-handler: device %s (%s) took over Code alerts from device %s (%s) as %q until %s", device, name, cur.holder, cur.name, client, expires.UTC().Format(time.RFC3339))
		s.emit(alertsLeaseEvent{Event: alertsLeaseTakenOver, DeviceID: device, DeviceName: name, ExpiresAtMs: expires.UnixMilli(), PreviousDeviceID: cur.holder})
		return view, true
	}
	log.Printf("notifications-handler: device %s (%s) took Code alerts as %q until %s", device, name, client, expires.UTC().Format(time.RFC3339))
	s.emit(alertsLeaseEvent{Event: alertsLeaseAcquired, DeviceID: device, DeviceName: name, ExpiresAtMs: expires.UnixMilli()})
	return view, true
}

// releaseIfHolder ends the live lease when device holds it.
func (s *alertsLeaseStore) releaseIfHolder(device string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveLocked()
	if l == nil || device == "" || l.holder != device {
		return false
	}
	s.endLocked(l, alertsLeaseRevoked, alertsLeaseCauseReleased)
	return true
}

// release ends any live lease for the person at this computer.
func (s *alertsLeaseStore) release() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveLocked()
	if l == nil {
		s.dropStaleLocked()
		return false
	}
	s.endLocked(l, alertsLeaseRevoked, alertsLeaseCauseTakenBack)
	return true
}

// current is the live lease, if any.
func (s *alertsLeaseStore) current() (alertsLeaseView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveLocked()
	if l == nil {
		s.dropStaleLocked()
		return alertsLeaseView{}, false
	}
	return alertsLeaseView{DeviceID: l.holder, Name: l.name, Client: l.client, Expires: l.expires}, true
}

// endLocked clears l and announces why, once.
func (s *alertsLeaseStore) endLocked(l *alertsLease, event, cause string) {
	if s.lease != l {
		return
	}
	s.lease = nil
	wake(l.kick)
	switch event {
	case alertsLeaseExpired:
		log.Printf("notifications-handler: Code alerts lease of device %s (%s) expired; this computer sends them again", l.holder, l.name)
	default:
		log.Printf("notifications-handler: Code alerts lease of device %s (%s) revoked (%s); this computer sends them again", l.holder, l.name, cause)
	}
	s.emit(alertsLeaseEvent{Event: event, DeviceID: l.holder, DeviceName: l.name, ExpiresAtMs: l.expires.UnixMilli(), Cause: cause})
}

// dropStaleLocked ends a lease that no longer counts (expired, or its device
// is no longer approved) and hasn't been announced yet.
func (s *alertsLeaseStore) dropStaleLocked() {
	l := s.lease
	if l == nil {
		return
	}
	if !s.now().Before(l.expires) {
		s.endLocked(l, alertsLeaseExpired, "")
	} else if s.approved == nil || !s.approved(l.holder) {
		s.endLocked(l, alertsLeaseRevoked, alertsLeaseCauseUnapproved)
	}
}

// watch announces the end of lease generation gen when it expires or its
// device stops being approved, so the holder hears it at once instead of on
// its next renewal. Push decisions never wait for it (liveLocked).
func (s *alertsLeaseStore) watch(gen uint64) {
	for {
		s.mu.Lock()
		l := s.lease
		if l == nil || l.gen != gen {
			s.mu.Unlock()
			return
		}
		// Subscribe before checking approval: a removal after the check
		// then still closes the channel this watcher waits on.
		var changed <-chan struct{}
		if s.changes != nil {
			changed = s.changes()
		}
		s.dropStaleLocked()
		if s.lease != l {
			s.mu.Unlock()
			return
		}
		wait := l.expires.Sub(s.now())
		kick := l.kick
		s.mu.Unlock()
		select {
		case <-s.after(wait):
		case <-changed:
		case <-kick:
		}
	}
}

func (s *alertsLeaseStore) emit(e alertsLeaseEvent) {
	if s.publish == nil {
		return
	}
	s.publish(uiEvent{Type: alertsLeaseEventType, Ts: s.now().UnixMilli(), Lease: &e})
}

func wake(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// clampAlertsLeaseTTL keeps a requested lease within 60–600 s; 0 asks for
// the longest.
func clampAlertsLeaseTTL(sec int) time.Duration {
	ttl := time.Duration(sec) * time.Second
	if sec <= 0 || ttl > alertsLeaseMaxTTL {
		return alertsLeaseMaxTTL
	}
	if ttl < alertsLeaseMinTTL {
		return alertsLeaseMinTTL
	}
	return ttl
}

// sanitizeAlertsClient makes a label safe to show (the client label, and the
// holder device's name as events and Settings show it): no control or
// formatting characters (bidi overrides included), single spaces, at most 64
// characters.
func sanitizeAlertsClient(s string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			if n+1 >= alertsLeaseClientMax {
				break
			}
			b.WriteRune(' ')
			n++
			space = false
		}
		if n >= alertsLeaseClientMax {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// alertsLeaseDevice is the approved device that made r: a scoped alerts
// device or one of the person's that signed it at this computer, or one of
// the person's that (over the relay) signed it or sent it on its private
// channel. Panes and workers never qualify, whatever they carry, and Agent
// permissions don't change that; nor does an unsigned caller.
func alertsLeaseDevice(r *http.Request, caller callerIdentity) string {
	switch {
	case caller.Kind == callerPane || caller.Kind == callerWorker:
		return ""
	case caller.ScopedDevice != "":
		return caller.ScopedDevice
	case caller.Kind != callerHuman:
		return ""
	case caller.Device != "":
		return caller.Device
	}
	return remoteVerifiedDevice(r)
}

const alertsLeaseDeviceMessage = "Only an approved device can take over this computer's alerts. Approve the app's device in Settings → Privacy & security, then try again from it."

func (a *apiServer) refuseAlertsLease(w http.ResponseWriter, caller callerIdentity, action string) {
	log.Printf("notifications-handler: refused %s caller=%s pane=%s pid=%d relay=%t", action, caller.Kind, caller.PaneID, caller.PID, caller.Relay)
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   "device_required",
		"caller":  string(caller.Kind),
		"message": alertsLeaseDeviceMessage,
	})
}

func alertsLeaseJSON(v alertsLeaseView) map[string]any {
	return map[string]any{
		"active":      true,
		"deviceName":  v.Name,
		"client":      v.Client,
		"expiresAtMs": v.Expires.UnixMilli(),
	}
}

// handlePutNotificationsHandler takes or renews the lease.
func (a *apiServer) handlePutNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	caller := a.identifyCaller(r)
	device := alertsLeaseDevice(r, caller)
	if device == "" {
		a.refuseAlertsLease(w, caller, "take the notifications handler")
		return
	}
	var body struct {
		Client string `json:"client"`
		TTLSec int    `json:"ttlSec"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, alertsLeaseBodyMaxLen)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "Send {\"client\": string, \"ttlSec\": number}."})
		return
	}
	v, ok := a.alertsLease().take(device, sanitizeAlertsClient(body.Client), clampAlertsLeaseTTL(body.TTLSec))
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "device_not_approved", "message": "This device is no longer approved on this computer."})
		return
	}
	out := alertsLeaseJSON(v)
	out["deviceId"] = v.DeviceID
	out["ttlSec"] = int(v.TTL / time.Second)
	out["takenOver"] = v.TakenOver
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteNotificationsHandler releases the caller's own lease: a
// caller that doesn't hold it (it expired, or another device took it)
// releases nothing and is told so, so a holder can always DELETE on exit
// without ending someone else's. With ?takeBack=1 ("Take alerts back" in
// Settings) the person at this computer ends whoever's lease it is; that
// needs no device signature because releasing only brings alerts back.
func (a *apiServer) handleDeleteNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	caller := a.identifyCaller(r)
	device := alertsLeaseDevice(r, caller)
	if device != "" && a.alertsLease().releaseIfHolder(device) {
		writeJSON(w, http.StatusOK, map[string]any{"released": true})
		return
	}
	person := atThisComputer(r) && caller.Kind == callerHuman
	if person && r.URL.Query().Get("takeBack") == "1" {
		a.logPersonAct(caller, "take alerts back")
		writeJSON(w, http.StatusOK, map[string]any{"released": a.alertsLease().release()})
		return
	}
	if person || device != "" {
		writeJSON(w, http.StatusOK, map[string]any{"released": false})
		return
	}
	if requestViaRelay(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "human_required",
			"message": "Only the device holding the alerts, or you at the computer itself, can take alerts back.",
		})
		return
	}
	a.refuseCaller(w, caller, "take alerts back", false)
}

// handleGetNotificationsHandler answers who holds the lease (device name and
// expiry only). Over the relay it answers approved devices only.
func (a *apiServer) handleGetNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) && remoteVerifiedDevice(r) == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "device_required", "message": alertsLeaseDeviceMessage})
		return
	}
	v, ok := a.alertsLease().current()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, alertsLeaseJSON(v))
}

// codeAlertKinds are the push kinds the notifications handler delivers
// instead of FCM: a pane's prompt or reply, and agents' asks. Every other
// kind (schedules, managed agents, workflows, billing, and any device,
// enrolment or security notice) is always sent.
var codeAlertKinds = map[string]bool{
	"choice-prompt":  true,
	"awaiting-input": true,
	"reply-complete": true,
	"user-ask":       true,
}

// withSkipFCM marks a notify body so the cloud sends web push but not FCM.
// Any error leaves the body as it was, so FCM is sent.
func withSkipFCM(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	m["skipFcm"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
