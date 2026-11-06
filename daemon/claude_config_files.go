package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// claudeConfigFileArgs keeps JSON away from cmd.exe's incompatible quoting and
// 8191-character command limit. Native executables and Unix retain their argv.
// The child owner must defer cleanup through Wait, including failed starts.
// Fail closed: never start without the requested hooks or MCP restrictions.
func claudeConfigFileArgs(bin string, args []string) ([]string, func(), error) {
	if runtime.GOOS != "windows" || !cmdStartsClaude([]string{bin}) || !strings.EqualFold(filepath.Ext(bin), ".cmd") {
		return args, func() {}, nil
	}
	return fileClaudeJSONArgs(args)
}

func fileClaudeJSONArgs(args []string) ([]string, func(), error) {
	out := append([]string(nil), args...)
	dir := ""
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if dir != "" {
				_ = os.RemoveAll(dir) // exact fresh directory owned by this invocation
			}
		})
	}
	for i := 0; i < len(out); i++ {
		if out[i] == "--" {
			break
		}
		flag, value, attached := strings.Cut(out[i], "=")
		if flag != "--settings" && flag != "--mcp-config" {
			continue
		}
		index := i
		if !attached {
			if i+1 == len(out) {
				continue
			}
			i++
			index = i
			value = out[index]
		}
		trimmed := strings.TrimSpace(value)
		if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(value)) {
			continue // user paths and invalid input retain the CLI's own handling
		}
		if dir == "" {
			var err error
			dir, err = newPrivateProviderConfigDir()
			if err != nil {
				return nil, func() {}, fmt.Errorf("create private config directory: %w", err)
			}
		}
		path := filepath.Join(dir, strconv.Itoa(index)+".json")
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("write private config: %w", err)
		}
		if attached {
			out[index] = flag + "=" + path
		} else {
			out[index] = path
		}
	}
	return out, cleanup, nil
}
