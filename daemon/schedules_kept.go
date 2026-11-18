package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The managed-agents removal (agents_removed.go) left two records in the data
// folder, and some imported schedules it could not export. These helpers name
// those records and let the person delete a kept schedule from the public
// schedules API, so they live outside the private agents files.

const (
	agentsExportFile = "agents-export.md"
	// agentsExportKeptFile records each kept imported schedule the person
	// deletes, before it is deleted (appended, never overwritten).
	agentsExportKeptFile = "agents-export-kept.md"
)

func agentsExportDisplayPath(dataDir string) string {
	return dataFileDisplayPath(dataDir, agentsExportFile)
}

// dataFileDisplayPath is dataDir/name as the person reads it (~/ for home).
func dataFileDisplayPath(dataDir, name string) string {
	p := filepath.Join(dataDir, name)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// exportLine collapses whitespace and clamps, for list items in the export.
func exportLine(s string, max int) string {
	return clampRunes(strings.Join(strings.Fields(s), " "), max)
}

// fenceSafe keeps stored text from closing the fenced block it sits in.
func fenceSafe(s string) string {
	return strings.ReplaceAll(s, "```", "`​``")
}

// keptImportedSchedule reports whether s is a task schedule imported into a
// managed-agent card that stage 2 kept, because the export does not record
// its prompt. It is listed but never runs (the removal's run gate refuses
// it); only the person can delete it.
func keptImportedSchedule(s Schedule) bool {
	return s.Kind == "task" && s.OwnedByAgentID != ""
}

// scheduleListed reports whether the person's schedule list and GET show s:
// every schedule except one a removed managed agent owned, unless it is a
// kept imported schedule (listed so the person can delete it).
func scheduleListed(s Schedule) bool {
	if s.isAgentRun() {
		return false
	}
	return s.OwnedByAgentID == "" || keptImportedSchedule(s)
}

// openKeptExport opens agents-export-kept.md for appending. A test seam.
var openKeptExport = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
}

// appendKeptScheduleRecord appends kept schedule s, prompt included, to
// dataDir/agents-export-kept.md: created 0600, only ever appended to, and
// synced to disk before it returns. Any error means nothing may be deleted.
func appendKeptScheduleRecord(dataDir string, s Schedule, now time.Time) error {
	path := filepath.Join(dataDir, agentsExportKeptFile)
	_, statErr := os.Lstat(path)
	f, err := openKeptExport(path)
	if err != nil {
		return err
	}
	var b strings.Builder
	if errors.Is(statErr, fs.ErrNotExist) {
		b.WriteString("# Schedules kept from removed managed agents\n\nEach entry was imported into a managed-agent card that SoluTerminal no longer has. You deleted it; its prompt is below.\n")
	}
	fmt.Fprintf(&b, "\n## %s\n\n- Schedule %s (%s), imported into the removed agent %s. Deleted by you on %s.\n", exportLine(s.Name, 80), s.ID, s.Cron, s.OwnedByAgentID, now.UTC().Format(time.RFC3339))
	if s.Cwd != "" {
		fmt.Fprintf(&b, "- Folder: %s\n", s.Cwd)
	}
	b.WriteString("\n```text\n" + fenceSafe(s.Prompt) + "\n```\n")
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// deleteKeptImportedSchedule removes kept imported schedule id from the store.
// Like stage 2 it keeps the schedule's run logs on disk. ok is false when id is
// not (or no longer) a kept imported schedule.
func deleteKeptImportedSchedule(scheds *ScheduleStore, id string) (ok bool, err error) {
	scheds.mu.Lock()
	s, found := scheds.items[id]
	if !found || !keptImportedSchedule(*s) {
		scheds.mu.Unlock()
		return false, nil
	}
	gone := *s
	scheds.unscheduleUnlocked(id)
	delete(scheds.items, id)
	scheds.order = removeString(scheds.order, id)
	err = scheds.saveUnlocked()
	if err == nil {
		scheds.recordDeletedUnlocked(gone, scheduleActorPerson, time.Now())
	}
	scheds.mu.Unlock()
	scheds.notifyChanged()
	return true, err
}
