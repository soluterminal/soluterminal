package main

import (
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// resolveNodeBin finds the node binary for daemon children. The daemon's own
// PATH is whatever it was launched with (systemd/service PATHs are minimal and
// nvm installs aren't on them — the same trap the systemd units pin around),
// so fall back to the same nvm/asdf layouts used by the Claude/Codex resolvers,
// then the same login-shell resolution pane spawns use.
func resolveNodeBin() (string, error) {
	if p, err := exec.LookPath("node"); err == nil {
		return p, nil
	}
	hd, _ := os.UserHomeDir()
	if hd != "" {
		if p := filepath.Join(hd, ".asdf", "shims", "node"); isExistingFile(p) {
			return p, nil
		}
		var matches []string
		for _, pattern := range []string{
			filepath.Join(hd, ".nvm", "versions", "node", "*", "bin", "node"),
			filepath.Join(hd, ".asdf", "installs", "nodejs", "*", "bin", "node"),
		} {
			found, _ := filepath.Glob(pattern)
			matches = append(matches, found...)
		}
		if p := pickLatestNode(matches); p != "" {
			return p, nil
		}
	}
	if p, err := loginShellCommandPath("node"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("node not found (PATH, nvm, asdf, login shell)")
}

var nodeVerRe = regexp.MustCompile(`/(?:v)?(\d+)\.(\d+)\.(\d+)/`)

// pickLatestNode chooses the highest-version path by NUMERIC semver compare —
// a lexicographic sort would rank v9 above v10.
func pickLatestNode(paths []string) string {
	type cand struct {
		path string
		v    [3]int
	}
	var cands []cand
	for _, p := range paths {
		m := nodeVerRe.FindStringSubmatch(filepath.ToSlash(p))
		if m == nil {
			continue
		}
		var v [3]int
		for i := 0; i < 3; i++ {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		cands = append(cands, cand{p, v})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].v, cands[j].v
		if a[0] != b[0] {
			return a[0] > b[0]
		}
		if a[1] != b[1] {
			return a[1] > b[1]
		}
		return a[2] > b[2]
	})
	return cands[0].path
}

// boundedBuf keeps the first `limit` bytes and drops the rest — enough for
// last-run.log without letting a runaway child eat daemon memory.
type boundedBuf struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *boundedBuf) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

// prependPathDir puts dir at the front of the env's PATH entry (adding one
// if absent), skipping the prepend when dir is already a PATH element.
func prependPathDir(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	if os.PathListSeparator == ';' {
		return prependDirsOnPath(env, []string{dir}, ";", true)
	}
	sep := string(os.PathListSeparator)
	for i, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			continue
		}
		val := kv[len("PATH="):]
		parts := strings.Split(val, sep)
		if len(parts) > 0 && parts[0] == dir {
			return env
		}
		next := []string{dir}
		for _, p := range parts {
			if p != dir {
				next = append(next, p)
			}
		}
		out := append([]string{}, env...)
		out[i] = "PATH=" + strings.Join(next, sep)
		return out
	}
	return append(append([]string{}, env...), "PATH="+dir)
}

// mergeEnv layers KEY=VALUE lists left-to-right, later lists overriding
// earlier keys. Duplicate keys in a child env are a getenv() trap (libc
// returns the FIRST match, Go the last) — emitting each key exactly once
// sidesteps the divergence.
func mergeEnv(layers ...[]string) []string {
	idx := map[string]int{}
	var out []string
	for _, layer := range layers {
		for _, kv := range layer {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				continue
			}
			if i, ok := idx[kv[:eq]]; ok {
				out[i] = kv
				continue
			}
			idx[kv[:eq]] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// withResolvedNodeBin prepends node's bin directory when the shared resolver
// finds one. Resolution is best-effort for shebang support: a worker whose
// executable does not need node must retain the service environment unchanged.
func withResolvedNodeBin(env []string, resolve func() (string, error)) []string {
	nodeBin, err := resolve()
	if err != nil || nodeBin == "" {
		return env
	}
	return prependPathDir(env, filepath.Dir(nodeBin))
}
