package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The provider logins the core daemon reports on. The public app has one
// account per user: the provider CLI's own login, which these types only
// read. A build with a credential manager (saved accounts, switching) plugs
// its store in instead through the same interfaces (apiServer.claudeLogin,
// codexLogin, museLogin).

// useCLILogins makes each provider CLI's own login the one the daemon
// reports on (a provider whose home cannot be resolved has none).
func (a *apiServer) useCLILogins() {
	if l, err := newClaudeCLILogin(); err == nil {
		a.claudeLogin = l
	}
	if l, err := newCodexCLILogin(); err == nil {
		a.codexLogin = l
	}
	if l, err := newMuseCLILogin(); err == nil {
		a.museLogin = l
	}
}

// claudeLoginState is what the core asks about the Claude login.
type claudeLoginState interface {
	// LiveCredentialPresent reports a valid live Claude credential.
	LiveCredentialPresent() bool
	// UsageAccountLabel names the account usage is attributed to ("" when
	// unknown).
	UsageAccountLabel() string
}

// codexLoginState is what the core asks about the Codex login.
type codexLoginState interface {
	// livePath is Codex's live auth.json.
	livePath() string
	// requireCodexFileStorage refuses a Codex configured to keep its login
	// outside auth.json.
	requireCodexFileStorage() error
	// UsageAccountResolver labels local Codex usage records with an account.
	UsageAccountResolver() codexAccountResolver
}

// museLoginState is what the core asks about the Muse login.
type museLoginState interface {
	LiveAuthPresent() bool
}

// teamSentinelName is a RESERVED account name kept only as a defensive filter
// for one release (see MINT-DISTRIBUTE.md §7 migration note). It was the
// pointer value for the retired cloud-LEASE team-mode; the v2 mint-and-distribute
// model never writes it, but validCredName still refuses it and the API layer
// still filters it so a stray dev-machine sentinel from the old model can never
// surface as a personal account. Relocated here from the deleted
// claudecreds_teammode.go because validCredName + filterTeamSentinel +
// SnapshotBundle (all NON-retired) reference it.
const teamSentinelName = "__team__"

// validCredName reports whether name is a safe account name. This is the
// SINGLE validator used by every name-bearing path (save, activate, delete,
// rename, and rename's newName). It is a SAFE denylist — it deliberately
// allows spaces and most symbols (so "Northwind Trading (work)" or "acct#1"
// are fine) while still guaranteeing the name can never escape the claude
// dir when interpolated into the ".credentials.<name>.json" filename.
//
// A name is valid iff ALL of these hold:
//   - non-empty after trimming, and identical to its trimmed form (no
//     leading/trailing whitespace).
//   - at most 128 runes long.
//   - contains no "/", no "\", no NUL, and no ASCII control char
//     (< 0x20 or 0x7f).
//   - is not "." and not "..".
//   - does not start with "." (so it can never collide with or masquerade
//     as the ".credentials." prefix / a hidden file).
//   - CRITICAL belt-and-suspenders: the candidate filename
//     ".credentials.<name>.json" is its own basename (no embedded path
//     separators survived the charset checks) AND joining it onto the
//     claude dir resolves to a path whose directory is exactly the claude
//     dir — so even if the charset checks are later loosened, no traversal
//     is possible.
func validCredName(name string) bool {
	if name == "" {
		return false
	}
	// teamSentinelName is a reserved name from the retired lease model — never a
	// personal account. Refuse it at the single boundary every personal-name
	// mutation (SaveCurrent/Activate/Rename/Delete) passes through, so a stray
	// sentinel can never be saved or selected as a personal credential.
	if name == teamSentinelName {
		return false
	}
	if name != strings.TrimSpace(name) {
		return false
	}
	if utf8.RuneCountInString(name) > 128 {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 {
			return false
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return false
	}
	// Belt-and-suspenders containment check against the resolved claude dir.
	return credNameWithinDir(name, credSafetyDir)
}

// credBytesValid reports whether b is a well-formed credential with usable
// tokens: parseable JSON whose claudeAiOauth object carries a non-empty
// accessToken AND a non-empty refreshToken. This is the single gate used
// everywhere we are about to overwrite or save-back a credential file — the
// whole point is to NEVER copy a zero-byte/torn/empty-token file over a good
// credential (live or snapshot).
func credBytesValid(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return false
	}
	raw, ok := top["claudeAiOauth"]
	if !ok {
		return false
	}
	var f claudeOAuthField
	if err := json.Unmarshal(raw, &f); err != nil {
		return false
	}
	return f.AccessToken != "" && f.RefreshToken != ""
}

// claudeOAuthField is the typed view of the one object we modify inside a
// credential file. Every OTHER top-level key (mcpOAuth, etc.) is preserved
// untouched via the raw map.
type claudeOAuthField struct {
	AccessToken              string `json:"accessToken,omitempty"`
	RefreshToken             string `json:"refreshToken,omitempty"`
	ExpiresAt                int64  `json:"expiresAt,omitempty"` // epoch MILLISECONDS
	SubscriptionType         string `json:"subscriptionType,omitempty"`
	RateLimitTier            string `json:"rateLimitTier,omitempty"`
	SubscriptionCreatedEpoch int64  `json:"subscriptionCreatedEpoch,omitempty"`
}

// resolveClaudeDir returns $CLAUDE_CONFIG_DIR if set, else
// ~/.claude. Mirrors the user's existing bash tool.
func resolveClaudeDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// museProviderMeta is the ONLY view the daemon takes of a provider record. It
// has no field for access_token / refresh_token / expires_at / api_key, so no
// decoder in this process can materialise one.
type museProviderMeta struct {
	Mechanism string `json:"mechanism"`
	UserEmail string `json:"user_email"`
}

type museAuthEnvelope struct {
	SchemaVersion json.Number                 `json:"schema_version"`
	Providers     map[string]museProviderMeta `json:"providers"`
}

// resolveMuseAuthPath implements muse's own precedence: MUSE_AUTH_PATH (the
// launcher's override) → $XDG_CONFIG_HOME/muse/auth.json → $HOME/.config/muse/
// auth.json.
//
// The two "relative value" cases are answered differently on purpose.
// MUSE_AUTH_PATH is an explicit instruction to put the credential at one exact
// place, so a value that does not name one is an error rather than a silent
// cwd-relative write. XDG_CONFIG_HOME is ambient environment, and the XDG base
// directory spec already says what a relative value means — "If
// $XDG_CONFIG_HOME is either not set or empty or contains a relative path, a
// default equal to $HOME/.config should be used" — so it falls back instead of
// disabling the whole Muse mailbox over a stray shell profile.
func resolveMuseAuthPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("MUSE_AUTH_PATH")); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("MUSE_AUTH_PATH must be absolute")
		}
		return override, nil
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "muse", "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "muse", "auth.json"), nil
}

func museAuthJSONValid(b []byte) bool {
	_, ok := parseMuseAuthEnvelope(b)
	return ok
}

// parseMuseAuthEnvelope decodes the non-secret envelope only.
func parseMuseAuthEnvelope(b []byte) (museAuthEnvelope, bool) {
	var env museAuthEnvelope
	if len(b) == 0 || len(b) > maxMuseAuthBytes {
		return env, false
	}
	// Two passes: the raw pass proves every provider is a JSON object without
	// giving any provider body a typed home; the typed pass reads mechanism and
	// user_email only.
	var raw struct {
		SchemaVersion *json.Number               `json:"schema_version"`
		Providers     map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return env, false
	}
	if raw.SchemaVersion == nil || *raw.SchemaVersion == "" || len(raw.Providers) == 0 {
		return env, false
	}
	for _, body := range raw.Providers {
		trimmed := strings.TrimSpace(string(body))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return env, false
		}
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return env, false
	}
	return env, true
}

const maxCodexAuthBytes = 64 * 1024

// codexAuthTokens extracts the probe-relevant fields from an auth.json body
// (the CLI's shape: {"tokens": {"access_token", "refresh_token", "account_id"}}).
func codexAuthTokens(authJSON []byte) (access, refresh, accountID string, ok bool) {
	var parsed struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(authJSON, &parsed); err != nil {
		return "", "", "", false
	}
	t := parsed.Tokens
	return t.AccessToken, t.RefreshToken, t.AccountID, t.AccessToken != ""
}

// JWT claims here are attribution hints on already locally trusted credentials,
// never a substitute for provider authentication. account_id can be a shared
// workspace, so include the user subject when the credential supplies one.
func codexAuthSubject(auth []byte) string {
	var a struct {
		Tokens struct {
			ID     string `json:"id_token"`
			Access string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(auth, &a) != nil {
		return ""
	}
	for _, token := range []string{a.Tokens.ID, a.Tokens.Access} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			Sub string `json:"sub"`
		}
		if json.Unmarshal(b, &claims) == nil && claims.Sub != "" {
			return claims.Sub
		}
	}
	return ""
}

func codexAuthFingerprint(auth []byte) string {
	access, refresh, id, _ := codexAuthTokens(auth)
	sum := sha256.Sum256([]byte(id + "\x00" + codexAuthSubject(auth) + "\x00" + access + "\x00" + refresh))
	return hex.EncodeToString(sum[:])
}

const maxMuseAuthBytes = 64 * 1024

// claudeCLILogin is Claude Code's own login, read from its credentials file.
type claudeCLILogin struct {
	// claudeDir is the resolved ~/.claude (or $CLAUDE_CONFIG_DIR).
	claudeDir string
}

func newClaudeCLILogin() (*claudeCLILogin, error) {
	dir, err := resolveClaudeDir()
	if err != nil {
		return nil, err
	}
	return &claudeCLILogin{claudeDir: dir}, nil
}

func (l *claudeCLILogin) livePath() string {
	return filepath.Join(l.claudeDir, ".credentials.json")
}

// LiveCredentialPresent reports whether a valid LIVE Claude credential exists
// (a non-empty access+refresh token in .credentials.json). This is the cheap,
// sound "is Claude authenticated" signal — zero network, zero token spend.
// macOS stores creds in the Keychain (not this file), so this returns false
// there — macOS auth detection is out of scope (we ship Linux/WSL).
func (l *claudeCLILogin) LiveCredentialPresent() bool {
	if l == nil {
		return false
	}
	b, err := os.ReadFile(l.livePath())
	if err != nil {
		return false
	}
	return credBytesValid(b)
}

// configJSONPath is Claude Code's .claude.json: inside $CLAUDE_CONFIG_DIR when
// set, otherwise in the home directory.
func (l *claudeCLILogin) configJSONPath() string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return filepath.Join(l.claudeDir, ".claude.json")
	}
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(filepath.Dir(l.claudeDir), ".claude.json")
}

// liveOauthEmail is the signed-in account's email from .claude.json ("" when
// unknown).
func (l *claudeCLILogin) liveOauthEmail() string {
	b, err := os.ReadFile(l.configJSONPath())
	if err != nil {
		return ""
	}
	var top struct {
		OauthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		return ""
	}
	return strings.TrimSpace(top.OauthAccount.EmailAddress)
}

// UsageAccountLabel attributes usage to the signed-in account's email.
func (l *claudeCLILogin) UsageAccountLabel() string {
	return strings.ToLower(l.liveOauthEmail())
}

// codexCLILogin is the Codex CLI's own login (auth.json in CODEX_HOME).
type codexCLILogin struct {
	codexDir string
}

func newCodexCLILogin() (*codexCLILogin, error) {
	dir, err := resolveCodexHome()
	if err != nil {
		return nil, err
	}
	return &codexCLILogin{codexDir: dir}, nil
}

// resolveCodexHome is $CODEX_HOME (which must be absolute) or ~/.codex.
func resolveCodexHome() (string, error) {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("CODEX_HOME must be absolute")
	}
	return dir, nil
}

func (l *codexCLILogin) livePath() string { return filepath.Join(l.codexDir, "auth.json") }

func (l *codexCLILogin) requireCodexFileStorage() error { return codexRequireFileStorage(l.codexDir) }

// codexRequireFileStorage refuses a Codex whose config.toml keeps the login
// outside auth.json (cli_auth_credentials_store other than "file").
func codexRequireFileStorage(codexDir string) error {
	b, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	setting := regexp.MustCompile(`^\s*cli_auth_credentials_store\s*=\s*["']([^"']+)["']\s*(?:#.*)?$`)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			break
		}
		if m := setting.FindStringSubmatch(line); m != nil && m[1] != "file" {
			return errors.New("Codex account switching requires file credential storage; set cli_auth_credentials_store = \"file\" in Codex config and sign in again")
		}
	}
	return nil
}

// museCLILogin is Muse's own login (its auth.json).
type museCLILogin struct {
	authPath string
}

func newMuseCLILogin() (*museCLILogin, error) {
	path, err := resolveMuseAuthPath()
	if err != nil {
		return nil, err
	}
	return &museCLILogin{authPath: path}, nil
}

func (l *museCLILogin) LiveAuthPresent() bool {
	if l == nil {
		return false
	}
	b, err := os.ReadFile(l.authPath)
	return err == nil && museAuthJSONValid(b)
}

// credSafetyDir is the directory the containment assertion in validCredName
// resolves against. It is a non-empty sentinel rather than "" so the
// filepath.Dir/Clean equality is meaningful even when validCredName is
// called as a pure helper (e.g. from the API layer before a store exists).
// The per-name candidate filename never contains a separator once the
// charset checks pass, so the specific directory value does not change the
// verdict — it only proves the join stays inside whatever dir is used.
const credSafetyDir = "/__claudecreds__"

// credNameWithinDir is the containment assertion: it builds the candidate
// filename for name and verifies it is its own basename and that joining it
// onto dir resolves to a path whose parent directory is exactly dir. Any
// surviving traversal makes one of these fail.
func credNameWithinDir(name, dir string) bool {
	candidate := ".credentials." + name + ".json"
	if filepath.Base(candidate) != candidate {
		return false
	}
	joined := filepath.Join(dir, candidate)
	return filepath.Dir(joined) == filepath.Clean(dir)
}
