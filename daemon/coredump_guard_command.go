package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os/exec"

	gopty "github.com/aymanbagabas/go-pty"
)

const coreDumpGuardDiagnostic = "soluterminal: WSL core-dump guard:"
const coreDumpGuardExitCode = 125

// coreDumpGuardError identifies a failure to establish the requested policy.
// Callers must not fall back to an unguarded launch after this error.
type coreDumpGuardError struct {
	Executable string
	Op         string
	Err        error
}

func (e *coreDumpGuardError) Error() string {
	return fmt.Sprintf("%s %q: %s: %v", coreDumpGuardDiagnostic, e.Executable, e.Op, e.Err)
}
func (e *coreDumpGuardError) Unwrap() error { return e.Err }

// commandCoreDumpSpec adapts only commands whose exec boundary we own. In
// particular, an unknown Start implementation must never silently bypass policy.
func commandCoreDumpSpec(cmd interface{ Start() error }) (path string, argv []string, dir string, err error) {
	switch c := cmd.(type) {
	case *exec.Cmd:
		if c != nil {
			if err := prepareProviderExec(c); err != nil {
				return c.Path, nil, c.Dir, err
			}
			return c.Path, append([]string(nil), c.Args...), c.Dir, nil
		}
	case *gopty.Cmd:
		if c != nil {
			if len(c.Args) > 0 {
				bin, args, err := providerBatchArgv(c.Path, c.Args[1:], c.Env)
				if err != nil {
					return c.Path, nil, c.Dir, err
				}
				if bin != c.Path {
					c.Path, c.Args = bin, append([]string{bin}, args...)
				}
			}
			return c.Path, append([]string(nil), c.Args...), c.Dir, nil
		}
	}
	return fmt.Sprintf("%T", cmd), nil, "", errors.New("unsupported or nil command")
}

func guardedRun(cmd *exec.Cmd) error {
	if err := startWithoutCoreDumps(cmd); err != nil {
		return err
	}
	return waitOwnedCommand(cmd)
}

// guardedOutput follows exec.Cmd.Output, including its bounded ExitError.Stderr.
func guardedOutput(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	captureErr := cmd.Stderr == nil
	var stderr coreDumpProbeStderr
	if captureErr {
		cmd.Stderr = &stderr
	}
	err := guardedRun(cmd)
	if ee, ok := err.(*exec.ExitError); ok && captureErr {
		ee.Stderr = stderr.Bytes()
	}
	if captureErr {
		err = reportCoreDumpGuardFailure(cmd, err, stderr.Bytes())
	}
	return out.Bytes(), err
}

func guardedCombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if cmd.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := guardedRun(cmd)
	return out.Bytes(), reportCoreDumpGuardFailure(cmd, err, out.Bytes())
}

func reportCoreDumpGuardFailure(cmd *exec.Cmd, err error, captured []byte) error {
	if err == nil {
		return nil
	}
	var guardErr *coreDumpGuardError
	if errors.As(err, &guardErr) {
		log.Print(err)
		return err
	}
	if bytes.Contains(captured, []byte(coreDumpGuardDiagnostic)) {
		// Ordinary failed capability probes stay quiet. Keep ExitError reachable
		// while distinguishing a helper failure from an optional missing capability.
		err = &coreDumpGuardError{Executable: cmd.Args[0], Op: "child setup", Err: fmt.Errorf("%s: %w", bytes.TrimSpace(captured), err)}
		log.Print(err)
	}
	return err
}

// Keep the first and last 32 KiB, matching os/exec's Output stderr contract.
type coreDumpProbeStderr struct {
	prefix, suffix []byte
	omitted        int
}

func (b *coreDumpProbeStderr) Write(p []byte) (int, error) {
	const n = 32 << 10
	size := len(p)
	if len(b.prefix) < n {
		take := min(n-len(b.prefix), len(p))
		b.prefix = append(b.prefix, p[:take]...)
		p = p[take:]
	}
	b.suffix = append(b.suffix, p...)
	if len(b.suffix) > n {
		drop := len(b.suffix) - n
		b.omitted += drop
		copy(b.suffix, b.suffix[drop:])
		b.suffix = b.suffix[:n]
	}
	return size, nil
}
func (b *coreDumpProbeStderr) Bytes() []byte {
	out := append([]byte(nil), b.prefix...)
	if b.omitted > 0 {
		out = fmt.Appendf(out, "\n... omitting %d bytes ...\n", b.omitted)
	}
	return append(out, b.suffix...)
}
