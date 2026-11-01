package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"soluterminal-shared/providers"
	"strings"
	"time"
)

//go:embed harness_assets/*.mjs harness_assets/*.sh
var harnessAssets embed.FS

func (a *apiServer) harnessStore(provider string) *harnessCredStore {
	a.harnessMu.Lock()
	defer a.harnessMu.Unlock()
	if a.harnessStores == nil {
		a.harnessStores = map[string]*harnessCredStore{}
	}
	if a.harnessStores[provider] == nil {
		a.harnessStores[provider] = newHarnessCredStore(a.dataDir, provider)
	}
	return a.harnessStores[provider]
}

// harnessPairing is the cloud pairing (base, token) and the scope a native
// harness's shared accounts belong to, captured together so a request keeps
// one identity through a concurrent re-pairing.
func (a *apiServer) harnessPairing() (base, token, scope string) {
	a.cloudMu.RLock()
	base, token, scope = a.cloudBase, a.cloudToken, a.harnessPairingScope
	a.cloudMu.RUnlock()
	if scope == "" && base != "" && token != "" {
		scope = harnessScopeForPairing(a.dataDir, base, token)
	}
	return base, token, scope
}

// harnessScope is the pairing scope of a provider's shared accounts.
func (a *apiServer) harnessScope(string) string {
	_, _, scope := a.harnessPairing()
	return scope
}

func harnessScopeForPairing(dataDir, base, token string) string {
	if base == "" || token == "" {
		return ""
	}
	identity := token
	// Claim identity survives ordinary access-token renewal and daemon restart.
	// Match the captured live token before using a claim being rewritten by pairing.
	if claim, err := LoadClaim(dataDir); err == nil && claim != nil && claim.Token == token && claim.MachineID != "" {
		if claimBase, err := relayToHTTPBase(claim.RelayURL); err == nil && claimBase == base {
			identity = claim.MachineID + "\x00" + claim.ClaimedAt
		}
	}
	h := sha256.Sum256([]byte(base + "\x00" + identity))
	return hex.EncodeToString(h[:])
}

func (a *apiServer) harnessRequest(w http.ResponseWriter, r *http.Request) (*harnessCredStore, bool) {
	provider := r.PathValue("provider")
	if !isHarnessProvider(provider) {
		http.NotFound(w, r)
		return nil, false
	}
	if r.Method != http.MethodGet && fileAccessOverRelay(r) {
		http.Error(w, "credential changes and harness launches are local-only", 403)
		return nil, false
	}
	s := a.harnessStore(provider)
	if err := s.clearLegacyTeamSecrets(); err != nil {
		http.Error(w, "could not clear old team credentials", http.StatusInternalServerError)
		return nil, false
	}
	scope := a.harnessScope(provider)
	if err := s.bindScope(scope); err != nil {
		http.Error(w, err.Error(), 500)
		return nil, false
	}
	return s, true
}

func harnessDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(out) != nil {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}

func (a *apiServer) registerHarnessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/harnesses/{provider}", a.getHarnessAccounts)
	mux.HandleFunc("PUT /api/harnesses/{provider}/accounts", a.putHarnessAccount)
	mux.HandleFunc("DELETE /api/harnesses/{provider}/accounts/{name}", a.deleteHarnessAccount)
	mux.HandleFunc("POST /api/harnesses/{provider}/select", a.selectHarnessAccount)
	mux.HandleFunc("POST /api/harnesses/{provider}/import", a.importHarnessAccount)
	mux.HandleFunc("POST /api/harnesses/{provider}/launch", a.launchHarness)
	mux.HandleFunc("POST /api/harnesses/{provider}/install", a.installHarness)
	mux.HandleFunc("POST /api/harnesses/{provider}/test", a.testHarness)
	mux.HandleFunc("POST /api/harnesses/{provider}/check", a.checkHarness)
	// DeepSeek per-key usage/spend (official CSV import + soft budget) and the
	// account-wide balance probe. Provider-gated inside each handler.
	mux.HandleFunc("GET /api/harnesses/{provider}/usage", a.getHarnessUsage)
	mux.HandleFunc("POST /api/harnesses/{provider}/usage/import", a.importHarnessUsage)
	mux.HandleFunc("PUT /api/harnesses/{provider}/usage/budget", a.putHarnessUsageBudget)
	mux.HandleFunc("GET /api/harnesses/{provider}/balance", a.getHarnessBalance)
	mux.HandleFunc("GET /api/harnesses/{provider}/pricing", a.getHarnessPricing)
}

func (a *apiServer) getHarnessAccounts(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	rows, err := s.list()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	p, _ := providers.Get(s.provider)
	installed := harnessRuntimeInstalled(p)
	// A status ladder, not a bare boolean: installed is the runtime on disk,
	// configured is any saved account, keyPresent is a usable selection, and
	// authenticationStatus reflects only an explicit, still-current Test
	// Connection. A key is never implied to be authenticated without one.
	configured := len(rows) > 0
	// Zero-manual-work provisioning: once a user has an account, install the
	// runtime in the background so they never have to click Install. The poll
	// after the save (and every later poll) observes `installing`, then the
	// finished install. Best-effort and idempotent.
	if a.harnessAutoInstall && configured && !installed {
		a.ensureHarnessRuntime(s.provider)
	}
	installing, installError := a.harnessInstallState(s.provider)
	keyPresent, authStatus := false, "not-tested"
	var lastTest *harnessTestDTO
	if !configured {
		authStatus = "no-account"
	} else if source, name, acc, selErr := s.selected(); selErr != nil {
		authStatus = "no-selection"
	} else {
		keyPresent = true
		if result, found := newHarnessTestStore(a.dataDir, s.provider).get(source+"/"+name, acc); found {
			dto := result.DTO()
			lastTest = &dto
			authStatus = result.Status
		}
	}
	writeJSON(w, 200, map[string]any{
		"accounts": rows, "installed": installed,
		"installing": installing, "installError": installError,
		"configured": configured, "keyPresent": keyPresent,
		"authenticationStatus": authStatus, "lastTest": lastTest,
	})
}

func (a *apiServer) putHarnessAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	var acc harnessAccount
	if !harnessDecode(w, r, &acc) {
		return
	}
	if err := s.save(acc); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *apiServer) deleteHarnessAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	if err := s.remove(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *apiServer) selectHarnessAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	var req struct {
		Source string `json:"source"`
		Name   string `json:"name"`
	}
	if !harnessDecode(w, r, &req) {
		return
	}
	if req.Source == "team" {
		if err := a.harnessRouteSync(r.Context(), s.provider); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
	}
	if err := s.selectAccount(req.Source, req.Name); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Import is explicit and never returns the existing secret to the browser.
func (a *apiServer) importHarnessAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Billing string `json:"billing"`
	}
	if !harnessDecode(w, r, &req) {
		return
	}
	p, _ := providers.Get(s.provider)
	if p.Harness == nil {
		http.Error(w, "no usable existing key to import", 400)
		return
	}
	var key string
	var err error
	model := p.Harness.DefaultModel
	switch {
	case p.Harness.ImportKeyFile == "zai-env-file":
		key, err = readZaiKeyFromEnvFile()
	case p.Harness.ImportKeyEnv != "" && harnessImportSecrets != nil && harnessImportSecrets(a) != nil:
		var found bool
		key, found, err = harnessImportSecrets(a).Resolve(p.Harness.ImportKeyProvider, p.Harness.ImportKeyEnv)
		if !found && err == nil {
			err = errors.New("no saved workflow key")
		}
	default:
		err = errors.New("no saved workflow key")
	}
	if err != nil {
		http.Error(w, "no usable existing key to import", 400)
		return
	}
	if err = s.save(harnessAccount{Name: req.Name, APIKey: key, Billing: req.Billing, Model: model}); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *apiServer) harnessLauncher() (string, error) {
	dir := filepath.Join(a.dataDir, "harness-launchers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if daemonURL, err := loopbackDaemonURL(a.addr); err == nil {
		if err := writeFileAtomicSynced(filepath.Join(dir, "daemon-url"), []byte(daemonURL+"\n"), 0600); err != nil {
			return "", err
		}
	}
	for _, name := range []string{"pins.mjs", "deepseek-model.mjs", "launch.mjs", "pi-approval.mjs", "deepseek-terminal.mjs", "install.mjs", "harness-shortcut.sh"} {
		b, err := harnessAssets.ReadFile("harness_assets/" + name)
		if err != nil {
			return "", err
		}
		if err = writeFileAtomicSynced(filepath.Join(dir, name), b, 0700); err != nil {
			return "", err
		}
	}
	// Stamp the extracting daemon's version beside the assets so a running pane
	// can report which client build it loaded (/status). Restarting the pane is
	// the only way to pick up a new build, so "which one am I on" is otherwise
	// unanswerable from inside the client.
	ver := strings.TrimSpace(a.ver)
	if ver == "" {
		ver = "dev"
	}
	if err := writeFileAtomicSynced(filepath.Join(dir, "VERSION"), []byte(ver+"\n"), 0600); err != nil {
		return "", err
	}
	return filepath.Join(dir, "launch.mjs"), nil
}

// managedHarnessLaunchCmd returns the argv that relaunches provider through the
// shared managed launcher, or false when the launcher or Node cannot be
// prepared. It mirrors launchHarness exactly — including the shortcut's
// Node-floor resolver — and is wired into the package-level resolver at boot
// (main.go) so a resume path holding only a PaneSnapshot can rebuild a
// hand-launched harness (the user typed `deepseek` in a plain shell) instead of
// dropping to a shell.
func (a *apiServer) managedHarnessLaunchCmd(provider string) ([]string, bool) {
	provider = strings.TrimSpace(provider)
	if a == nil || provider == "" {
		return nil, false
	}
	launcher, err := a.harnessLauncher()
	if err != nil {
		return nil, false
	}
	node, err := resolveNodeBin()
	if err != nil || node == "" {
		return nil, false
	}
	return harnessShortcutCommand(node, launcher, provider, a.dataDir), true
}

// harnessRuntimeRoot is the per-user managed runtime tree.
func harnessRuntimeRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "soluterminal", "harnesses")
}

// harnessRuntimeInstalled mirrors the UI's installed status: the runtime
// entrypoint exists and, when declared, its guard marker too.
func harnessRuntimeInstalled(p providers.Provider) bool {
	if p.Harness == nil {
		return false
	}
	runtime := filepath.Join(append([]string{harnessRuntimeRoot(), p.Harness.InstallDir}, p.Harness.RuntimePath...)...)
	st, err := os.Stat(runtime)
	if err != nil || st.IsDir() {
		return false
	}
	if len(p.Harness.RuntimeGuard) > 0 {
		guard := filepath.Join(append([]string{filepath.Dir(runtime)}, p.Harness.RuntimeGuard...)...)
		if _, err := os.Stat(guard); err != nil {
			return false
		}
	}
	return true
}

// ensureHarnessCommands refreshes the extracted launcher assets and self-heals
// the managed shell shortcuts for every harness. The shortcut always points at
// the extracted launcher, so `deepseek`/`pi`/`zcode` run the current client on
// any machine with no repo checkout and no manual `--install`.
func (a *apiServer) ensureHarnessCommands() {
	launcher, err := a.harnessLauncher()
	if err != nil {
		log.Printf("harness command refresh: %v", err)
		return
	}
	node, err := resolveNodeBin()
	if err != nil || node == "" {
		log.Printf("harness command refresh: node unavailable: %v", err)
		return
	}
	for _, slug := range providers.HarnessSlugs() {
		p, ok := providers.Get(slug)
		if !ok || harnessCommandName(p) == "" {
			continue
		}
		if err := writeHarnessCommand(p, node, launcher, a.dataDir); err != nil {
			log.Printf("harness command %s: %v", harnessCommandName(p), err)
		}
	}
}

// Shortcut installation belongs to daemon startup, not the shared provider
// registry loaded by long-lived pane wrappers. Keep these filename-safe names
// here: changing an installer shortcut must not invalidate every running pane.
// Unknown providers fail closed rather than becoming arbitrary filesystem paths.
func harnessCommandName(p providers.Provider) string {
	if p.Harness == nil {
		return ""
	}
	switch p.Slug {
	case "deepseek", "pi", "pi-glm":
		return p.Slug
	case "glm":
		return "zcode"
	default:
		return ""
	}
}

// managedShortcutDataDir returns the --data-dir value a managed shortcut was
// written with, or "" when the script has none. The installers always quote the
// value, but tolerate unquoted output so a hand-edited shortcut is still
// attributed rather than silently treated as unowned.
func managedShortcutDataDir(script string) string {
	const marker = "--data-dir "
	i := strings.Index(script, marker)
	if i < 0 {
		return ""
	}
	rest := script[i+len(marker):]
	if rest == "" {
		return ""
	}
	if rest[0] == '\'' || rest[0] == '"' {
		quote := rest[0]
		rest = rest[1:]
		if end := strings.IndexByte(rest, quote); end >= 0 {
			return rest[:end]
		}
		return ""
	}
	if end := strings.IndexAny(rest, " \t\r\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// sameCleanPath compares two paths after absolutizing both, so a shortcut
// written as ./data and an install run with /abs/data compare equal.
func sameCleanPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ap, aErr := filepath.Abs(a)
	bp, bErr := filepath.Abs(b)
	if aErr != nil || bErr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ap == bp
}

// managedShortcutReplaceable reports whether an install running with dataDir may
// (re)write the managed shortcut described by existing. One command name is
// shared by every data dir on the machine, so a side-by-side install (a test or
// dev data dir) must never silently repoint the command a user relies on to
// reach the real account. The default data dir always wins so it can self-heal
// after such a hijack; any other data dir may only rewrite a shortcut that
// already points at it.
func managedShortcutReplaceable(existing, dataDir, defaultDataDir string) bool {
	if current := managedShortcutDataDir(existing); current != "" && sameCleanPath(current, dataDir) {
		return true
	}
	return sameCleanPath(dataDir, defaultDataDir)
}

// writeHarnessCommand installs one managed shortcut, mirroring install.mjs: an
// unrelated command of the same name is preserved, a shortcut owned by another
// data dir is left alone, and a missing file, one already pointing at this data
// dir, or the default data dir self-healing a hijack is (re)written atomically.
// The shortcut execs the shared harness-shortcut.sh resolver rather than Node
// directly, so a pinned interpreter that was later moved or removed does not
// strand the command.
func writeHarnessCommand(p providers.Provider, node, launcher, dataDir string) error {
	command := harnessCommandName(p)
	if command == "" {
		return errors.New("harness has no managed command name")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	shortcut := filepath.Join(binDir, command)
	existing, readErr := os.ReadFile(shortcut)
	switch {
	case readErr == nil:
		if !strings.Contains(string(existing), "# soluterminal-managed-harness") {
			return nil
		}
		if !managedShortcutReplaceable(string(existing), dataDir, filepath.Join(home, ".solu")) {
			log.Printf("harness command %s: leaving %s in place; it belongs to data dir %q",
				command, shortcut, managedShortcutDataDir(string(existing)))
			return nil
		}
	case errors.Is(readErr, os.ErrNotExist):
		// No shortcut yet: create one below.
	default:
		return readErr
	}
	quote := func(text string) string { return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'" }
	// Mirror renderManagedShortcut in launch.mjs. /bin/sh is explicit so the
	// resolver still runs if the extracted asset loses its executable bit.
	resolver := harnessShortcutResolver(launcher)
	script := "#!/bin/sh\n# soluterminal-managed-harness\nexec /bin/sh " +
		quote(resolver) + " " + quote(node) + " " + quote(launcher) + " " + quote(p.Slug) + " --data-dir " + quote(dataDir) + " \"$@\"\n"
	return writeFileAtomicSynced(shortcut, []byte(script), 0700)
}

// harnessShortcutResolver is the shared interpreter resolver extracted beside
// the launcher. Managed install/launch route through it so the provider's
// supported-Node gate picks the interpreter; resolveNodeBin alone prefers the
// first node on the daemon PATH, which can be a distro Node below the runtime's
// floor even when a newer nvm/asdf install is present.
func harnessShortcutResolver(launcher string) string {
	return filepath.Join(filepath.Dir(launcher), "harness-shortcut.sh")
}

// harnessShortcutCommand builds the argv for a managed harness install or
// launch. extra is appended after --data-dir (for example --install). The
// managed harness only supports the WSL/Linux and macOS backends, so Windows
// keeps the raw command and lets install.mjs report the unsupported platform.
func harnessShortcutCommand(node, launcher, provider, dataDir string, extra ...string) []string {
	if runtime.GOOS == "windows" {
		return append([]string{node, launcher, provider, "--data-dir", dataDir}, extra...)
	}
	argv := []string{"/bin/sh", harnessShortcutResolver(launcher), node, launcher, provider, "--data-dir", dataDir}
	return append(argv, extra...)
}

// ensureHarnessRuntime starts the pinned installer in the background when a
// provider has credentials but no runtime. One attempt per provider per daemon
// process, so a persistent failure cannot loop npm; the UI surfaces the error
// and offers a manual install. Success re-runs the shortcut self-heal.
func (a *apiServer) ensureHarnessRuntime(provider string) {
	a.ensureHarnessRuntimeWithStarter(provider, func(cmd *exec.Cmd) error {
		return startWithoutCoreDumps(cmd)
	})
}

// Keep the launch dependency local so tests can exercise guard setup errors
// through the real installation state/log lifecycle without global hooks.
func (a *apiServer) ensureHarnessRuntimeWithStarter(provider string, start func(*exec.Cmd) error) {
	p, ok := providers.Get(provider)
	if !ok || p.Harness == nil || harnessRuntimeInstalled(p) {
		return
	}
	a.harnessInstallMu.Lock()
	if a.harnessInstalling == nil {
		a.harnessInstalling = map[string]bool{}
	}
	if a.harnessInstallTried == nil {
		a.harnessInstallTried = map[string]bool{}
	}
	if a.harnessInstalling[provider] || a.harnessInstallTried[provider] {
		a.harnessInstallMu.Unlock()
		return
	}
	a.harnessInstalling[provider] = true
	a.harnessInstallTried[provider] = true
	delete(a.harnessInstallErrs, provider)
	a.harnessInstallMu.Unlock()

	launcher, err := a.harnessLauncher()
	if err != nil {
		a.finishHarnessInstall(provider, err)
		return
	}
	node, err := resolveNodeBin()
	if err != nil || node == "" {
		a.finishHarnessInstall(provider, errors.New("Node.js is not installed"))
		return
	}
	installer := harnessShortcutCommand(node, launcher, provider, a.dataDir, "--install")
	logFile, logErr := os.OpenFile(filepath.Join(a.dataDir, "harness-install-"+provider+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if logErr != nil {
		a.finishHarnessInstall(provider, logErr)
		return
	}
	cmd := exec.Command(installer[0], installer[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := start(cmd); err != nil {
		_ = logFile.Close()
		a.finishHarnessInstall(provider, err)
		return
	}
	go func() {
		err := waitOwnedCommand(cmd)
		_ = logFile.Close()
		a.finishHarnessInstall(provider, err)
		if err == nil {
			a.ensureHarnessCommands()
		}
	}()
}

func (a *apiServer) finishHarnessInstall(provider string, err error) {
	a.harnessInstallMu.Lock()
	defer a.harnessInstallMu.Unlock()
	delete(a.harnessInstalling, provider)
	if err != nil {
		if a.harnessInstallErrs == nil {
			a.harnessInstallErrs = map[string]string{}
		}
		a.harnessInstallErrs[provider] = err.Error()
	} else {
		delete(a.harnessInstallErrs, provider)
	}
}

func (a *apiServer) harnessInstallState(provider string) (bool, string) {
	a.harnessInstallMu.Lock()
	defer a.harnessInstallMu.Unlock()
	return a.harnessInstalling[provider], a.harnessInstallErrs[provider]
}

func (a *apiServer) launchHarness(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	rows, err := s.list()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	selected := false
	for _, row := range rows {
		if row.Selected {
			selected = true
			if row.Source == "team" {
				if err = a.harnessRouteSync(r.Context(), s.provider); err != nil {
					http.Error(w, err.Error(), 502)
					return
				}
			}
		}
	}
	if !selected {
		http.Error(w, "select an API account first", 400)
		return
	}
	launcher, err := a.harnessLauncher()
	if err != nil {
		http.Error(w, "could not prepare harness launcher", 500)
		return
	}
	node, err := resolveNodeBin()
	if err != nil {
		http.Error(w, "Node.js is not installed", 400)
		return
	}
	writeJSON(w, 200, map[string]any{"cmd": harnessShortcutCommand(node, launcher, s.provider, a.dataDir)})
}

func (a *apiServer) installHarness(w http.ResponseWriter, r *http.Request) {
	s, ok := a.harnessRequest(w, r)
	if !ok {
		return
	}
	launcher, err := a.harnessLauncher()
	if err != nil {
		http.Error(w, "could not prepare installer", 500)
		return
	}
	node, err := resolveNodeBin()
	if err != nil {
		http.Error(w, "Node.js is not installed", 400)
		return
	}
	writeJSON(w, 200, map[string]any{"cmd": harnessShortcutCommand(node, launcher, s.provider, a.dataDir, "--install")})
}

// harnessProbeTarget resolves the selected account, endpoint, and store that a
// Test or Check may use — with a fresh team sync first. No fallback to an
// unselected credential.
func (a *apiServer) harnessProbeTarget(w http.ResponseWriter, r *http.Request) (s *harnessCredStore, source, name string, acc harnessAccount, base string, ok bool) {
	s, ok = a.harnessRequest(w, r)
	if !ok {
		return nil, "", "", harnessAccount{}, "", false
	}
	source, name, acc, err := s.selected()
	if err != nil {
		http.Error(w, err.Error(), 400)
		return nil, "", "", harnessAccount{}, "", false
	}
	if source == "team" {
		if err = a.harnessRouteSync(r.Context(), s.provider); err != nil {
			http.Error(w, err.Error(), 502)
			return nil, "", "", harnessAccount{}, "", false
		}
		if source, name, acc, err = s.selected(); err != nil {
			http.Error(w, err.Error(), 400)
			return nil, "", "", harnessAccount{}, "", false
		}
	}
	base, err = harnessEndpoint(s.provider, acc.Billing)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return nil, "", "", harnessAccount{}, "", false
	}
	if source == "team" {
		if harnessRoute.route == nil {
			http.Error(w, errHarnessTeamUnavailable.Error(), http.StatusConflict)
			return nil, "", "", harnessAccount{}, "", false
		}
		key, routed, status, routeErr := harnessRoute.route(a, s, name, acc)
		if routeErr != nil {
			http.Error(w, routeErr.Error(), status)
			return nil, "", "", harnessAccount{}, "", false
		}
		acc.APIKey, base = key, routed
	}
	return s, source, name, acc, base, true
}

// testHarness is the explicit, bounded Test Connection. It sends ONE minimal
// completion for the selected account and returns only classification
// metadata. It runs only on a user action — never on a schedule or at startup.
func (a *apiServer) testHarness(w http.ResponseWriter, r *http.Request) {
	s, source, name, acc, base, ok := a.harnessProbeTarget(w, r)
	if !ok {
		return
	}
	if source == "team" {
		defer harnessRoute.release(a, acc.APIKey) // minted for this one probe
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	result := probeHarnessAccountForProvider(ctx, base, s.provider, acc, nil)
	if source == "team" && acc.BrokerOnly {
		result.KeyHash = harnessKeyHash("")
	}
	// The result is worth reporting even if persisting the metadata fails.
	_ = newHarnessTestStore(a.dataDir, s.provider).put(source+"/"+name, result)
	writeJSON(w, 200, result.DTO())
}

// checkHarness is the free counterpart to Test: it confirms the key is accepted
// by reading the provider's model list. It spends nothing and is still only
// run on a user action. A provider with no models route reports "unsupported".
func (a *apiServer) checkHarness(w http.ResponseWriter, r *http.Request) {
	s, source, name, acc, base, ok := a.harnessProbeTarget(w, r)
	if !ok {
		return
	}
	if source == "team" {
		defer harnessRoute.release(a, acc.APIKey) // minted for this one probe
	}
	if source == "team" && acc.BrokerOnly {
		writeJSON(w, 200, harnessTestDTO{Status: "unsupported", At: time.Now().Unix(), Model: acc.Model, Detail: "Use Test Connection to check this shared account."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result := probeHarnessAuth(ctx, base, acc, nil)
	// "unsupported" says nothing about the key, so it is not persisted as status.
	if result.Status != "unsupported" {
		_ = newHarnessTestStore(a.dataDir, s.provider).put(source+"/"+name, result)
	}
	writeJSON(w, 200, result.DTO())
}

// loopbackDaemonURL is this daemon's loopback address for local clients.
func loopbackDaemonURL(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", errors.New("daemon listen address unavailable")
	}
	return "http://127.0.0.1:" + port, nil
}

var errHarnessTeamUnavailable = errors.New("team accounts are unavailable in this build")

// harnessRouteSync refreshes the provider's routed accounts before one is
// used.
func (a *apiServer) harnessRouteSync(ctx context.Context, provider string) error {
	if harnessRoute.sync == nil {
		return errHarnessTeamUnavailable
	}
	return harnessRoute.sync(a, ctx, provider)
}

// harnessImportSecrets is the saved runtime key store a harness account can
// import an existing key from; nil (or a nil result) when there is none.
var harnessImportSecrets func(a *apiServer) secretResolver
