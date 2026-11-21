//go:build linux

package migration

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the actual reviewed distro resource. The Python suite constructs
// only synthetic temporary directories, never a profile, daemon or WSL distro.
func TestLinuxMigrationTransaction(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is unavailable for the Linux transaction fixtures")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	probe := "import errno,sys\ntry:\n import ctypes,fcntl,pwd\n pwd.getpwuid(__import__('os').geteuid())\n lib=ctypes.CDLL(None,use_errno=True)\n f=lib.renameat2\n getattr(lib,'fstatfs')\n ctypes.set_errno(0)\n ok=f(-1,b'probe',-1,b'probe-target',1)==-1 and ctypes.get_errno()==errno.EBADF\nexcept (ImportError,AttributeError,KeyError):\n ok=False\nsys.exit(0 if ok else 77)"
	if output, err := exec.CommandContext(ctx, python, "-I", "-c", probe).CombinedOutput(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
			t.Skip("Linux migration fixture primitives are unavailable")
		}
		t.Fatalf("fixture capability check: %v\n%s", err, output)
	}
	suite, err := filepath.Abs("../../desktop/src-tauri/migration/test_linux_transaction.py")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, python, "-I", suite).CombinedOutput(); err != nil {
		t.Fatalf("Linux transaction fixtures: %v\n%s", err, output)
	}
}
