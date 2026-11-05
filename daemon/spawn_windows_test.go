//go:build windows

package main

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

func TestApplyNoWindowKeepsExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup, HideWindow: true}
	applyNoWindow(cmd)
	if got, want := cmd.SysProcAttr.CreationFlags, createNewProcessGroup|createNoWindow; got != want {
		t.Fatalf("CreationFlags = %#x, want %#x", got, want)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("applyNoWindow dropped an existing attribute")
	}
}

func TestHiddenCommandSetsNoWindow(t *testing.T) {
	cmd := hiddenCommand(context.Background(), "cmd", "/c", "exit 0")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("SysProcAttr = %+v, want CREATE_NO_WINDOW", cmd.SysProcAttr)
	}
}
