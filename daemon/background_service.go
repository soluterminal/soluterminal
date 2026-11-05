package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed background_service_wsl.sh
var backgroundServiceWSLScript string

type serviceRestartProof struct {
	Method   string
	Identity string
}

func serviceCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	home, _ := os.UserHomeDir()
	runtimeDir := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + home, "XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus"}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func inspectBackgroundService(backend string) (serviceRestartProof, error) {
	return inspectBackgroundServiceWithCommand(backend, serviceCommand)
}

func inspectBackgroundServiceWithCommand(backend string, run func(...string) (string, error)) (serviceRestartProof, error) {
	if runtime.GOOS != "linux" {
		return serviceRestartProof{}, fmt.Errorf("no supported service manager")
	}
	if backend == "wsl" {
		out, err := run("/bin/bash", "--noprofile", "--norc", "-p", "-c", backgroundServiceWSLScript, "service-proof", "inspect")
		if err != nil || !strings.HasPrefix(out, strconv.Itoa(os.Getpid())+" ") {
			return serviceRestartProof{}, fmt.Errorf("WSL supervisor ownership could not be verified")
		}
		return serviceRestartProof{"wsl-supervisor", out}, nil
	}
	out, err := run("/usr/bin/systemctl", "--user", "show", "soluterminal.service", "--property=MainPID,KillMode")
	if err != nil || !strings.Contains("\n"+out+"\n", "\nMainPID="+strconv.Itoa(os.Getpid())+"\n") || !strings.Contains("\n"+out+"\n", "\nKillMode=process\n") {
		return serviceRestartProof{}, fmt.Errorf("this daemon is not managed by soluterminal.service")
	}
	return serviceRestartProof{"systemd-user", out}, nil
}

func (a *apiServer) serviceProof() (serviceRestartProof, error) {
	if a.inspectService != nil {
		return a.inspectService()
	}
	return inspectBackgroundService(a.backendID)
}

func (a *apiServer) serviceReservation() *atomic.Bool {
	if a.serviceRestartGate != nil {
		return a.serviceRestartGate
	}
	return &selfRestartInProgress
}

func (a *apiServer) serviceDaemonIdentity() string {
	if a.trustedDevices == nil {
		return ""
	}
	return a.trustedDevices.localTarget()
}

func (a *apiServer) serviceLossCounts() (held, receipts int) {
	if a.mgr == nil {
		return
	}
	for _, p := range a.mgr.List() {
		p.mu.Lock()
		held += len(p.heldDeliveries)
		if p.unsentMessage != nil && p.unsentMessage.PutBack {
			receipts++
		}
		p.mu.Unlock()
	}
	return
}

// The only accepted body is one PID binding the confirmation to this daemon.
// Unknown, duplicate and trailing fields/documents are all refused.
func serviceExpectedPID(r *http.Request) (int, error) {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1024))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return 0, fmt.Errorf("expected a PID object")
	}
	key, err := d.Token()
	if err != nil || key != "pid" {
		return 0, fmt.Errorf("expected only pid")
	}
	var pid int
	if err := d.Decode(&pid); err != nil {
		return 0, err
	}
	if d.More() {
		return 0, fmt.Errorf("extra or repeated field")
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') {
		return 0, fmt.Errorf("invalid object")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return 0, fmt.Errorf("trailing body")
	}
	return pid, nil
}

func (a *apiServer) serviceCapability(w http.ResponseWriter, r *http.Request) {
	// Discovery never changes service state. The POST independently rechecks all
	// transport, authority, update and ownership conditions.
	if requestViaRelay(r) {
		writeJSON(w, 200, map[string]any{"supported": false})
		return
	}
	p, err := a.serviceProof()
	held, receipts := a.serviceLossCounts()
	busy := a.serviceReservation().Load()
	if a.updater != nil && a.updater.Status().ShellApplying {
		busy = true
	}
	writeJSON(w, 200, map[string]any{"supported": err == nil && a.stopService != nil && !requestViaRelay(r), "pid": os.Getpid(), "method": p.Method, "backendID": a.backendID, "busy": busy, "ownership": p.Identity, "daemonIdentity": a.serviceDaemonIdentity(), "heldDeliveries": held, "putBackReceipts": receipts})
}

func serviceOwnOrigin(r *http.Request, signed bool) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// The Windows byte proxy and Dual router rewrite Host before forwarding to
	// WSL. Only approved-device requests may use those fixed desktop origins.
	return signed && (u.Host == "127.0.0.1:18787" || u.Host == "127.0.0.1:28787")
}

func (a *apiServer) restartService(w http.ResponseWriter, r *http.Request) {
	id := a.identifyCaller(r)
	if requestViaRelay(r) || !serviceRequestLocal(r, a.backendID, id.Device != "") || !serviceOwnOrigin(r, id.Device != "") {
		http.Error(w, "restart the background service at this computer itself", http.StatusForbidden)
		return
	}
	if !a.requireHuman(w, r, "restart the background service") {
		return
	}
	pid, err := serviceExpectedPID(r)
	if err != nil || pid != os.Getpid() {
		http.Error(w, "expected only the current daemon PID", http.StatusConflict)
		return
	}

	if !a.serviceReservation().CompareAndSwap(false, true) {
		http.Error(w, "restart or update already in progress", http.StatusConflict)
		return
	}
	keep := false
	defer func() {
		if !keep {
			a.serviceReservation().Store(false)
		}
	}()
	if a.updater != nil && a.updater.Status().ShellApplying {
		http.Error(w, "desktop update in progress", http.StatusConflict)
		return
	}
	proof, err := a.serviceProof()
	if err != nil || a.stopService == nil {
		http.Error(w, "no safe service restart path is available", http.StatusConflict)
		return
	}
	// Recheck the complete proof immediately before the only destructive action.
	fresh, err := a.serviceProof()
	if err != nil || fresh != proof {
		http.Error(w, "service ownership changed", http.StatusConflict)
		return
	}
	if proof.Method == "systemd-user" {
		run := a.runServiceCommand
		if run == nil {
			run = serviceCommand
		}
		if _, err := run("/usr/bin/systemctl", "--user", "--no-block", "restart", "soluterminal.service"); err != nil {
			http.Error(w, "service manager refused the restart", http.StatusServiceUnavailable)
			return
		}
	}
	keep = true
	log.Printf("background service restart: caller pid=%d kind=%s source=%s method=%s", id.PID, id.Kind, r.RemoteAddr, proof.Method)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "pid": os.Getpid(), "backendID": a.backendID})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if proof.Method == "wsl-supervisor" {
		a.stopService()
	}
}

// The native tray can signal without an HTTP caller. Its bounded intent marker
// makes the next startup diagnosable; it never authorizes or starts any work.
func logBackgroundServiceRestartReason(backend string) {
	if runtime.GOOS != "linux" || backend != "wsl" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".local/share/soluterminal/service-restart.reason")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(f, 129))
	_ = f.Close()
	_ = os.Remove(path)
	fields := strings.Fields(string(raw))
	if len(raw) > 128 || len(fields) != 3 || fields[0] != "native-tray" {
		return
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return
	}
	at, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Since(time.Unix(at, 0)) < 0 || time.Since(time.Unix(at, 0)) > 5*time.Minute {
		return
	}
	log.Printf("background service startup after native tray restart request: previous_pid=%d pid=%d", pid, os.Getpid())
}
