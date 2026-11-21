//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Reserve and exercise both numeric families at one port before releasing it.
// Client-first FIN/EOF keeps TIME_WAIT off the reservation port. Consomme's
// asynchronous tracker also needs to observe the live sockets before they close;
// otherwise its grace period for an unseen bind outlives the release budget.
func reserveWSLColdStartFixture(t *testing.T) (string, func()) {
	t.Helper()
	var v4, v6 net.Listener
	for attempt := 0; attempt < 20; attempt++ {
		var err error
		v4, err = listenOnConfiguredPort("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := v4.Addr().(*net.TCPAddr).Port
		v6, err = listenOnConfiguredPort("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if err == nil {
			break
		}
		_ = v4.Close()
		if attempt == 19 {
			t.Fatalf("matching IPv6 reservation required: %v", err)
		}
	}
	t.Cleanup(func() { _ = v4.Close(); _ = v6.Close() })
	observedAfter := time.Now().Add(time.Second)
	for _, listener := range []net.Listener{v4, v6} {
		if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = accepted.Close() })
		if err := accepted.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		const payload = "fixture-family-probe"
		if _, err := io.WriteString(client, payload); err != nil {
			t.Fatal(err)
		}
		probe := make([]byte, len(payload))
		if _, err := io.ReadFull(accepted, probe); err != nil || string(probe) != payload {
			t.Fatalf("family probe did not reach reservation %s: %q %v", listener.Addr(), probe, err)
		}
		if err := client.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if n, err := accepted.Read(probe[:1]); n != 0 || err != io.EOF {
			t.Fatalf("client FIN not observed: n=%d err=%v", n, err)
		}
		_ = accepted.Close()
		_ = client.Close()
	}
	address := v4.Addr().String()
	return address, func() {
		if remaining := time.Until(observedAfter); remaining > 0 {
			time.Sleep(remaining)
		}
		_ = v4.Close()
		_ = v6.Close()
		_, port, _ := net.SplitHostPort(address)
		deadline := time.Now().Add(5 * time.Second)
		for {
			free := true
			for _, host := range []string{"127.0.0.1", "::1"} {
				connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 250*time.Millisecond)
				if connection != nil {
					_ = connection.Close()
				}
				if !errors.Is(err, syscall.ECONNREFUSED) {
					free = false
				}
			}
			if free {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("released fixture port %s still accepts or times out in at least one family", port)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// Exercise the maintained generated admission, launch and wait functions with
// compiled main, an isolated data directory and authenticated numeric HTTP.
// The test-owned exit trap cleans up only its own job; the production cleanup
// function intentionally recognizes the standard port, not this fixture port.
// This is Linux startup coverage, not Windows/WebView2 forwarding acceptance.
func TestWSLLauncherRealNetworkColdStart(t *testing.T) {
	requireTools(t, "bash", "timeout", "awk", "flock", "go")
	home := t.TempDir()
	bin := filepath.Join(home, "solud")
	buildContext, cancelBuild := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelBuild()
	if output, err := exec.CommandContext(buildContext, "go", "build", "-ldflags=-X main.version=1.0.0", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	data := filepath.Join(home, ".solu")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	const token = "launcher-fixture"
	if err := os.WriteFile(filepath.Join(data, "token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	address, release := reserveWSLColdStartFixture(t)
	body := renderWSLStartScript(t)
	launch := extractShellFunc(t, body, "launch_daemon")
	if strings.Count(launch, `"$LOCAL" --addr`) != 1 {
		t.Fatal("generated launch no longer has exactly one daemon command")
	}
	launch = strings.Replace(launch, `"$LOCAL" --addr`, `"$LOCAL" --data "$FIXTURE_DATA" --addr`, 1)
	launch = strings.ReplaceAll(launch, "--relay https://relay.example", "--relay ''")
	pidfile := filepath.Join(home, "fixture-daemon.pid")
	script := admissionScript(t, home, address, "1.0.0")
	script += fmt.Sprintf("\nLOCAL=%s\nFIXTURE_DATA=%s\nPIDFILE=%s\n", shellSingleQuote(bin), shellSingleQuote(data), shellSingleQuote(pidfile))
	script += extractShellFunc(t, body, "refuse_migrated_linux") + "\n" + launch + "\n" + extractShellFunc(t, body, "await_daemon") + "\n"
	// A job spec cannot accidentally name an unrelated process after PID reuse.
	script += "trap 'kill -TERM %1 2>/dev/null || true; wait || true' EXIT\ntrap 'exit 0' TERM INT\nclaim_supervision\nlaunch_daemon\nawait_daemon\nexit \"$status\"\n"
	log, err := os.Create(filepath.Join(home, "launcher.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	command := exec.Command("bash", "-c", script)
	command.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "SOLU_PANE_ID=", "SOLU_ENV_CLEANED=1", "SOLU_WORKFLOW_DISABLE=1", "SOLU_SKIP_MCP_REGISTER=1", "SOLU_SKIP_HOOK_REGISTER=1", "SOLU_DISABLE_TURN_SNAPSHOTS=1", "SOLU_MEM_GUARD_MB=0", "SOLU_LOOPBACK_REQUIRE_AUTH=1"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout, command.Stderr = log, log
	release()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var commandError error
	go func() { commandError = command.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	var identity struct {
		Pid       int    `json:"pid"`
		BackendID string `json:"backendID"`
		Version   string `json:"version"`
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastStatus, lastPID int
	var lastProbeError error
	for {
		select {
		case <-done:
			output, _ := os.ReadFile(log.Name())
			t.Fatalf("generated launcher exited before readiness: %v\n%s", commandError, output)
		default:
		}
		pidBytes, _ := os.ReadFile(pidfile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		lastPID = pid
		// Do not compete with the admission probe before the launch branch fires.
		if pid > 0 {
			request, _ := http.NewRequest("GET", "http://"+address+"/api/health?identity=1", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := client.Do(request)
			if err == nil {
				lastStatus = response.StatusCode
				err = json.NewDecoder(response.Body).Decode(&identity)
				_ = response.Body.Close()
				if err == nil && response.StatusCode == http.StatusOK && identity.Pid == pid && identity.BackendID == "wsl" && identity.Version == "1.0.0" {
					break
				}
			}
			lastProbeError = err
		}
		if time.Now().After(deadline) {
			output, _ := os.ReadFile(log.Name())
			t.Fatalf("no authenticated identity at exact address %s (status=%d launcherPID=%d identity=%+v error=%v)\n%s", address, lastStatus, lastPID, identity, lastProbeError, output)
		}
		time.Sleep(50 * time.Millisecond)
	}
	request, _ := http.NewRequest("GET", "http://"+address+"/api/health?identity=1", nil)
	request.Header.Set("Authorization", "Bearer incorrect-fixture-token")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("incorrect bearer was not rejected: status=%d", response.StatusCode)
	}
	results := make(chan error, 2)
	reuse := admissionScript(t, home, address, "1.0.0") + "claim_supervision\necho DUPLICATE-LAUNCH\n"
	for attempt := 0; attempt < 2; attempt++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			duplicate := exec.CommandContext(ctx, "bash", "-c", reuse)
			duplicate.Env = command.Env
			output, err := duplicate.CombinedOutput()
			if err != nil || strings.Contains(string(output), "DUPLICATE-LAUNCH") || !strings.Contains(string(output), fmt.Sprintf("pid=%d", identity.Pid)) {
				results <- fmt.Errorf("concurrent start did not reuse original daemon: %v\n%s", err, output)
				return
			}
			results <- nil
		}()
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	finalPID, _ := os.ReadFile(pidfile)
	if strings.TrimSpace(string(finalPID)) != strconv.Itoa(identity.Pid) {
		t.Fatal("a duplicate start changed the original daemon PID")
	}
}
