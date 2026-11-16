//go:build !linux && !darwin && !windows

package main

import "time"

// workerGitBirthTime is unknown here: new worktrees keep holding workers.
func workerGitBirthTime(string) (time.Time, bool) { return time.Time{}, false }

// workerGitLocalClock is unknown here.
func workerGitLocalClock(string) bool { return false }
