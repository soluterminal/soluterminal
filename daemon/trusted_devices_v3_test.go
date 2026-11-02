package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceCodeV3KeyHashVector(t *testing.T) {
	raw, err := os.ReadFile("../web/src/lib/testdata/device-code-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		KeyHash struct{ SPKI, DerSHA256, NonCanonicalSPKI string }
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	got, err := deviceSPKIHash(v.KeyHash.SPKI)
	if err != nil || got != v.KeyHash.DerSHA256 {
		t.Fatalf("DER hash: %s %v", got, err)
	}
	canonical, _ := base64.StdEncoding.DecodeString(v.KeyHash.SPKI)
	alias, _ := base64.StdEncoding.DecodeString(v.KeyHash.NonCanonicalSPKI)
	if string(canonical) != string(alias) {
		t.Fatal("fixture alias does not encode the same DER")
	}
	for _, spki := range []string{v.KeyHash.NonCanonicalSPKI, strings.TrimRight(v.KeyHash.SPKI, "="), v.KeyHash.SPKI + "\n", "AA=="} {
		if _, err := deviceSPKIHash(spki); err == nil {
			t.Fatalf("accepted noncanonical or invalid SPKI %q", spki)
		}
	}
}

func TestDeviceCodeV3Vector(t *testing.T) {
	raw, err := os.ReadFile("../web/src/lib/testdata/device-code-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		DeviceSPKI, DaemonSPKI, EnrolmentID, DeviceNonce, DaemonNonce, DeviceID, Commit, Code string
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if _, err := parseDevicePublicKey(v.DeviceSPKI); err != nil {
		t.Fatal(err)
	}
	if _, err := parseDevicePublicKey(v.DaemonSPKI); err != nil {
		t.Fatal(err)
	}
	if got := deviceID(v.DeviceSPKI); got != v.DeviceID {
		t.Fatalf("device id: %s", got)
	}
	if got := deviceCommitV3(v.DeviceID, v.DeviceNonce); got != v.Commit {
		t.Fatalf("commit: %s", got)
	}
	if got := deviceCodeV3(v.DeviceSPKI, v.DaemonSPKI, v.EnrolmentID, v.DeviceNonce, v.DaemonNonce); got != v.Code {
		t.Fatalf("code: %s", got)
	}
}

func TestDeviceCodeV3CommitPersistence(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s := a.trustedDevices
	d := newTestDevice(t)
	commit := deviceCommitV3(deviceID(d.spki), strings.Repeat("01", 32))
	first, created, err := s.commitDeviceV3("phone", d.spki, commit)
	if err != nil || !created {
		t.Fatalf("commit: %v, created %v", err, created)
	}
	if first.Status != deviceStatusCommitted || first.Code != "" || !validEnrolmentIDV3(first.EnrolmentID) || !validDeviceNonceV3(first.DaemonNonce) {
		t.Fatalf("committed record: %+v", first)
	}
	view, err := json.Marshal(viewOfDevice(first))
	if err != nil || strings.Contains(string(view), `"code":`) || strings.Contains(string(view), `"daemonNonce":`) {
		t.Fatalf("committed view: %s, %v", view, err)
	}
	reloaded := newTrustedDevices(filepath.Dir(s.path)).find(first.ID)
	if reloaded == nil || reloaded.Commit != commit || reloaded.DaemonNonce != first.DaemonNonce {
		t.Fatal("commit was not durable before returning the computer nonce")
	}
	second, created, err := s.commitDeviceV3("phone", d.spki, commit)
	if err != nil || created || second.EnrolmentID == first.EnrolmentID || second.DaemonNonce == first.DaemonNonce {
		t.Fatalf("refresh: %v, created %v", err, created)
	}

	// Failed persistence leaves the previous snapshot available.
	before := s.find(first.ID)
	s.path = filepath.Join(t.TempDir(), "missing", "record.json")
	if _, _, err := s.commitDeviceV3("phone", d.spki, commit); err == nil {
		t.Fatal("commit succeeded without persistence")
	}
	if s.find(first.ID) != before {
		t.Fatal("failed write published an attempt")
	}
}

func TestDeviceCodeV3CommitLimitAndExpiry(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s := a.trustedDevices
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	legacy := newTestDevice(t)
	if _, err := s.enroll("legacy", legacy.spki, 2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	for i := 0; i < devicePendingLimit; i++ {
		d := newTestDevice(t)
		if _, _, err := s.commitDeviceV3("phone", d.spki, deviceCommitV3(deviceID(d.spki), strings.Repeat("01", 32))); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.record.Devices) != devicePendingLimit || s.find(deviceID(legacy.spki)) != nil {
		t.Fatal("committed and pending records did not share the queue limit")
	}
	now = now.Add(deviceCommitLifetime)
	s.mu.Lock()
	err := s.pruneCommittedV3Locked()
	s.mu.Unlock()
	if err != nil || len(s.record.Devices) != 0 {
		t.Fatalf("expiry: %v, %d records", err, len(s.record.Devices))
	}
}

func TestDeviceCodeV3CommitFields(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	for _, commit := range []string{"", strings.Repeat("0", 63), strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		if _, _, err := a.trustedDevices.commitDeviceV3("phone", d.spki, commit); err == nil {
			t.Fatalf("accepted commitment %q", commit)
		}
	}
	if _, _, err := a.trustedDevices.commitDeviceV3("phone", d.spki+"\n", strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted noncanonical SPKI")
	}
	if len(a.trustedDevices.record.Devices) != 0 {
		t.Fatal("invalid fields created a record")
	}
}
