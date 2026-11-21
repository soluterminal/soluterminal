package migrationfence

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerFenceBeforeAnyDownloadOrServiceAction(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "shared", "installer", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "refuse_migrated_linux() {\n")
	if start < 0 {
		t.Fatal("installer fence missing")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("installer fence incomplete")
	}
	function := text[start : start+end+3]
	call := strings.Index(text, `then refuse_migrated_linux "$DATA_DIR" || exit 78; fi`)
	for _, operation := range []string{"command -v curl", "systemctl --user restart", "systemctl --user enable"} {
		if at := strings.Index(text, operation); call < 0 || at < 0 || call >= at {
			t.Fatalf("fence must precede %s", operation)
		}
	}
	// A bad root is uncertainty, not proof that this ordinary Linux PC moved.
	bad := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bad, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", function+"\nrefuse_migrated_linux \"$1\"", "fixture", bad)
	cmd.Env = []string{"PATH=" + t.TempDir()}
	output, uncertainErr := cmd.CombinedOutput()
	exit, ok := uncertainErr.(*exec.ExitError)
	if !ok || exit.ExitCode() != ExitCode || strings.Contains(string(output), Message) || !strings.Contains(string(output), "can't check migration state") {
		t.Fatalf("exit=%v output=%s", uncertainErr, output)
	}
	for _, name := range []string{InProgress, Retired} {
		for _, kind := range []string{"missing", "file", "directory", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "data")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "file":
					err = os.WriteFile(filepath.Join(dir, name), nil, 0600)
				case "directory":
					err = os.Mkdir(filepath.Join(dir, name), 0700)
				case "symlink":
					err = os.Symlink(filepath.Join(root, "absent"), filepath.Join(dir, name))
				}
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("/bin/sh", "-c", function+"\nrefuse_migrated_linux \"$1\"", "fixture", dir)
				cmd.Env = []string{"PATH=" + root}
				output, err := cmd.CombinedOutput()
				if kind == "missing" {
					if err != nil {
						t.Fatalf("ordinary Linux refused: %s %v", output, err)
					}
					return
				}
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != ExitCode || !strings.Contains(string(output), Message) {
					t.Fatalf("exit=%v output=%s", err, output)
				}
			})
		}
	}
}
