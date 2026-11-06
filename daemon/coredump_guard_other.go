//go:build !linux

package main

// Outside Linux there is no rlimit manipulation. Command adaptation still
// normalizes recognized Windows npm shims to native argv before starting.
func startWithoutCoreDumps(cmd interface{ Start() error }) error {
	target, _, _, err := commandCoreDumpSpec(cmd)
	if err != nil {
		return &coreDumpGuardError{Executable: target, Op: "command adaptation", Err: err}
	}
	return cmd.Start()
}
