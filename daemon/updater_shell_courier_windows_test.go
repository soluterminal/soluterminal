package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise cmd's real path expansion and Windows PowerShell's BOM/file handling;
// this script only writes a sentinel, never installs or launches an app.
func TestSilentCourierBatchRunsBOMFileOnWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Zoë O'Brien %! &")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	courier := filepath.Join(dir, "silent-update.ps1")
	script := "\ufeff$ErrorActionPreference='Stop'\r\nif (-not (Get-FileHash -Algorithm SHA256 -LiteralPath $PSCommandPath).Hash) { throw 'hash failed' }\r\nSet-Content -LiteralPath (Join-Path $PSScriptRoot 'result.txt') -Value 'courier-file-ok'\r\n"
	if err := os.WriteFile(courier, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmdPath := filepath.Join(dir, "silent-update.cmd")
	if err := os.WriteFile(cmdPath, []byte(silentCmdScript(courier)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cmd.exe")
	// Inherit Restricted without changing the user or machine policy. The
	// courier must supply its own process policy before loading the BOM file.
	cmd.Env = append(os.Environ(), "PSExecutionPolicyPreference=Restricted")
	// cmd /s strips only the outer pair, preserving quotes around a path
	// containing &, rather than relying on CreateProcess argument escaping.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c ""` + cmdPath + `""`}
	// cmd's start hands its inherited stdout and stderr to the detached
	// PowerShell. A pipe would stay open until PowerShell exits, so Wait
	// would hang on a slow start-up; a file is passed straight through and
	// Wait returns when cmd itself exits. The sentinel poll below covers
	// PowerShell's own time. The file sits outside t.TempDir: PowerShell
	// keeps its handle until it exits, which could outlast the temp dir's
	// cleanup on Windows.
	outFile, err := os.CreateTemp("", "solu-batch-output-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	outPath := outFile.Name()
	t.Cleanup(func() { _ = os.Remove(outPath) })
	cmd.Stdout, cmd.Stderr = outFile, outFile
	runErr := cmd.Run()
	_ = outFile.Close()
	if runErr != nil {
		out, _ := os.ReadFile(outPath)
		t.Fatalf("batch: %v: %s", runErr, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(dir, "result.txt"))
		if err == nil && strings.TrimSpace(string(b)) == "courier-file-ok" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("courier file did not write its sentinel")
}
