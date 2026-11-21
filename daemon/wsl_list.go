package main

import (
	"strings"
	"unicode/utf16"
)

// decodeWSLListing decodes wsl.exe's output: UTF-16LE when it starts with
// a BOM or holds a NUL byte (wsl.exe --list writes UTF-16), UTF-8 otherwise.
// A port of desktop/src-tauri/src/daemon.rs decode_wsl_listing; an odd
// trailing byte is dropped, as there.
func decodeWSLListing(raw []byte) string {
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe || strings.IndexByte(string(raw), 0) >= 0 {
		units := make([]uint16, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
		}
		return strings.TrimPrefix(string(utf16.Decode(units)), "\ufeff")
	}
	return string(raw)
}

// wslDistroNames reads `wsl.exe --list --quiet`: one distro name per line.
func wslDistroNames(quiet string) []string {
	var names []string
	for _, line := range strings.Split(quiet, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// wslDefaultDistro finds the default distro: the `* ` row of
// `wsl.exe --list --verbose`, matched against the names from
// `--list --quiet`, so localized headings and state names are never read
// (as desktop daemon.rs default_wsl_is_running does). A name must be
// followed by at least two spaces, so "Ubuntu" never matches "Ubuntu Dev";
// the longest matching name wins.
func wslDefaultDistro(verbose, quiet string) (string, bool) {
	var row string
	for _, line := range strings.Split(verbose, "\n") {
		if r, ok := strings.CutPrefix(strings.TrimLeft(line, " \t"), "* "); ok {
			row = strings.TrimRight(r, "\r")
			break
		}
	}
	if row == "" {
		return "", false
	}
	best := ""
	for _, name := range wslDistroNames(quiet) {
		rest, ok := strings.CutPrefix(row, name)
		if !ok || !strings.HasPrefix(rest, "  ") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 || (fields[len(fields)-1] != "1" && fields[len(fields)-1] != "2") {
			continue
		}
		if len(name) > len(best) {
			best = name
		}
	}
	return best, best != ""
}
