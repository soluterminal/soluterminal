package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceCodeV3RejectClearsExistingVerification(t *testing.T) {
	for _, revealed := range []bool{false, true} {
		name := "committed"
		if revealed {
			name = "revealed"
		}
		t.Run(name, func(t *testing.T) {
			a := trustedDevicesTestServer(t)
			s, d := a.trustedDevices, newTestDevice(t)
			approveTestDevice(t, a, d)
			id, nonce := deviceID(d.spki), strings.Repeat("01", 32)
			original := *s.find(id)
			first := startDeviceVerificationForTest(t, s, d, nonce)
			if _, err := s.revealDeviceV3(id, first.EnrolmentID, nonce); err != nil {
				t.Fatal(err)
			}
			if _, err := s.finishVerificationV3(id, first.EnrolmentID, false); err != nil {
				t.Fatal(err)
			}
			if !s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
				t.Fatal("setup did not verify the device")
			}
			proof := *s.find(id).V3Verified
			current := startDeviceVerificationForTest(t, s, d, nonce)
			if revealed {
				if _, err := s.revealDeviceV3(id, current.EnrolmentID, nonce); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.finishVerificationV3(id, first.EnrolmentID, true); !errors.Is(err, errDeviceEnrolment) {
				t.Fatalf("stale reject: %v", err)
			}
			if s.find(id).V3Verified == nil || *s.find(id).V3Verified != proof || s.find(id).Verify.EnrolmentID != current.EnrolmentID {
				t.Fatal("stale reject changed the current verification")
			}
			view, err := s.finishVerificationV3(id, current.EnrolmentID, true)
			if err != nil {
				t.Fatal(err)
			}
			if view.V3Verified != nil || view.Verify != nil || s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
				t.Fatal("rejected comparison retained verification")
			}
			reloaded := newTrustedDevices(filepath.Dir(s.path))
			reloaded.daemonKey = s.daemonKey
			rec := reloaded.find(id)
			if rec == nil || rec.V3Verified != nil || rec.Verify != nil || rec.Status != original.Status || !rec.ApprovedAt.Equal(*original.ApprovedAt) {
				t.Fatal("reject did not persist the cleared proof while preserving approval")
			}
			if reloaded.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
				t.Fatal("verification returned after reload")
			}
		})
	}
}

func TestDeviceCodeV3ApprovalRecordsVerification(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s, d := a.trustedDevices, newTestDevice(t)
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	nonce := strings.Repeat("01", 32)
	first := commitDeviceV3ForTest(t, s, d, nonce)
	current := commitDeviceV3ForTest(t, s, d, nonce)
	if _, err := s.approve(current.ID, current.EnrolmentID); !errors.Is(err, errDeviceEnrolment) {
		t.Fatalf("approve before reveal: %v", err)
	}
	if _, err := s.revealDeviceV3(current.ID, current.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.approve(current.ID, first.EnrolmentID); !errors.Is(err, errDeviceEnrolment) {
		t.Fatalf("stale approve: %v", err)
	}
	if s.find(current.ID).V3Verified != nil {
		t.Fatal("refused approval recorded a proof")
	}
	approved, err := s.approve(current.ID, current.EnrolmentID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.V3Verified == nil || !approved.V3Verified.At.Equal(now) || !approved.V3Verified.At.Equal(*approved.ApprovedAt) {
		t.Fatal("approval did not record verification at the approval time")
	}
	proof := *approved.V3Verified
	reloaded := newTrustedDevices(filepath.Dir(s.path))
	reloaded.daemonKey = s.daemonKey
	if rec := reloaded.find(current.ID); rec == nil || rec.V3Verified == nil || *rec.V3Verified != proof || !reloaded.v3VerifiedForChannel(current.ID, d.spki, s.daemonSPKI()) {
		t.Fatal("approval proof did not persist for the current keys")
	}
	now = now.Add(time.Second)
	verify := startDeviceVerificationForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(current.ID, verify.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.finishVerificationV3(current.ID, verify.EnrolmentID, false)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.V3Verified == nil || confirmed.V3Verified.DeviceSPKIHash != proof.DeviceSPKIHash || confirmed.V3Verified.DaemonSPKIHash != proof.DaemonSPKIHash || !confirmed.V3Verified.At.Equal(now) {
		t.Fatal("approval and verification confirmation recorded different key fingerprints")
	}
}
