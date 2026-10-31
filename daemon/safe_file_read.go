package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Reads of a caller-supplied path (the files API, MCP read_file and
// preview_file, write-line's read-modify-write) go through openUserFile /
// readUserFile so they only ever read a regular file of bounded size.
//
// The checks are made on the OPENED handle, never on the path: the file is
// opened first (non-blocking on Unix, so opening a named pipe returns at
// once), then fstat'd, and anything that is not a regular file is refused
// before a single byte is read. The size is checked twice: up front from the
// handle's Stat, and again while reading through an io.LimitReader, which
// covers a file that grows after the Stat and pseudo-files that report size 0.

var (
	// errUserFileNotRegular: a directory, device, named pipe, socket or other
	// non-regular file. Wrapped with a description of what was found.
	errUserFileNotRegular = errors.New("not a regular file")
	// errUserFileIsDir is the directory case of errUserFileNotRegular.
	errUserFileIsDir = fmt.Errorf("%w: path is a directory", errUserFileNotRegular)
	// errUserFileTooLarge: the file is over the caller's size cap.
	errUserFileTooLarge = errors.New("file too large")
)

// userFileKind names a non-regular file mode for an error message.
func userFileKind(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeCharDevice != 0, mode&fs.ModeDevice != 0:
		return "a device"
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode&fs.ModeIrregular != 0:
		return "a special file"
	}
	return "not a regular file"
}

func notRegularErr(mode fs.FileMode) error {
	if mode.IsDir() {
		return errUserFileIsDir
	}
	return fmt.Errorf("%w: path is %s; only regular files can be read", errUserFileNotRegular, userFileKind(mode))
}

// openUserFile opens abs for reading and returns the handle and its fstat
// info only if it is a regular file no larger than maxBytes. The caller owns
// the returned file. abs should already be canonical (canonicalizeFilePath
// resolves symlinks); a symlink swapped in afterwards is not followed on Unix,
// and the handle's own type is what is checked either way.
func openUserFile(abs string, maxBytes int64) (*os.File, fs.FileInfo, error) {
	if userFileWindowsNames {
		if reason := windowsDevicePathRefusal(abs); reason != "" {
			return nil, nil, fmt.Errorf("%w: %s", errUserFileNotRegular, reason)
		}
	}
	f, err := openUserFileHandle(abs)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, notRegularErr(info.Mode())
	}
	if maxBytes >= 0 && info.Size() > maxBytes {
		_ = f.Close()
		return nil, nil, errUserFileTooLarge
	}
	return f, info, nil
}

// readUserFile reads a whole regular file of at most maxBytes. The returned
// info is the opened file's fstat.
func readUserFile(abs string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	f, info, err := openUserFile(abs, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, nil, errUserFileTooLarge
	}
	return data, info, nil
}

// windowsDevicePathRefusal reports why p names a Windows device or pipe
// rather than a file: a device/pipe namespace prefix (\\.\, \\?\ including
// \\?\GLOBALROOT, \??\, or their forward-slash spellings), or a reserved
// device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9, CONIN$, CONOUT$) as any
// path component, with or without an extension. It is pure string logic so it
// is tested on every platform; openUserFile applies it only on Windows, where
// those names really are devices.
func windowsDevicePathRefusal(p string) string {
	s := strings.ReplaceAll(p, "/", `\`)
	for _, prefix := range []string{`\\.\`, `\\?\`, `\??\`} {
		if strings.HasPrefix(s, prefix) {
			return "device and pipe paths can't be read"
		}
	}
	for _, comp := range strings.Split(s, `\`) {
		if windowsReservedName(comp) {
			return fmt.Sprintf("%q is a reserved device name", comp)
		}
	}
	return ""
}

func windowsReservedName(comp string) bool {
	// Windows ignores everything from the first dot (NUL.txt is NUL), a
	// trailing colon (NUL:), and trailing spaces (NUL .txt).
	if i := strings.IndexAny(comp, ".:"); i >= 0 {
		comp = comp[:i]
	}
	comp = strings.ToUpper(strings.TrimRight(comp, " "))
	switch comp {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(comp) == 4 && (strings.HasPrefix(comp, "COM") || strings.HasPrefix(comp, "LPT")) {
		return comp[3] >= '1' && comp[3] <= '9'
	}
	// Superscript digits (COM¹, LPT²...) are device names too.
	for _, base := range []string{"COM", "LPT"} {
		if rest, ok := strings.CutPrefix(comp, base); ok && (rest == "¹" || rest == "²" || rest == "³") {
			return true
		}
	}
	return false
}
