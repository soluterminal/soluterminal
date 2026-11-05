package main

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Git-remote resolution for cc-usage capture.
//
// A transcript line carries the turn's `cwd` and `gitBranch`, but NOT the
// repository's remote URL — that lives only in the working tree's git config.
// The daemon runs on the same machine as those working trees, so it is the ONE
// place that can map a cwd to its GitHub (or other host) repo. We resolve it
// here, attach a normalized https URL to each captured row (ccUsageRecord.GitRepo),
// and the cloud surfaces it in top_projects as a clickable link.
//
// Cheap + safe: `git config --get remote.origin.url` only READS config — it
// runs no repo hooks, touches no network, and needs no credentials. Results are
// cached per-cwd for the daemon's lifetime (a remote URL effectively never
// changes; a daemon restart re-resolves), and a negative result ("" — not a
// repo, no origin, git missing) is cached too so we never re-exec for the same
// non-repo dir. git worktrees resolve correctly because git itself walks to the
// common dir for us.

// ccRepoResolveTimeout bounds a single `git config` exec so a hung git (e.g. a
// stale network filesystem) can never stall a scan pass.
const ccRepoResolveTimeout = 3 * time.Second

// repoResolver caches cwd → normalized-repo-URL ("" = no repo / unresolvable).
// Safe for the single scan goroutine; the mutex is defensive insurance.
type repoResolver struct {
	mu    sync.Mutex
	cache map[string]string
}

func newRepoResolver() *repoResolver {
	return &repoResolver{cache: map[string]string{}}
}

// resolve returns the normalized https repo URL for cwd, or "" when cwd is not
// in a git repo / has no origin remote / git is unavailable. Cached per cwd
// (including the "" negative result).
func (rr *repoResolver) resolve(ctx context.Context, cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	rr.mu.Lock()
	if v, ok := rr.cache[cwd]; ok {
		rr.mu.Unlock()
		return v
	}
	rr.mu.Unlock()

	repo := normalizeGitRemote(gitOriginURL(ctx, cwd))

	rr.mu.Lock()
	rr.cache[cwd] = repo
	rr.mu.Unlock()
	return repo
}

// gitOriginURL execs `git -C <cwd> config --get remote.origin.url` with a hard
// timeout. Any error (not a repo, no origin, git missing, timeout) yields "".
// The hardened command never prompts and reads only the repository's config.
func gitOriginURL(ctx context.Context, cwd string) string {
	cctx, cancel := context.WithTimeout(ctx, ccRepoResolveTimeout)
	defer cancel()
	out, err := hardenedGitCommand(cctx, cwd, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// normalizeGitRemote canonicalizes any git remote URL to a clean
// "https://<host>/<owner>/<repo>" (no scheme variance, no creds, no port, no
// trailing ".git"), or "" when it can't be parsed. Handles the three forms git
// emits:
//
//	scp-like : git@github.com:owner/repo.git
//	ssh/https: ssh://git@github.com:22/owner/repo.git, https://github.com/owner/repo.git
//	creds    : https://user:token@github.com/owner/repo.git  → creds stripped
//
// Host-agnostic: GitHub, GitLab (incl. nested groups in the path), Bitbucket,
// and self-hosted all normalize the same way, so the cloud/web treat the result
// as a generic clickable URL.
func normalizeGitRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	var host, repoPath string
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return ""
		}
		host = u.Hostname() // strips userinfo + port
		repoPath = u.Path
	} else if strings.Contains(s, ":") {
		// scp-like [user@]host:path — the FIRST colon splits host from path.
		colon := strings.Index(s, ":")
		hostPart := s[:colon]
		repoPath = s[colon+1:]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = hostPart
	} else {
		return ""
	}

	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, found := strings.Cut(host, ":"); found { // strip any leftover :port
		host = h
	}
	repoPath = strings.Trim(repoPath, "/")
	// Strip a trailing ".git" case-insensitively (git emits lowercase, but a
	// hand-typed remote could be ".GIT").
	if len(repoPath) >= 4 && strings.EqualFold(repoPath[len(repoPath)-4:], ".git") {
		repoPath = repoPath[:len(repoPath)-4]
	}
	repoPath = strings.Trim(repoPath, "/")
	if host == "" || repoPath == "" {
		return ""
	}
	return "https://" + host + "/" + repoPath
}
