package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	deviceCommitLifetime = 10 * time.Minute
	deviceRevealLifetime = 30 * time.Minute
	// deviceFailedLifetime is how long a failed V3 request stays listed. It
	// still counts toward devicePendingLimit meanwhile, so failing on purpose
	// can't grow the list; then it is pruned, so burnt attempts can't fill
	// it for good.
	deviceFailedLifetime = 10 * time.Minute
)

var errDeviceEnrolment = errors.New("device comparison changed or expired; start again")
var errDeviceV3Fields = errors.New("invalid V3 fields")

type deviceV3Attempt struct {
	EnrolmentID string     `json:"enrolmentId,omitempty"`
	State       string     `json:"state"`
	Commit      string     `json:"commit,omitempty"`
	DaemonNonce string     `json:"daemonNonce,omitempty"`
	DeviceNonce string     `json:"deviceNonce,omitempty"`
	DaemonSPKI  string     `json:"daemonSPKI,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	RevealedAt  *time.Time `json:"revealedAt,omitempty"`
	Code        string     `json:"code,omitempty"`
}

type deviceV3Verified struct {
	At             time.Time `json:"at"`
	DeviceSPKIHash string    `json:"deviceSPKIHash"`
	DaemonSPKIHash string    `json:"daemonSPKIHash"`
}

type deviceV3AttemptView struct {
	EnrolmentID string `json:"enrolmentId,omitempty"`
	State       string `json:"state"`
	Code        string `json:"code,omitempty"`
}

func newDeviceV3Attempt(commit, computer string, now time.Time) (*deviceV3Attempt, error) {
	if !validDeviceNonceV3(commit) {
		return nil, errDeviceV3Fields
	}
	if computer == "" {
		return nil, errors.New("computer identity unavailable")
	}
	var randomID [16]byte
	var nonce [32]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return &deviceV3Attempt{
		EnrolmentID: "enr_" + base64.RawURLEncoding.EncodeToString(randomID[:]),
		State:       deviceStatusCommitted, Commit: commit, DaemonNonce: hex.EncodeToString(nonce[:]),
		DaemonSPKI: computer, CreatedAt: now.UTC(),
	}, nil
}

func validDeviceNonceV3(nonce string) bool {
	if len(nonce) != 64 || strings.ToLower(nonce) != nonce {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}

func validEnrolmentIDV3(id string) bool {
	if !strings.HasPrefix(id, "enr_") {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(id, "enr_"))
	return err == nil && len(b) == 16
}

func deviceCommitV3(id, nonce string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"SOLU-DEVICE-COMMIT-V3", id, nonce}, "\n")))
	return hex.EncodeToString(sum[:])
}

func deviceCodeV3(deviceSPKI, daemonSPKI, enrolmentID, deviceNonce, daemonNonce string) string {
	code := crockfordCode(sha256.Sum256([]byte(strings.Join([]string{
		"SOLU-DEVICE-CODE-V3", deviceSPKI, daemonSPKI, enrolmentID, deviceNonce, daemonNonce,
	}, "\n"))))
	feedbackPairingCodes.remember(code, time.Now().Add(deviceRevealLifetime))
	return code
}

func deviceSPKIHash(spki string) (string, error) {
	der, err := base64.StdEncoding.Strict().DecodeString(spki)
	if err != nil || base64.StdEncoding.EncodeToString(der) != spki {
		return "", errDeviceV3Fields
	}
	if _, err := parseDevicePublicKey(spki); err != nil {
		return "", errDeviceV3Fields
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

func newDeviceV3Verified(deviceSPKI, computerSPKI string, now time.Time) (*deviceV3Verified, error) {
	deviceHash, err := deviceSPKIHash(deviceSPKI)
	if err != nil {
		return nil, err
	}
	computerHash, err := deviceSPKIHash(computerSPKI)
	if err != nil {
		return nil, err
	}
	return &deviceV3Verified{At: now.UTC(), DeviceSPKIHash: deviceHash, DaemonSPKIHash: computerHash}, nil
}

// saveDevicesV3Locked publishes a new snapshot only after its durable write.
func (s *trustedDevices) saveDevicesV3Locked(devices []*trustedDevice) error {
	rec := s.record
	rec.Devices = devices
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return err
	}
	s.record = rec
	return nil
}

func (s *trustedDevices) pruneCommittedV3Locked() error {
	kept := make([]*trustedDevice, 0, len(s.record.Devices))
	for _, d := range s.record.Devices {
		if d.CodeVersion == 3 && ((d.Status == deviceStatusCommitted && !s.now().Before(d.CreatedAt.Add(deviceCommitLifetime))) ||
			(d.Status == deviceStatusPending && (d.RevealedAt == nil || !s.now().Before(d.RevealedAt.Add(deviceRevealLifetime)))) ||
			(d.Status == deviceStatusFailed && !s.now().Before(failedAt(d).Add(deviceFailedLifetime)))) {
			continue
		}
		// A V1/V2 request waits devicePendingLifetime: a full list refuses
		// new ones, so it must clear by itself.
		if d.CodeVersion < 3 && d.Status == deviceStatusPending && !s.now().Before(d.CreatedAt.Add(devicePendingLifetime)) {
			continue
		}
		if d.Verify != nil && d.Verify.expired(s.now()) {
			next := *d
			next.Verify = nil
			d = &next
		}
		kept = append(kept, d)
	}
	changed := len(kept) != len(s.record.Devices)
	if !changed {
		for i := range kept {
			if kept[i] != s.record.Devices[i] {
				changed = true
				break
			}
		}
	}
	if !changed {
		return nil
	}
	return s.saveDevicesV3Locked(kept)
}

// failedAt is when d's request failed: FailedAt, or its creation for records
// from before FailedAt was kept.
func failedAt(d *trustedDevice) time.Time {
	if d.FailedAt != nil {
		return *d.FailedAt
	}
	return d.CreatedAt
}

func (v *deviceV3Attempt) expired(now time.Time) bool {
	if v.State == deviceStatusCommitted {
		return !now.Before(v.CreatedAt.Add(deviceCommitLifetime))
	}
	if v.State == "revealed" {
		return v.RevealedAt == nil || !now.Before(v.RevealedAt.Add(deviceRevealLifetime))
	}
	return false
}

func (s *trustedDevices) replaceDeviceV3Locked(next *trustedDevice) error {
	devices := append([]*trustedDevice(nil), s.record.Devices...)
	for i, d := range devices {
		if d.ID == next.ID {
			devices[i] = next
			return s.saveDevicesV3Locked(devices)
		}
	}
	return errDeviceEnrolment
}

func (s *trustedDevices) commitDeviceV3(name, spki, commit string) (*trustedDevice, bool, error) {
	return s.commitDeviceV3From(name, spki, commit, false)
}

// commitDeviceV3From is commitDeviceV3; forwarded as in enrollReportFrom.
func (s *trustedDevices) commitDeviceV3From(name, spki, commit string, forwarded bool) (*trustedDevice, bool, error) {
	return s.commitDeviceV3Scoped(name, spki, commit, forwarded, "")
}

// commitDeviceV3Scoped is commitDeviceV3From for a device with scope (""
// for the person's own device). The scope is fixed here: a request for a key
// already approved keeps that record's scope, and one whose code was shown
// can't be replaced.
func (s *trustedDevices) commitDeviceV3Scoped(name, spki, commit string, forwarded bool, scope string) (*trustedDevice, bool, error) {
	key, err := parseDevicePublicKey(spki)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errDeviceV3Fields, err)
	}
	der, err := base64.StdEncoding.Strict().DecodeString(spki)
	if err != nil || base64.StdEncoding.EncodeToString(der) != spki || !validDeviceNonceV3(commit) {
		return nil, false, errDeviceV3Fields
	}
	computer := s.daemonSPKI()
	attempt, err := newDeviceV3Attempt(commit, computer, s.now())
	if err != nil {
		return nil, false, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Unnamed device"
	}
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	id := deviceID(spki)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		return nil, false, err
	}
	devices := append([]*trustedDevice(nil), s.record.Devices...)
	index := -1
	for i, d := range devices {
		if d.ID == id {
			// Same key, same scope: a key never moves between scopes by
			// asking again, whatever its record's state.
			if d.scope() != scope {
				return nil, false, errDeviceScopeMismatch
			}
			if d.Status == deviceStatusApproved || d.Status == deviceStatusApprovedAlerts {
				snapshot := *d
				return &snapshot, false, nil
			}
			// A request whose code was shown (any V1/V2 request, or a
			// revealed V3 one) waits as it is: anyone can send its key.
			if d.Status == deviceStatusPending {
				return nil, false, errDevicePendingKept
			}
			index = i
			break
		}
	}
	created := index == -1
	if created {
		if err := admitPendingLocked(devices, forwarded); err != nil {
			return nil, false, err
		}
		index = len(devices)
		devices = append(devices, nil)
	}
	d := &trustedDevice{
		ID: id, Name: name, PublicKey: spki, key: key, Forwarded: forwarded || (!created && devices[index].Forwarded),
		Status: deviceStatusCommitted, CodeVersion: 3, CreatedAt: s.now().UTC(),
		EnrolmentID: attempt.EnrolmentID,
		Commit:      commit, DaemonNonce: attempt.DaemonNonce, CodeIdentity: computer,
		Scope: scope,
	}
	devices[index] = d
	if err := s.saveDevicesV3Locked(devices); err != nil {
		return nil, false, err
	}
	return d, created, nil
}

func (s *trustedDevices) startVerificationV3(id, commit string) (trustedDeviceView, error) {
	computer := s.daemonSPKI()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		return trustedDeviceView{}, err
	}
	d := s.find(id)
	if d == nil || d.Status != deviceStatusApproved {
		return trustedDeviceView{}, errDeviceEnrolment
	}
	attempt, err := newDeviceV3Attempt(commit, computer, s.now())
	if err != nil {
		return trustedDeviceView{}, err
	}
	next := *d
	next.Verify = attempt
	if err := s.replaceDeviceV3Locked(&next); err != nil {
		return trustedDeviceView{}, err
	}
	return trustedDeviceView{ID: id, Status: deviceStatusCommitted, CodeVersion: 3, EnrolmentID: attempt.EnrolmentID,
		DaemonIdentity: computer, DaemonNonce: attempt.DaemonNonce}, nil
}

func (s *trustedDevices) revealDeviceV3(id, enrolmentID, nonce string) (trustedDeviceView, error) {
	computer := s.daemonSPKI()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		return trustedDeviceView{}, err
	}
	d := s.find(id)
	if d == nil || !validEnrolmentIDV3(enrolmentID) {
		return trustedDeviceView{}, errDeviceEnrolment
	}
	var attempt deviceV3Attempt
	verify := d.Status == deviceStatusApproved
	if verify {
		if d.Verify == nil {
			return trustedDeviceView{}, errDeviceEnrolment
		}
		attempt = *d.Verify
	} else {
		attempt = deviceV3Attempt{EnrolmentID: d.EnrolmentID, State: d.Status, Commit: d.Commit,
			DaemonNonce: d.DaemonNonce, DaemonSPKI: d.CodeIdentity}
	}
	if attempt.EnrolmentID != enrolmentID || attempt.State != deviceStatusCommitted {
		return trustedDeviceView{}, errDeviceEnrolment
	}
	next := *d
	expected := deviceCommitV3(id, nonce)
	if !validDeviceNonceV3(nonce) || subtle.ConstantTimeCompare([]byte(expected), []byte(attempt.Commit)) != 1 || computer != attempt.DaemonSPKI {
		if verify {
			next.Verify = &deviceV3Attempt{State: deviceStatusFailed}
		} else {
			failed := s.now().UTC()
			next.Status, next.Code, next.EnrolmentID, next.FailedAt = deviceStatusFailed, "", "", &failed
			next.Commit, next.DaemonNonce, next.DeviceNonce, next.CodeIdentity = "", "", "", ""
		}
		if err := s.replaceDeviceV3Locked(&next); err != nil {
			return trustedDeviceView{}, err
		}
		return trustedDeviceView{}, errDeviceEnrolment
	}
	now := s.now().UTC()
	code := deviceCodeV3(d.PublicKey, computer, enrolmentID, nonce, attempt.DaemonNonce)
	if verify {
		attempt.State, attempt.RevealedAt, attempt.DeviceNonce, attempt.Code = "revealed", &now, nonce, code
		next.Verify = &attempt
	} else {
		next.Status, next.RevealedAt, next.DeviceNonce, next.Code = deviceStatusPending, &now, nonce, code
	}
	if err := s.replaceDeviceV3Locked(&next); err != nil {
		return trustedDeviceView{}, err
	}
	v := viewOfDevice(&next)
	v.DaemonIdentity = computer
	return v, nil
}

func (s *trustedDevices) finishVerificationV3(id, enrolmentID string, reject bool) (trustedDeviceView, error) {
	computer := s.daemonSPKI()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pruneCommittedV3Locked(); err != nil {
		return trustedDeviceView{}, err
	}
	d := s.find(id)
	if d == nil || d.Status != deviceStatusApproved || d.Verify == nil || enrolmentID == "" || d.Verify.EnrolmentID != enrolmentID {
		return trustedDeviceView{}, errDeviceEnrolment
	}
	if !reject && (d.Verify.State != "revealed" || d.Verify.DaemonSPKI != computer) {
		return trustedDeviceView{}, errDeviceEnrolment
	}
	next := *d
	if reject {
		next.V3Verified = nil
	} else {
		proof, err := newDeviceV3Verified(d.PublicKey, computer, s.now())
		if err != nil {
			return trustedDeviceView{}, err
		}
		next.V3Verified = proof
	}
	next.Verify = nil
	if err := s.replaceDeviceV3Locked(&next); err != nil {
		return trustedDeviceView{}, err
	}
	v := viewOfDevice(&next)
	v.DaemonIdentity = computer
	return v, nil
}

func (s *trustedDevices) v3VerifiedForChannel(id, deviceSPKI, computerSPKI string) bool {
	deviceHash, err := deviceSPKIHash(deviceSPKI)
	if err != nil {
		return false
	}
	computerHash, err := deviceSPKIHash(computerSPKI)
	if err != nil {
		return false
	}
	computer := s.daemonSPKI()
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil || d.Status != deviceStatusApproved || d.PublicKey != deviceSPKI || computer != computerSPKI || d.V3Verified == nil {
		return false
	}
	matched := subtle.ConstantTimeCompare([]byte(d.V3Verified.DeviceSPKIHash), []byte(deviceHash)) &
		subtle.ConstantTimeCompare([]byte(d.V3Verified.DaemonSPKIHash), []byte(computerHash))
	return matched == 1
}
