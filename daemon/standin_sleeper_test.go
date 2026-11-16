package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// standInSleeperSource sleeps for its first argument in seconds and, like
// sleep, exits with an error when given none. It never looks at its own name.
const standInSleeperSource = `package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "missing operand")
		os.Exit(1)
	}
	seconds, err := strconv.ParseFloat(os.Args[1], 64)
	if err != nil || seconds < 0 {
		fmt.Fprintln(os.Stderr, "invalid time interval", os.Args[1])
		os.Exit(1)
	}
	time.Sleep(time.Duration(seconds * float64(time.Second)))
}
`

var standIn struct {
	sync.Mutex
	dir, path string
}

// standInSleeper returns a program that sleeps for its first argument (in
// seconds) whatever it is named or however it is started. Tests copy or link it
// under the names the daemon recognises (codex, solud, chrome, the courier's
// app) and read those names back from /proc.
//
// It replaces copies of the host's sleep. Multi-call coreutils pick the applet
// from the name they are run as: uutils (the default coreutils since Ubuntu
// 25.10, so on GitHub's ubuntu-latest) or BusyBox exit at once when renamed.
// Keeping argv[0] as sleep is not enough either: a copy named solud started
// that way still died at once on GitHub's Ubuntu 26.04 runner. The stand-in is a
// Go program with no main.version, so the daemon reads it as a foreign binary
// of unknown version, like the copies of sleep it replaces.
//
// It is built once per test process and removed by TestMain. A failed build
// fails the test: a skip would hide that these process-tree tests never ran.
func standInSleeper(t testing.TB) string {
	t.Helper()
	standIn.Lock()
	defer standIn.Unlock()
	if standIn.path != "" {
		if _, err := os.Stat(standIn.path); err == nil {
			return standIn.path
		}
	}
	if standIn.dir == "" {
		dir, err := os.MkdirTemp("", "soluterminal-standin-sleeper-")
		if err != nil {
			t.Fatal(err)
		}
		standIn.dir = dir
	}
	src := filepath.Join(standIn.dir, "main.go")
	if err := os.MkdirAll(standIn.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(standInSleeperSource), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(standIn.dir, "sleeper")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, src)
	build.Dir = standIn.dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=on", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the stand-in sleeper: %v\n%s", err, out)
	}
	standIn.path = bin
	return bin
}

// removeStandInSleeper deletes the stand-in's build directory; TestMain calls
// it once every test has finished.
func removeStandInSleeper() {
	standIn.Lock()
	defer standIn.Unlock()
	if standIn.dir != "" {
		_ = os.RemoveAll(standIn.dir)
		standIn.dir, standIn.path = "", ""
	}
}
