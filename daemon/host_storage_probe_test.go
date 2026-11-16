package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testHostProber(t *testing.T, output string) (*hostStorageProber, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	p := &hostStorageProber{
		insideWSL: func() bool { return true }, distroName: func(context.Context) (string, error) { return "Ubuntu", nil },
		resolvePS: func() (string, error) { return "powershell.exe", nil }, env: func(string) []string { return []string{"PATH=x"} },
		run: func(context.Context, string, []string, string) ([]byte, error) {
			calls.Add(1)
			return []byte(output), nil
		},
		now: time.Now, timeout: time.Second, minInterval: time.Minute,
	}
	return p, calls
}

func TestWfHostStorageProbeParsesAndCaches(t *testing.T) {
	path := `C:\Users\alice\AppData\Local\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}\ext4.vhdx`
	p, calls := testHostProber(t, "noise\n"+`{"ok":true,"distro":"Ubuntu","vhdx":"`+strings.ReplaceAll(path, `\`, `\\`)+`","vhdxUnresolved":false,"volume":"C:\\","freeBytes":42949672960,"totalBytes":2199023255552,"vhdxLogicalBytes":1099511627776,"vhdxPhysicalBytes":593880940544,"sparse":false,"localAppData":"C:\\Users\\alice\\AppData\\Local","ownerSid":"S-1-5-21-1000","basePath":"C:\\Users\\alice\\AppData\\Local\\wsl\\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}","registrationCount":1,"reparsePoint":false,"error":""}`+"\n")
	s := p.Sample(context.Background())
	if !s.Available || s.State != hostStorageOK || s.Volume != `C:\` || s.FreeBytes != 42949672960 || s.VHDXSparse == nil || *s.VHDXSparse {
		t.Fatalf("unexpected sample: %+v", s)
	}
	if s.VHDXPathRaw != path || !strings.HasPrefix(s.VHDXPath, `~\`) {
		t.Fatalf("path redaction/raw mismatch: %q / %q", s.VHDXPath, s.VHDXPathRaw)
	}
	if s.OwnerSID != "S-1-5-21-1000" || s.RegistrationCount != 1 || s.BasePath == "" || s.LocalAppData == "" {
		t.Fatalf("registration metadata missing: %+v", s)
	}
	if cached := p.Sample(context.Background()); cached.Source != "cache" || calls.Load() != 1 {
		t.Fatalf("cache=%+v calls=%d", cached, calls.Load())
	}
}

func TestWfHostStorageProbeUnavailableStates(t *testing.T) {
	t.Run("not wsl", func(t *testing.T) {
		p, calls := testHostProber(t, `{}`)
		p.insideWSL = func() bool { return false }
		if s := p.Sample(context.Background()); s.State != hostStorageNotWSL || calls.Load() != 0 {
			t.Fatalf("%+v calls=%d", s, calls.Load())
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("SOLU_HOST_STORAGE_PROBE", "0")
		p, calls := testHostProber(t, `{}`)
		if s := p.Sample(context.Background()); s.State != hostStorageDisabled || calls.Load() != 0 {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("no powershell", func(t *testing.T) {
		p, calls := testHostProber(t, `{}`)
		p.resolvePS = func() (string, error) { return "", errors.New("missing") }
		if s := p.Sample(context.Background()); s.State != hostStorageNoPowerShell || calls.Load() != 0 {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("interop", func(t *testing.T) {
		p, _ := testHostProber(t, `{}`)
		p.run = func(context.Context, string, []string, string) ([]byte, error) {
			return nil, errors.New("accept4 WSL_INTEROP")
		}
		if s := p.Sample(context.Background()); s.State != hostStorageInterop {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		p, _ := testHostProber(t, `not json`)
		if s := p.Sample(context.Background()); s.State != hostStorageParse || strings.Contains(s.Detail, "secret=") {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("unresolved", func(t *testing.T) {
		p, _ := testHostProber(t, `{"ok":true,"distro":"Ubuntu","vhdx":"","vhdxUnresolved":true,"volume":"C:\\","freeBytes":1000,"totalBytes":2000,"vhdxLogicalBytes":0,"vhdxPhysicalBytes":0,"sparse":null,"localAppData":"","error":""}`)
		if s := p.Sample(context.Background()); s.State != hostStorageVHDXUnresolved || !s.Available {
			t.Fatalf("%+v", s)
		}
	})
}

func TestWfHostStorageProbeTimeoutUnsafeNameAndBackoff(t *testing.T) {
	p, calls := testHostProber(t, `{}`)
	p.timeout = 15 * time.Millisecond
	p.run = func(ctx context.Context, _ string, _ []string, _ string) ([]byte, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	if s := p.Sample(context.Background()); s.State != hostStorageTimeout || time.Since(started) > p.timeout+200*time.Millisecond {
		t.Fatalf("%+v elapsed=%s", s, time.Since(started))
	}
	_ = p.Sample(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("failure backoff executed again: %d", calls.Load())
	}
	_ = p.Refresh(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("refresh ignored failure backoff: %d", calls.Load())
	}

	unsafe, unsafeCalls := testHostProber(t, `{}`)
	unsafe.distroName = func(context.Context) (string, error) { return "Ubuntu';Remove-Item", nil }
	if s := unsafe.Sample(context.Background()); s.State != hostStorageParse || s.Detail != "distro name unsafe" || unsafeCalls.Load() != 0 {
		t.Fatalf("%+v calls=%d", s, unsafeCalls.Load())
	}
}

func TestWfHostStorageProbeTimeoutWaitsForCancelledRunToExit(t *testing.T) {
	p, _ := testHostProber(t, `{}`)
	p.timeout = 15 * time.Millisecond
	exited := make(chan struct{})
	p.run = func(ctx context.Context, _ string, _ []string, _ string) ([]byte, error) {
		<-ctx.Done()
		// Model CommandContext's bounded process-reap work after cancellation.
		time.Sleep(25 * time.Millisecond)
		close(exited)
		return nil, ctx.Err()
	}
	if s := p.Sample(context.Background()); s.State != hostStorageTimeout {
		t.Fatalf("sample=%+v", s)
	}
	select {
	case <-exited:
	default:
		t.Fatal("probe returned while its cancelled run was still live")
	}
}

func TestWfDistroFromWindowsRoot(t *testing.T) {
	for in, want := range map[string]string{`\\wsl.localhost\Ubuntu\`: "Ubuntu", `\\wsl$\Ubuntu-22.04\`: "Ubuntu-22.04"} {
		if got, ok := distroFromWindowsRoot(in); !ok || got != want {
			t.Fatalf("%q => %q,%v", in, got, ok)
		}
	}
}

func TestWfHostStorageResolverOutputLayoutsAndRegistrationCounts(t *testing.T) {
	local := `C:\Users\owner\AppData\Local`
	for _, tc := range []struct {
		name, base string
		count      int
		ok         bool
	}{
		{name: "guid", base: local + `\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}`, count: 1, ok: true},
		{name: "named", base: local + `\wsl\Ubuntu`, count: 1, ok: true},
		{name: "store", base: local + `\Packages\CanonicalGroup.Ubuntu_abc\LocalState`, count: 1, ok: true},
		{name: "zero", count: 0},
		{name: "duplicate", count: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.base != "" {
				path = tc.base + `\ext4.vhdx`
			}
			payload := map[string]any{
				"ok": true, "distro": "Ubuntu", "vhdx": path, "vhdxUnresolved": tc.count != 1,
				"volume": `C:\`, "freeBytes": int64(1000), "totalBytes": int64(2000),
				"vhdxLogicalBytes": int64(900), "vhdxPhysicalBytes": int64(700), "sparse": false,
				"localAppData": local, "ownerSid": "S-1-5-21-1000", "basePath": tc.base, "registrationCount": tc.count, "reparsePoint": false,
			}
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			s := parseHostStorageOutput(b, "fallback")
			if s.RegistrationCount != tc.count || s.OwnerSID != "S-1-5-21-1000" || s.LocalAppData != local || s.BasePath != tc.base {
				t.Fatalf("metadata=%+v", s)
			}
			if tc.ok {
				if s.State != hostStorageOK || s.VHDXPathRaw != path {
					t.Fatalf("resolved=%+v", s)
				}
			} else if s.State != hostStorageVHDXUnresolved {
				t.Fatalf("ambiguous registration did not remain unresolved: %+v", s)
			}
		})
	}

	malformed := parseHostStorageOutput([]byte(`{"ok":false,"distro":"Ubuntu","vhdx":"","vhdxUnresolved":true,"volume":"","freeBytes":0,"totalBytes":0,"localAppData":"C:\\Users\\owner\\AppData\\Local","ownerSid":"S-1-5-21-1000","basePath":"","registrationCount":1,"error":"Registered BasePath is not a safe rooted local path."}`), "Ubuntu")
	if malformed.State != hostStorageParse || !strings.Contains(malformed.Detail, "BasePath") {
		t.Fatalf("malformed BasePath output=%+v", malformed)
	}
}

func TestWfHostStorageResolverScriptIsCurrentOwnerReadOnlyAndFailClosed(t *testing.T) {
	script := hostStorageScript("Ubuntu")
	for _, want := range []string{"HKEY_USERS\\", "WindowsIdentity]::GetCurrent", "registrationCount", "Environment]::ExpandEnvironmentVariables", "GetFullPath", "FileAttributes]::SparseFile"} {
		if !strings.Contains(script, want) {
			t.Errorf("resolver script missing %q", want)
		}
	}
	for _, forbidden := range []string{"HKCU:", "Select-Object -First", "Set-Item", "New-Item", "Remove-Item", "Add-Content", "wsl.exe"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("resolver script contains mutating/ambiguous construct %q", forbidden)
		}
	}
}
