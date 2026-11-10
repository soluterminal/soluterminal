package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestRemoveStaleClaimTempsKeepsAnInFlightSave(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(`{"token":"t"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale := write(claimTempPrefix+"111", time.Hour)
	fresh := write(claimTempPrefix+"222", time.Second)
	claim := write("claim.json", time.Hour)
	mirror := write("claim.json.tmp", time.Hour) // the desktop's own fixed-name copy
	if err := os.Mkdir(filepath.Join(dir, claimTempPrefix+"dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n := removeStaleClaimTemps(dir, now); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp still present: %v", err)
	}
	for _, path := range []string{fresh, claim, mirror, filepath.Join(dir, claimTempPrefix+"dir")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed: %v", path, err)
		}
	}
}
