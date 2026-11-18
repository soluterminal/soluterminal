package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostSamplerRecordsEachReadingInTheGrowthLedger(t *testing.T) {
	// Nothing here may touch the real Windows profile: the launcher policy goes
	// to a temp dir, no offender probe (PowerShell), no crash-dump rotation.
	dir := t.TempDir()
	prevPath := hostStorageWSLPath
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return []byte(t.TempDir() + "\n"), nil }
	t.Cleanup(func() { hostStorageWSLPath = prevPath })
	t.Setenv("SOLU_WORKFLOW_ROTATE_WSL_CRASH_DUMPS", "")
	s := newHostStorageService(dir, nil)
	s.hostOffender = nil
	s.hostStorage = &fakeHostStorageSource{sample: registeredHostStorageSample(`C:\Users\alice\AppData\Local\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}\ext4.vhdx`)}
	s.sampleHost(context.Background())
	var ledger string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			ledger += string(b)
		}
		return nil
	})
	if !strings.Contains(ledger, "sample") {
		t.Fatalf("no sample in the host storage ledger: %q", ledger)
	}
}

func TestHostSamplerIdlesWithoutAHostDisk(t *testing.T) {
	s := newHostStorageService(t.TempDir(), nil)
	s.hostStorage = nil // not inside WSL
	done := make(chan struct{})
	go func() { s.runSampler(context.Background(), time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sampler must return at once where there is no host disk")
	}
}

