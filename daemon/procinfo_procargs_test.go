package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

// lowerNoExe is how caller_authority.go compares process names.
func lowerNoExe(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".exe")
}

func procArgs2Fixture(execPath string, argv, env []string) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(len(argv)))
	b = append(b, execPath...)
	b = append(b, 0, 0, 0, 0) // NUL then alignment padding
	for _, a := range argv {
		b = append(append(b, a...), 0)
	}
	for _, e := range env {
		b = append(append(b, e...), 0)
	}
	return append(b, 0, 0)
}

func TestParseProcArgs2(t *testing.T) {
	b := procArgs2Fixture("/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper",
		[]string{"Google Chrome Helper", "--type=utility", "--utility-sub-type=network.mojom.NetworkService", ""},
		[]string{"HOME=/Users/b", "SOLU_PANE_ID=pane_1", "EMPTY="})
	a, ok := parseProcArgs2(b)
	if !ok {
		t.Fatal("not parsed")
	}
	if got := processNameFromArgs(a); got != "Google Chrome Helper" {
		t.Fatalf("name = %q", got)
	}
	if len(a.argv) != 4 || a.argv[1] != "--type=utility" || a.argv[3] != "" {
		t.Fatalf("argv = %q", a.argv)
	}
	if a.env["SOLU_PANE_ID"] != "pane_1" || a.env["HOME"] != "/Users/b" || len(a.env) != 3 {
		t.Fatalf("env = %v", a.env)
	}
	if !browserProcessNames[lowerNoExe(processNameFromArgs(a))] {
		t.Fatal("Chrome's macOS network helper is not taken for a browser")
	}
	webkit, _ := parseProcArgs2(procArgs2Fixture("/System/Library/Frameworks/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.Networking.xpc/Contents/MacOS/com.apple.WebKit.Networking", []string{"x"}, nil))
	if !sharedNetworkServiceNames[lowerNoExe(processNameFromArgs(webkit))] {
		t.Fatal("WebKit's network service is not recognised as shared")
	}
	for _, bad := range [][]byte{nil, {1, 0, 0}, {1, 0, 0, 0, 'x'}} {
		if _, ok := parseProcArgs2(bad); ok {
			t.Errorf("parsed %v", bad)
		}
	}
}
