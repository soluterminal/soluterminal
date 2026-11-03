package main

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"strings"
)

// procArgs is what macOS's kern.procargs2 sysctl reports for a process: the
// executable's path, its argv and its environment.
type procArgs struct {
	exec string
	argv []string
	env  map[string]string
}

// parseProcArgs2 decodes kern.procargs2: a native-endian int32 argc, the
// executable path, NUL padding, argc NUL-terminated arguments, then the
// NUL-terminated environment. Kept free of build tags so it is tested on
// every platform (procinfo_darwin.go reads it).
func parseProcArgs2(b []byte) (procArgs, bool) {
	if len(b) < 4 {
		return procArgs{}, false
	}
	argc := int(int32(binary.LittleEndian.Uint32(b[:4])))
	rest := b[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 || argc < 0 || argc > 1<<16 {
		return procArgs{}, false
	}
	out := procArgs{exec: string(rest[:end]), env: map[string]string{}}
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	fields := bytes.Split(rest, []byte{0})
	i := 0
	for ; i < len(fields) && len(out.argv) < argc; i++ {
		out.argv = append(out.argv, string(fields[i]))
	}
	for ; i < len(fields); i++ {
		kv := string(fields[i])
		if kv == "" {
			break
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			out.env[k] = v
		}
	}
	return out, true
}

// processNameFromArgs is the executable's file name, as the caller checks
// (caller_authority.go compares lower-cased names without ".exe").
func processNameFromArgs(a procArgs) string {
	if a.exec != "" {
		return filepath.Base(a.exec)
	}
	if len(a.argv) > 0 {
		return filepath.Base(a.argv[0])
	}
	return ""
}
