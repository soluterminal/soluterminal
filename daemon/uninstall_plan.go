package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type uninstallEnv struct {
	HomeDir      string
	DataDir      string
	Executable   string
	GOOS         string
	CodexHome    string
	LocalAppData string
	StatFn       func(string) (os.FileInfo, error)
}

type uninstallEntry struct {
	Path             string `json:"path"`
	Exists           bool   `json:"exists"`
	Size             int64  `json:"size"`
	Category         string `json:"category"`
	Default          string `json:"default"`
	SuggestedCommand string `json:"suggestedCommand"`
}

type uninstallReport struct {
	Platform string           `json:"platform"`
	ReadOnly bool             `json:"readOnly"`
	Entries  []uninstallEntry `json:"entries"`
}

type registryArtifactInfo string

func (i registryArtifactInfo) Name() string     { return string(i) }
func (registryArtifactInfo) Size() int64        { return 0 }
func (registryArtifactInfo) Mode() os.FileMode  { return 0 }
func (registryArtifactInfo) ModTime() time.Time { return time.Time{} }
func (registryArtifactInfo) IsDir() bool        { return false }
func (registryArtifactInfo) Sys() any           { return nil }

func uninstallArtifactStat(path string) (os.FileInfo, error) {
	if runtime.GOOS == "windows" && path == `WSL:$HOME/.solu` {
		if err := hiddenCommand(context.Background(), "wsl.exe", "-e", "sh", "-lc", `test -e "$HOME/.solu"`).Run(); err != nil {
			return nil, os.ErrNotExist
		}
		return registryArtifactInfo(path), nil
	}
	if runtime.GOOS == "windows" && strings.HasPrefix(path, `HKCU\`) {
		key := `HKCU\` + strings.TrimPrefix(path, `HKCU\`)
		if err := hiddenCommand(context.Background(), "reg.exe", "query", key).Run(); err != nil {
			return nil, os.ErrNotExist
		}
		return registryArtifactInfo(filepath.Base(path)), nil
	}
	return os.Stat(path)
}

func posixShellLiteral(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
}

func powershellLiteral(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "''") + "'"
}

func removalCommand(goos, path string, recursive bool) string {
	if goos == "windows" {
		command := "Remove-Item -LiteralPath " + powershellLiteral(path) + " -Force"
		if recursive {
			command += " -Recurse"
		}
		return command
	}
	if recursive {
		return "rm -rf -- " + posixShellLiteral(path)
	}
	return "rm -f -- " + posixShellLiteral(path)
}

func uninstallPlan(env uninstallEnv) uninstallReport {
	if env.StatFn == nil {
		env.StatFn = os.Stat
	}
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	if env.DataDir == "" {
		env.DataDir = filepath.Join(env.HomeDir, ".solu")
	}
	if env.CodexHome == "" {
		env.CodexHome = filepath.Join(env.HomeDir, ".codex")
	}
	entries := make([]uninstallEntry, 0, 32)
	remove := func(path string, recursive bool) string {
		return removalCommand(env.GOOS, path, recursive)
	}
	add := func(path, category, def, command string) {
		entry := uninstallEntry{Path: path, Category: category, Default: def, SuggestedCommand: command}
		if info, err := env.StatFn(path); err == nil {
			entry.Exists = true
			entry.Size = info.Size()
		}
		entries = append(entries, entry)
	}

	switch env.GOOS {
	case "darwin":
		plist := filepath.Join(env.HomeDir, "Library", "LaunchAgents", "com.soluterminal.daemon.plist")
		add(plist, "service", "delete", "launchctl bootout gui/$(id -u)/com.soluterminal.daemon; rm -f "+posixShellLiteral(plist))
	case "windows":
		key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\soluterminal`
		add(key, "service", "delete", `Start-Process ms-settings:appsfeatures`)
	default:
		unit := filepath.Join(env.HomeDir, ".config", "systemd", "user", "soluterminal.service")
		add(unit, "service", "delete", "systemctl --user disable --now soluterminal.service; rm -f "+posixShellLiteral(unit)+"; systemctl --user daemon-reload")
	}

	if env.Executable != "" {
		add(env.Executable, "binary", "delete", remove(env.Executable, false))
		for _, suffix := range []string{".old", ".old.1", ".old.2", ".version", ".update-pending", ".successor-ok", ".restart-intent", ".rollback", ".rollback.tmp"} {
			path := env.Executable + suffix
			add(path, "binary", "delete", remove(path, false))
		}
		logPath := filepath.Join(filepath.Dir(env.Executable), "successor.log")
		add(logPath, "binary", "delete", remove(logPath, false))
	}

	addStateTree := func(root string) {
		add(root, "state", "keep", remove(root, true))
		for _, name := range []string{"shell-update", "logs", "workflow", "compute", "claim.json", "service.env", "update_rollback.json"} {
			path := filepath.Join(root, name)
			add(path, "state", "keep", remove(path, true))
		}
	}
	addStateTree(env.DataDir)
	if env.GOOS == "windows" {
		if env.LocalAppData != "" {
			shellState := filepath.Join(env.LocalAppData, "soluterminal")
			if shellState != env.DataDir {
				add(shellState, "state", "keep", remove(shellState, true))
				add(filepath.Join(shellState, "shell-update"), "state", "keep", remove(filepath.Join(shellState, "shell-update"), true))
				add(filepath.Join(shellState, "autostart.txt"), "state", "keep", remove(filepath.Join(shellState, "autostart.txt"), false))
			}
		}
		const wslState = `WSL:$HOME/.solu`
		add(wslState, "state", "keep", `wsl.exe -- sh -lc 'rm -rf -- "$HOME/.solu"'`)
	}
	for _, path := range []string{
		filepath.Join(env.HomeDir, ".claude"),
		filepath.Join(env.HomeDir, ".claude.json"),
		env.CodexHome,
		filepath.Join(env.CodexHome, "auth.json"),
		filepath.Join(env.HomeDir, ".config", "muse", "auth.json"),
	} {
		add(path, "credential", "keep", remove(path, true))
	}

	sort.SliceStable(entries, func(i, j int) bool {
		order := map[string]int{"service": 0, "binary": 1, "state": 2, "credential": 3}
		if order[entries[i].Category] != order[entries[j].Category] {
			return order[entries[i].Category] < order[entries[j].Category]
		}
		return entries[i].Path < entries[j].Path
	})
	return uninstallReport{Platform: env.GOOS, ReadOnly: true, Entries: entries}
}

func defaultUninstallEnv() uninstallEnv {
	home, _ := os.UserHomeDir()
	executable, _ := os.Executable()
	return uninstallEnv{
		HomeDir: home, DataDir: defaultDataDir(), Executable: executable, GOOS: runtime.GOOS,
		CodexHome: os.Getenv("CODEX_HOME"), LocalAppData: os.Getenv("LOCALAPPDATA"), StatFn: uninstallArtifactStat,
	}
}

func runUninstallPlanCommand(args []string) int {
	return runUninstallPlanCommandWithEnv(args, defaultUninstallEnv(), os.Stdout, os.Stderr)
}

func runUninstallPlanCommandWithEnv(args []string, env uninstallEnv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall-plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "uninstall-plan: unexpected arguments")
		}
		return 2
	}
	report := uninstallPlan(env)
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(stderr, "uninstall-plan:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "soluterminal uninstall inventory (%s) — READ ONLY; nothing was deleted\n", report.Platform)
	lastCategory := ""
	for _, entry := range report.Entries {
		if entry.Category != lastCategory {
			lastCategory = entry.Category
			label := strings.ToUpper(lastCategory)
			if lastCategory == "credential" {
				label += "S — KEEP BY DEFAULT"
			}
			fmt.Fprintf(stdout, "\n%s\n", label)
		}
		presence := "absent"
		if entry.Exists {
			presence = fmt.Sprintf("present, %d bytes", entry.Size)
		}
		fmt.Fprintf(stdout, "[%s] %s (%s)\n  if you choose delete: %s\n", strings.ToUpper(entry.Default), entry.Path, presence, entry.SuggestedCommand)
	}
	fmt.Fprintln(stdout, "\nReview each KEEP/DELETE choice, then run only the commands you intend. Credential contents were never read.")
	return 0
}
