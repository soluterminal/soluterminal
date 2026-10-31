package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Exported with the public daemon: pr-agent/fix-agent run here with their
// implementation files physically absent, as well as with unknown kinds.
func TestScheduleUnavailableBuildPreservesDisk(t *testing.T) {
	for _, target := range []struct{ kind, agent string }{{"agent", "pr-agent"}, {"agent", "fix-agent"}, {"agent", "future-agent"}, {"future-kind", "future-agent"}, {agentKindSchedule, "future-agent"}} {
		t.Run(target.kind+"/"+target.agent, func(t *testing.T) {
			if scheduleAvailable(&Schedule{Kind: target.kind, AgentID: target.agent}) {
				t.Skip("supported in this build")
			}
			for _, trigger := range []string{"cron", "expired", "one-time", "snoozed", "invalid-cron"} {
				t.Run(trigger, func(t *testing.T) {
					record := map[string]any{"id": "foreign", "kind": target.kind, "agentId": target.agent, "enabled": true, "cron": "* * * * *", "createdAt": "2020-01-01T00:00:00Z", "futureField": map[string]any{"keep": []any{"unknown", 42}}, "model": "deepseek-pro", "fireCount": 7, "errorCount": 3}
					switch trigger {
					case "expired":
						record["until"] = "2020-01-02T00:00:00Z"
					case "one-time":
						delete(record, "cron")
						record["runAt"] = "2020-01-02T00:00:00Z"
					case "snoozed":
						record["snoozeUntil"] = "2099-01-01T00:00:00Z"
					case "invalid-cron":
						record["cron"] = "future syntax"
					}
					raw, _ := json.Marshal(record)
					var want map[string]any
					if err := json.Unmarshal(raw, &want); err != nil {
						t.Fatal(err)
					}
					original, err := json.Marshal(map[string]any{"schedules": []any{record}, "order": []string{"foreign"}})
					if err != nil {
						t.Fatal(err)
					}
					dir := t.TempDir()
					path := filepath.Join(dir, "schedules.json")
					if err := os.WriteFile(path, original, 0600); err != nil {
						t.Fatal(err)
					}
					store, err := NewScheduleStore(dir, NewManager(dir), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer store.Stop()
					before, _ := store.Get("foreign")
					if len(store.entryIDs) != 0 || len(store.entryTimers) != 0 {
						t.Fatal("unsupported schedule wired into scheduler")
					}
					// Exercise a scheduler tick, including callbacks retained before a switch.
					store.fire("foreign")
					store.checkPreFire()
					after, _ := store.Get("foreign")
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("schedule changed: before=%+v after=%+v", before, after)
					}
					checks := []func() error{
						func() error { _, e := store.SetEnabled("foreign", false); return e },
						func() error { _, e := store.MarkRead("foreign", time.Now()); return e },
						func() error { _, e := store.FireNow("foreign"); return e },
						func() error { _, e := store.SnoozeNext("foreign"); return e },
						func() error {
							_, e := store.Update("foreign", ScheduleInput{Cron: "* * * * *", PaneID: "pane", Prompt: "hello"})
							return e
						},
						func() error { return store.Delete("foreign") },
					}
					for i, check := range checks {
						if err := check(); err == nil {
							t.Fatalf("mutation %d accepted", i)
						}
					}
					disk, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(original, disk) {
						t.Fatalf("unsupported schedule rewritten:\n%s", disk)
					}
					// Editing an ordinary schedule must retain unknown fields in this record.
					disabled := false
					if _, err := store.Create(ScheduleInput{Cron: "* * * * *", PaneID: "pane", Prompt: "hello", Enabled: &disabled}); err != nil {
						t.Fatal(err)
					}
					disk, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var saved struct {
						Schedules []map[string]any `json:"schedules"`
					}
					if err := json.Unmarshal(disk, &saved); err != nil {
						t.Fatal(err)
					}
					if len(saved.Schedules) != 2 || !reflect.DeepEqual(want, saved.Schedules[0]) {
						t.Fatalf("foreign fields changed: %s", disk)
					}
				})
			}
		})
	}
}
