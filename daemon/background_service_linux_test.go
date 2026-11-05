//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServiceNATGatewayAndDesktopOrigin(t *testing.T) {
	routes := "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 00000000 010214AC 0003 0 0 100 00000000\n"
	if !serviceNATGateway("172.20.2.1:1234", routes) || serviceNATGateway("192.168.1.4:1234", routes) {
		t.Fatal("gateway match is not exact")
	}
	for _, c := range []struct {
		backend, mode string
		signed, want  bool
	}{
		{"wsl", "nat", true, true}, {"wsl", "nat", false, false}, {"wsl", "mirrored", true, false}, {"host", "nat", true, false},
	} {
		if got := serviceGatewayAllowed(c.backend, c.signed, c.mode, "172.20.2.1:1234", routes); got != c.want {
			t.Fatalf("%+v got %v", c, got)
		}
	}

}

func TestBackgroundServiceFixtureProcess(t *testing.T) {
	if os.Getenv("SOLU_SERVICE_TEST_CHILD") != "1" {
		return
	}
	loadEnvFile(os.Getenv("SOLU_SERVICE_FIXTURE_ENV"))
	if err := os.WriteFile(os.Getenv("SOLU_SERVICE_FIXTURE_MARKER"), []byte(os.Getenv("ST7_ENV_RECOVERY")), 0600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestWSLRestartProofStopsOnlyDaemonAndExistingLoopRelaunches(t *testing.T) {
	requireTools(t, "bash", "flock", "getent", "awk", "sha256sum", "stat")
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	installed := filepath.Join(home, ".local/share/soluterminal")
	if err := os.MkdirAll(installed, 0700); err != nil {
		t.Fatal(err)
	}
	envDir := filepath.Join(home, ".solu")
	if err := os.MkdirAll(envDir, 0700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(envDir, "env")
	if err := os.WriteFile(envFile, []byte("ST7_ENV_RECOVERY=before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "env-loaded")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(installed, "solud"), os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err = dst.Close(); err != nil {
		t.Fatal(err)
	}
	// Real supervisor lock, PID files and process identities; use the shipped
	// wait/relaunch loop, with its unrelated staging/probe functions stubbed.
	body := renderWSLStartScript(t)
	at := strings.Index(body, "restarts=0\nwhile true; do\n")
	end := strings.Index(body[at:], "\ndone\n")
	loop := body[at : at+end+len("\ndone\n")]
	script := fmt.Sprintf(`#!/bin/bash
set -e
DAEMON_DIR=%q
PIDFILE="$DAEMON_DIR/solud-wsl.pid"
exec 9>"$DAEMON_DIR/start-wsl.lock"
flock -n 9
echo $$ > "$DAEMON_DIR/start-wsl.pid"
launch_daemon() { SOLU_SERVICE_TEST_CHILD=1 "$DAEMON_DIR/solud" -test.run '^TestBackgroundServiceFixtureProcess$' -- --backend-id wsl 9>&- & daemon_pid=$!; echo "$daemon_pid" > "$PIDFILE"; }
await_daemon() { set +e; wait "$daemon_pid"; status=$?; set -e; }
stop_port_holder() { :; }
daemon_bind_failed() { return 1; }
rotate_daemon_log_if_big() { :; }
trap 'kill "$daemon_pid" 2>/dev/null || true; exit' TERM INT
launch_daemon
`, installed) + loop
	supPath := filepath.Join(dir, "start-wsl.sh")
	if err := os.WriteFile(supPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sup := exec.Command("bash", supPath)
	sup.Env = append(os.Environ(), "SOLU_SERVICE_FIXTURE_ENV="+envDir, "SOLU_SERVICE_FIXTURE_MARKER="+marker)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Process.Signal(syscall.SIGTERM); _ = sup.Wait() })
	pane := exec.Command("sleep", "60")
	if err := pane.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pane.Process.Kill(); _ = pane.Wait() }()
	proofScript := strings.Replace(backgroundServiceWSLScript, `home="$(getent passwd "$(id -u)" | cut -d: -f6)"`, fmt.Sprintf("home=%q", home), 1)
	inspect := func() (string, error) {
		out, e := exec.Command("bash", "-c", proofScript, "test-proof", "inspect").CombinedOutput()
		return strings.TrimSpace(string(out)), e
	}
	var proof string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		proof, err = inspect()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("ownership proof failed: %v %s", err, proof)
	}
	parts := strings.Fields(proof)
	if len(parts) != 2 {
		t.Fatalf("proof: %q", proof)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		if string(b) == "before" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if b, _ := os.ReadFile(marker); string(b) != "before" {
		t.Fatalf("initial env was not loaded: %q", b)
	}
	if err := os.WriteFile(envFile, []byte("ST7_ENV_RECOVERY=after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldPID, _ := strconv.Atoi(parts[0])
	if oldPID <= 1 {
		t.Fatal("bad PID")
	}
	stop := func(hash string) error {
		return exec.Command("bash", "-c", proofScript, "test-stop", "stop", parts[0], hash).Run()
	}
	if stop(strings.Repeat("0", 64)) == nil {
		t.Fatal("changed identity was accepted")
	}
	if err := stop(parts[1]); err != nil {
		t.Fatalf("stop proven daemon: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	var newProof string
	for time.Now().Before(deadline) {
		newProof, err = inspect()
		if err == nil && !strings.HasPrefix(newProof, parts[0]+" ") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || newProof == proof {
		t.Fatalf("existing loop failed to relaunch: %v %q", err, newProof)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		if string(b) == "after" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if b, _ := os.ReadFile(marker); string(b) != "after" {
		t.Fatalf("restarted daemon did not reload env: %q", b)
	}
	if err := pane.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("pane host was stopped")
	}
	if err := sup.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("supervisor was stopped")
	}
	if stop(parts[1]) == nil {
		t.Fatal("old proof stopped the replacement")
	}
}

// Each table row breaks one ownership fact. The stop must refuse and the
// stand-in daemon must remain alive; fake /proc data is injected only by this
// test's source substitution, never through production arguments or env.
func TestWSLRestartEachOwnershipGuardRefusesWithoutSignal(t *testing.T) {
	requireTools(t, "bash", "stat", "sha256sum", "awk")
	for _, change := range []string{"daemon-live-check", "supervisor-live-check", "daemon-pid", "supervisor-pid", "same-pid", "exe", "daemon-cmdline", "supervisor-cmdline", "lock-path", "lock-not-held", "parent-chain", "daemon-start", "supervisor-start", "update-pending"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			dir := filepath.Join(home, ".local/share/soluterminal")
			proc := filepath.Join(root, "proc")
			daemon := exec.Command("sleep", "60")
			if err := daemon.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- daemon.Wait() }()
			defer func() {
				_ = daemon.Process.Kill()
				select {
				case <-done:
				case <-time.After(time.Second):
				}
			}()
			sup := exec.Command("sleep", "60")
			if err := sup.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sup.Process.Kill(); _ = sup.Wait() }()
			pid, supervisor := strconv.Itoa(daemon.Process.Pid), strconv.Itoa(sup.Process.Pid)
			for _, p := range []string{dir, filepath.Join(proc, pid), filepath.Join(proc, supervisor, "fd"), filepath.Join(proc, supervisor, "fdinfo")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, text string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			statLine := func(id, parent, start string) string {
				return id + " (solud) S " + parent + " " + strings.Repeat("0 ", 17) + start + "\n"
			}
			write(filepath.Join(dir, "solud-wsl.pid"), pid)
			write(filepath.Join(dir, "start-wsl.pid"), supervisor)
			write(filepath.Join(dir, "solud"), "fixture")
			write(filepath.Join(dir, "start-wsl.lock"), "")
			write(filepath.Join(proc, pid, "stat"), statLine(pid, supervisor, "100"))
			write(filepath.Join(proc, supervisor, "stat"), statLine(supervisor, "1", "90"))
			write(filepath.Join(proc, pid, "cmdline"), "solud\x00--backend-id\x00wsl\x00")
			write(filepath.Join(proc, supervisor, "cmdline"), "bash\x00/path/start-wsl.sh\x00")
			write(filepath.Join(proc, supervisor, "fdinfo/9"), "lock: 1: FLOCK ADVISORY WRITE "+supervisor+" 00:00:1 0 EOF\n")
			if err := os.Symlink(filepath.Join(dir, "solud"), filepath.Join(proc, pid, "exe")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "start-wsl.lock"), filepath.Join(proc, supervisor, "fd/9")); err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(backgroundServiceWSLScript, `home="$(getent passwd "$(id -u)" | cut -d: -f6)"`, fmt.Sprintf("home=%q", home), 1)
			script = strings.Replace(script, "proc=/proc", fmt.Sprintf("proc=%q", proc), 1)
			inspect := exec.Command("bash", "-c", script, "inspect-test", "inspect")
			out, err := inspect.CombinedOutput()
			if err != nil {
				t.Fatalf("valid fixture: %v %s", err, out)
			}
			proof := strings.Fields(string(out))
			if len(proof) != 2 {
				t.Fatal("invalid fixture proof")
			}
			switch change {
			case "daemon-live-check":
				script = strings.Replace(script, `kill -0 "$pid"`, `false`, 1)
			case "supervisor-live-check":
				script = strings.Replace(script, `kill -0 "$sup"`, `false`, 1)
			case "daemon-pid":
				write(filepath.Join(dir, "solud-wsl.pid"), "999999999")
			case "supervisor-pid":
				write(filepath.Join(dir, "start-wsl.pid"), "999999999")
			case "same-pid":
				write(filepath.Join(dir, "solud-wsl.pid"), supervisor)
			case "exe":
				_ = os.Remove(filepath.Join(proc, pid, "exe"))
				_ = os.Symlink("/bin/sleep", filepath.Join(proc, pid, "exe"))
			case "daemon-cmdline":
				write(filepath.Join(proc, pid, "cmdline"), "solud\x00--backend-id\x00host\x00")
			case "supervisor-cmdline":
				write(filepath.Join(proc, supervisor, "cmdline"), "bash\x00/other.sh\x00")
			case "lock-path":
				_ = os.Remove(filepath.Join(proc, supervisor, "fd/9"))
				_ = os.Symlink("/dev/null", filepath.Join(proc, supervisor, "fd/9"))
			case "lock-not-held":
				write(filepath.Join(proc, supervisor, "fdinfo/9"), "")
			case "parent-chain":
				write(filepath.Join(proc, pid, "stat"), statLine(pid, "1", "100"))
			case "daemon-start":
				write(filepath.Join(proc, pid, "stat"), statLine(pid, supervisor, "101"))
			case "supervisor-start":
				write(filepath.Join(proc, supervisor, "stat"), statLine(supervisor, "1", "91"))
			case "update-pending":
				write(filepath.Join(dir, "solud.update-pending"), "pending")
			}
			if err := exec.Command("bash", "-c", script, "stop-test", "stop", proof[0], proof[1]).Run(); err == nil {
				t.Fatal("broken ownership guard admitted stop")
			}
			select {
			case err := <-done:
				t.Fatalf("daemon received a signal: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestServiceUnitProofRefusesWrongPIDAndPaneKillingUnit(t *testing.T) {
	for _, c := range []struct {
		name, properties string
		want             bool
	}{
		{"owned process-only unit", fmt.Sprintf("MainPID=%d\nKillMode=process", os.Getpid()), true},
		{"wrong owner", fmt.Sprintf("MainPID=%d\nKillMode=process", os.Getpid()+1), false},
		{"would kill pane hosts", fmt.Sprintf("MainPID=%d\nKillMode=control-group", os.Getpid()), false},
		{"mixed would kill panes", fmt.Sprintf("MainPID=%d\nKillMode=mixed", os.Getpid()), false},
		{"missing safety property", fmt.Sprintf("MainPID=%d", os.Getpid()), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			proof, err := inspectBackgroundServiceWithCommand("host", func(args ...string) (string, error) {
				if strings.Join(args, " ") != "/usr/bin/systemctl --user show soluterminal.service --property=MainPID,KillMode" {
					t.Fatalf("unexpected command: %q", args)
				}
				return c.properties, nil
			})
			if (err == nil) != c.want || (c.want && proof.Method != "systemd-user") {
				t.Fatalf("proof=%+v err=%v", proof, err)
			}
		})
	}
}
