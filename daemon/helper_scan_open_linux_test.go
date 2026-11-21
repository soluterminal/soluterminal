//go:build linux

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func helperScanFixtureRoot(t *testing.T) (string, *os.File, uint32) {
	t.Helper()
	dir := t.TempDir()
	uid := uint32(os.Getuid())
	root, err := helperScanFixtureHome(dir, uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root, uid
}

func helperScanBackends() map[string]helperScanOpenat2Func {
	return map[string]helperScanOpenat2Func{
		"openat2":  unix.Openat2,
		"fallback": func(int, string, *unix.OpenHow) (int, error) { return -1, unix.ENOSYS },
	}
}

func TestHelperScanOpenRegularFixture(t *testing.T) {
	dir, root, uid := helperScanFixtureRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "usage.jsonl"), []byte("FIXTURE_CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, backend := range helperScanBackends() {
		t.Run(name, func(t *testing.T) {
			f, err := helperScanOpenRegularWith(root, "nested/usage.jsonl", uid, backend)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			b, err := io.ReadAll(f)
			if err != nil || string(b) != "FIXTURE_CONTENT" {
				t.Fatal("wrong fixture read")
			}
			flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Fatal("scan file fd may survive exec")
			}
		})
	}
}

func TestHelperScanOpenRejectsSymlinksTraversalAndTypes(t *testing.T) {
	dir, root, uid := helperScanFixtureRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SECRET_FILE"), []byte("SECRET_CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "SECRET_FILE"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{"link", "parent/SECRET_FILE", "directory", "fifo", "", ".", "..", "../SECRET_FILE", "parent/../link", "parent//SECRET_FILE", filepath.Join(outside, "SECRET_FILE"), "bad\x00name"}
	for name, backend := range helperScanBackends() {
		t.Run(name, func(t *testing.T) {
			for _, path := range paths {
				f, err := helperScanOpenRegularWith(root, path, uid, backend)
				if f != nil {
					_ = f.Close()
					t.Fatal("unsafe path returned an fd")
				}
				if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), outside) {
					t.Fatal("unsafe open accepted or path echoed")
				}
			}
		})
	}
}

func TestHelperScanOpenRefusalDoesNotFallback(t *testing.T) {
	dir, root, uid := helperScanFixtureRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "exists"), []byte("FIXTURE"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []error{unix.EPERM, unix.EACCES, unix.EINVAL} {
		f, err := helperScanOpenRegularWith(root, "exists", uid, func(int, string, *unix.OpenHow) (int, error) { return -1, refused })
		if err == nil || f != nil {
			if f != nil {
				_ = f.Close()
			}
			t.Fatal("runtime refusal bypassed through fallback")
		}
	}
	if f, err := helperScanOpenRegular(root, "absent", uid); !errors.Is(err, fs.ErrNotExist) || f != nil {
		t.Fatal("missing path not classified")
	}
}

func TestHelperScanOpenSymlinkSwapFixture(t *testing.T) {
	for name, backend := range helperScanBackends() {
		t.Run(name, func(t *testing.T) {
			dir, root, uid := helperScanFixtureRoot(t)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("SECRET_OUTSIDE"), 0o600); err != nil {
				t.Fatal(err)
			}
			slot := filepath.Join(dir, "slot")
			if err := os.WriteFile(slot, []byte("FIXTURE_INSIDE"), 0o600); err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			fail := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					select {
					case <-stop:
						return
					default:
					}
					next := filepath.Join(dir, "next")
					if err := os.WriteFile(next, []byte("FIXTURE_INSIDE"), 0o600); err != nil {
						fail <- err
						return
					}
					if err := os.Rename(next, slot); err != nil {
						fail <- err
						return
					}
					link := filepath.Join(dir, "next-link")
					if err := os.Symlink(outside, link); err != nil {
						fail <- err
						return
					}
					if err := os.Rename(link, slot); err != nil {
						fail <- err
						return
					}
				}
			}()
			defer func() {
				close(stop)
				wg.Wait()
				select {
				case <-fail:
					t.Error("fixture swap failed")
				default:
				}
			}()
			for i := 0; i < 200; i++ {
				f, err := helperScanOpenRegularWith(root, "slot", uid, backend)
				if err != nil {
					continue
				}
				b, readErr := io.ReadAll(f)
				_ = f.Close()
				if readErr != nil || string(b) != "FIXTURE_INSIDE" {
					t.Fatal("symlink swap escaped fixture root")
				}
			}
		})
	}
}

func TestHelperScanOpenRetainsRootDuringConcurrentClose(t *testing.T) {
	dir, root, uid := helperScanFixtureRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "same"), []byte("FIXTURE_INSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "same"), []byte("SECRET_OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := helperScanOpenRegularWith(root, "same", uid, func(fd int, path string, how *unix.OpenHow) (int, error) {
		if err := root.Close(); err != nil {
			return -1, err
		}
		other, err := unix.Open(outside, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, err
		}
		defer unix.Close(other)
		return unix.Openat2(fd, path, how)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "FIXTURE_INSIDE" {
		t.Fatal("root close redirected the open")
	}
}

func TestHelperScanOpenSearchOnlyParents(t *testing.T) {
	home := t.TempDir()
	parent := filepath.Join(home, "search-only")
	rootPath := filepath.Join(parent, "root")
	if err := os.MkdirAll(filepath.Join(rootPath, "middle"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "middle", "file"), []byte("FIXTURE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o111); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o700)
	uid := uint32(os.Getuid())
	root, err := helperScanFixtureHome(rootPath, uid)
	if err != nil {
		t.Fatal("search-only root ancestor was rejected")
	}
	defer root.Close()
	middle := filepath.Join(rootPath, "middle")
	if err := os.Chmod(middle, 0o111); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(middle, 0o700)
	for _, backend := range helperScanBackends() {
		f, err := helperScanOpenRegularWith(root, "middle/file", uid, backend)
		if err != nil {
			t.Fatal("search-only relative ancestor was rejected")
		}
		raw, err := f.SyscallConn()
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		var flags int
		var flagErr error
		err = raw.Control(func(fd uintptr) { flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) })
		_ = f.Close()
		if err != nil || flagErr != nil || flags&unix.O_NONBLOCK != 0 {
			t.Fatal("returned scan file remains nonblocking")
		}
	}
}

func TestHelperScanOpenOwnerAndInterruptedCall(t *testing.T) {
	dir, root, uid := helperScanFixtureRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("FIXTURE"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	f, err := helperScanOpenRegularWith(root, "file", uid+1, func(int, string, *unix.OpenHow) (int, error) { called = true; return -1, unix.ENOSYS })
	if f != nil || !errors.Is(err, errHelperScanOwner) || called {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("wrong-owner root reached opener")
	}
	calls := 0
	f, err = helperScanOpenRegularWith(root, "file", uid, func(fd int, path string, how *unix.OpenHow) (int, error) {
		calls++
		if calls == 1 {
			return -1, unix.EINTR
		}
		return unix.Openat2(fd, path, how)
	})
	if err != nil || f == nil || calls != 2 {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("transient interruption was not retried")
	}
	_ = f.Close()
	calls = 0
	f, err = helperScanOpenRegularWith(root, "file", uid, func(int, string, *unix.OpenHow) (int, error) { calls++; return -1, unix.EINTR })
	if f != nil || !errors.Is(err, errHelperScanUnavailable) || calls != 8 {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("persistent interruption was not bounded")
	}
}

// Every passwd lookup in these tests is synthetic. Never call the production
// home constructor, even when a test changes HOME to another fixture.
func helperScanFixtureHome(path string, uid uint32) (*os.File, error) {
	return helperScanOpenHomeWith(uid, func(id string) (*user.User, error) {
		return &user.User{Uid: id, HomeDir: path}, nil
	})
}

func TestHelperScanPasswdHomeTrustBoundary(t *testing.T) {
	fixture := t.TempDir()
	real := filepath.Join(fixture, "real-home")
	if err := os.MkdirAll(filepath.Join(real, ".claude", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(fixture, "passwd-home")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	t.Setenv("HOME", outside)
	uid := uint32(os.Getuid())
	home, err := helperScanFixtureHome(alias, uid)
	if err != nil {
		t.Fatal("symlinked passwd home rejected", err)
	}
	defer home.Close()
	root, err := helperScanOpenDirectory(home, ".claude/projects", uid)
	if err != nil {
		t.Fatal("fixed root under passwd home rejected", err)
	}
	defer root.Close()
	want, err := os.Stat(filepath.Join(real, ".claude", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := root.Stat()
	if err != nil || !os.SameFile(want, got) {
		t.Fatal("environment redirected scan root")
	}
	flags, err := unix.FcntlInt(root.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("scan root descriptor may survive exec")
	}
}

func TestHelperScanPasswdLookupFailsClosed(t *testing.T) {
	uid := uint32(os.Getuid())
	for name, lookup := range map[string]func(*testing.T, string) (*user.User, error){
		"error": func(*testing.T, string) (*user.User, error) { return nil, errors.New("SECRET_LOOKUP_DIAGNOSTIC") },
		"nil":   func(*testing.T, string) (*user.User, error) { return nil, nil },
		"wrong-user": func(t *testing.T, _ string) (*user.User, error) {
			return &user.User{Uid: strconv.FormatUint(uint64(uid)+1, 10), HomeDir: t.TempDir()}, nil
		},
		"relative": func(_ *testing.T, id string) (*user.User, error) {
			return &user.User{Uid: id, HomeDir: "SECRET_RELATIVE_HOME"}, nil
		},
		"missing": func(t *testing.T, id string) (*user.User, error) {
			return &user.User{Uid: id, HomeDir: filepath.Join(t.TempDir(), "SECRET_MISSING_HOME")}, nil
		},
		"not-directory": func(t *testing.T, id string) (*user.User, error) {
			path := filepath.Join(t.TempDir(), "SECRET_FILE")
			if err := os.WriteFile(path, []byte("FIXTURE"), 0o600); err != nil {
				t.Fatal(err)
			}
			return &user.User{Uid: id, HomeDir: path}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			home, err := helperScanOpenHomeWith(uid, func(id string) (*user.User, error) {
				called = true
				if id != strconv.FormatUint(uint64(uid), 10) {
					t.Fatal("lookup used a different uid")
				}
				return lookup(t, id)
			})
			if home != nil {
				_ = home.Close()
				t.Fatal("invalid passwd entry returned a descriptor")
			}
			if !called || err == nil || strings.Contains(err.Error(), "SECRET_") {
				t.Fatal("lookup did not fail closed with a fixed diagnostic")
			}
		})
	}
	if home, err := helperScanFixtureHome(t.TempDir(), uid+1); home != nil || !errors.Is(err, errHelperScanOwner) {
		if home != nil {
			_ = home.Close()
		}
		t.Fatal("wrong-owner passwd home accepted")
	}
}

func TestHelperScanRootComponentsRejectSymlinksAndTraversal(t *testing.T) {
	dir, home, uid := helperScanFixtureRoot(t)
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, ".claude"), filepath.Join(dir, "SECRET_PARENT_ALIAS")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("projects", filepath.Join(dir, ".claude", "SECRET_ROOT_ALIAS")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"SECRET_PARENT_ALIAS/projects", ".claude/SECRET_ROOT_ALIAS", "fifo", ".", "../outside", "/absolute", "bad//path", "bad\x00path"} {
		root, err := helperScanOpenDirectory(home, path, uid)
		if root != nil {
			_ = root.Close()
			t.Fatal("unsafe fixed-root component accepted")
		}
		if err == nil || strings.Contains(err.Error(), "SECRET_") {
			t.Fatal("component accepted or echoed")
		}
	}
	if root, err := helperScanOpenDirectory(home, ".claude/projects", uid+1); root != nil || !errors.Is(err, errHelperScanOwner) {
		if root != nil {
			_ = root.Close()
		}
		t.Fatal("wrong-owner starting descriptor accepted")
	}
}

func TestHelperScanDirectoryDescriptorSurvivesPathReplacement(t *testing.T) {
	dir, home, uid := helperScanFixtureRoot(t)
	if err := os.MkdirAll(filepath.Join(dir, ".codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := helperScanOpenDirectory(home, ".codex/sessions", uid)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, ".codex", "sessions", "file"), []byte("FIXTURE_INSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("SECRET_OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, ".codex"), filepath.Join(dir, "old-codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".codex")); err != nil {
		t.Fatal(err)
	}
	f, err := helperScanOpenRegular(root, "file", uid)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "FIXTURE_INSIDE" {
		t.Fatal("root replacement redirected file open")
	}
	if next, err := helperScanOpenDirectory(home, ".codex", uid); err == nil || next != nil {
		if next != nil {
			_ = next.Close()
		}
		t.Fatal("new symlinked parent accepted")
	}
}

func TestHelperScanFileOpenFlagsBeforeTypeCheck(t *testing.T) {
	dir, root, uid := helperScanFixtureRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("FIXTURE"), 0o600); err != nil {
		t.Fatal(err)
	}
	checked := false
	f, err := helperScanOpenRegularWith(root, "file", uid, func(fd int, path string, how *unix.OpenHow) (int, error) {
		checked = true
		for _, flag := range []int{unix.O_NONBLOCK, unix.O_NOCTTY, unix.O_NOFOLLOW, unix.O_CLOEXEC} {
			if how.Flags&uint64(flag) == 0 {
				t.Fatal("file open omitted a required flag before type checking")
			}
		}
		return unix.Openat2(fd, path, how)
	})
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		_ = f.Close()
	}
	if !checked {
		t.Fatal("open flags were not observed")
	}
}

func TestHelperScanDirectoryChecksEachOwnerBeforeDescent(t *testing.T) {
	dir, home, uid := helperScanFixtureRoot(t)
	if err := os.MkdirAll(filepath.Join(dir, ".codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	var parent, target unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, ".codex"), &parent); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Join(dir, ".codex", "sessions"), &target); err != nil {
		t.Fatal(err)
	}
	// Synthetic fstat outcomes exercise wrong uid without privileged chown.
	for name, reject := range map[string]unix.Stat_t{"parent": parent, "root": target} {
		t.Run(name, func(t *testing.T) {
			seen := false
			root, err := helperScanOpenDirectoryWith(home, ".codex/sessions", uid, func(fd int, st *unix.Stat_t) error {
				if err := unix.Fstat(fd, st); err != nil {
					return err
				}
				if name == "parent" && st.Dev == target.Dev && st.Ino == target.Ino {
					t.Fatal("walk descended through a rejected parent")
				}
				if st.Dev == reject.Dev && st.Ino == reject.Ino {
					st.Uid = uid + 1
					seen = true
				}
				return nil
			})
			if root != nil {
				_ = root.Close()
				t.Fatal("wrong-owner directory accepted")
			}
			if !seen || !errors.Is(err, errHelperScanOwner) {
				t.Fatal("component owner was not checked")
			}
		})
	}
}
