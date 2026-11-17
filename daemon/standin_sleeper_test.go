package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	build := exec.Command(standInGoCommand(t), "build", "-trimpath", "-buildvcs=false", "-o", bin, src)
	build.Dir = standIn.dir
	// GOTOOLCHAIN=local: the go command found above builds with its own tools
	// and never switches to another release.
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=on", "GOFLAGS=", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the stand-in sleeper: %v\n%s", err, out)
	}
	standIn.path = bin
	return bin
}

// standInGoCommand returns the go command in this test's GOROOT (set by `go
// test`, or inherited when the daemon gate runs the compiled test binary), not
// whichever go comes first on PATH. A go command of another release reading
// that GOROOT runs a compiler that is not its own and fails: on the 0.0.1244
// testbox PATH's go1.25.13 met go.mod's go1.26.11 tools ("compile: version
// "go1.26.11" does not match go tool version "go1.25.13"").
func standInGoCommand(t testing.TB) string {
	t.Helper()
	root := runtime.GOROOT()
	if root == "" {
		// No GOROOT to disagree with: PATH's go uses its own tools.
		return "go"
	}
	goCmd := filepath.Join(root, "bin", "go")
	if runtime.GOOS == "windows" {
		goCmd += ".exe"
	}
	if _, err := os.Stat(goCmd); err != nil {
		t.Fatalf("the go command of this test's toolchain (GOROOT %s): %v", root, err)
	}
	return goCmd
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

// TestStandInGoCommandUsesItsOwnGOROOT: the stand-in's go command is the one
// in the GOROOT this test was handed, whatever go comes first on PATH, so the
// go command and the compiler it runs are one release.
func TestStandInGoCommandUsesItsOwnGOROOT(t *testing.T) {
	root := runtime.GOROOT()
	if root == "" {
		t.Skip("no GOROOT: PATH's go is the only go command and uses its own tools")
	}
	fake := t.TempDir()
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.bat"
	}
	if err := os.WriteFile(filepath.Join(fake, name), []byte("exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	goCmd := standInGoCommand(t)
	if !filepath.IsAbs(goCmd) || filepath.Dir(goCmd) == fake {
		t.Fatalf("stand-in go command %q comes from PATH", goCmd)
	}
	cmd := exec.Command(goCmd, "env", "GOROOT")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s env GOROOT: %v", goCmd, err)
	}
	if got := filepath.Clean(strings.TrimSpace(string(out))); got != filepath.Clean(root) {
		t.Fatalf("%s uses the tools in %s, not this test's GOROOT %s", goCmd, got, root)
	}
}
