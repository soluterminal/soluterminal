//go:build windows

package main

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func sweepProviderConfigsAtStartup(started time.Time) {
	logProviderConfigSweep(os.TempDir(), started, oldOwnedProviderConfigDir, providerConfigProcessesIdle, log.Printf)
}

// Filter in the filesystem rather than walking unrelated TEMP entries, so a
// busy temp directory cannot starve the same candidates on every restart.
func providerConfigCandidates(root string) (func() (string, error), func(), error) {
	pattern, err := windows.UTF16PtrFromString(filepath.Join(root, providerConfigPrefix+"*"))
	if err != nil {
		return nil, nil, err
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(pattern, &data)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return func() (string, error) { return "", io.EOF }, func() {}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	first := true
	return func() (string, error) {
		if !first {
			if err := windows.FindNextFile(h, &data); err != nil {
				if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
					return "", io.EOF
				}
				return "", err
			}
		}
		first = false
		return windows.UTF16ToString(data.FileName[:]), nil
	}, func() { windows.FindClose(h) }, nil
}

func oldOwnedProviderConfigDir(path string, started time.Time) (bool, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	var data windows.Win32FileAttributeData
	if err := windows.GetFileAttributesEx(ptr, windows.GetFileExInfoStandard, (*byte)(unsafe.Pointer(&data))); err != nil {
		return false, err
	}
	if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || data.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || !time.Unix(0, data.CreationTime.Nanoseconds()).Before(started) {
		return false, nil
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	return providerConfigOwnedBy(path, user.User.Sid)
}

func providerConfigOwnedBy(path string, sid *windows.SID) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.Equals(sid), nil
}

// Legacy directories have no lease receipt. Windows does not provide argv via
// readHarnessCmdline, and an agent need not keep its JSON open. Conservatively
// defer all deletion if any possible consumer survives, even another user's.
// This deliberately prefers retained private files over breaking a live pane.
func providerConfigProcessesIdle() (bool, error) {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil {
		return false, err
	}
	for range 32768 {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if pe.ProcessID != uint32(os.Getpid()) && (name == "node.exe" || name == "claude.exe" || name == "cmd.exe" || name == "solud.exe" || strings.HasPrefix(name, "solud-")) {
			return false, nil
		}
		if err := windows.Process32Next(h, &pe); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return true, nil
			}
			return false, err
		}
	}
	return false, errors.New("config sweep process limit reached")
}
