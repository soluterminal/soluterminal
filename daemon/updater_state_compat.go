package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

type candidateCapabilities struct {
	Version      string         `json:"version"`
	RollbackHold int            `json:"rollbackHold"`
	StateReaders map[string]int `json:"stateReaders"`
}

type rollbackIncompatibleError struct {
	Store          string
	RequiredReader int
	TargetReader   int
	TargetVersion  string
	Reason         string
}

func (e *rollbackIncompatibleError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("rollback to %s declined: %s; nothing was changed", e.TargetVersion, e.Reason)
	}
	return fmt.Sprintf("rollback to %s declined: %s state requires reader version %d but the target supports %d; the older daemon would be unable to read existing state, so nothing was changed",
		e.TargetVersion, e.Store, e.RequiredReader, e.TargetReader)
}

func onDiskStateReaderRequirements(dataDir string) map[string]int {
	req := make(map[string]int)
	for _, store := range []string{"workflow", "compute"} {
		path := filepath.Join(dataDir, store, "version.json")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			req[store] = math.MaxInt
			continue
		}
		var marker struct {
			MinReaderVersion int `json:"minReaderVersion"`
		}
		if err := json.Unmarshal(raw, &marker); err != nil || marker.MinReaderVersion < 0 {
			req[store] = math.MaxInt
			continue
		}
		if marker.MinReaderVersion > 0 {
			req[store] = marker.MinReaderVersion
		}
	}
	return req
}

func probeCandidateCapabilities(ctx context.Context, binPath string) (candidateCapabilities, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binPath, "--update-capabilities")
	// Staged candidates have a .rollback suffix. Explicitly invoke daemon
	// mode rather than letting that filename be interpreted as a pane shim.
	cmd.Args[0] = "solud"
	out, err := cmd.Output()
	if err != nil {
		return candidateCapabilities{}, fmt.Errorf("run rollback capability probe: %w", err)
	}
	var caps candidateCapabilities
	if err := json.Unmarshal(out, &caps); err != nil {
		return candidateCapabilities{}, fmt.Errorf("parse rollback capability probe: %w", err)
	}
	if caps.RollbackHold < 1 {
		return candidateCapabilities{}, fmt.Errorf("target predates persisted rollback holds")
	}
	if caps.StateReaders == nil {
		caps.StateReaders = map[string]int{}
	}
	return caps, nil
}

func checkRollbackStateCompatible(req map[string]int, caps candidateCapabilities) error {
	stores := make([]string, 0, len(req))
	for store := range req {
		stores = append(stores, store)
	}
	sort.Strings(stores)
	for _, store := range stores {
		if req[store] > caps.StateReaders[store] {
			return &rollbackIncompatibleError{
				Store: store, RequiredReader: req[store], TargetReader: caps.StateReaders[store], TargetVersion: caps.Version,
			}
		}
	}
	return nil
}

func updateCapabilitiesJSON(releaseVersion, dataDir string) ([]byte, error) {
	caps := candidateCapabilities{
		Version: releaseVersion, RollbackHold: 1,
		StateReaders: map[string]int{"workflow": storeReaderVersion, "compute": computeStoreReaderVersion},
	}
	return json.Marshal(caps)
}
