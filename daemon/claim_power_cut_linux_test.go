//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A power cut during SaveClaim leaves a .claim.json.tmp-* file (from
// writeFileAtomicSynced) beside the last claim.json that landed, or beside
// none on a first pairing. Each row puts a data directory in one of those
// states, starts the compiled solud on it, and reads what the person sees:
// whether the pairing survived (GET /api/cloud/status) and which remote-access
// policy the first start decided (GET /api/remote-devices). It then pairs
// again through SaveClaim, as both pairing flows do, and restarts: the pairing
// must take and the policy must not be decided a second time.
func TestClaimSaveCutByPowerLossRecoversOnRestart(t *testing.T) {
	requireTools(t, "go")
	bin := filepath.Join(t.TempDir(), "solud")
	// hostedCloud=off: the fixture daemon must never check in with the real cloud.
	build := exec.Command("go", "build", "-ldflags=-X main.version=1.0.0 -X main.hostedCloud=off", "-o", bin, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build solud: %v\n%s", err, output)
	}
	// Nothing listens here, so the relay stays offline and no request leaves
	// this machine.
	relay := "ws://" + unusedLoopbackAddr(t) + "/relay"
	before := Claim{RelayURL: relay, Token: "machine-token-before-cut", MachineID: "m1", MachineName: "office-pc"}
	rotated, err := json.Marshal(Claim{RelayURL: relay, Token: "machine-token-being-saved", MachineID: "m1", MachineName: "office-pc"})
	if err != nil {
		t.Fatal(err)
	}
	// cut leaves the temp file SaveClaim was writing when the power went.
	cut := func(t *testing.T, data string, body []byte) {
		t.Helper()
		f, err := os.CreateTemp(data, "."+filepath.Base(claimFilePath(data))+".tmp-*")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(filepath.Base(f.Name()), claimTempPrefix) {
			t.Fatalf("temp file %s does not match claimTempPrefix %q", f.Name(), claimTempPrefix)
		}
		if _, err := f.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	saved := func(t *testing.T, data string) {
		t.Helper()
		if err := SaveClaim(data, &before); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		name    string
		seed    func(t *testing.T, data string)
		paired  bool // claim.json still holds the pairing from before the cut
		corrupt bool
		policy  string
	}{
		{"first pairing cut before claim.json landed", func(t *testing.T, data string) {
			cut(t, data, nil) // unsynced data reads back empty
		}, false, false, remoteAccessApproved},
		{"token rotation cut on a new install", func(t *testing.T, data string) {
			saved(t, data)
			cut(t, data, rotated[:len(rotated)/2])
		}, true, false, remoteAccessApproved},
		{"token rotation cut on an existing install", func(t *testing.T, data string) {
			saved(t, data)
			if err := os.WriteFile(filepath.Join(data, "workspaces.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			cut(t, data, rotated[:len(rotated)/2])
		}, true, false, remoteAccessAny},
		// What the save before this change could leave: no temp file, a torn
		// claim.json. The daemon must still start and say it needs pairing.
		{"claim.json itself torn", func(t *testing.T, data string) {
			if err := os.WriteFile(claimFilePath(data), rotated[:len(rotated)/2], 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, true, remoteAccessApproved},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			home := t.TempDir()
			data := filepath.Join(home, ".solu")
			if err := os.MkdirAll(data, 0o700); err != nil {
				t.Fatal(err)
			}
			row.seed(t, data)

			base, stop := startPowerCutFixtureDaemon(t, bin, home, relay)
			st := waitForCloudStatus(t, base, home, func(st cloudStatus) bool {
				return st.Configured && st.HasCreds == row.paired && st.ClaimCorrupt == row.corrupt
			})
			if st.CredsInvalid {
				t.Fatalf("after the cut: cloud status %+v marks the token invalid", st)
			}
			if row.paired {
				if c, err := LoadClaim(data); err != nil || c == nil || c.Token != before.Token || c.MachineID != before.MachineID {
					t.Fatalf("after the cut: claim.json = %+v, %v; want the pairing from before the cut", c, err)
				}
			}
			if got := remoteAccessPolicyOf(t, base); got != row.policy {
				t.Fatalf("after the cut: remote-access policy = %q, want %q", got, row.policy)
			}
			stop()

			// Pair again, the way a person would from the Cloud screen.
			repaired := Claim{RelayURL: relay, Token: "machine-token-repaired", MachineID: "m2", MachineName: "office-pc"}
			if err := SaveClaim(data, &repaired); err != nil {
				t.Fatalf("re-pair: %v", err)
			}
			base, stop = startPowerCutFixtureDaemon(t, bin, home, relay)
			waitForCloudStatus(t, base, home, func(st cloudStatus) bool {
				return st.Configured && st.HasCreds && !st.ClaimCorrupt && !st.CredsInvalid
			})
			if c, err := LoadClaim(data); err != nil || c == nil || c.Token != repaired.Token {
				t.Fatalf("after re-pairing: claim.json = %+v, %v", c, err)
			}
			if got := remoteAccessPolicyOf(t, base); got != row.policy {
				t.Fatalf("after re-pairing: remote-access policy = %q, want %q (decided once)", got, row.policy)
			}
			stop()
		})
	}
}

func unusedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// startPowerCutFixtureDaemon runs bin on home/.solu until /api/health answers.
// --relay is set even with no readable claim, so the Cloud screen offers pairing.
// stop is safe to call twice and also runs at cleanup.
func startPowerCutFixtureDaemon(t *testing.T, bin, home, relay string) (string, func()) {
	t.Helper()
	addr := unusedLoopbackAddr(t)
	log, err := os.OpenFile(filepath.Join(home, "solud.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--data", filepath.Join(home, ".solu"), "--addr", addr, "--relay", relay)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "SOLU_PANE_ID=", "SOLU_ENV_CLEANED=1", "SOLU_WORKFLOW_DISABLE=1", "SOLU_SKIP_MCP_REGISTER=1", "SOLU_SKIP_HOOK_REGISTER=1", "SOLU_DISABLE_TURN_SNAPSHOTS=1", "SOLU_MEM_GUARD_MB=0"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
			_ = log.Close()
		})
	}
	t.Cleanup(stop)
	base := "http://" + addr
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-done:
			t.Fatalf("solud exited before it was healthy\n%s", powerCutFixtureLog(home))
		default:
		}
		if resp, err := client.Get(base + "/api/health"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, stop
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("solud never became healthy at %s\n%s", addr, powerCutFixtureLog(home))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForCloudStatus(t *testing.T, base, home string, ok func(cloudStatus) bool) cloudStatus {
	t.Helper()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	var last cloudStatusResponse
	var lastErr error
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(base + "/api/cloud/status")
		if err == nil {
			last = cloudStatusResponse{}
			err = json.NewDecoder(resp.Body).Decode(&last)
			_ = resp.Body.Close()
			if err == nil && ok(last.cloudStatus) {
				return last.cloudStatus
			}
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("cloud status never matched: last %+v, %v\n%s", last.cloudStatus, lastErr, powerCutFixtureLog(home))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func remoteAccessPolicyOf(t *testing.T, base string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	resp, err := client.Get(base + "/api/remote-devices")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Policy string `json:"policy"`
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/remote-devices = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Policy
}

func powerCutFixtureLog(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "solud.log"))
	if err != nil {
		return fmt.Sprintf("(no solud.log: %v)", err)
	}
	const tail = 8 << 10
	if len(b) > tail {
		b = b[len(b)-tail:]
	}
	return string(b)
}
