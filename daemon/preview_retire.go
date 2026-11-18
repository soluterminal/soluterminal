package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Shareable preview links were removed. A machine that ran them (SOLU_PREVIEWS=1)
// may still have a "spawn" preview server running from the previous release,
// recorded in <dataDir>/previews.json with its pid and process start identity.
// Nothing manages it any more (no expiry, no stop route), so stop it at boot.
// Only a process whose live start identity matches the recorded one is
// touched, so a recycled pid is never killed.
//
// Once nothing recorded is still running, the registry is renamed to
// previews.json.retired so later boots never probe its pids again: on Linux
// the start identity is clock ticks since boot, which a later boot can
// repeat. The bundles and logs the previews kept (preview-cache, up to the
// old 500 MB cap, and preview-logs) are removed then too. Each server's
// descendants are written to the registry under their own identities before
// any signal, and whatever outlives the stop stays there for the next boot
// to retry (a descendant by its own pid), with the folders kept until then.
// Entries this step writes carry the Linux boot id, so a later boot never
// matches them to an unrelated process. A registry that can't be read or
// parsed is left alone, folders included.

var (
	retiredPreviewStopGrace = 5 * time.Second
	// retiredPreviewSettle is how long a killed process gets to disappear
	// before it counts as having outlived the stop pass.
	retiredPreviewSettle = 2 * time.Second
)

const retiredPreviewRegistry = "previews.json"

// retiredPreviewDirs are the data folders only the removed previews used.
var retiredPreviewDirs = []string{"preview-cache", "preview-logs"}

type retiredPreviewEntry struct {
	ID                   string `json:"id"`
	Mode                 string `json:"mode"`
	PID                  int    `json:"pid,omitempty"`
	ProcessStartIdentity string `json:"processStartIdentity,omitempty"`
	// Child marks a server's descendant this step recorded: it is stopped
	// by its own pid, never as a process-group leader.
	Child bool `json:"child,omitempty"`
	// BootID is the Linux kernel boot id when this step recorded the entry
	// (the previous release wrote none). An entry from another boot names
	// no live process: Linux start identities repeat across boots.
	BootID string `json:"bootId,omitempty"`
}

// readRetiredPreviewRegistry reads the registry. A missing file is no
// entries and no error; one that can't be read or parsed is an error.
func readRetiredPreviewRegistry(dataDir string) ([]*retiredPreviewEntry, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, retiredPreviewRegistry))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []*retiredPreviewEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// retiredPreviewBootID is the Linux kernel boot id ("" elsewhere).
func retiredPreviewBootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// verifiedRetiredPreviews keeps the spawn entries whose process is still the
// one the old release started. bootID is this boot's id ("" when unknown);
// an entry recorded under another boot is skipped.
func verifiedRetiredPreviews(entries []*retiredPreviewEntry, identity func(int) string, bootID string) []retiredPreviewEntry {
	var out []retiredPreviewEntry
	for _, e := range entries {
		if e == nil || e.Mode != "spawn" || e.PID <= 0 || e.ProcessStartIdentity == "" {
			continue
		}
		if e.BootID != "" && bootID != "" && e.BootID != bootID {
			continue
		}
		if live := identity(e.PID); live == "" || live != e.ProcessStartIdentity {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// retiredPreviewTargets returns the recorded spawn previews whose process is
// still the one the old release started (none when the registry is missing
// or unreadable).
func retiredPreviewTargets(dataDir string, identity func(int) string) []retiredPreviewEntry {
	entries, err := readRetiredPreviewRegistry(dataDir)
	if err != nil {
		return nil
	}
	return verifiedRetiredPreviews(entries, identity, retiredPreviewBootID())
}

// stopRetiredPreviewProcesses is the boot step (retireRemovedPreviews with
// the real process probes). Returns how many servers it stopped.
func stopRetiredPreviewProcesses(dataDir string) int {
	return retireRemovedPreviews(dataDir, retiredPreviewOps{
		identity: previewProcessStartIdentity,
		descendants: func(pid int) []int {
			return snapshotProcessTreeMaxAge(0).descendantPids(pid)
		},
		terminate: terminateChildTree,
		kill:      killChildTree,
		killPid:   hardKillPid,
		bootID:    retiredPreviewBootID,
	})
}

// retireRemovedPreviews stops each verified recorded process. Before any
// signal it records every target's descendants, under their own identities,
// in the registry: once a server exits its tree can't be found from its pid
// again. When nothing outlives the stop it renames the registry to
// previews.json.retired and removes preview-cache and preview-logs;
// otherwise the registry holds just the survivors and the folders stay for
// the next boot. If the registry can't be written first, nothing is
// signalled.
func retireRemovedPreviews(dataDir string, ops retiredPreviewOps) int {
	entries, err := readRetiredPreviewRegistry(dataDir)
	if err != nil {
		log.Printf("preview-retire: %s unreadable (%v); left as it is, preview folders kept", retiredPreviewRegistry, err)
		return 0
	}
	boot := ""
	if ops.bootID != nil {
		boot = ops.bootID()
	}
	reg := filepath.Join(dataDir, retiredPreviewRegistry)
	targets := verifiedRetiredPreviews(entries, ops.identity, boot)
	if len(targets) > 0 {
		var record []retiredPreviewEntry
		for i, e := range targets {
			if e.BootID == "" {
				e.BootID = boot
			}
			targets[i] = e
			record = append(record, e)
			if !e.Child {
				record = append(record, retiredPreviewDescendants(e, ops, boot)...)
			}
		}
		if err := writeRetiredPreviewRegistry(reg, record); err != nil {
			log.Printf("preview-retire: %s could not be updated before stopping (%v); nothing stopped this boot", retiredPreviewRegistry, err)
			return 0
		}
		// The recorded descendants are targets too.
		targets = record
	}
	stopped := 0
	var survivors []retiredPreviewEntry
	stoppedLeaders := map[string]bool{}
	for _, e := range targets {
		if e.Child && stoppedLeaders[retiredPreviewParent(e.ID)] {
			continue // its server's stop already dealt with it
		}
		ok, left := stopRetiredPreview(e, ops, boot)
		if ok {
			stopped++
			if !e.Child {
				stoppedLeaders[e.ID] = true
			}
		}
		survivors = append(survivors, left...)
	}
	survivors = settleRetiredPreviews(survivors, ops.identity)
	if len(survivors) > 0 {
		if err := writeRetiredPreviewRegistry(reg, survivors); err != nil {
			log.Printf("preview-retire: %d process(es) still running; %s keeps its earlier record (%v)", len(survivors), retiredPreviewRegistry, err)
		} else {
			log.Printf("preview-retire: %d process(es) still running; recorded in %s for the next boot", len(survivors), retiredPreviewRegistry)
		}
		return stopped
	}
	if err := os.Rename(reg, reg+".retired"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("preview-retire: could not retire %s: %v; preview folders kept", retiredPreviewRegistry, err)
		return stopped
	}
	for _, name := range retiredPreviewDirs {
		dir := filepath.Join(dataDir, name)
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("preview-retire: could not remove %s: %v", name, err)
			continue
		}
		log.Printf("preview-retire: removed %s (shareable previews were removed)", name)
	}
	return stopped
}

// retiredPreviewDescendants records server e's live descendants under their
// own identities, as Child entries named "<e.ID>/<pid>".
func retiredPreviewDescendants(e retiredPreviewEntry, ops retiredPreviewOps, boot string) []retiredPreviewEntry {
	var out []retiredPreviewEntry
	for _, pid := range ops.descendants(e.PID) {
		if pid == e.PID {
			continue
		}
		if id := ops.identity(pid); id != "" {
			out = append(out, retiredPreviewEntry{ID: fmt.Sprintf("%s/%d", e.ID, pid), Mode: "spawn", PID: pid, ProcessStartIdentity: id, Child: true, BootID: boot})
		}
	}
	return out
}

// retiredPreviewParent is the server id a Child entry's id starts with.
func retiredPreviewParent(childID string) string {
	if i := strings.LastIndexByte(childID, '/'); i > 0 {
		return childID[:i]
	}
	return childID
}

func writeRetiredPreviewRegistry(path string, entries []retiredPreviewEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(path, data, 0o600)
}

// settleRetiredPreviews gives killed processes retiredPreviewSettle to
// disappear and returns those whose identity still matches.
func settleRetiredPreviews(left []retiredPreviewEntry, identity func(int) string) []retiredPreviewEntry {
	deadline := time.Now().Add(retiredPreviewSettle)
	for {
		var still []retiredPreviewEntry
		for _, e := range left {
			if live := identity(e.PID); live != "" && live == e.ProcessStartIdentity {
				still = append(still, e)
			}
		}
		if len(still) == 0 || !time.Now().Before(deadline) {
			return still
		}
		left = still
		time.Sleep(100 * time.Millisecond)
	}
}

// retiredPreviewOps are the process probes and signals stopRetiredPreview
// uses; tests replace them.
type retiredPreviewOps struct {
	identity    func(int) string
	descendants func(int) []int // a fresh snapshot, root included
	terminate   func(*os.Process) error
	kill        func(*os.Process) error
	killPid     func(int) error
	bootID      func() string // nil: unknown
}

// stopRetiredPreview re-checks the process identity immediately before each
// signal, because an earlier target's grace period can give a later pid time
// to exit and be reused: a pid whose start identity no longer matches the
// record is left alone.
//
// A Child entry is one process: it is killed by its pid. A server's
// descendants (a dev server's workers, a shell's children) are captured with
// their own start identities before the first signal. The leader can exit on
// SIGTERM while a child that ignores it lives on, and on Windows terminate
// stops only the leader; once the leader is gone its tree can no longer be
// found from its pid. So after the grace period every captured descendant
// whose identity still matches is killed, whatever happened to the leader.
//
// It reports whether it signalled e, and every process (e or a captured
// descendant) whose identity still matched after its kill.
func stopRetiredPreview(e retiredPreviewEntry, ops retiredPreviewOps, boot string) (bool, []retiredPreviewEntry) {
	same := func() bool {
		live := ops.identity(e.PID)
		return live != "" && live == e.ProcessStartIdentity
	}
	if !same() {
		return false, nil
	}
	if e.Child {
		log.Printf("preview-retire: stopping %s (pid %d), left over from a preview server", e.ID, e.PID)
		_ = ops.killPid(e.PID)
		if same() {
			return true, []retiredPreviewEntry{e}
		}
		return true, nil
	}
	proc, err := os.FindProcess(e.PID)
	if err != nil {
		return false, nil
	}
	children := retiredPreviewDescendants(e, ops, boot)
	if !same() {
		return false, nil
	}
	log.Printf("preview-retire: stopping preview server %s (pid %d, %d descendants); shareable previews were removed", e.ID, e.PID, len(children))
	_ = ops.terminate(proc)
	deadline := time.Now().Add(retiredPreviewStopGrace)
	for same() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if same() {
		_ = ops.kill(proc)
	}
	var left []retiredPreviewEntry
	if same() {
		left = append(left, e)
	}
	for _, c := range children {
		if live := ops.identity(c.PID); live != "" && live == c.ProcessStartIdentity {
			_ = ops.killPid(c.PID)
			if live := ops.identity(c.PID); live != "" && live == c.ProcessStartIdentity {
				left = append(left, c)
			}
		}
	}
	return true, left
}
