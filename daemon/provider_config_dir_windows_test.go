//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsProviderConfigSweepEligibility(t *testing.T) {
	before := time.Now().Add(-time.Second)
	dir, err := newPrivateProviderConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if ok, err := oldOwnedProviderConfigDir(dir, before); err != nil || ok {
		t.Fatalf("new directory eligible: %t %v", ok, err)
	}
	if ok, err := oldOwnedProviderConfigDir(dir, time.Now().Add(time.Second)); err != nil || !ok {
		t.Fatalf("old owned directory ineligible: %t %v", ok, err)
	}
	otherUser, err := windows.StringToSid("S-1-5-21-111-222-333-444")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := providerConfigOwnedBy(dir, otherUser); err != nil || ok {
		t.Fatalf("directory accepted as another user's: %t %v", ok, err)
	}
}

func TestWindowsClaudeConfigFilesACL(t *testing.T) {
	args, cleanup, err := claudeConfigFileArgs(`C:\npm\claude.cmd`, claudeInjectArgs(testInjectionRecord(), "pane_private", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	file := injectFlagValue(args, "--settings")
	for _, path := range []string{filepath.Dir(file), file, injectFlagValue(args, "--mcp-config")} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 1 {
			t.Fatalf("expected exactly one user ACE for %s: %v", path, err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
			t.Fatalf("non-private ACL: %s", sd.String())
		}
	}
}

func TestWindowsPanePathRetainsNode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, pathKey := range []string{"Path", "PATH", "path"} {
		env := []string{pathKey + `=C:\Program Files\nodejs;C:\Windows\System32`, "HOME=" + os.Getenv("HOME")}
		got := ensureLocalBinOnPath(env)
		got = prependPathDir(got, `C:\npm`)
		var paths []string
		for _, kv := range got {
			k, value, _ := strings.Cut(kv, "=")
			if strings.EqualFold(k, "PATH") {
				paths = append(paths, value)
			}
		}
		if len(paths) != 1 || !strings.Contains(paths[0], `C:\Program Files\nodejs`) || !strings.Contains(paths[0], `C:\Windows\System32`) {
			t.Fatalf("%s shadowed Node's PATH: %v", pathKey, paths)
		}
	}
}

func TestWindowsNPMProviderNativeArgv(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "codex.cmd")
	if err := os.WriteFile(bin, []byte(npmCodexNodeShim), 0600); err != nil {
		t.Fatal(err)
	}
	script, _ := npmProviderShimEntrypoint(bin)
	if err := os.MkdirAll(filepath.Dir(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("fixture, never executed"), 0600); err != nil {
		t.Fatal(err)
	}
	nodeDir := filepath.Join(root, "node with spaces")
	if err := os.MkdirAll(nodeDir, 0700); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(nodeDir, "node.exe")
	if err := os.WriteFile(node, nil, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-c", `mcp_servers.soluterminal.env_http_headers={"X-Solu-Pane"="SOLU_PANE_ID"}`, "prompt with spaces & \"quotes\" %PATH% !caret^"}
	actualBin, got, err := providerBatchArgv(bin, args, []string{"Path=" + nodeDir})
	if err != nil || actualBin != node || !reflect.DeepEqual(got, append([]string{script}, args...)) {
		t.Fatalf("native Node argv lost quoting: %s %q %v", actualBin, got, err)
	}
	// A sibling Node is the first choice of npm's shim, ahead of child PATH.
	sibling := filepath.Join(root, "node.exe")
	if err := os.WriteFile(sibling, nil, 0600); err != nil {
		t.Fatal(err)
	}
	actualBin, _, err = providerBatchArgv(bin, args, []string{"Path=" + nodeDir})
	if err != nil || actualBin != sibling {
		t.Fatalf("lost npm sibling Node preference: %s %v", actualBin, err)
	}
}

func TestWindowsProviderPathSkipsUnixShim(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("PATHEXT", ".EXE;.CMD")
	name := "solu-npm-resolver-fixture"
	for _, suffix := range []string{"", ".cmd"} {
		if err := os.WriteFile(filepath.Join(root, name+suffix), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := findOnWindowsPath(name); !ok || got != filepath.Join(root, name+".cmd") {
		t.Fatalf("selected extensionless Unix shim: %s (%t)", got, ok)
	}
	if err := os.WriteFile(filepath.Join(root, name+".exe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := findOnWindowsPath(name); !ok || got != filepath.Join(root, name+".exe") {
		t.Fatalf("lost native executable preference: %s (%t)", got, ok)
	}
	if got, ok := findOnWindowsPath(name + ".cmd"); !ok || got != filepath.Join(root, name+".cmd") {
		t.Fatalf("changed explicitly selected shim: %s (%t)", got, ok)
	}
}
