package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assertUpdateOnlyEngineRefused(t *testing.T, start func()) {
	t.Helper()
	defer func() {
		if got := recover(); got == nil || !strings.Contains(fmt.Sprint(got), updateOnlyRefusal) {
			t.Fatalf("engine refusal = %v, want the update-only refusal", got)
		}
	}()
	start()
}

func TestUpdateOnlyEngineConstructorsRefuseToStart(t *testing.T) {
	if !runInUpdateOnlyTestProcess(t) {
		return
	}
	// The real boot removes the marker from the live environment. The captured
	// process-start marker must still refuse every normal engine afterwards.
	t.Setenv(updateOnlyEnv, "")
	if err := os.Unsetenv(updateOnlyEnv); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "must-not-be-created")
	s, err := NewScheduleStore(data, nil, nil)
	if s != nil || err == nil || !strings.Contains(err.Error(), updateOnlyRefusal) {
		t.Fatalf("scheduler = %v, %v; want nil and the update-only refusal", s, err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		start func()
	}{
		{"manager", func() { NewManager(data) }},
		{"relay", func() { newRelaySupervisor(ctx, data, "", "host", nil) }},
		{"relay loop", func() { (&relaySupervisor{}).run() }},
		{"ports", func() { NewPortRegistry(nil) }},
		{"ports loop", func() { (&PortRegistry{}).Start(time.Second) }},
		{"pairing", func() { newPairingManager(ctx, "", data, nil) }},
		{"pairing start", func() { _, _ = (&pairingManager{}).start(ctx) }},
		{"checkin", func() { newInstallCheckin(data, "dev", nil, nil) }},
		{"checkin loop", func() { (&installCheckin{}).run(ctx) }},
		{"warden", func() { NewWarden(nil, nil, nil, nil, data, "") }},
		{"warden loop", func() { (&Warden{}).Start(ctx) }},
		{"memory guard", func() { newMemGuard(memGuardConfig{role: "daemon"}) }},
		{"activity", func() { startActivityWatcher(ctx, nil, nil, nil) }},
		{"update poll", func() { (&Updater{}).StartBackgroundPoll(ctx) }},
		{"entitlements loop", func() { (&teamEntitlements{}).run(ctx) }},
		{"Claude MCP registration", func() { ensureClaudeMcpRegistration("") }},
		{"Codex MCP registration", func() { ensureCodexMcpRegistration("") }},
		{"Claude hooks", func() { ensureClaudeHooksRegistered("") }},
		{"agent reaper", func() { reapOrphanAgents(nil, data) }},
	} {
		t.Run(tc.name, func(t *testing.T) { assertUpdateOnlyEngineRefused(t, tc.start) })
	}
	// Agent subcommands run before daemon boot. Their own memory protection
	// remains usable even when a normal daemon's env file passes on the marker.
	if g := newMemGuard(memGuardConfig{role: "agent", softMB: 1}); g == nil {
		t.Fatal("an agent's memory guard was disabled by an inherited marker")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("refused engines touched their data directory: %v", err)
	}
}
