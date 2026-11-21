package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"soluterminal/helperproto"
)

func goodIntegration() linuxPaneIntegration {
	return linuxPaneIntegration{BinPath: testLinuxBin, RuntimeDir: testLinuxRun, Kernel: "6.6.87.2-microsoft-standard-WSL2", HasClaude: true, HasCodex: true}
}

func TestLinuxPaneAgentCmdFor(t *testing.T) {
	claude := []string{"claude", "--model", "opus"}
	if got := linuxPaneAgentCmdFor(true, goodIntegration(), nil, claude); !slices.Contains(got, "--mcp-config") || !slices.Contains(got, "--settings") {
		t.Fatalf("enabled, good helper: %q", got)
	}
	old := goodIntegration()
	old.Kernel = "5.15.167.4-microsoft-standard-WSL2"
	bad := goodIntegration()
	bad.RuntimeDir = "/run/my dir"
	for name, tc := range map[string]struct {
		enabled bool
		in      linuxPaneIntegration
		err     error
		cmd     []string
	}{
		"fwd off":            {false, goodIntegration(), nil, claude},
		"helper unavailable": {true, linuxPaneIntegration{}, errors.New("no helper"), claude},
		"kernel before 6.5":  {true, old, nil, claude},
		"unsafe paths":       {true, bad, nil, claude},
		"not an agent":       {true, goodIntegration(), nil, []string{"htop"}},
		"shell":              {true, goodIntegration(), nil, nil},
	} {
		if got := linuxPaneAgentCmdFor(tc.enabled, tc.in, tc.err, tc.cmd); !slices.Equal(got, tc.cmd) {
			t.Errorf("%s: rewritten to %q", name, got)
		}
	}
}

// countingBackend records Integration calls; everything else is unused here.
type countingBackend struct {
	linuxPaneBackend
	calls atomic.Int32
	in    linuxPaneIntegration
	fails int32 // the first fails calls return an error
}

func (b *countingBackend) Integration(context.Context, string) (linuxPaneIntegration, error) {
	if n := b.calls.Add(1); n <= b.fails {
		return linuxPaneIntegration{}, errors.New("the distro's helper stopped")
	}
	return b.in, nil
}

// A transient "helper stopped" between Integration's steps is retried once.
func TestLinuxPaneIntegrationRetry(t *testing.T) {
	once := &countingBackend{in: goodIntegration(), fails: 1}
	if in, err := linuxPaneIntegrationRetry(context.Background(), once, "Ubuntu"); err != nil || in.BinPath != testLinuxBin || once.calls.Load() != 2 {
		t.Fatalf("one failure: %+v %v after %d calls", in, err, once.calls.Load())
	}
	twice := &countingBackend{in: goodIntegration(), fails: 2}
	if _, err := linuxPaneIntegrationRetry(context.Background(), twice, "Ubuntu"); err == nil || twice.calls.Load() != 2 {
		t.Fatalf("two failures: %v after %d calls", err, twice.calls.Load())
	}
}

// With fwd off (this build), a Linux claude pane's command is untouched and
// the helper isn't even asked.
func TestLinuxPaneWithAgentConfigOffByDefault(t *testing.T) {
	b := &countingBackend{in: goodIntegration()}
	m := NewManager(t.TempDir())
	m.linux = b
	cmd := []string{"claude"}
	if got := m.linuxPaneWithAgentConfig(context.Background(), "Ubuntu", cmd); !slices.Equal(got, cmd) {
		t.Fatalf("rewritten with fwd off: %q", got)
	}
	if b.calls.Load() != 0 {
		t.Fatal("asked the helper with fwd off")
	}
}

func TestLinuxPaneBackedBy(t *testing.T) {
	m := NewManager(t.TempDir())
	p := transactionTestPane(pid0Pane)
	p.Status = PaneRunning
	p.Linux = &linuxPaneRef{Distro: "Ubuntu", AgentID: testAttestAgentID}
	m.panes[p.ID] = p
	local := transactionTestPane(pid0Other)
	local.Status = PaneRunning
	m.panes[local.ID] = local

	if !linuxPaneBackedBy(m, "Ubuntu", pid0Pane, testAttestAgentID) {
		t.Fatal("current agent refused")
	}
	for name, ok := range map[string]bool{
		"other agent":  linuxPaneBackedBy(m, "Ubuntu", pid0Pane, "fedcba9876543210fedcba9876543210"),
		"other distro": linuxPaneBackedBy(m, "Debian", pid0Pane, testAttestAgentID),
		"local pane":   linuxPaneBackedBy(m, "Ubuntu", pid0Other, testAttestAgentID),
		"no such pane": linuxPaneBackedBy(m, "Ubuntu", "pane_00000000000000cc", testAttestAgentID),
		"no manager":   linuxPaneBackedBy(nil, "Ubuntu", pid0Pane, testAttestAgentID),
	} {
		if ok {
			t.Errorf("%s accepted", name)
		}
	}
	p.Status = PaneExited
	if linuxPaneBackedBy(m, "Ubuntu", pid0Pane, testAttestAgentID) {
		t.Fatal("exited pane accepted")
	}
}

func TestHelperFwdHostsWiring(t *testing.T) {
	f := newHelperFwdHosts(NewManager(t.TempDir()))
	if f.forDistro("Ubuntu") != f.forDistro("Ubuntu") || f.forDistro("Ubuntu") == f.forDistro("Debian") {
		t.Fatal("one host per distro")
	}
	// fwd off: nothing registered with the supervisor.
	if f.streamHandlers("Ubuntu") != nil || f.hooks().StreamHandlers("Ubuntu") != nil {
		t.Fatal("fwd handlers registered with fwd off")
	}
	f.hooks().OnHello("Ubuntu", helperproto.HelloResult{Kernel: "5.15.0"}, helperPlacement{})

	// The API handler is reached only once main has set it.
	h := f.forDistro("Ubuntu")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before setHandler: %d", rec.Code)
	}
	f.setHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("api")) }))
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "api") {
		t.Fatalf("after setHandler: %d %q", rec.Code, rec.Body.String())
	}
}
