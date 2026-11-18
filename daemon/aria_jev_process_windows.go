//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func probeJevProcessIdentity(ctx context.Context, root int) (jevProcessIdentity, error) {
	return probeJevProcessIdentityWithSnapshots(ctx, root, jevProcessWindowsSource, readJevWindowsProcessSnapshot)
}

// Use fresh Toolhelp snapshots, not the UI cache. ConPTY does not expose a
// foreground group: all owned descendants participate, including background
// helpers. Their churn conservatively invalidates a pending evaluation.
func readJevWindowsProcessSnapshot(ctx context.Context, root int) (jevProcessSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, errJevProcessUnavailable
	}
	defer windows.CloseHandle(handle)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(handle, &entry); err != nil {
		return nil, errJevProcessUnavailable
	}
	snapshot := make(jevProcessSnapshot)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.ProcessID > 0 {
			pid := int(entry.ProcessID)
			snapshot[pid] = jevProcessRecord{PID: pid, Parent: int(entry.ParentProcessID)}
		}
		if len(snapshot) > jevProcessMaxEntries {
			return nil, errJevProcessUnavailable
		}
		if err := windows.Process32Next(handle, &entry); err != nil {
			if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return nil, errJevProcessUnavailable
			}
			break
		}
	}
	owned, err := jevOwnedProcessIDs(ctx, snapshot, root)
	if err != nil {
		return nil, err
	}
	for _, pid := range owned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start, order, err := readJevWindowsProcessStart(pid)
		if err != nil {
			return nil, err
		}
		record := snapshot[pid]
		record.Start, record.StartOrder = start, order
		snapshot[pid] = record
	}
	return snapshot, nil
}

func readJevWindowsProcessStart(pid int) (string, uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", 0, errJevProcessUnavailable
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", 0, errJevProcessUnavailable
	}
	return jevWindowsProcessStart(created, exited)
}

func jevWindowsProcessStart(created, exited windows.Filetime) (string, uint64, error) {
	order := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if order == 0 || exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return "", 0, errJevProcessUnavailable
	}
	// "windows:<creation time in ns>" is the process start identity convention.
	return fmt.Sprintf("windows:%d", created.Nanoseconds()), order, nil
}
