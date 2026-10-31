package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Real daemon processes and real agent sockets with no host configuration,
// credentials, pane ids, automation or integration settings in their env.
func TestAutoOffAcrossDaemonProcessRestart(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix agent process fixture")
	}
	for _, locked := range []bool{false, true} {
		t.Run(fmt.Sprint(locked), func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			var env []string
			for _, kv := range os.Environ() {
				key, _, _ := strings.Cut(kv, "=")
				if key == "HOME" || strings.HasPrefix(key, "SOLU_") || strings.HasPrefix(key, "CLAUDE") || strings.HasPrefix(key, "CODEX") || strings.HasPrefix(key, "XDG_") {
					continue
				}
				env = append(env, kv)
			}
			env = append(env, "HOME="+home, "SOLU_ENV_CLEANED=1", "SOLU_SKIP_MCP_REGISTER=1", "SOLU_SKIP_HOOK_REGISTER=1", "SOLU_AUTO_RESUME=0")
			start := func() (*httptestDaemon, func()) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := listener.Addr().String()
				_ = listener.Close()
				logFile, err := os.CreateTemp(dir, "daemon-*.log")
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(exe, "policy-test-daemon", "--addr", addr, "--data", dir)
				// This is an explicit daemon boot, even when go test names its
				// executable daemon.test. Keep production shim dispatch strict.
				cmd.Args[0] = "solud"
				cmd.Env = env
				cmd.Stdout = logFile
				cmd.Stderr = logFile
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				stopped := false
				stop := func() {
					if stopped {
						return
					}
					stopped = true
					_ = cmd.Process.Signal(syscall.SIGTERM)
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						_ = cmd.Process.Kill()
						<-done
					}
					_ = logFile.Close()
				}
				t.Cleanup(stop)
				client := &http.Client{Timeout: 3 * time.Second}
				daemon := &httptestDaemon{url: "http://" + addr, client: client, t: t}
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					resp, err := client.Get(daemon.url + "/api/panes")
					if err == nil {
						_ = resp.Body.Close()
						if resp.StatusCode == 200 {
							return daemon, stop
						}
					}
					time.Sleep(25 * time.Millisecond)
				}
				b, _ := os.ReadFile(logFile.Name())
				t.Fatalf("scratch daemon did not start: %s", b)
				return nil, nil
			}
			first, stop := start()
			body := first.post("/api/panes", fmt.Sprintf(`{"cwd":%q,"cmd":["/bin/sh"],"cols":80,"rows":24}`, home), 201)
			var pane PaneSnapshot
			if err := json.Unmarshal(body, &pane); err != nil {
				t.Fatal(err)
			}
			base := "/api/panes/" + pane.ID
			first.post(base+"/auto-mode", `{"autoMode":"always"}`, 200)
			first.post(base+"/auto-mode", `{"autoMode":""}`, 200)
			if locked {
				first.post(base+"/never-auto-answer", `{"neverAutoAnswer":true}`, 200)
			}
			first.post(base+"/restart?fresh=1&force=1", "", 200)
			stop()
			second, _ := start()
			resp, err := second.client.Get(second.url + "/api/panes")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var panes []PaneSnapshot
			if err := json.NewDecoder(resp.Body).Decode(&panes); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range panes {
				if p.ID == pane.ID {
					found = true
					if p.AutoMode != "" || p.NeverAutoAnswer != locked {
						t.Fatalf("daemon restart reverted: mode=%q lock=%v", p.AutoMode, p.NeverAutoAnswer)
					}
				}
			}
			if !found {
				t.Fatal("pane lost across daemon restart")
			}
			// Reap only the fixture pane; the scratch daemon also cleans its record.
			req, _ := http.NewRequest("DELETE", second.url+base, nil)
			resp, err = second.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 200 && resp.StatusCode != 204 {
				t.Fatalf("scratch pane cleanup: %s", resp.Status)
			}
			if _, err := os.Stat(filepath.Join(dir, "panes.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type httptestDaemon struct {
	url    string
	client *http.Client
	t      *testing.T
}

func (d *httptestDaemon) post(path, body string, want int) []byte {
	d.t.Helper()
	resp, err := d.client.Post(d.url+path, "application/json", strings.NewReader(body))
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	if resp.StatusCode != want {
		d.t.Fatalf("scratch POST %s: %d, want %d: %s", path, resp.StatusCode, want, b)
	}
	return b
}
