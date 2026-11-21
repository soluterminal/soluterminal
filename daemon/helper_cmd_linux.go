//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"

	"soluterminal/helperproto"

	"golang.org/x/sys/unix"
)

// helperLogLineMax caps one helper stderr line (the host caps it too).
const helperLogLineMax = 16 << 10

// runHelperCommand is `solud helper --proto 1`: the stateless per-distro
// helper the Windows host starts through wsl.exe. stdin and stdout carry the
// yamux session; nothing else may touch stdout.
func runHelperCommand(args []string) int {
	// First of all: keep the real stdout for the session, and point fd 1 at
	// stderr, so a stray print anywhere can't corrupt the yamux stream.
	sessionOut, err := takeHelperStdout()
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: stdout: %v\n", err)
		return 2
	}
	// No SIGHUP SIG_IGN here: an ignored signal survives exec into the
	// agent and on into every pane process. The agent catches SIGHUP itself
	// under --capped-log (agent_capped_log.go).

	fs := flag.NewFlagSet("helper", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	proto := fs.Int("proto", 0, "helper protocol version (1)")
	if err := fs.Parse(args); err != nil || *proto != helperproto.Proto || fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "helper: usage: solud helper --proto %d\n", helperproto.Proto)
		return 2
	}

	exe, err := os.Executable()
	if err != nil {
		helperLogf("executable: %v", err)
		return 1
	}
	h := &linuxHelper{
		version: version,
		exe:     exe,
		uid:     os.Getuid(),
		getenv:  os.Getenv,
		logf:    helperLogf,
	}
	if u, err := user.Current(); err == nil {
		h.user, h.home = u.Username, u.HomeDir
	} else {
		h.user, h.home = strconv.Itoa(h.uid), os.Getenv("HOME")
	}
	sess, err := helperproto.NewHelperSession(helperStdio{r: os.Stdin, w: sessionOut}, h, helperproto.HelperOptions{Logf: helperLogf})
	if err != nil {
		helperLogf("session: %v", err)
		return 1
	}
	err = sess.Serve()
	switch {
	case err == nil, errors.Is(err, helperproto.ErrShutdown):
		return 0
	case helperproto.ErrorCode(err) == helperproto.CodeUnsafeRuntimeDir:
		helperLogf("refused the runtime dir: %v", err)
		return 3
	default:
		helperLogf("session ended: %v", err)
		return 1
	}
}

// takeHelperStdout returns a private duplicate of fd 1 (close-on-exec) and
// then makes fd 1 a duplicate of stderr.
func takeHelperStdout() (*os.File, error) {
	fd, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	if err := unix.Dup3(2, 1, 0); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	os.Stdout = os.Stderr
	return os.NewFile(uintptr(fd), "helper-session-out"), nil
}

type helperStdio struct {
	r io.Reader
	w io.WriteCloser
}

func (s helperStdio) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s helperStdio) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s helperStdio) Close() error                { return s.w.Close() }

// helperLogf writes one escaped line to stderr, which the host logs.
func helperLogf(format string, args ...any) {
	line := strconv.Quote(fmt.Sprintf(format, args...))
	line = line[1 : len(line)-1] // escaped, without the quotes
	if len(line) > helperLogLineMax-16 {
		line = line[:helperLogLineMax-16] + "…"
	}
	fmt.Fprintf(os.Stderr, "helper: %s\n", line)
}
