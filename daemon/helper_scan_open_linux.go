//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const helperScanFileOpenFlags = unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_NOFOLLOW | unix.O_CLOEXEC

var (
	errHelperScanUnsafePath  = errors.New("helper scan: unsafe path or file type")
	errHelperScanOwner       = errors.New("helper scan: wrong owner")
	errHelperScanUnavailable = errors.New("helper scan: file access unavailable")
)

// helperScanOpenHome resolves the helper's real uid through passwd. HOME
// and the helper's environment-fallback home are never used for scan access.
// Static CGO_ENABLED=0 builds support /etc/passwd entries, not NSS-only users.
// No runtime caller is registered yet.
func helperScanOpenHome() (*os.File, error) {
	return helperScanOpenHomeWith(uint32(os.Getuid()), user.LookupId)
}

// The kernel-resolved passwd home is the trust boundary. A symlink at or above
// that home is allowed; only the opened directory's type and uid are trusted.
// Tests inject passwd metadata pointing only at synthetic fixture directories.
func helperScanOpenHomeWith(uid uint32, lookup func(string) (*user.User, error)) (*os.File, error) {
	id := strconv.FormatUint(uint64(uid), 10)
	u, err := lookup(id)
	if err != nil || u == nil {
		return nil, errHelperScanUnavailable
	}
	if u.Uid != id {
		return nil, errHelperScanOwner
	}
	if !filepath.IsAbs(u.HomeDir) || strings.ContainsRune(u.HomeDir, 0) {
		return nil, errHelperScanUnsafePath
	}
	fd, err := helperScanRetryOpen(func() (int, error) {
		return unix.Open(u.HomeDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	})
	if err != nil {
		return nil, helperScanOpenError(err)
	}
	if err := helperScanCheckDirectory(fd, uid, unix.Fstat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "scan-home"), nil
}

// helperScanOpenDirectory walks from an already checked home descriptor.
// Callers supply fixed local components (.claude/projects, .codex/sessions,
// credential parents, or the scan-key directory), never a host-supplied path.
// Every component is opened without following symlinks and fstat-checked for
// directory type and uid before it can be used to open the next component.
func helperScanOpenDirectory(home *os.File, relative string, uid uint32) (*os.File, error) {
	return helperScanOpenDirectoryWith(home, relative, uid, unix.Fstat)
}

func helperScanOpenDirectoryWith(home *os.File, relative string, uid uint32, stat func(int, *unix.Stat_t) error) (*os.File, error) {
	if home == nil || !helperScanRelativePath(relative) {
		return nil, errHelperScanUnsafePath
	}
	fd, err := helperScanDuplicateRoot(home)
	if err != nil {
		return nil, err
	}
	if err := helperScanCheckDirectory(fd, uid, stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i == len(parts)-1 {
			flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC
		}
		next, openErr := helperScanRetryOpen(func() (int, error) { return unix.Openat(fd, part, flags, 0) })
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, helperScanOpenError(openErr)
		}
		fd = next
		if err := helperScanCheckDirectory(fd, uid, stat); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), "scan-root"), nil
}

func helperScanCheckDirectory(fd int, uid uint32, stat func(int, *unix.Stat_t) error) error {
	var st unix.Stat_t
	if err := stat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errHelperScanUnsafePath
	}
	if st.Uid != uid {
		return errHelperScanOwner
	}
	return nil
}

// helperScanOpenRegular opens beneath an already checked root fd. The returned
// fd is owned by the caller and CLOEXEC. This function returns only fixed
// classifications: neither a path nor a raw OS error is returned or logged.
func helperScanOpenRegular(root *os.File, relative string, uid uint32) (*os.File, error) {
	return helperScanOpenRegularWith(root, relative, uid, unix.Openat2)
}

type helperScanOpenat2Func func(int, string, *unix.OpenHow) (int, error)

func helperScanOpenRegularWith(root *os.File, relative string, uid uint32, openat2 helperScanOpenat2Func) (*os.File, error) {
	if root == nil || !helperScanRelativePath(relative) {
		return nil, errHelperScanUnsafePath
	}
	rootFD, err := helperScanDuplicateRoot(root)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errHelperScanUnsafePath
	}
	if rootStat.Uid != uid {
		return nil, errHelperScanOwner
	}
	how := &unix.OpenHow{
		Flags:   uint64(helperScanFileOpenFlags),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := helperScanRetryOpen(func() (int, error) { return openat2(rootFD, relative, how) })
	// Only a missing syscall permits the component-walk fallback. An access
	// refusal, including a seccomp EPERM, must not turn into a different open.
	if errors.Is(err, unix.ENOSYS) {
		fd, err = helperScanOpenatFallback(rootFD, relative)
	}
	if err != nil {
		return nil, helperScanOpenError(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, errHelperScanUnsafePath
	}
	if st.Uid != uid {
		_ = unix.Close(fd)
		return nil, errHelperScanOwner
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, helperScanOpenError(err)
	}
	return os.NewFile(uintptr(fd), "scan-file"), nil
}

// Duplicate under RawConn.Control: the original fd cannot be closed/reused
// during duplication. The private copy remains valid even if shutdown closes
// the caller's os.File while the open is in progress.
func helperScanDuplicateRoot(root *os.File) (int, error) {
	raw, err := root.SyscallConn()
	if err != nil {
		return -1, errHelperScanUnsafePath
	}
	fd := -1
	var dupErr error
	err = raw.Control(func(source uintptr) {
		fd, dupErr = unix.FcntlInt(source, unix.F_DUPFD_CLOEXEC, 0)
	})
	if err != nil {
		return -1, errHelperScanUnsafePath
	}
	if dupErr != nil {
		return -1, helperScanOpenError(dupErr)
	}
	return fd, nil
}

func helperScanRelativePath(path string) bool {
	return path != "." && fs.ValidPath(path) && !strings.ContainsRune(path, 0)
}

func helperScanOpenatFallback(rootFD int, relative string) (int, error) {
	fd, err := unix.FcntlInt(uintptr(rootFD), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		flags := helperScanFileOpenFlags
		if i < len(parts)-1 {
			flags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		next, openErr := helperScanRetryOpen(func() (int, error) { return unix.Openat(fd, part, flags, 0) })
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, openErr
		}
		fd = next
	}
	return fd, nil
}

func helperScanOpenError(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return fs.ErrNotExist
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return fs.ErrPermission
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENAMETOOLONG), errors.Is(err, unix.EBADF):
		return errHelperScanUnsafePath
	default:
		return errHelperScanUnavailable
	}
}

// Retry transient interruption, but leave a persistent signal storm bounded.
func helperScanRetryOpen(open func() (int, error)) (int, error) {
	for i := 0; i < 8; i++ {
		fd, err := open()
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
	return -1, unix.EINTR
}
