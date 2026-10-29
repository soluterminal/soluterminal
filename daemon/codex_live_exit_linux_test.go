//go:build linux

package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCodexLiveClosedChannelReportsEngineSignal(t *testing.T) {
	err := exec.Command("sh", "-c", "kill -TERM $$").Run()
	if err == nil {
		t.Fatal("fixture was not terminated")
	}
	done := make(chan error, 1)
	done <- err
	got := codexLiveClosedChannel(done).Error()
	if !strings.Contains(got, "signal: terminated") || strings.Contains(got, "authentication") {
		t.Fatalf("engine signal hidden by authentication error: %s", got)
	}
	if len(done) != 1 {
		t.Fatal("exit reporting consumed the cleanup result")
	}
}

func TestCodexLiveClientExitWaitsForEngineSignal(t *testing.T) {
	err := exec.Command("sh", "-c", "kill -TERM $$").Run()
	done := make(chan error, 1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		done <- err
	}()
	got := codexLivePendingEngineExit(done)
	if got == nil || !strings.Contains(got.Error(), "signal: terminated") || len(done) != 1 {
		t.Fatalf("client-first exit lost engine signal or cleanup result: %v", got)
	}
}

func TestCodexLiveOrdinaryClientExitRemainsSuccessful(t *testing.T) {
	if err := codexLivePendingEngineExit(make(chan error, 1)); err != nil {
		t.Fatalf("ordinary terminal exit became an engine failure: %v", err)
	}
}

func TestCodexLiveCleanClientAndEngineExitRemainSuccessful(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	if err := codexLivePendingEngineExit(done); err != nil {
		t.Fatalf("clean engine exit after terminal exit became a failure: %v", err)
	}
	if len(done) != 1 {
		t.Fatal("clean exit reporting consumed the cleanup result")
	}
}

func TestCodexLiveClosedChannelDoesNotAssumeEngineExited(t *testing.T) {
	done := make(chan error, 1)
	got := codexLiveClosedChannel(done).Error()
	if got != "Codex engine connection closed; resume this conversation" {
		t.Fatalf("unproven engine termination reported: %s", got)
	}
}
