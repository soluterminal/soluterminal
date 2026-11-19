package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Run logs (one file per run) are kept for runLogRetention, within
// runLogMaxFiles and runLogMaxBytes per folder, newest first. The newest
// runLogKeepNewest always stay, whatever their age, so an automation that
// runs monthly still has its last reports to open. Before stage 2 only the
// newest 10 were kept, which a run every five minutes used up in an hour.
const (
	runLogKeepNewest = 10
	runLogRetention  = 30 * 24 * time.Hour
	runLogMaxFiles   = 2000
)

// runLogMaxBytes caps one folder of run logs. A var so tests can shrink it.
var runLogMaxBytes int64 = 64 << 20

var runLogNameRe = regexp.MustCompile(`^run-(\d+)\.log$`)

// writeRunLog persists one run's captured output into `dir` and prunes it
// (above). Returns the written basename, or "" on any failure — the run
// record then simply carries no log link (never fatal: the log is a nicety,
// the run result is the truth). Agent runs pass <agentHome>/runs; task runs
// pass <dataDir>/schedule-runs/<scheduleID>.
func writeRunLog(dir string, start time.Time, data []byte) string {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	name := fmt.Sprintf("run-%d.log", start.UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return ""
	}
	pruneRunLogs(dir, time.Now())
	return name
}

// pruneRunLogs applies the retention above to one folder of run logs.
func pruneRunLogs(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type runLog struct {
		ts   int64
		size int64
	}
	var logs []runLog
	for _, e := range entries {
		if m := runLogNameRe.FindStringSubmatch(e.Name()); m != nil {
			if ts, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				var size int64
				if fi, err := e.Info(); err == nil {
					size = fi.Size()
				}
				logs = append(logs, runLog{ts, size})
			}
		}
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].ts > logs[j].ts }) // newest first
	cutoff := now.Add(-runLogRetention).UnixMilli()
	var total int64
	for i, l := range logs {
		total += l.size
		if i < runLogKeepNewest {
			continue
		}
		if l.ts < cutoff || i >= runLogMaxFiles || total > runLogMaxBytes {
			_ = os.Remove(filepath.Join(dir, fmt.Sprintf("run-%d.log", l.ts)))
		}
	}
}
