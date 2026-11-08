package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveClaimRoundTripsPrivatelyWithoutTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := SaveClaim(dir, &Claim{Token: "machine-token", MachineID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateClaimToken(dir, "rotated-token"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClaim(dir)
	if err != nil || got == nil || got.Token != "rotated-token" || got.MachineID != "m1" {
		t.Fatalf("LoadClaim = %+v, %v", got, err)
	}
	info, err := os.Stat(claimFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("claim.json mode = %o, want 600", perm)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("data dir = %v, %v; want only claim.json", entries, err)
	}
}
