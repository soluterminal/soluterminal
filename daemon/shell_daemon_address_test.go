//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellDaemonDiscoveryRequiresLiveLocalOwner(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		held          bool
		want          string
	}{
		{"wildcard", "0.0.0.0:12345", true, "http://127.0.0.1:12345"},
		{"ipv6", "[::]:12345", true, "http://[::1]:12345"},
		{"unlocked", "127.0.0.1:12345", false, ""},
		{"remote", "example.com:12345", true, ""},
		{"invalid-port", "127.0.0.1:0", true, ""},
		{"invalid-address", "http://127.0.0.1:12345/", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lock, err := acquireDataDirLock(dir, dataDirLockHolder{Addr: tc.address})
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			if !tc.held {
				lock.Release()
			}
			old := "http://127.0.0.1:48887"
			env := []string{paneDaemonURLEnv + "=" + old, codexInlineShimEnvKey + "=" + filepath.Join(dir, codexInlineShimDirName)}
			want := tc.want
			if want == "" {
				want = old
			}
			if got := envValueFromList(shellDaemonEnvironment(env), paneDaemonURLEnv); got != want {
				t.Fatalf("address = %q, want %q", got, want)
			}
			// Reading discovery must never rewrite the owner's metadata.
			body, err := os.ReadFile(filepath.Join(dir, dataDirLockName))
			var holder dataDirLockHolder
			if err != nil || json.Unmarshal(body, &holder) != nil || holder.Addr != tc.address {
				t.Fatal("owner metadata changed")
			}
		})
	}
}

func TestShellDaemonLockInspectionRejectsWaitersAndOtherOwners(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"1: FLOCK ADVISORY WRITE 123 00:27:456 0 EOF", true},
		{"1: FLOCK ADVISORY WRITE 124 00:27:456 0 EOF", false},
		{"1: FLOCK ADVISORY WRITE 123 00:27:457 0 EOF", false},
		{"1: -> FLOCK ADVISORY WRITE 123 00:27:456 0 EOF", false},
		{"1: FLOCK ADVISORY READ 123 00:27:456 0 EOF", false},
		{"1: POSIX ADVISORY WRITE 123 00:27:456 0 EOF", false},
	} {
		if got := shellDaemonLockListed(strings.NewReader(tc.line), 123, "00:27:456"); got != tc.want {
			t.Errorf("lock record %q = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestCodexShimFollowsCurrentOwnerOnlyForModelInvocations(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireDataDirLock(dir, dataDirLockHolder{Addr: "127.0.0.1:12345"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	old := "http://127.0.0.1:48887"
	env := []string{paneDaemonURLEnv + "=" + old, codexInlineShimEnvKey + "=" + filepath.Join(dir, codexInlineShimDirName)}
	if got := envValueFromList(codexShimEnvironment([]string{"resume", "saved-thread"}, env), paneDaemonURLEnv); got != "http://127.0.0.1:12345" {
		t.Fatalf("model invocation kept the stale daemon address: %q", got)
	}
	if got := envValueFromList(codexShimEnvironment([]string{"--version"}, env), paneDaemonURLEnv); got != old {
		t.Fatalf("utility invocation rewrote the environment: %q", got)
	}
}
