//go:build !windows

package main

import (
	"context"
	"errors"

	"testing"
)

func TestSeedDistroEntryValidation(t *testing.T) {
	good := seedPaneSpec{Title: " rig ", Distro: "SoluTerminalRig"}
	if err := validateSeedPane(&good); err != nil || good.Title != "rig" {
		t.Fatalf("a plain distro entry was refused: %v", err)
	}
	for name, spec := range map[string]seedPaneSpec{
		"argv":       {Distro: "Ubuntu", Argv: []string{"claude"}},
		"cwd":        {Distro: "Ubuntu", Cwd: "/"},
		"bad name":   {Distro: "-d evil"},
		"long title": {Distro: "Ubuntu", Title: string(make([]byte, 81))},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSeedPane(&spec); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestSeedDistroPanesAtBoot(t *testing.T) {
	side := decideDaemonRole("linux", "127.0.0.1:18899", "/scratch", "/home/u/.solu")
	old := listWSLDistros
	t.Cleanup(func() { listWSLDistros = old })
	installed := []linuxDistro{{Name: "SoluTerminalRig", Default: true}}
	var listErr error
	listWSLDistros = func(context.Context) ([]linuxDistro, error) { return installed, listErr }

	run := func(t *testing.T, linuxOn bool, body string) []SpawnOpts {
		t.Helper()
		data := t.TempDir()
		writeSeedFile(t, data, body, 0o600)
		a := &apiServer{dataDir: data, mgr: NewManager(data)}
		if linuxOn {
			a.mgr.linux = &pairBackend{distro: "SoluTerminalRig"}
		}
		var spawned []SpawnOpts
		a.spawnPane = func(o SpawnOpts) (*Pane, error) {
			spawned = append(spawned, o)
			return transactionTestPane("pane_seed"), nil
		}
		a.seedPanesAtBoot(true, side)
		return spawned
	}
	doc := `{"panes":[{"title":"rig","distro":"SoluTerminalRig"}]}`

	got := run(t, true, doc)
	if len(got) != 1 || got[0].Distro != "SoluTerminalRig" || got[0].Title != "rig" || len(got[0].Cmd) != 0 ||
		got[0].Cwd != "" || got[0].Agent != "" || got[0].Origin == nil || got[0].Origin.Source != seedPaneOriginSrc {
		t.Fatalf("seeded %+v", got)
	}
	if n := len(run(t, false, doc)); n != 0 {
		t.Fatal("a distro pane was seeded with Linux panes switched off")
	}
	if n := len(run(t, true, `{"panes":[{"title":"x","distro":"Other"}]}`)); n != 0 {
		t.Fatal("a distro wsl.exe doesn't list was seeded")
	}
	// One bad distro refuses the whole file, claude entries too.
	cwd := t.TempDir()
	mixed := `{"panes":[{"title":"a","cwd":` + quoteJSON(cwd) + `,"argv":["claude"]},{"title":"b","distro":"Other"}]}`
	if n := len(run(t, true, mixed)); n != 0 {
		t.Fatal("a file with an unlisted distro seeded its other panes")
	}
	listErr = errors.New("wsl.exe failed")
	if n := len(run(t, true, doc)); n != 0 {
		t.Fatal("seeded although wsl.exe couldn't list distros")
	}
}

func quoteJSON(s string) string {
	b := []byte{'"'}
	for _, r := range s {
		if r == '"' || r == '\\' {
			b = append(b, '\\')
		}
		b = append(b, string(r)...)
	}
	return string(append(b, '"'))
}
