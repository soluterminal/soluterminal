package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRollbackStateCompatibilityReadsMarkers(t *testing.T) {
	dir := t.TempDir()
	for store, version := range map[string]string{"workflow": `{"minReaderVersion":3}`, "compute": `{"minReaderVersion":2}`} {
		if err := os.MkdirAll(filepath.Join(dir, store), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, store, "version.json"), []byte(version), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := onDiskStateReaderRequirements(dir)
	err := checkRollbackStateCompatible(req, candidateCapabilities{Version: "1.0.0", RollbackHold: 1, StateReaders: map[string]int{"workflow": 2, "compute": 2}})
	var incompatible *rollbackIncompatibleError
	if !errors.As(err, &incompatible) || incompatible.Store != "workflow" || incompatible.RequiredReader != 3 || incompatible.TargetReader != 2 {
		t.Fatalf("compatibility error = %#v (%v)", incompatible, err)
	}
}

func TestProbeCandidateCapabilities(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is unix-only")
	}
	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{\"version\":\"1.0.0\",\"rollbackHold\":1,\"stateReaders\":{\"workflow\":2,\"compute\":3}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	caps, err := probeCandidateCapabilities(context.Background(), path)
	if err != nil || caps.Version != "1.0.0" || caps.StateReaders["compute"] != 3 {
		t.Fatalf("caps = %+v, err=%v", caps, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := probeCandidateCapabilities(context.Background(), path); err == nil {
		t.Fatal("failed capability probe was accepted")
	}
}

// A staged Go executable sees argv0 directly (a shell script replaces it).
// Keep rollback probes compatible with the strict daemon/shim dispatch.
func TestProbeCandidateCapabilitiesStagedDaemonArgv0(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a tiny staged daemon fixture")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	code := `package main
import ("fmt"; "os")
func main() {
 if os.Args[0] != "solud" || len(os.Args) != 2 || os.Args[1] != "--update-capabilities" { os.Exit(127) }
 fmt.Println("{\"version\":\"1.0.0\",\"rollbackHold\":1}")
}`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "solud.rollback")
	cmd := exec.Command("go", "build", "-o", bin, source)
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build staged fixture: %v: %s", err, out)
	}
	caps, err := probeCandidateCapabilities(context.Background(), bin)
	if err != nil || caps.Version != "1.0.0" {
		t.Fatalf("staged daemon probe: caps=%+v err=%v", caps, err)
	}
}
