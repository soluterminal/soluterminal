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
	deviceStatusCommitted     = "committed"
	deviceStatusApproved      = "approved"
	deviceStatusFailed        = "failed"
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
	CodeVersion  int               `json:"codeVersion,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	ApprovedAt   *time.Time        `json:"approvedAt,omitempty"`
	LastSeenAt   *time.Time        `json:"lastSeenAt,omitempty"`
	EnrolmentID  string            `json:"enrolmentId,omitempty"`
	Commit       string            `json:"commit,omitempty"`
	DaemonNonce  string            `json:"daemonNonce,omitempty"`
	DeviceNonce  string            `json:"deviceNonce,omitempty"`
	CodeIdentity string            `json:"codeIdentity,omitempty"`
	RevealedAt   *time.Time        `json:"revealedAt,omitempty"`
	Verify       *deviceV3Attempt  `json:"verify,omitempty"`
	V3Verified   *deviceV3Verified `json:"v3Verified,omitempty"`
	// Surface is where the device enrolled from: "relay" or "local" (this
	// computer). Empty on older records.
	Surface string `json:"surface,omitempty"`
	// ApprovedVia says how it was approved: "first-device", "device:<id>"
	// (signed by an approved device), "relay-device:<id>" (an approved
	// device over the relay), "traced" (a browser traced at this computer)
	// or "headers" (browser headers alone, before approved devices were
	// required). Empty on older records.
	ApprovedVia string `json:"approvedVia,omitempty"`
	// ApprovedBy is the approved device that approved this one, and Lineage
	// every device above it in that chain, oldest first. Lineage is copied
	// at approval, so it outlives the devices it names. Empty when a person
	// at the computer, the first-device step or env recovery approved it,
	// and on older records, which are taken as untainted.
	ApprovedBy string   `json:"approvedBy,omitempty"`
	Lineage    []string `json:"lineage,omitempty"`

	key *ecdsa.PublicKey
}

const (
	deviceSurfaceRelay = "relay"
	deviceSurfaceLocal = "local"
)

var errDeviceAlreadyBootstrapped = errors.New("a device is already approved")

// errApproverGone: the device approving another was removed (or is no
// longer approved) before the approval was made.
var errApproverGone = errors.New("the approving device is no longer approved")

type trustedDevicesRecord struct {
	Policy  string           `json:"policy"`
	Devices []*trustedDevice `json:"devices"`
	// ApprovedEver counts the devices ever approved here; older records
	// lack it, and then the devices approved now are the count.
	ApprovedEver int `json:"approvedEver,omitempty"`
	// LastApprovedRemoval is when an approved device was last removed.
	LastApprovedRemoval *time.Time `json:"lastApprovedRemoval,omitempty"`
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

// deviceCode retains the V1/V2 comparison format for existing clients.
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
	if err := s.pruneCommittedV3Locked(); err != nil {
		return nil, false, err
	}
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
		if d.Status != deviceStatusApproved {
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

func (s *trustedDevices) approve(id string, enrolmentIDs ...string) (*trustedDevice, error) {
	enrolmentID := ""
	if len(enrolmentIDs) > 0 {
		enrolmentID = enrolmentIDs[0]
	}
	return s.approveWith(id, enrolmentID, false, "")
}

// approveWith approves id. With onlyFirst it succeeds only while no device
// is approved, decided under the same lock, so of two racing first
// approvals one gets errDeviceAlreadyBootstrapped.
func (s *trustedDevices) approveWith(id, enrolmentID string, onlyFirst bool, via string) (*trustedDevice, error) {
	computer := s.daemonSPKI()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		return nil, err
	}
	if onlyFirst {
		for _, d := range s.record.Devices {
			if d.Status == deviceStatusApproved {
				return nil, errDeviceAlreadyBootstrapped
			}
		}
	}
	// The approver is checked again here, under the lock its removal takes:
	// a device removed since it signed can't approve, nor pass on a chain
	// that no longer names its own approvers.
	if by := approverIn(via); by != "" {
		if p := s.find(by); p == nil || p.Status != deviceStatusApproved {
			return nil, errApproverGone
		}
	}
	d := s.find(id)
	if d == nil {
		if enrolmentID != "" {
			return nil, errDeviceEnrolment
		}
		return nil, os.ErrNotExist
	}
	if d.CodeVersion == 3 {
		if d.Status != deviceStatusPending || d.EnrolmentID != enrolmentID || enrolmentID == "" || d.CodeIdentity != computer {
			return nil, errDeviceEnrolment
		}
		next := *d
		now := s.now().UTC()
		proof, err := newDeviceV3Verified(d.PublicKey, computer, now)
		if err != nil {
			return nil, err
		}
		next.Status, next.ApprovedAt = deviceStatusApproved, &now
		next.V3Verified = proof
		next.Commit, next.DaemonNonce, next.DeviceNonce = "", "", ""
		s.recordApprovalLocked(&next, via)
		if err := s.replaceDeviceV3Locked(&next); err != nil {
			return nil, err
		}
		return &next, nil
	}
	if enrolmentID != "" {
		return nil, errDeviceEnrolment
	}
	now := s.now().UTC()
	d.Status, d.ApprovedAt = deviceStatusApproved, &now
	s.recordApprovalLocked(d, via)
	return d, s.saveLocked()
}

// recordApprovalLocked notes how d was approved, in the same save as the
// approval itself: a device approved by another inherits its approver's
// chain at once, so it can never act as untainted in between.
func (s *trustedDevices) recordApprovalLocked(d *trustedDevice, via string) {
	s.record.ApprovedEver = s.approvedEverLocked(d.ID) + 1
	if via == "" {
		return
	}
	d.ApprovedVia = via
	by := approverIn(via)
	if by == "" {
		return
	}
	d.ApprovedBy = by
	d.Lineage = []string{by}
	if p := s.find(by); p != nil {
		d.Lineage = append(append([]string(nil), p.Lineage...), by)
	}
}

// approverIn is the approving device an approvedVia names, or "".
func approverIn(via string) string {
	if rest, ok := strings.CutPrefix(via, "relay-device:"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(via, "device:"); ok {
		return rest
	}
	return ""
}

// approvedEverLocked is how many devices were ever approved here, not
// counting except (one being approved now).
func (s *trustedDevices) approvedEverLocked(except string) int {
	n := 0
	for _, d := range s.record.Devices {
		if d.Status == deviceStatusApproved && d.ID != except {
			n++
		}
	}
	return max(n, s.record.ApprovedEver)
}

// deviceDisplayName is the name a device is stored under.
func deviceDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Unnamed device"
	}
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	return name
}

// bootstrapLocal records the first device as approved, in one step, while no
// device is approved. The check and the write are under one lock, so two
// racing first devices can't both get in.
func (s *trustedDevices) bootstrapLocal(name, spkiB64, surface string) (*trustedDevice, error) {
	key, err := parseDevicePublicKey(spkiB64)
	if err != nil {
		return nil, err
	}
	computer := s.daemonSPKI()
	name = deviceDisplayName(name)
	id := deviceID(spkiB64)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.record.Devices {
		if d.Status == deviceStatusApproved {
			return nil, errDeviceAlreadyBootstrapped
		}
	}
	kept := s.record.Devices[:0]
	for _, d := range s.record.Devices {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	s.record.Devices = kept
	now := s.now().UTC()
	d := &trustedDevice{
		ID: id, Name: name, PublicKey: strings.TrimSpace(spkiB64), Status: deviceStatusApproved,
		Code: deviceCode(spkiB64, computer), CodeVersion: codeVersionOf(computer), CreatedAt: now, ApprovedAt: &now,
		Surface: surface, ApprovedVia: "first-device", key: key,
	}
	s.record.ApprovedEver = s.approvedEverLocked("") + 1
	s.record.Devices = append(s.record.Devices, d)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return d, nil
}

// noteApproval records how a device was approved.
func (s *trustedDevices) remove(id string) error {
	_, err := s.removeDevice(id, false)
	return err
}

var errLastApprovedDevice = errors.New("the last approved device")

// removeDevice removes id and returns what it removed. With keepOne it
// refuses to remove the last approved device, deciding under the same lock
// as the removal so two concurrent removals can't both pass.
func (s *trustedDevices) removeDevice(id string, keepOne bool) (*trustedDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keepOne {
		if d := s.find(id); d != nil && d.Status == deviceStatusApproved {
			approved := 0
			for _, other := range s.record.Devices {
				if other.Status == deviceStatusApproved {
					approved++
				}
			}
			if approved <= 1 {
				return nil, errLastApprovedDevice
			}
		}
	}
	for i, d := range s.record.Devices {
		if d.ID == id {
			if d.Status == deviceStatusApproved {
				s.record.ApprovedEver = s.approvedEverLocked("")
				now := s.now().UTC()
				s.record.LastApprovedRemoval = &now
			}
			s.record.Devices = append(s.record.Devices[:i], s.record.Devices[i+1:]...)
			s.notifyLocked()
			return d, s.saveLocked()
		}
	}
	return nil, os.ErrNotExist
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
	machineID, backendID := "", ""
	if s.identity != nil {
		machineID, backendID = s.identity()
	}
	return s.verifySigned(r, deviceSigCheck{
		targetOK: func(t string) bool { return deviceTargetMatches(t, machineID, backendID) },
		maxBody:  deviceSigMaxBody,
	})
}

// deviceSigCheck is what one signature check accepts.
type deviceSigCheck struct {
	targetOK func(string) bool
	maxBody  int64
	// keepOversize leaves a body over maxBody intact for the handler (the
	// request then counts as unsigned) instead of failing the request.
	keepOversize bool
	// possession verifies against this key instead of an approved device's:
	// a device proving it holds the key it is enrolling.
	possession *ecdsa.PublicKey
}

func (s *trustedDevices) verifySigned(r *http.Request, c deviceSigCheck) (*trustedDevice, error) {
	f := deviceSigFieldsFrom(r)
	if f.id == "" || f.sig == "" || f.nonce == "" || f.time == "" || f.target == "" {
		return nil, errDeviceUnsigned
	}
	if !c.targetOK(f.target) {
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
		if c.keepOversize && r.ContentLength > c.maxBody {
			return nil, errDeviceBodyTooLong
		}
		orig := r.Body
		body, err = io.ReadAll(io.LimitReader(orig, c.maxBody+1))
		if c.keepOversize && (err != nil || int64(len(body)) > c.maxBody) {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), orig), orig}
			if err != nil {
				return nil, errDeviceBadSig
			}
			return nil, errDeviceBodyTooLong
		}
		_ = orig.Close()
		if err != nil {
			return nil, errDeviceBadSig
		}
		if int64(len(body)) > c.maxBody {
			return nil, errDeviceBodyTooLong
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	rawQuery := queryWithoutDeviceFields(r.URL.RawQuery)
	msg := deviceSigMessage(f.target, r.Method, r.URL.Path, rawQuery, f.time, f.nonce, body)

	s.mu.Lock()
	defer s.mu.Unlock()
	var d *trustedDevice
	key := c.possession
	if key == nil {
		d = s.find(f.id)
		if d == nil || d.Status != deviceStatusApproved || d.key == nil {
			return nil, errDeviceUnknown
		}
		key = d.key
	}
	if !verifyDeviceSignature(key, msg, f.sig) {
		return nil, errDeviceBadSig
	}
	nonceKey := f.id + "/" + f.nonce
	s.pruneNoncesLocked(now)
	if _, seen := s.nonces[nonceKey]; seen {
		return nil, errDeviceReplay
	}
	s.nonces[nonceKey] = now.Add(2 * deviceSigMaxSkew)
	if d != nil {
		seen := now.UTC()
		d.LastSeenAt = &seen
	} else {
		d = &trustedDevice{ID: f.id}
	}
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
	case r.Method == http.MethodPost && r.URL.Path == remoteDevicesEnrollPath+"/reveal":
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
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Code        string     `json:"code,omitempty"`
	CodeVersion int        `json:"codeVersion"`
	EnrolmentID string     `json:"enrolmentId,omitempty"`
	DaemonNonce string     `json:"daemonNonce,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ApprovedAt  *time.Time `json:"approvedAt,omitempty"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`
	// VerifiesComputer: the code was V2, so approving it also proved this
	// computer's identity to the device (private relay channel).
	VerifiesComputer bool `json:"verifiesComputer"`
	// DaemonIdentity is this computer's identity key (base64 SPKI), returned
	// to the enrolling device: it computes the approval code over both keys
	// and pins this one for the private relay channel.
	DaemonIdentity string               `json:"daemonIdentity,omitempty"`
	Verify         *deviceV3AttemptView `json:"verify,omitempty"`
	V3Verified     *deviceV3Verified    `json:"v3Verified,omitempty"`
	Surface        string               `json:"surface,omitempty"`
	ApprovedVia    string               `json:"approvedVia,omitempty"`
}

func viewOfDevice(d *trustedDevice) trustedDeviceView {
	v := trustedDeviceView{ID: d.ID, Name: d.Name, Status: d.Status, Code: d.Code, CreatedAt: d.CreatedAt, ApprovedAt: d.ApprovedAt, LastSeenAt: d.LastSeenAt, VerifiesComputer: d.CodeVersion >= 2, CodeVersion: max(1, d.CodeVersion), EnrolmentID: d.EnrolmentID}
	if d.Status == deviceStatusCommitted {
		v.Code = ""
	}
	if d.Verify != nil {
		v.Code = ""
		v.Verify = &deviceV3AttemptView{State: d.Verify.State, EnrolmentID: d.Verify.EnrolmentID}
		if d.Verify.State == "revealed" {
			v.Verify.Code = d.Verify.Code
		}
	}
	v.V3Verified = d.V3Verified
	v.Surface, v.ApprovedVia = d.Surface, d.ApprovedVia
	return v
}

func (a *apiServer) handleListRemoteDevices(w http.ResponseWriter, _ *http.Request) {
	s := a.trustedDevices
	s.mu.Lock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		s.mu.Unlock()
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
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
	if !requestViaRelay(r) {
		if pid, ok := lookupPeerPID(r); ok && pid > 0 && peerIsAutomationBrowser(pid) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":   "automation_browser",
				"message": "A browser run by automation can't be enrolled as your device.",
			})
			return
		}
	}
	var body struct {
		Name      string `json:"name"`
		PublicKey string `json:"publicKey"`
		// CodeVersion 2: the client computes the approval code over both
		// keys and pins daemonIdentity (private relay channel).
		CodeVersion int    `json:"codeVersion"`
		Commit      string `json:"commit"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, deviceEnrollMaxBody))
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.CodeVersion < 0 || body.CodeVersion > 3 {
		http.Error(w, "unsupported code version", http.StatusBadRequest)
		return
	}
	if body.CodeVersion == 3 {
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
	}
	var d *trustedDevice
	var created bool
	var err error
	if body.CodeVersion == 3 {
		d, created, err = a.trustedDevices.commitDeviceV3(body.Name, body.PublicKey, body.Commit)
	} else {
		d, created, err = a.trustedDevices.enrollReport(body.Name, body.PublicKey, body.CodeVersion)
	}
	if err != nil {
		if body.CodeVersion == 3 {
			writeDeviceV3Error(w, err)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if created {
		a.trustedDevices.mu.Lock()
		d.Surface = deviceSurface(r)
		_ = a.trustedDevices.saveLocked()
		a.trustedDevices.mu.Unlock()
	}
	log.Printf("remote-devices: enrolment %s (%s) is %s", d.ID, d.Name, d.Status)
	// A new request is announced on this computer, where it is approved; a
	// device asking again while it waits is not.
	if created && a.uiBus != nil && a.trustedDevices.announceNewRequest(time.Now()) {
		a.uiBus.publish(uiEvent{Type: "remote-device.pending", Text: d.Name})
	}
	a.trustedDevices.mu.Lock()
	v := viewOfDevice(d)
	if body.CodeVersion == 3 && d.Status == deviceStatusCommitted {
		v.DaemonNonce = d.DaemonNonce
	}
	a.trustedDevices.mu.Unlock()
	v.DaemonIdentity = a.trustedDevices.daemonSPKI()
	writeJSON(w, http.StatusOK, v)
}

func (a *apiServer) handleRemoteDeviceStatus(w http.ResponseWriter, r *http.Request) {
	s := a.trustedDevices
	s.mu.Lock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		s.mu.Unlock()
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
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

// remoteVerifiedDevice is the approved device a request from the relay
// proved itself to be: by its signature (relayDeviceGate) or by the
// handshake of the private channel it arrived on. "" otherwise.
func remoteVerifiedDevice(r *http.Request) string {
	if d := relayVerifiedDevice(r); d != "" {
		return d
	}
	if relayTransport(r) {
		return privateRelayDevice(r)
	}
	return ""
}

// deviceManagementProven reports a caller that may manage devices once one
// is approved: an approved device (signed locally, signed over the relay, or
// on its private channel) or, outside require, a browser traced at this
// computer that isn't run by automation. Under require a browser known only
// by its executable's name doesn't count.
func deviceManagementProven(r *http.Request, caller callerIdentity, require bool) bool {
	return caller.Device != "" || remoteVerifiedDevice(r) != "" ||
		(!require && caller.Kind == callerHuman && caller.PID > 0 && !caller.Unproven)
}

// requireDeviceManagementProof refuses r unless no device is approved yet or
// the caller proved more than browser headers (deviceManagementProven).
// SOLU_HUMAN_PROOF=off, the recovery the person sets at the computer, also
// lets browser headers through (envRecovery), so losing every approved
// browser can't lock them out of approving a new one; that is logged, and
// an approval made that way is announced until acknowledged.
func (a *apiServer) requireDeviceManagementProof(w http.ResponseWriter, r *http.Request, caller callerIdentity, action string) (ok, envRecovery bool) {
	if a.approvedDeviceCount() == 0 || deviceManagementProven(r, caller, a.humanProofMode() == humanProofRequire) {
		return true, false
	}
	if a.humanProof != nil && a.humanProof.envOff && caller.Kind == callerHuman {
		log.Printf("remote-devices: %s on browser headers under %s=off (env-recovery, pid %d)", action, humanProofEnv, caller.PID)
		return true, true
	}
	log.Printf("remote-devices: refused %s without device proof (pid %d)", action, caller.PID)
	writeDeviceProofRequired(w, action)
	return false, false
}

func writeDeviceProofRequired(w http.ResponseWriter, action string) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   "device_required",
		"message": "This screen isn't an approved device yet, so it can't " + action + ". Open Settings → Privacy & security → Ask to approve this browser, then approve it from a browser or app you've already approved. If that button isn't shown here, use an already-approved device instead. If you're locked out, recovery steps are in Settings → Privacy & security on the computer.",
	})
}

// handleApproveRemoteDevice is a person's act at the computer itself, or
// from a browser or app the person already approved.
func (a *apiServer) handleApproveRemoteDevice(w http.ResponseWriter, r *http.Request) {
	relayDevice := remoteVerifiedDevice(r)
	if requestViaRelay(r) && relayDevice == "" {
		http.Error(w, "devices can only be approved at the computer itself or from an approved device", http.StatusForbidden)
		return
	}
	caller := a.identifyCaller(r)
	if !a.requireHuman(w, r, "approve a remote device") {
		return
	}
	via := "headers"
	switch {
	case relayDevice != "":
		via = "relay-device:" + relayDevice
	case caller.Device != "":
		via = "device:" + caller.Device
	case caller.PID > 0 && !caller.Unproven && a.humanProofMode() != humanProofRequire:
		via = "traced"
	}
	// Once any device is approved, in every mode, a new one is approved
	// from an approved device or, outside require, a browser traced at this
	// computer (under require a browser known only by its name doesn't count):
	// otherwise a program could approve its own key and keep it. Browser
	// headers alone approve only the first device (decided
	// atomically by approveWith) or, under the env recovery, any device;
	// both are announced until an approved device acknowledges them.
	onlyFirst := false
	if via == "headers" {
		ok, envRecovery := a.requireDeviceManagementProof(w, r, caller, "approve a device")
		if !ok {
			return
		}
		if envRecovery {
			via = "env-recovery"
		} else {
			via, onlyFirst = "first-device", true
		}
	}
	var body struct {
		EnrolmentID string `json:"enrolmentId"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !readDeviceV3JSON(w, r, &body) {
			return
		}
	}
	// A notice goes in before the approval it announces, so the device is
	// never approved without it, not even between the two saves; noticeMu
	// keeps an acknowledgement from landing in between.
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	noticed := via == "first-device" || via == "env-recovery"
	noticeAt := time.Now().UTC()
	if noticed {
		pending, ok := a.trustedDevices.deviceName(r.PathValue("id"))
		if !ok {
			http.Error(w, "unknown device", http.StatusNotFound)
			return
		}
		if err := a.humanProof.noteApprovalNotice(&trustedDevice{ID: r.PathValue("id"), Name: pending}, via, noticeAt); err != nil {
			log.Printf("human-proof: record %s approval: %v", via, err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	d, err := a.trustedDevices.approveWith(r.PathValue("id"), body.EnrolmentID, onlyFirst, via)
	// Withdrawn only if the device didn't become approved: a failed save
	// can leave it approved in memory, and then the notice stays.
	if noticed && (approvalNotMade(err) || (err != nil && !a.trustedDevices.isApproved(r.PathValue("id")))) {
		a.humanProof.dropNotice(r.PathValue("id"), noticeAt)
	}
	if errors.Is(err, errDeviceAlreadyBootstrapped) {
		writeDeviceProofRequired(w, "approve a device")
		return
	}
	if errors.Is(err, errApproverGone) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "device_required", "message": "The device approving this one is no longer approved."})
		return
	}
	if errors.Is(err, errDeviceEnrolment) {
		writeDeviceV3Error(w, err)
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	log.Printf("remote-devices: approved %s (%s) via %s", d.ID, d.Name, via)
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "remote-device.approved", Text: d.Name})
	}
	a.trustedDevices.mu.Lock()
	v := viewOfDevice(d)
	a.trustedDevices.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// handleRemoveRemoteDevice revokes a device: from an approved device (here
// or remote) or a browser traced at this computer once any is approved.
func (a *apiServer) handleRemoveRemoteDevice(w http.ResponseWriter, r *http.Request) {
	caller := a.identifyCaller(r)
	if !a.requireHuman(w, r, "remove a remote device") {
		return
	}
	// Removing the person's devices on browser headers alone would let any
	// program lock them out; once a device is approved it takes the same
	// proof as approving one.
	if ok, _ := a.requireDeviceManagementProof(w, r, caller, "remove a device"); !ok {
		return
	}
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	id := r.PathValue("id")
	remover := caller.Device
	if remover == "" {
		remover = remoteVerifiedDevice(r)
	}
	// Checked again under noticeMu: a device removed since it signed can't
	// remove others.
	if remover != "" && !a.trustedDevices.isApproved(remover) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "device_required", "message": "This browser or app is no longer approved."})
		return
	}
	// A device that is flagged, or was approved by one that is, leaves a
	// notice when it removes another approved device, so removing the
	// others can't hide where it came from. The notice is saved first and
	// withdrawn if the removal doesn't happen.
	var noticeAt time.Time
	if remover != "" && remover != id && a.trustedDevices.isApproved(id) && a.deviceFlagged(remover) {
		removerName, _ := a.trustedDevices.deviceName(remover)
		targetName, _ := a.trustedDevices.deviceName(id)
		noticeAt = time.Now().UTC()
		if err := a.humanProof.addNotice(humanProofNotice{ID: remover, Name: removerName, At: noticeAt, Via: "removed", Removed: targetName}); err != nil {
			log.Printf("human-proof: record removal by %s: %v", remover, err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	removed, err := a.trustedDevices.removeDevice(id, a.humanProofMode() == humanProofRequire)
	if err != nil && !noticeAt.IsZero() {
		if _, still := a.trustedDevices.deviceName(id); still {
			a.humanProof.dropNotice(remover, noticeAt)
		}
	}
	if errors.Is(err, errLastApprovedDevice) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "last_device",
			"message": "This is the last approved device. Stop requiring approved devices first, or approve another one.",
		})
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	log.Printf("remote-devices: removed %s (%s) by device %q, pid %d", removed.ID, removed.Name, remover, caller.PID)
	w.WriteHeader(http.StatusNoContent)
}

// deviceName is the stored name of device id, approved or not.
func (s *trustedDevices) deviceName(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return "", false
	}
	return d.Name, true
}

// handlePutRemoteAccessPolicy switches between "any" and "approved". Both
// directions are local, person-only: loosening is a security change, and
// tightening from afar could lock the person out of their own machine.
func (a *apiServer) handlePutRemoteAccessPolicy(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "remote access can only be changed at the computer itself", http.StatusForbidden)
		return
	}
	caller := a.identifyCaller(r)
	if !a.requireHuman(w, r, "change remote access") {
		return
	}
	// The policy outlives a later switch to approved devices only, so once a
	// device is approved it takes the same proof as managing devices.
	if ok, _ := a.requireDeviceManagementProof(w, r, caller, "change remote access"); !ok {
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
