package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trusted devices let the daemon decide for itself which remote devices may
// use it through the cloud relay, instead of trusting whatever the cloud
// forwards. See docs/DESIGN-trusted-devices-and-private-relay.md (step 1).
//
// Each device holds an ECDSA P-256 key the cloud never sees. A person approves
// the device on this computer; afterwards every relayed request must be signed
// with that key. A compromised cloud, a leaked session secret or a stolen login
// can then no longer start anything here, and cannot replay or alter a signed
// request. Reading traffic and injecting frames into an open WebSocket need the
// encrypted channel of step 2.
//
// The policy starts as "any" (today's behaviour). "approved" turns the check on.

const (
	trustedDevicesFile        = "trusted-devices.json"
	remoteAccessAny           = "any"
	remoteAccessApproved      = "approved"
	deviceSigVersion          = "SOLU-DEVICE-V1"
	deviceSigMaxSkew          = 60 * time.Second
	deviceSigMaxBody          = 64 << 20
	devicePendingLimit        = 5
	deviceEnrollMaxBody       = 4 << 10
	deviceHeaderID            = "X-Solu-Device"
	deviceHeaderTime          = "X-Solu-Device-Time"
	deviceHeaderNonce         = "X-Solu-Device-Nonce"
	deviceHeaderSig           = "X-Solu-Device-Sig"
	deviceHeaderTarget        = "X-Solu-Device-Target"
	deviceQueryID             = "solu_dev"
	deviceQueryTime           = "solu_dev_t"
	deviceQueryNonce          = "solu_dev_n"
	deviceQuerySig            = "solu_dev_sig"
	deviceQueryTarget         = "solu_dev_to"
	deviceStatusPending       = "pending"
	deviceStatusApproved      = "approved"
	remoteDevicesPathPrefix   = "/api/remote-devices"
	remoteDevicesEnrollPath   = "/api/remote-devices/enroll"
	remoteDevicesStatusSuffix = "/status"
)

type trustedDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"` // base64 SPKI DER
	Status    string `json:"status"`
	Code      string `json:"code"` // derived from the key; compared on both sides
	// CodeVersion 2: the code covers this computer's identity key too, so an
	// approval also verified the computer to the device. 1 (or 0, older
	// records): device key only.
	CodeVersion int        `json:"codeVersion,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ApprovedAt  *time.Time `json:"approvedAt,omitempty"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`

	key *ecdsa.PublicKey
}

type trustedDevicesRecord struct {
	Policy  string           `json:"policy"`
	Devices []*trustedDevice `json:"devices"`
}

type trustedDevices struct {
	mu     sync.Mutex
	path   string
	record trustedDevicesRecord
	nonces map[string]time.Time
	// identity reports this daemon's machine id (from claim.json) and
	// backend id, which signatures must name.
	identity func() (machineID, backendID string)
	// daemonKey is this computer's identity key (daemon_identity.go):
	// approval codes cover it and the private relay channel proves it.
	daemonKey func() *daemonIdentity
	// now is replaced in tests.
	now func() time.Time
	// changed is closed (and replaced) whenever a device is removed or the
	// record changes in a way that can withdraw access; see changes().
	changed chan struct{}
	// onPolicyChange runs after the policy changes (main: resync reminders).
	onPolicyChange func()
	// announcedAt is when a new request was last announced on this computer.
	announcedAt time.Time
}

// newRequestAnnounceGap spaces announcements of new device requests:
// enrolment needs no approval, so anyone who can reach the relay could
// otherwise fill the computer's screen with prompts. Every request still
// waits in Settings → Privacy & security.
const newRequestAnnounceGap = 30 * time.Second

// announceNewRequest reports whether a new device request should be shown on
// this computer now: only under "Only devices I approve" (under "any" a
// device needs no approval), and at most once per newRequestAnnounceGap.
func (s *trustedDevices) announceNewRequest(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Policy != remoteAccessApproved {
		return false
	}
	if !s.announcedAt.IsZero() && now.Sub(s.announcedAt) < newRequestAnnounceGap {
		return false
	}
	s.announcedAt = now
	return true
}

// changes returns a channel closed at the next change that can withdraw a
// device's access (removal, policy change).
func (s *trustedDevices) changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.changed
}

func (s *trustedDevices) notifyLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// isApproved reports whether id is currently an approved device.
func (s *trustedDevices) isApproved(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	return d != nil && d.Status == deviceStatusApproved
}

func newTrustedDevices(dataDir string) *trustedDevices {
	s := &trustedDevices{
		path:   filepath.Join(dataDir, trustedDevicesFile),
		record: trustedDevicesRecord{Policy: remoteAccessAny},
		nonces: map[string]time.Time{},
		now:    time.Now,
	}
	if b, err := os.ReadFile(s.path); err == nil {
		var rec trustedDevicesRecord
		if json.Unmarshal(b, &rec) == nil {
			if rec.Policy != remoteAccessApproved {
				rec.Policy = remoteAccessAny
			}
			kept := rec.Devices[:0]
			for _, d := range rec.Devices {
				if key, err := parseDevicePublicKey(d.PublicKey); err == nil {
					d.key = key
					kept = append(kept, d)
				}
			}
			rec.Devices = kept
			s.record = rec
		}
	}
	return s
}

// freshDataDirEntries are what may already be in a daemon's data directory
// before its first start: the installer's claim (`solud --claim`) and
// service.env, launchd's log directory, the desktop's copy of WSL's claim,
// and the shell-info.json the desktop writes on every launch. Anything else
// was written by a daemon that already ran, so the install is existing.
var freshDataDirEntries = map[string]bool{
	"claim.json": true, "claim.json.tmp": true, "service.env": true, "logs": true,
	"shell-info.json": true,
}

// decideRemoteAccessPolicyOnce records, the first time a daemon carrying this
// change starts, which remote-access policy this computer begins with, and
// never decides again: later starts, re-pairing and updates keep whatever is
// recorded (or whatever a person later chose). A brand-new data directory
// begins with "Only devices I approve". Anything else (an existing install,
// or a service given --relay-token, which has no claim.json and would have
// no way to approve a first device over the relay) keeps "any". It must run
// before anything else writes to the data directory. Returns the policy it
// recorded, or "" when one was already recorded or the directory can't be
// read.
func decideRemoteAccessPolicyOnce(dataDir string, explicitRelayToken bool) (string, error) {
	path := filepath.Join(dataDir, trustedDevicesFile)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	policy := remoteAccessApproved
	if explicitRelayToken {
		policy = remoteAccessAny
	} else {
		entries, err := os.ReadDir(dataDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		for _, e := range entries {
			if !freshDataDirEntries[e.Name()] {
				policy = remoteAccessAny
				break
			}
		}
	}
	b, err := json.MarshalIndent(trustedDevicesRecord{Policy: policy, Devices: []*trustedDevice{}}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomicSynced(path, b, 0o600); err != nil {
		return "", err
	}
	return policy, nil
}

func (s *trustedDevices) daemonSPKI() string {
	if s.daemonKey == nil {
		return ""
	}
	if id := s.daemonKey(); id != nil {
		return id.spki
	}
	return ""
}

func (s *trustedDevices) saveLocked() error {
	b, err := json.MarshalIndent(s.record, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, b, 0o600)
}

func (s *trustedDevices) policy() string {
	if s == nil {
		return remoteAccessAny
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record.Policy
}

func parseDevicePublicKey(b64 string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("public key is not base64: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("public key is not SPKI: %w", err)
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("public key must be ECDSA P-256")
	}
	return key, nil
}

// deviceID is derived from the key, so a device cannot claim another's id.
func deviceID(spkiB64 string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(spkiB64)))
	return "dev_" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

// deviceCode is the fingerprint a person compares on the device and on this
// computer before approving: 80 bits of a hash over both public keys — the
// device's and this computer's identity (daemon_identity.go) — as four groups
// of four characters. A relay that swaps either key during enrolment makes the
// two sides show different codes, and 80 bits is far beyond what a relay could
// grind for a match. daemonSPKI is empty only when the identity is
// unavailable; the code then covers the device key alone (V1).
func deviceCode(spkiB64, daemonSPKI string) string {
	input := "SOLU-DEVICE-CODE-V1\n" + strings.TrimSpace(spkiB64)
	if daemonSPKI != "" {
		input = "SOLU-DEVICE-CODE-V2\n" + strings.TrimSpace(spkiB64) + "\n" + strings.TrimSpace(daemonSPKI)
	}
	return crockfordCode(sha256.Sum256([]byte(input)))
}

func codeVersionOf(daemonSPKI string) int {
	if daemonSPKI != "" {
		return 2
	}
	return 1
}

func crockfordCode(sum [32]byte) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32
	n := new(big.Int).SetBytes(sum[:10])
	out := make([]byte, 0, 19)
	for i := 0; i < 16; i++ {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		shift := uint(75 - 5*i)
		idx := new(big.Int).And(new(big.Int).Rsh(n, shift), big.NewInt(31)).Int64()
		out = append(out, alphabet[idx])
	}
	return string(out)
}

// enroll records a device as pending, or returns the existing record.
// codeVersion 2 (clients that pin the computer's identity) gets the approval
// code over both keys; older clients compute the device-key-only code.
func (s *trustedDevices) enroll(name, spkiB64 string, codeVersion ...int) (*trustedDevice, error) {
	d, _, err := s.enrollReport(name, spkiB64, codeVersion...)
	return d, err
}

// enrollReport is enroll, also reporting whether this call created the
// record (decided under the lock, so concurrent requests agree).
func (s *trustedDevices) enrollReport(name, spkiB64 string, codeVersion ...int) (*trustedDevice, bool, error) {
	key, err := parseDevicePublicKey(spkiB64)
	if err != nil {
		return nil, false, err
	}
	daemonSPKIForCode := ""
	if len(codeVersion) > 0 && codeVersion[0] >= 2 {
		daemonSPKIForCode = s.daemonSPKI()
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Unnamed device"
	}
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	id := deviceID(spkiB64)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.record.Devices {
		if d.ID == id {
			// A request still waiting under an older (V1) code moves to the
			// V2 code the updated device now shows; approved records keep
			// theirs (re-approval is the device's call).
			if d.Status == deviceStatusPending && d.CodeVersion < 2 && daemonSPKIForCode != "" {
				d.Code, d.CodeVersion = deviceCode(spkiB64, daemonSPKIForCode), 2
				if err := s.saveLocked(); err != nil {
					return nil, false, err
				}
			}
			return d, false, nil
		}
	}
	// Keep the pending queue short so an unauthenticated caller cannot fill it.
	var pending []int
	for i, d := range s.record.Devices {
		if d.Status == deviceStatusPending {
			pending = append(pending, i)
		}
	}
	if len(pending) >= devicePendingLimit {
		oldest := pending[0]
		for _, i := range pending[1:] {
			if s.record.Devices[i].CreatedAt.Before(s.record.Devices[oldest].CreatedAt) {
				oldest = i
			}
		}
		s.record.Devices = append(s.record.Devices[:oldest], s.record.Devices[oldest+1:]...)
	}
	d := &trustedDevice{
		ID: id, Name: name, PublicKey: strings.TrimSpace(spkiB64), Status: deviceStatusPending,
		Code: deviceCode(spkiB64, daemonSPKIForCode), CodeVersion: codeVersionOf(daemonSPKIForCode), CreatedAt: s.now().UTC(), key: key,
	}
	s.record.Devices = append(s.record.Devices, d)
	if err := s.saveLocked(); err != nil {
		return nil, false, err
	}
	return d, true, nil
}

// pruneNoncesLocked forgets nonces past their replay window. Both request
// signatures and private-channel handshakes record nonces, so both prune.
func (s *trustedDevices) pruneNoncesLocked(now time.Time) {
	for k, exp := range s.nonces {
		if now.After(exp) {
			delete(s.nonces, k)
		}
	}
}

func (s *trustedDevices) find(id string) *trustedDevice {
	for _, d := range s.record.Devices {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (s *trustedDevices) approve(id string) (*trustedDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return nil, os.ErrNotExist
	}
	now := s.now().UTC()
	d.Status, d.ApprovedAt = deviceStatusApproved, &now
	return d, s.saveLocked()
}

func (s *trustedDevices) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.record.Devices {
		if d.ID == id {
			s.record.Devices = append(s.record.Devices[:i], s.record.Devices[i+1:]...)
			s.notifyLocked()
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

func (s *trustedDevices) setPolicy(policy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.record.Policy != policy
	s.record.Policy = policy
	s.notifyLocked()
	err := s.saveLocked()
	if changed && s.onPolicyChange != nil {
		go s.onPolicyChange() // outside s.mu; the hook may read the policy
	}
	return err
}

// deviceSigFields are the signature inputs a request carries, in headers or,
// for WebSocket and EventSource (which cannot set headers), in the query.
type deviceSigFields struct {
	id, time, nonce, sig, target string
	fromQuery                    bool
}

func deviceSigFieldsFrom(r *http.Request) deviceSigFields {
	if id := r.Header.Get(deviceHeaderID); id != "" {
		return deviceSigFields{id: id, time: r.Header.Get(deviceHeaderTime), nonce: r.Header.Get(deviceHeaderNonce), sig: r.Header.Get(deviceHeaderSig), target: r.Header.Get(deviceHeaderTarget)}
	}
	q := r.URL.Query()
	return deviceSigFields{id: q.Get(deviceQueryID), time: q.Get(deviceQueryTime), nonce: q.Get(deviceQueryNonce), sig: q.Get(deviceQuerySig), target: q.Get(deviceQueryTarget), fromQuery: true}
}

// deviceTarget names the daemon a signature is for: "<machineId>" for the
// machine's default backend, "<machineId>/<backendId>" for a named one. The
// cloud strips /m/<id>/b/<id> before forwarding, so without this a relay
// could replay a signed request to another machine or backend where the same
// device is approved.
func deviceTargetMatches(target, machineID, backendID string) bool {
	if machineID == "" {
		return false
	}
	mid, bid, named := strings.Cut(target, "/")
	if mid != machineID {
		return false
	}
	// Backends that share one claim (wsl, win) must be named: a bare
	// machine target is only for a daemon without a backend id, or the relay
	// could replay it to the machine's other backend.
	if !named {
		return backendID == ""
	}
	return bid == backendID
}

// queryWithoutDeviceFields removes the signature parameters from a raw query
// while keeping every other parameter in its original order and encoding.
func queryWithoutDeviceFields(raw string) string {
	if raw == "" {
		return ""
	}
	var kept []string
	for _, part := range strings.Split(raw, "&") {
		key := part
		if i := strings.IndexByte(part, '='); i >= 0 {
			key = part[:i]
		}
		switch key {
		case deviceQueryID, deviceQueryTime, deviceQueryNonce, deviceQuerySig, deviceQueryTarget:
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "&")
}

// canonicalDeviceQuery is the query form a device signs: keys sorted, each
// key's values in their original order, every byte outside A-Z a-z 0-9 - _ . ~
// percent-encoded. The cloud re-encodes the query (url.Values.Encode) before
// forwarding, so the daemon cannot verify the client's raw bytes.
func canonicalDeviceQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, _ := url.ParseQuery(raw)
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range vals[k] {
			parts = append(parts, deviceQueryEscape(k)+"="+deviceQueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func deviceQueryEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

// deviceSigMessage is the exact byte string a device signs.
func deviceSigMessage(daemon, method, path, rawQuery, unix, nonce string, body []byte) []byte {
	target := path
	if q := canonicalDeviceQuery(rawQuery); q != "" {
		target += "?" + q
	}
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{deviceSigVersion, daemon, strings.ToUpper(method), target, unix, nonce, hex.EncodeToString(sum[:])}, "\n"))
}

// verifyDeviceSignature checks a P1363 (r||s) signature, the form WebCrypto
// produces, over SHA-256 of msg.
func verifyDeviceSignature(key *ecdsa.PublicKey, msg []byte, sigB64 string) bool {
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sigB64, "="))
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256(msg)
	r := new(big.Int).SetBytes(sig[:32])
	sv := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(key, digest[:], r, sv)
}

var (
	errDeviceUnsigned    = errors.New("request is not signed by a device")
	errDeviceUnknown     = errors.New("device is not approved on this computer")
	errDeviceStale       = errors.New("device signature is too old or from the future")
	errDeviceReplay      = errors.New("device signature was already used")
	errDeviceBadSig      = errors.New("device signature does not match")
	errDeviceBodyTooLong = errors.New("request body too large to verify")
	errDeviceWrongTarget = errors.New("device signature is for another machine or backend")
)

// verifyRequest checks r's device signature and, when it passes, returns the
// device and restores the body for the handler. The signature fields are
// removed from the query so handlers see the request the device made.
func (s *trustedDevices) verifyRequest(r *http.Request) (*trustedDevice, error) {
	f := deviceSigFieldsFrom(r)
	if f.id == "" || f.sig == "" || f.nonce == "" || f.time == "" || f.target == "" {
		return nil, errDeviceUnsigned
	}
	machineID, backendID := "", ""
	if s.identity != nil {
		machineID, backendID = s.identity()
	}
	if !deviceTargetMatches(f.target, machineID, backendID) {
		return nil, errDeviceWrongTarget
	}
	unix, err := strconv.ParseInt(f.time, 10, 64)
	if err != nil {
		return nil, errDeviceStale
	}
	now := s.now()
	if d := now.Sub(time.Unix(unix, 0)); d > deviceSigMaxSkew || d < -deviceSigMaxSkew {
		return nil, errDeviceStale
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, deviceSigMaxBody+1))
		_ = r.Body.Close()
		if err != nil {
			return nil, errDeviceBadSig
		}
		if len(body) > deviceSigMaxBody {
			return nil, errDeviceBodyTooLong
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	rawQuery := queryWithoutDeviceFields(r.URL.RawQuery)
	msg := deviceSigMessage(f.target, r.Method, r.URL.Path, rawQuery, f.time, f.nonce, body)

	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(f.id)
	if d == nil || d.Status != deviceStatusApproved || d.key == nil {
		return nil, errDeviceUnknown
	}
	if !verifyDeviceSignature(d.key, msg, f.sig) {
		return nil, errDeviceBadSig
	}
	nonceKey := d.ID + "/" + f.nonce
	s.pruneNoncesLocked(now)
	if _, seen := s.nonces[nonceKey]; seen {
		return nil, errDeviceReplay
	}
	s.nonces[nonceKey] = now.Add(2 * deviceSigMaxSkew)
	seen := now.UTC()
	d.LastSeenAt = &seen
	if f.fromQuery {
		r.URL.RawQuery = rawQuery
	}
	for _, h := range []string{deviceHeaderID, deviceHeaderTime, deviceHeaderNonce, deviceHeaderSig, deviceHeaderTarget} {
		r.Header.Del(h)
	}
	return d, nil
}

// relayDeviceExempt lists what an unapproved device needs to reach to ask for
// approval: enrolment, its own status, and the health probe.
func relayDeviceExempt(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == remoteDevicesEnrollPath:
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, remoteDevicesPathPrefix+"/") && strings.HasSuffix(r.URL.Path, remoteDevicesStatusSuffix):
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/api/health":
		return true
	}
	return false
}

// relayDeviceGate guards the relay tunnel. Under the "approved" policy every
// relayed request except enrolment must carry a valid device signature.
func (a *apiServer) relayDeviceGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if relayDeviceExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		device := ""
		if a.trustedDevices.policy() != remoteAccessApproved {
			// Not required, but a signed request from an approved device is
			// recorded as that device, so turning on approved-only later
			// doesn't cut it off.
			if r.Header.Get(deviceHeaderID) != "" || r.URL.Query().Get(deviceQueryID) != "" {
				if d, err := a.trustedDevices.verifyRequest(r); err == nil {
					device = d.ID
				}
			}
		} else {
			d, err := a.trustedDevices.verifyRequest(r)
			if err != nil {
				log.Printf("remote-devices: refused %s %s: %v", r.Method, r.URL.Path, err)
				if isPreviewPageRequest(r) {
					// A preview frame loads its pages itself, so it cannot sign
					// them: say why instead of showing raw JSON.
					writePreviewRefusedPage(w)
					return
				}
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":   "device_not_approved",
					"message": "This device isn't approved on that computer. Approve it there in Settings → Privacy & security.",
				})
				return
			}
			device = d.ID
		}
		if device != "" {
			r = r.WithContext(context.WithValue(r.Context(), relayVerifiedDeviceKey{}, device))
		}
		a.serveRevocable(w, r, device, next)
	})
}

type relayVerifiedDeviceKey struct{}

// relayVerifiedDevice reports the device whose signature relayDeviceGate
// verified on this very request, or "". Handlers that allow a narrow action
// over the relay to approved devices only read it; it is never set from
// anything the request itself carries.
func relayVerifiedDevice(r *http.Request) string {
	v, _ := r.Context().Value(relayVerifiedDeviceKey{}).(string)
	return v
}

// approvedDeviceName returns the name of device id while it is approved.
func (s *trustedDevices) approvedDeviceName(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil || d.Status != deviceStatusApproved {
		return "", false
	}
	return d.Name, true
}

// relayAllowed reports whether a relayed connection opened by device ("" =
// unsigned) may stay open under the current policy.
func (s *trustedDevices) relayAllowed(device string) bool {
	if s.policy() != remoteAccessApproved {
		return true
	}
	return device != "" && s.isApproved(device)
}

// serveRevocable serves a relayed request whose access can be withdrawn
// while it runs: a WebSocket or event stream opened under policy "any", or
// by a device that is later removed, ends as soon as "Only devices I
// approve" is turned on or the device is removed — not only new requests
// are refused. Event streams end through the request context; upgraded
// connections are closed directly.
func (a *apiServer) serveRevocable(w http.ResponseWriter, r *http.Request, device string, next http.Handler) {
	ctx, cancel := context.WithCancel(r.Context())
	rw := &revocableWriter{ResponseWriter: w, upgraded: make(chan struct{})}
	finished := make(chan struct{})
	go func() {
		upgraded := rw.upgraded
		var connDone <-chan struct{}
		for {
			changed := a.trustedDevices.changes()
			if !a.trustedDevices.relayAllowed(device) {
				log.Printf("remote-devices: access withdrawn; closing %s %s (device %q)", r.Method, r.URL.Path, device)
				cancel()
				rw.revoke()
				return
			}
			select {
			case <-changed:
			case <-finished:
				return
			case <-upgraded: // now watch the upgraded connection instead
				upgraded, connDone = nil, rw.hijackedDone()
			case <-connDone:
				return
			}
		}
	}()
	defer func() {
		if !rw.isHijacked() {
			close(finished)
			cancel()
		}
	}()
	next.ServeHTTP(rw, r.WithContext(ctx))
}

// revocableWriter remembers an upgraded connection so a revocation can close
// it; Flush, Hijack and Unwrap pass through.
type revocableWriter struct {
	http.ResponseWriter
	upgraded chan struct{} // closed once Hijack succeeds
	mu       sync.Mutex
	conn     net.Conn
	connDone chan struct{}
	revoked  bool // access withdrawn: an upgrade still in flight is closed
}

func (rw *revocableWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *revocableWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *revocableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	rw.mu.Lock()
	if rw.revoked {
		rw.mu.Unlock()
		_ = conn.Close()
		return nil, nil, errors.New("relay access withdrawn")
	}
	rw.connDone = make(chan struct{})
	tracked := &revocableConn{Conn: conn, done: rw.connDone}
	rw.conn = tracked
	rw.mu.Unlock()
	close(rw.upgraded)
	return tracked, brw, nil
}

func (rw *revocableWriter) isHijacked() bool {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.conn != nil
}

// hijackedDone is closed when the upgraded connection is closed.
func (rw *revocableWriter) hijackedDone() <-chan struct{} {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.connDone
}

// revoke closes the upgraded connection, or makes a later upgrade fail.
func (rw *revocableWriter) revoke() {
	rw.mu.Lock()
	rw.revoked = true
	conn := rw.conn
	rw.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

type revocableConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *revocableConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// ---- HTTP ------------------------------------------------------------------

type trustedDeviceView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Code       string     `json:"code"`
	CreatedAt  time.Time  `json:"createdAt"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
	// VerifiesComputer: the code was V2, so approving it also proved this
	// computer's identity to the device (private relay channel).
	VerifiesComputer bool `json:"verifiesComputer"`
	// DaemonIdentity is this computer's identity key (base64 SPKI), returned
	// to the enrolling device: it computes the approval code over both keys
	// and pins this one for the private relay channel.
	DaemonIdentity string `json:"daemonIdentity,omitempty"`
}

func viewOfDevice(d *trustedDevice) trustedDeviceView {
	return trustedDeviceView{ID: d.ID, Name: d.Name, Status: d.Status, Code: d.Code, CreatedAt: d.CreatedAt, ApprovedAt: d.ApprovedAt, LastSeenAt: d.LastSeenAt, VerifiesComputer: d.CodeVersion >= 2}
}

func (a *apiServer) handleListRemoteDevices(w http.ResponseWriter, _ *http.Request) {
	s := a.trustedDevices
	s.mu.Lock()
	out := struct {
		Policy  string              `json:"policy"`
		Devices []trustedDeviceView `json:"devices"`
	}{Policy: s.record.Policy, Devices: []trustedDeviceView{}}
	for _, d := range s.record.Devices {
		out.Devices = append(out.Devices, viewOfDevice(d))
	}
	s.mu.Unlock()
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].CreatedAt.Before(out.Devices[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

func (a *apiServer) handleEnrollRemoteDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		PublicKey string `json:"publicKey"`
		// CodeVersion 2: the client computes the approval code over both
		// keys and pins daemonIdentity (private relay channel).
		CodeVersion int `json:"codeVersion"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, deviceEnrollMaxBody)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	d, created, err := a.trustedDevices.enrollReport(body.Name, body.PublicKey, body.CodeVersion)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("remote-devices: enrolment %s (%s) is %s", d.ID, d.Name, d.Status)
	// A new request is announced on this computer, where it is approved; a
	// device asking again while it waits is not.
	if created && a.uiBus != nil && a.trustedDevices.announceNewRequest(time.Now()) {
		a.uiBus.publish(uiEvent{Type: "remote-device.pending", Text: d.Name})
	}
	a.trustedDevices.mu.Lock()
	v := viewOfDevice(d)
	a.trustedDevices.mu.Unlock()
	v.DaemonIdentity = a.trustedDevices.daemonSPKI()
	writeJSON(w, http.StatusOK, v)
}

func (a *apiServer) handleRemoteDeviceStatus(w http.ResponseWriter, r *http.Request) {
	s := a.trustedDevices
	s.mu.Lock()
	d := s.find(r.PathValue("id"))
	var v trustedDeviceView
	if d != nil {
		v = viewOfDevice(d)
	}
	s.mu.Unlock()
	if d == nil {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	v.DaemonIdentity = s.daemonSPKI()
	writeJSON(w, http.StatusOK, v)
}

// handleApproveRemoteDevice is a person's act at the computer itself.
func (a *apiServer) handleApproveRemoteDevice(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "devices can only be approved at the computer itself", http.StatusForbidden)
		return
	}
	if !a.requireHuman(w, r, "approve a remote device") {
		return
	}
	d, err := a.trustedDevices.approve(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	log.Printf("remote-devices: approved %s (%s)", d.ID, d.Name)
	a.trustedDevices.mu.Lock()
	v := viewOfDevice(d)
	a.trustedDevices.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// handleRemoveRemoteDevice revokes a device. Removing access is open to a
// person anywhere, including from another approved device.
func (a *apiServer) handleRemoveRemoteDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "remove a remote device") {
		return
	}
	if err := a.trustedDevices.remove(r.PathValue("id")); errors.Is(err, os.ErrNotExist) {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePutRemoteAccessPolicy switches between "any" and "approved". Both
// directions are local, person-only: loosening is a security change, and
// tightening from afar could lock the person out of their own machine.
func (a *apiServer) handlePutRemoteAccessPolicy(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "remote access can only be changed at the computer itself", http.StatusForbidden)
		return
	}
	if !a.requireHuman(w, r, "change remote access") {
		return
	}
	var body struct {
		Policy string `json:"policy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil ||
		(body.Policy != remoteAccessAny && body.Policy != remoteAccessApproved) {
		http.Error(w, `policy must be "any" or "approved"`, http.StatusBadRequest)
		return
	}
	if err := a.trustedDevices.setPolicy(body.Policy); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	log.Printf("remote-devices: remote access policy is now %s", body.Policy)
	a.handleListRemoteDevices(w, r)
}

// isPreviewPageRequest reports a browser loading a dev-server preview page
// (`/api/proxy/<port>/…`) as a document, as a preview frame does.
func isPreviewPageRequest(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.HasPrefix(r.URL.Path, "/api/proxy/") &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

const previewRefusedPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Preview not available here</title>
<style>body{font:15px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#111;color:#ddd}
main{max-width:30rem;padding:1.5rem}h1{font-size:1.1rem;margin:0 0 .5rem}</style></head>
<body><main><h1>This preview opens on the computer itself</h1>
<p>This computer only accepts devices you approve, and a preview loads its pages in a way this browser can't sign yet,
so it isn't available through the relay. Open the preview on the computer (in SoluTerminal there, or a browser on it).</p>
</main></body></html>`

func writePreviewRefusedPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, previewRefusedPage)
}
