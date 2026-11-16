//go:build windows

package main

// maybeReexecCleanEnv is a no-op on Windows. The Windows daemon is launched by
// the Tauri desktop app from a clean GUI session (no CLAUDE_CODE_* inherited),
// and there is no syscall.Exec equivalent to re-exec in place. The agent-level
// stripInheritedClaudeEnv still sanitizes every pane command's env.
func maybeReexecCleanEnv() {}

// maybeDetachFromClaudeSession is a no-op on Windows (ST-118 is a Unix
// session and process-group problem).
func maybeDetachFromClaudeSession(addr, dataDir, backendID, shellVersion string) {}
