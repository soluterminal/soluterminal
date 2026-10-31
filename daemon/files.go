package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// File reading + line-level editing for the FileTile feature.
//
// Design intent: this is a *viewer* with deliberate, one-line-at-a-time
// edits — not a code editor. Hard caps on file size and strict mtime
// checking on writes keep accidents contained.
const maxFileBytes = 512 * 1024 // 512 KB — plenty for configs; refuses on source files, which should be edited in the IDE

type fileReadResp struct {
	Path  string   `json:"path"`
	Lines []string `json:"lines"`
	Mtime int64    `json:"mtime"` // unix millis
	Size  int64    `json:"size"`
	IsEnv bool     `json:"isEnv"`
	// TrailingNewline is true if the file ends with \n. Preserved on
	// writes so line-edits don't accidentally strip or add a final newline.
	TrailingNewline bool `json:"trailingNewline"`
}

// fileAccessOverRelay reports whether this request arrived over the
// internet-exposed cloud relay rather than the local, auth-gated server.
// The cloud proxy stamps X-Forwarded-Prefix=/m/<machineId> on every relayed
// request and *overwrites* any client-supplied value (cloud/proxy.go's
// rewriteMachineProxyRequest), so a remote caller cannot forge "I'm local".
// A local caller could fake "I'm remote", but that only further restricts
// itself — the safe direction. We use this to refuse secret/credential files
// ONLY over the relay, while the trusted local operator keeps full access to
// their own .env / key / credential files.
func fileAccessOverRelay(r *http.Request) bool {
	return requestViaRelay(r)
}

// daemonCredentialDir reports whether a single path component names one of the
// daemon's own credential/launcher/runtime directories.
func daemonCredentialDir(part string) bool {
	switch part {
	case "harness-accounts", "harness-launchers", "harness-state":
		return true
	}
	return false
}

// daemonOwnedSecretPathRefusal refuses LOCAL file-API access to the daemon's
// own credential/launcher state: the API-key stores, launcher scripts, DSH
// home, and the bearer token / claim / service env at the data-directory root.
// It is deliberately narrower than secretPathRefusal (which stays relay-only
// so the local operator can still open their own .env / .ssh): a second OS
// account that reaches the loopback port must not be able to read the
// credentials the daemon holds on the owner's behalf. Reads and writes both
// apply it, so a peer cannot overwrite launcher scripts or key material either.
func daemonOwnedSecretPathRefusal(abs, dataDir string) string {
	if reason := credentialPathRefusal(abs, dataDir); reason != "" {
		return reason
	}
	const refusal = "refusing to access daemon credential/state files"
	for _, part := range strings.Split(filepath.ToSlash(abs), "/") {
		if daemonCredentialDir(part) {
			return refusal
		}
	}
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	// Compare against both the literal and the symlink-resolved data dir: the
	// request path is canonicalized, but the data dir may (macOS /var →
	// /private/var) or may not need resolving, and either side can be the
	// resolved one.
	roots := []string{}
	if a, err := filepath.Abs(dataDir); err == nil {
		roots = append(roots, filepath.Clean(a))
	}
	if resolved, err := filepath.EvalSymlinks(dataDir); err == nil {
		roots = append(roots, filepath.Clean(resolved))
	}
	base := strings.ToLower(filepath.Base(abs))
	for _, root := range roots {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		switch base {
		case "token", "claim.json", "env", "team-broker-capabilities.json":
			return refusal
		}
	}
	return ""
}

func (a *apiServer) daemonSecretPathRefusal(abs string) string {
	return daemonOwnedSecretPathRefusal(abs, a.dataDir)
}

func (a *apiServer) readFile(w http.ResponseWriter, r *http.Request) {
	abs, err := canonicalizeFilePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Secret/credential refusal depends on the transport. The relay gets the
	// BROAD list (an unconfined remote read is an exfiltration primitive). The
	// local path keeps the operator's own .env/.ssh viewable but still refuses
	// the daemon's own credential/state dirs and token/claim/env files, which a
	// second local OS account reaching the loopback port must not read.
	if reason := a.daemonSecretPathRefusal(abs); reason != "" {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	if fileAccessOverRelay(r) {
		// Root confinement first (files_relay_roots.go), then the deny-list
		// as a second layer inside the roots.
		if reason := a.relayFileRefusal(r, abs); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
		if reason := secretPathRefusal(abs); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}
	data, info, err := readUserFile(abs, maxFileBytes)
	if err != nil {
		msg, status := userFileReadError(err)
		http.Error(w, msg, status)
		return
	}
	if containsNullByte(data) {
		http.Error(w, "file appears binary (contains null bytes)", 400)
		return
	}

	content := string(data)
	trailing := strings.HasSuffix(content, "\n")
	if trailing {
		content = strings.TrimSuffix(content, "\n")
	}
	lines := strings.Split(content, "\n")

	writeJSON(w, 200, fileReadResp{
		Path:            abs,
		Lines:           lines,
		Mtime:           info.ModTime().UnixMilli(),
		Size:            info.Size(),
		IsEnv:           looksLikeEnvFile(abs),
		TrailingNewline: trailing,
	})
}

type writeLineReq struct {
	Path          string `json:"path"`
	LineNo        int    `json:"lineNo"` // 0-indexed
	NewText       string `json:"newText"`
	ExpectedMtime int64  `json:"expectedMtime"`
}

type writeLineResp struct {
	Mtime int64 `json:"mtime"`
}

func (a *apiServer) writeFileLine(w http.ResponseWriter, r *http.Request) {
	var req writeLineReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	// Over the relay, refuse writes to secret/credential files. Reads are
	// already refused; an unguarded write is the worse primitive — it can
	// plant an authorized_keys line or overwrite the daemon's own token.
	// Locally, keep the operator's own edits available but still refuse the
	// daemon's credential/state dirs, so a peer on the loopback port cannot
	// overwrite launcher scripts or key material.
	if abs, cerr := canonicalizeFilePath(req.Path); cerr == nil {
		if reason := a.daemonSecretPathRefusal(abs); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}
	if fileAccessOverRelay(r) {
		abs, cerr := canonicalizeFilePath(req.Path)
		if cerr != nil {
			http.Error(w, cerr.Error(), http.StatusBadRequest)
			return
		}
		if reason := a.relayFileRefusal(r, abs); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
		if reason := secretPathRefusal(abs); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}
	abs, newMtime, status, err := writeFileLineAt(req.Path, req.LineNo, req.NewText, req.ExpectedMtime)
	_ = abs
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, 200, writeLineResp{Mtime: newMtime})
}

// writeFileLineAt replaces a single 0-indexed line of a text file in
// place, atomically, guarded by an optimistic mtime check. It is the
// shared core behind both the /api/files/write-line HTTP handler and
// the MCP write_file_line tool — keep the two callers thin.
//
// expectedMtime is the file's mtime (unix millis) the caller last saw;
// if it's non-zero and no longer matches the file on disk, the write
// is refused so a concurrent edit isn't silently clobbered. Returns the
// canonical path, the post-write mtime, and an HTTP-style status code
// describing the failure class so the HTTP handler can pass it through.
func writeFileLineAt(path string, lineNo int, newText string, expectedMtime int64) (abs string, newMtime int64, status int, err error) {
	if strings.Contains(newText, "\n") {
		return "", 0, 400, errors.New("newText must not contain newlines")
	}
	abs, err = canonicalizeFilePath(path)
	if err != nil {
		return "", 0, 400, err
	}
	data, info, err := readUserFile(abs, maxFileBytes)
	if err != nil {
		msg, status := userFileReadError(err)
		return "", 0, status, errors.New(msg)
	}
	curMtime := info.ModTime().UnixMilli()
	if expectedMtime != 0 && expectedMtime != curMtime {
		// Optimistic-concurrency check — refuse if the file changed
		// since the client loaded it. Client should re-fetch and retry.
		return "", 0, 409, errors.New("file modified externally since load (mtime mismatch)")
	}

	if containsNullByte(data) {
		return "", 0, 400, errors.New("file appears binary (contains null bytes)")
	}

	content := string(data)
	trailing := strings.HasSuffix(content, "\n")
	if trailing {
		content = strings.TrimSuffix(content, "\n")
	}
	lines := strings.Split(content, "\n")

	if lineNo < 0 || lineNo >= len(lines) {
		return "", 0, 400, errors.New("line number out of range")
	}
	lines[lineNo] = newText
	next := strings.Join(lines, "\n")
	if trailing {
		next += "\n"
	}

	if err := writeFileAtomic(abs, []byte(next), info.Mode()); err != nil {
		return "", 0, 500, err
	}
	newInfo, err := os.Stat(abs)
	if err != nil {
		return "", 0, 500, err
	}
	return abs, newInfo.ModTime().UnixMilli(), 200, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	// Atomic replace through a unique sibling temp file. os.CreateTemp uses
	// O_EXCL, so a pre-existing "<target>.solu-tmp" symlink cannot redirect
	// writes into another file.
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".solu-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// canonicalizeFilePath resolves a user-supplied path to an absolute form.
// Handles leading ~ for the daemon user's home dir, and resolves any
// symlinks so the returned path is the real on-disk target — this way
// any future allowlist / boundary check sees the actual file, not a
// symlink that could redirect outside the intent. If the file doesn't
// exist yet (or symlink resolution fails for any other reason), we
// fall back to the cleaned absolute path so callers can still surface
// a clean 404 / "not found" rather than a confusing canonicalize error.
// userFileReadError maps a readUserFile / openUserFile error to the message
// and HTTP status the files API returns.
func userFileReadError(err error) (string, int) {
	switch {
	case errors.Is(err, errUserFileTooLarge):
		return fmt.Sprintf("file too large (max %d KB — too big for a viewer, edit in your IDE)", maxFileBytes/1024), http.StatusRequestEntityTooLarge
	case errors.Is(err, errUserFileNotRegular):
		return err.Error(), http.StatusBadRequest
	case errors.Is(err, fs.ErrNotExist):
		return "not found: " + err.Error(), http.StatusNotFound
	}
	return "cannot read: " + err.Error(), http.StatusInternalServerError
}

func canonicalizeFilePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path required")
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved, nil
	}
	return abs, nil
}

func containsNullByte(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

// looksLikeEnvFile matches common secret-file naming conventions —
// `.env`, `.env.local`, `.env.production`, `foo.env`. The FileTile
// will auto-mask values in these files by default.
func looksLikeEnvFile(p string) bool {
	name := filepath.Base(p)
	return strings.HasPrefix(name, ".env") || strings.HasSuffix(name, ".env")
}

// secretPathRefusal returns a non-empty reason when a path points at a
// likely secret/credential file that file tools must not touch. It
// guards BOTH the read path (readFile, below) and the agent write path
// (mcpWriteFileLineRefusal). The read-path guard matters because the
// file endpoints are reachable over the internet-exposed cloud relay
// without the local auth gate, so an unconfined read is a credential
// exfiltration primitive (e.g. ~/.solu/token, ~/.ssh/id_rsa).
func secretPathRefusal(abs string) string {
	name := strings.ToLower(filepath.Base(abs))
	if looksLikeEnvFile(abs) || name == "env" {
		return "refusing to access likely secret/env file"
	}
	switch name {
	case "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "identity", "authorized_keys":
		return "refusing to access likely key/credential file"
	case "claim.json", "token", "credentials.json", "auth.json":
		// The daemon's own machine claim/token, gcloud/AWS json creds, and
		// Codex auth.json (plaintext access/refresh tokens).
		return "refusing to access likely credential file"
	case ".netrc", ".npmrc", ".pgpass", ".git-credentials", "credentials", ".claude.json":
		// Well-known tool credential stores: curl/ftp (.netrc), npm authToken
		// (.npmrc), postgres password (.pgpass), git stored creds, and
		// ~/.claude.json (Claude Code stores OAuth/bearer tokens + MCP server
		// credentials inline).
		return "refusing to access likely credential file"
	}
	for _, suffix := range []string{".key", ".pem", ".p12", ".pfx", ".crt"} {
		if strings.HasSuffix(name, suffix) {
			return "refusing to access likely key/certificate file"
		}
	}
	for _, marker := range []string{"secret", "token", "credential", "credentials", "password", "passwd", "apikey", "api_key", "private_key"} {
		if strings.Contains(name, marker) {
			return "refusing to access likely secret/credential file"
		}
	}
	slashPath := filepath.ToSlash(strings.ToLower(abs))
	// ~/.config/muse holds muse's auth.json (and the auth.json.solu-bak a team
	// apply leaves behind, which the "credential" name markers above do not
	// catch).
	if strings.Contains(slashPath, "/.config/gcloud/") || strings.Contains(slashPath, "/.config/muse/") {
		return "refusing to access likely credential file"
	}
	for _, part := range strings.FieldsFunc(slashPath, func(r rune) bool {
		return r == '/'
	}) {
		// .solu is the daemon's own state dir (bearer token, claim.json,
		// service env); .claude holds Claude Code's .credentials.json (live
		// OAuth tokens) + account snapshots. The rest are well-known
		// credential stores (.docker/config.json holds registry auth,
		// .kube/config holds cluster tokens; .codex holds Codex auth.json.
		if part == ".ssh" || part == ".gnupg" || part == ".aws" || part == ".solu" || part == ".docker" || part == ".kube" || part == ".claude" || part == ".codex" || part == ".zcode" || daemonCredentialDir(part) {
			return "refusing to access likely credential file"
		}
	}
	return ""
}

// mcpWriteFileLineRefusal returns a reason when write_file_line should
// refuse a path before touching disk. This is intentionally MCP-scoped:
// the local UI file tile may still expose explicit human-driven edits,
// while agent-initiated writes stay away from env, key, and credential
// files even when the caller knows the path and mtime.
func mcpWriteFileLineRefusal(abs string) string {
	return secretPathRefusal(abs)
}
