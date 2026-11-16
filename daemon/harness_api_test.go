package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessAccountPutRejectsRetiredDeepseekModels(t *testing.T) {
	a := &apiServer{dataDir: t.TempDir()}
	mux := http.NewServeMux()
	a.registerHarnessRoutes(mux)
	a.registerExtensionRoutes(mux)
	for _, raw := range []string{"deepseek-v4-pro", " DEEPSEEK-V4-PRO ", "vendor/deepseek-v4-pro", "deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"} {
		acc := harnessTestAccount("main", "key")
		acc.Model = raw
		body, _ := json.Marshal(acc)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(http.MethodPut, "/api/harnesses/deepseek/accounts", strings.NewReader(string(body))))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("model %q status=%d body=%s", raw, res.Code, res.Body.String())
		}
	}
	for _, model := range []string{"deepseek-flash", "deepseek-v4-flash-vision-exp"} {
		acc := harnessTestAccount("main", "key")
		acc.Model = model
		body, _ := json.Marshal(acc)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(http.MethodPut, "/api/harnesses/deepseek/accounts", strings.NewReader(string(body))))
		if res.Code != http.StatusOK {
			t.Fatalf("model %q status=%d body=%s", model, res.Code, res.Body.String())
		}
	}
}

// The daemon re-extracts the embedded harness launchers at boot
// (main.go) so a self-update reaches panes without a manual
// Credentials -> Run. Lock the core behaviour in: a stale on-disk copy
// is overwritten with the embedded asset at mode 0700.
func TestHarnessLauncherOverwritesStaleAssets(t *testing.T) {
	dir := t.TempDir()
	a := &apiServer{dataDir: dir, ver: "0.0.1066"}
	launcherDir := filepath.Join(dir, "harness-launchers")
	if err := os.MkdirAll(launcherDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launcherDir, "deepseek-terminal.mjs"), []byte("// stale copy"), 0700); err != nil {
		t.Fatal(err)
	}

	path, err := a.harnessLauncher()
	if err != nil {
		t.Fatalf("harnessLauncher: %v", err)
	}
	if want := filepath.Join(launcherDir, "launch.mjs"); path != want {
		t.Fatalf("launcher path = %q, want %q", path, want)
	}
	for _, name := range []string{"pins.mjs", "launch.mjs", "pi-approval.mjs", "deepseek-model.mjs", "deepseek-terminal.mjs", "install.mjs", "harness-shortcut.sh", "pi-lock.json", "dsh-lock.json"} {
		got, err := os.ReadFile(filepath.Join(launcherDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		want, err := harnessAssets.ReadFile("harness_assets/" + name)
		if err != nil {
			t.Fatalf("embedded %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s was not refreshed to the embedded asset", name)
		}
		info, err := os.Stat(filepath.Join(launcherDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%s mode = %v, want 0700", name, info.Mode().Perm())
		}
	}
	// The build stamp is what a pane reports via /status, so it must track the
	// extracting daemon rather than the stale on-disk copy.
	stamp, err := os.ReadFile(filepath.Join(launcherDir, "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	if string(stamp) != "0.0.1066\n" {
		t.Fatalf("VERSION = %q, want %q", stamp, "0.0.1066\n")
	}
}
