package main

import (
	"context"
	"os/exec"
)

// hiddenCommand is exec.CommandContext for a background child, such as a probe
// or a CLI helper. On Windows the child gets no console window; a daemon with no
// console of its own would otherwise show one for every console program it runs.
// console_window_guard_test.go requires this or an equivalent for every spawn.
func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if err := prepareProviderExec(cmd); err != nil {
		cmd.Err = err
	}
	applyNoWindow(cmd)
	return cmd
}
