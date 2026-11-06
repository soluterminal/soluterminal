//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func providerBatchArgv(bin string, args, env []string) (string, []string, error) {
	if !filepath.IsAbs(bin) {
		return bin, args, nil // preserve cwd-relative command resolution
	}
	script, needsNode := npmProviderShimEntrypoint(bin)
	if script == "" {
		return bin, args, nil
	}
	if !isExistingFile(script) {
		return "", nil, errors.New("npm provider entrypoint is missing")
	}
	if !needsNode {
		return script, args, nil
	}
	node := filepath.Join(filepath.Dir(bin), "node.exe")
	if !isExistingFile(node) {
		if env == nil {
			env = os.Environ()
		}
		node = ""
		// Match the effective Windows child PATH, including mixed-case keys.
		path := ""
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
				path = v
			}
		}
		for _, dir := range filepath.SplitList(path) {
			dir = strings.Trim(dir, `"`)
			if !filepath.IsAbs(dir) {
				continue // never bypass Go's implicit-current-directory guard
			}
			candidate := filepath.Join(dir, "node.exe")
			if isExistingFile(candidate) {
				node = candidate
				break
			}
		}
		if node == "" {
			// A Node install made after daemon startup may be on live registry
			// PATH only. This is the same recovery used for provider lookup.
			if p, ok := findOnWindowsPath("node.exe"); ok && filepath.IsAbs(p) && strings.EqualFold(filepath.Ext(p), ".exe") {
				node = p
			}
		}
	}
	if node == "" {
		return "", nil, errors.New("npm provider Node executable is unavailable")
	}
	return node, append([]string{script}, args...), nil
}
