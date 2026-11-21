package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	source, host := filepath.Join(base, "wsl"), filepath.Join(base, "host")
	for _, dir := range []string{source, host} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return source, host
}

func put(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func fileBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[e.Name()] = string(data)
	}
	return result
}

func TestInventoryReadonlyConflictsAndSecretOmission(t *testing.T) {
	source, host := fixture(t)
	put(t, source, "panes.json", `[{"id":"same","cwd":"PRIVATE-PATH","lastPrompts":["PRIVATE-PROMPT"],"future":"PRIVATE-FUTURE"},{"id":"linux-only"}]`)
	put(t, host, "panes.json", `[{"id":"same"},{"id":"windows-only"}]`)
	put(t, source, "workspaces.json", `[{"name":"Default","layout":null,"paneIds":["same"],"activeAt":"2026-11-20T00:00:00Z"},{"name":"Linux","layout":null,"paneIds":["linux-only"],"activeAt":"2026-11-20T00:00:00Z"}]`)
	put(t, host, "workspaces.json", `[{"name":"Default","layout":null,"paneIds":["same"],"activeAt":"2026-11-20T00:00:00Z"}]`)
	put(t, source, "trusted-devices.json", `{"policy":"approved","devices":[{"publicKey":"PRIVATE-DEVICE-KEY"}]}`)
	put(t, host, "trusted-devices.json", `{"policy":"any","devices":[]}`)
	for _, dir := range []string{source, host} {
		put(t, dir, "claim.json", `{"machineId":"same-pc","relayUrl":"wss://fixture.invalid/relay","token":"PRIVATE-CLAIM-TOKEN"}`)
		put(t, dir, "daemon-identity.json", "PRIVATE-DAEMON-KEY")
		put(t, dir, "token", "PRIVATE-OWNER-TOKEN")
		put(t, dir, "env", "PRIVATE-ENV")
	}
	beforeSource, beforeHost := fileBytes(t, source), fileBytes(t, host)
	i, err := Inspect(source, host)
	if err != nil {
		t.Fatal(err)
	}
	if i.ApplyReady || i.Source != (Counts{2, 2, 1}) || i.Host != (Counts{2, 1, 0}) {
		t.Fatalf("counts or readiness: %+v", i)
	}
	want := []Collision{{"pane", "same"}, {"workspace", "Default"}}
	if !reflect.DeepEqual(i.Collisions, want) {
		t.Fatalf("collisions = %#v", i.Collisions)
	}
	if i.ClaimAction != "stop-mirror-then-rename-source-revoke-nothing" {
		t.Fatal(i.ClaimAction)
	}
	if i.SourcePolicy != "approved" || i.HostPolicy != "any" {
		t.Fatal("policies were changed")
	}
	data, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE-") {
		t.Fatalf("private data escaped into report: %s", data)
	}
	if !reflect.DeepEqual(fileBytes(t, source), beforeSource) || !reflect.DeepEqual(fileBytes(t, host), beforeHost) {
		t.Fatal("inventory changed fixture files")
	}
}

func TestInventoryClaimDirections(t *testing.T) {
	for _, tc := range []struct{ name, source, host, want string }{
		{"unpaired", `{}`, `{}`, "no-source-claim"},
		{"host-only", `{}`, `{"machineId":"h","relayUrl":"wss://fixture.invalid","token":"host"}`, "no-source-claim"},
		{"unpaired-host", `{"machineId":"s","relayUrl":"wss://fixture.invalid","token":"source"}`, `{}`, "host-pairing-required"},
		{"shared", `{"machineId":"s","relayUrl":"wss://fixture.invalid","token":"old"}`, `{"machineId":"s","relayUrl":"wss://fixture.invalid","token":"new"}`, "stop-mirror-then-rename-source-revoke-nothing"},
		{"distinct", `{"machineId":"s","relayUrl":"wss://fixture.invalid","token":"source"}`, `{"machineId":"h","relayUrl":"wss://fixture.invalid","token":"host"}`, "verify-live-host-stop-source-then-owner-revoke-source"},
		{"different-cloud", `{"machineId":"same","relayUrl":"wss://one.invalid","token":"source"}`, `{"machineId":"same","relayUrl":"wss://two.invalid","token":"host"}`, "relay-mismatch-needs-decision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, host := fixture(t)
			put(t, source, "claim.json", tc.source)
			put(t, host, "claim.json", tc.host)
			i, err := Inspect(source, host)
			if err != nil {
				t.Fatal(err)
			}
			if i.ClaimAction != tc.want || i.ApplyReady {
				t.Fatalf("action = %q, ready = %v", i.ClaimAction, i.ApplyReady)
			}
		})
	}
}

func TestInventoryRejectsMalformedAndAmbiguousRecords(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"panes.json", `[{"id":"same"},{"id":"same"}]`},
		{"panes.json", `[{"id":""}]`},
		{"panes.json", `{}`},
		{"panes.json", `[{"id":"p","future":{"x":1,"x":2}}]`},
		{"workspaces.json", `[{"name":"same"},{"name":"same"}]`},
		{"workspaces.json", `[{"name":""}]`},
		{"workspaces.json", `[null]`},
		{"workspaces.json", `[{}]`},
		{"workspaces.json", `{"invented-map":{"name":"invented-map"}}`},
		{"trusted-devices.json", `{"policy":"future-policy"}`},
		{"trusted-devices.json", `null`},
		{"trusted-devices.json", `{"devices":[null]}`},
		{"trusted-devices.json", `{"devices":[123]}`},
		{"claim.json", `{"token":"PRIVATE-SECRET","machineId":"s"}`},
		{"claim.json", `PRIVATE-SECRET invalid JSON`},
	} {
		t.Run(tc.name+tc.data, func(t *testing.T) {
			source, host := fixture(t)
			put(t, source, tc.name, tc.data)
			_, err := Inspect(source, host)
			if err == nil {
				t.Fatal("accepted invalid inventory")
			}
			if strings.Contains(err.Error(), "PRIVATE-SECRET") {
				t.Fatal("error disclosed contents")
			}
			if got := fileBytes(t, source)[tc.name]; got != tc.data {
				t.Fatal("invalid file was changed or quarantined")
			}
		})
	}
}

func TestInventoryRejectsOverlappingAndMissingRoots(t *testing.T) {
	source, host := fixture(t)
	if _, err := Inspect(source, source); err == nil {
		t.Fatal("same directory accepted")
	}
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{source, nested}, {nested, source}, {source, filepath.Join(host, "missing")}} {
		if _, err := Inspect(pair[0], pair[1]); err == nil {
			t.Fatal("invalid roots accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(host, "missing")); !os.IsNotExist(err) {
		t.Fatal("created missing host")
	}
}

func TestInventoryAcceptsPersistedEmptyWorkspaceLists(t *testing.T) {
	for _, data := range []string{`[]`, `null`} {
		source, host := fixture(t)
		put(t, source, "workspaces.json", data)
		put(t, host, "workspaces.json", data)
		i, err := Inspect(source, host)
		if err != nil || i.Source.Workspaces != 0 || i.Host.Workspaces != 0 {
			t.Fatalf("workspace inventory for %s: %+v, %v", data, i, err)
		}
	}
}

func TestInventoryRevisionIncludesUnknownFieldsAndMissingFiles(t *testing.T) {
	source, host := fixture(t)
	a, err := Inspect(source, host)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Inspect(source, host)
	if err != nil || a.Revision != b.Revision {
		t.Fatal("unstable empty inventory")
	}
	put(t, source, "panes.json", `[]`)
	b, err = Inspect(source, host)
	if err != nil || a.Revision == b.Revision {
		t.Fatal("missing and empty file have same revision")
	}
	put(t, source, "panes.json", `[{"id":"p","unknown":1}]`)
	c, err := Inspect(source, host)
	if err != nil {
		t.Fatal(err)
	}
	put(t, source, "panes.json", `[{"id":"p","unknown":2}]`)
	d, err := Inspect(source, host)
	if err != nil || c.Revision == d.Revision {
		t.Fatal("unknown fields did not affect revision")
	}
	if !reflect.DeepEqual(c.Source, d.Source) {
		t.Fatal("unexpected inventory difference")
	}
}

func TestInventoryRefusesStateSymlinksAndOversizeFiles(t *testing.T) {
	source, host := fixture(t)
	name := filepath.Join(source, "panes.json")
	if runtime.GOOS != "windows" {
		put(t, source, "daemon-identity.json", "PRIVATE-KEY")
		if err := os.Symlink("daemon-identity.json", name); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(source, host); err == nil {
			t.Fatal("accepted state symlink")
		}
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxStateBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(source, host); err == nil {
		t.Fatal("accepted oversized state")
	}
}

type auditedRoot struct {
	*os.Root
	opened     []string
	beforeOpen func(string)
}

func (r *auditedRoot) Open(name string) (*os.File, error) {
	r.opened = append(r.opened, name)
	if r.beforeOpen != nil {
		r.beforeOpen(name)
	}
	return r.Root.Open(name)
}

func TestInventoryNeverOpensIdentityKeysOrAccountCredentials(t *testing.T) {
	source, _ := fixture(t)
	put(t, source, "panes.json", `[]`)
	for _, name := range []string{"daemon-identity.json", "token", "env", "credentials.json"} {
		put(t, source, name, "PRIVATE-SECRET")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	audit := &auditedRoot{Root: root}
	if _, err := readSnapshot(audit, "source"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit.opened, []string{"panes.json"}) {
		t.Fatalf("opened files: %v", audit.opened)
	}
}

func TestInventoryRefusesReplacementBeforeReading(t *testing.T) {
	source, _ := fixture(t)
	put(t, source, "panes.json", `[]`)
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	audit := &auditedRoot{Root: root, beforeOpen: func(name string) {
		path := filepath.Join(source, name)
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		put(t, source, name, "PRIVATE-REPLACEMENT")
	}}
	if _, err := readSnapshot(audit, "source"); err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("replacement error = %v", err)
	}
}
