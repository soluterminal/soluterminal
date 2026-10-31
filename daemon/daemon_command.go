package main

import (
	"os/exec"
	"runtime"
)

// Internal daemon launches must name their mode explicitly: on Linux an
// update can rename the running executable to solud.old before it exits.
func daemonCommand(executable string, args ...string) *exec.Cmd {
	cmd := exec.Command(executable, args...)
	cmd.Args[0] = "solud"
	return cmd
}

// Shell/agent command arrays cannot carry exec.Cmd's separate Path and argv0.
// Bash's exec builtin preserves the executable path while setting argv0.
func daemonCommandArgv(executable string, args ...string) []string {
	if runtime.GOOS == "windows" {
		// Windows does not rename the running image during a binary swap.
		return append([]string{executable}, args...)
	}
	return append([]string{"bash", "-c", `exec -a solud "$@"`, "--", executable}, args...)
}
