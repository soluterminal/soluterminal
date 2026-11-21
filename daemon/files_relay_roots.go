package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Relay file confinement.
//
// The file endpoints (/api/file, /api/file/line, /api/dir/list,
// /api/files/search, /api/git/diff, /api/recent-files) are reachable over the
// internet-exposed cloud relay. A deny-list of secret-looking names
// (secretPathRefusal) is not a boundary: anything it fails to name is readable.
// Over the relay we therefore ALSO confine every path to a set of workspace
// roots, and keep the deny-list as a second layer inside them. Local/loopback
// access is unchanged — the owner at the machine keeps full access.
//
// Root set (recomputed per request, so it follows panes as they come and go):
//   - the spawn cwd and live cwd of every live pane, plus each one's git
//     top-level (found by walking up to the nearest .git entry);
//   - the repository of every workflow task the daemon knows;
//   - extra roots from SOLU_RELAY_FILE_ROOTS (os.PathListSeparator-separated).
//
// Never a root: the filesystem root, the daemon data dir or anything inside
// it, and — unless listed explicitly in SOLU_RELAY_FILE_ROOTS — the home
// directory or any ancestor of it (a pane sitting in ~ must not expose all of
// home). Roots and targets are both symlink-resolved before the containment
// check, so `..` and symlink escapes are refused.
//
// Escape hatch: SOLU_RELAY_FILE_CONFINEMENT=off (also 0/false/disabled)
// restores the legacy deny-list-only behaviour. It exists as the owner's
// rollback if confinement blocks a workflow he relies on from his phone; the
// deny-list and credential refusals still apply with it off.
const (
	relayFileConfinementEnv  = "SOLU_RELAY_FILE_CONFINEMENT"
	relayFileRootsEnv        = "SOLU_RELAY_FILE_ROOTS"
	relayOutsideRootsRefusal = "path is outside the workspace roots reachable over the relay"
)

func relayFileConfinementDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(relayFileConfinementEnv))) {
	case "off", "0", "false", "disabled", "no":
		return true
	}
	return false
}

// relayFileScope is one request's view of the reachable roots. A nil scope
// means "not confined" (local request, or confinement switched off).
type relayFileScope struct {
	roots    []string // canonical, deduplicated
	excluded []string // canonical data-dir variants; never reachable
}

// relayFileScopeFor returns the confinement scope for a relayed request, or
// nil when the request is local or confinement is disabled.
func (a *apiServer) relayFileScopeFor(r *http.Request) *relayFileScope {
	if !fileAccessOverRelay(r) || relayFileConfinementDisabled() {
		return nil
	}
	return a.relayFileScope()
}

// relayFileRefusal is the single-path form for handlers that touch one path.
func (a *apiServer) relayFileRefusal(r *http.Request, path string) string {
	return a.relayFileScopeFor(r).refusal(path)
}

func (a *apiServer) relayFileScope() *relayFileScope {
	var paneDirs []string
	if a.mgr != nil {
		for _, p := range a.mgr.List() {
			if p.Linux != nil {
				// A distro path ("/home/u") would read as a Windows path on
				// the current drive: no relay root until a \\wsl$ mapping.
				continue
			}
			paneDirs = append(paneDirs, p.Cwd, liveCwdOfPane(p))
		}
	}
	paneDirs = append(paneDirs, a.relayPaneDirsForTest...)
	var repos []string
	home, _ := os.UserHomeDir()
	return buildRelayFileScope(paneDirs, repos, relayExplicitRoots(home), home, a.dataDir)
}

func relayExplicitRoots(home string) []string {
	var out []string
	for _, p := range filepath.SplitList(os.Getenv(relayFileRootsEnv)) {
		p = strings.TrimSpace(p)
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/") && home != "":
			p = filepath.Join(home, p[2:])
		}
		if p != "" && filepath.IsAbs(p) {
			out = append(out, p)
		}
	}
	return out
}

// buildRelayFileScope applies the root rules. paneDirs also contribute their
// git top-levels; repos and explicit roots are taken as given.
func buildRelayFileScope(paneDirs, repos, explicit []string, home, dataDir string) *relayFileScope {
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	s := &relayFileScope{excluded: relayPathVariants(dataDir)}
	homes := relayPathVariants(home)
	seen := map[string]bool{}
	add := func(p string, isExplicit bool) {
		if p == "" {
			return
		}
		c, err := filepath.Abs(p)
		if err != nil {
			return
		}
		// A root must exist: resolving it is what defeats a symlinked root.
		if c, err = filepath.EvalSymlinks(c); err != nil {
			return
		}
		if info, err := os.Stat(c); err != nil || !info.IsDir() {
			return
		}
		if seen[c] || filepath.Dir(c) == c {
			return
		}
		for _, ex := range s.excluded {
			if relayPathWithin(ex, c) {
				return
			}
		}
		if !isExplicit {
			for _, h := range homes {
				if relayPathWithin(c, h) {
					return // home itself, or an ancestor of it
				}
			}
		}
		seen[c] = true
		s.roots = append(s.roots, c)
	}
	for _, d := range paneDirs {
		add(d, false)
		add(gitTopLevel(d), false)
	}
	for _, r := range repos {
		add(r, false)
	}
	for _, r := range explicit {
		add(r, true)
	}
	return s
}

// relayPathVariants returns the cleaned absolute and symlink-resolved forms.
func relayPathVariants(p string) []string {
	if p == "" {
		return nil
	}
	var out []string
	if a, err := filepath.Abs(p); err == nil {
		out = append(out, filepath.Clean(a))
	}
	if r, err := filepath.EvalSymlinks(p); err == nil && (len(out) == 0 || r != out[0]) {
		out = append(out, r)
	}
	return out
}

// gitTopLevel walks up from dir to the nearest directory holding a .git entry
// (a directory for a checkout, a file for a worktree or submodule). It avoids
// spawning git per pane on every relayed request.
func gitTopLevel(dir string) string {
	if dir == "" {
		return ""
	}
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// relayCanonicalPath resolves a target to its real on-disk location. A path
// that does not exist yet resolves through its nearest existing ancestor, so a
// symlinked parent cannot smuggle it outside a root.
func relayCanonicalPath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, true
	}
	if r := resolveExistingAncestor(abs); r != "" {
		return filepath.Clean(r), true
	}
	return "", false
}

// relayPathWithin reports whether target is root or lies beneath it.
func relayPathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// refusal returns a non-empty reason when path is not reachable over the
// relay. A nil scope never refuses.
func (s *relayFileScope) refusal(path string) string {
	if s == nil {
		return ""
	}
	c, ok := relayCanonicalPath(path)
	if !ok || !s.allowsCanonical(c) {
		return relayOutsideRootsRefusal
	}
	return ""
}

// allowsCanonical checks an already-resolved path without touching disk.
func (s *relayFileScope) allowsCanonical(c string) bool {
	if s == nil {
		return true
	}
	for _, ex := range s.excluded {
		if relayPathWithin(ex, c) {
			return false
		}
	}
	for _, root := range s.roots {
		if relayPathWithin(root, c) {
			return true
		}
	}
	return false
}

// leadsToRootCanonical reports whether c is a strict ancestor of a root, i.e.
// a directory the relay may traverse (listing only the entries that lead on
// to a root) without being allowed to see the rest of it.
func (s *relayFileScope) leadsToRootCanonical(c string) bool {
	if s == nil {
		return true
	}
	for _, root := range s.roots {
		if root != c && relayPathWithin(c, root) {
			return true
		}
	}
	return false
}

// relayDirEntryVisible decides whether one entry of a relayed directory
// listing is shown. parent is the canonical directory being listed. Entries
// within a root are shown; outside one, only directories on the way to a root
// are (so the phone can still navigate up from a pane cwd). Non-symlink entries
// are resolved by joining (no syscall); symlinks are resolved on disk so a
// link pointing out of the roots is hidden.
func (s *relayFileScope) relayDirEntryVisible(parent, name string, isSymlink bool) bool {
	if s == nil {
		return true
	}
	c := filepath.Join(parent, name)
	if isSymlink {
		resolved, ok := relayCanonicalPath(c)
		if !ok {
			return false
		}
		c = resolved
	}
	return s.allowsCanonical(c) || s.leadsToRootCanonical(c)
}
