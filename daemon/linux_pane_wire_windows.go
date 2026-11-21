//go:build windows

package main

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// helperHostInputsForThisHost: <LocalAppData>\SoluTerminalHost\manifest
// (the known folder, not %LOCALAPPDATA%; host-record-and-lease-v1), and the
// .gz beside the canonical solud.exe (M1-5 lays both).
func helperHostInputsForThisHost() (helperHostInputs, error) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return helperHostInputs{}, err
	}
	gz, err := helperGzBeside()
	if err != nil {
		return helperHostInputs{}, err
	}
	return helperHostInputs{
		manifestDir: filepath.Join(local, "SoluTerminalHost", "manifest"),
		gzPath:      gz,
	}, nil
}
