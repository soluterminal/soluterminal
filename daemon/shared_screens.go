package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"

	"strconv"
	"strings"
)

func runCaptureCmd(ctx context.Context, cmd *exec.Cmd) error {
	applyDetached(cmd)
	// This is the single choke point for every preview/browser worker:
	// screen capture, ensurePlaywrightBrowser, previewEnsureDeps,
	// previewRunBuildArgv, and previewProbeMountedFromOriginWithScreens all
	// route their screenshot, install, build, and Playwright work through it.
	finishScope, err := startWorkflowCommand(ctx, cmd)
	if err != nil {
		return err
	}
	defer finishScope()
	lowerProcessPriority(cmd.Process)
	done := make(chan error, 1)
	go func() { done <- waitOwnedCommand(cmd) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = killChildTree(cmd.Process)
		<-done
		return ctx.Err()
	}
}

func resolvePlaywrightModuleForApp(worktree, originRepo, appDir string) (string, []string, error) {
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("PLAYWRIGHT_MODULE_PATH")); v != "" {
		candidates = append(candidates, v)
	}
	if worktree != "" {
		candidates = append(candidates,
			filepath.Join(worktree, filepath.FromSlash(appDir), "node_modules", "playwright"),
			filepath.Join(worktree, filepath.FromSlash(appDir), "node_modules", "playwright-core"),
			filepath.Join(worktree, "node_modules", "playwright"),
			filepath.Join(worktree, "node_modules", "playwright-core"),
		)
	}
	if originRepo != "" {
		candidates = append(candidates,
			filepath.Join(originRepo, filepath.FromSlash(appDir), "node_modules", "playwright"),
			filepath.Join(originRepo, filepath.FromSlash(appDir), "node_modules", "playwright-core"),
			filepath.Join(originRepo, "node_modules", "playwright"),
			filepath.Join(originRepo, "node_modules", "playwright-core"),
		)
	}
	var skipped []string
	resolve := func(candidates []string) string {
		for _, dir := range candidates {
			info, err := os.Stat(dir)
			if err != nil {
				continue
			}
			if !info.IsDir() {
				skipped = append(skipped, dir+": not a directory")
				continue
			}
			if reason := validatePlaywrightDir(dir); reason != "" {
				skipped = append(skipped, dir+": "+reason)
				continue
			}
			return dir
		}
		return ""
	}
	if dir := resolve(candidates); dir != "" {
		return dir, skipped, nil
	}

	// Keep the npm-global source as the final fallback, but do not invoke npm
	// when an earlier candidate resolved. Besides preserving precedence, this
	// keeps the fresh-worktree path independent of global npm state.
	if npmBin, err := exec.LookPath("npm"); err == nil {
		if out, err := guardedOutput(hiddenCommand(context.Background(), npmBin, "root", "-g")); err == nil {
			if root := strings.TrimSpace(string(out)); root != "" {
				candidates = []string{
					filepath.Join(root, "playwright"),
					filepath.Join(root, "playwright-core"),
				}
				if dir := resolve(candidates); dir != "" {
					return dir, skipped, nil
				}
			}
		} else {
			var guardErr *coreDumpGuardError
			if errors.As(err, &guardErr) {
				return "", skipped, err
			}

		}
	}
	return "", skipped, nil
}

// validatePlaywrightDir returns "" when dir holds a compatible Playwright
// package, else a short reason for the skipped diagnostics.
func validatePlaywrightDir(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "no readable package.json"
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return "invalid package.json"
	}
	if pkg.Name != "playwright" && pkg.Name != "playwright-core" {
		return fmt.Sprintf("package %q is not playwright", pkg.Name)
	}
	majorText, _, _ := strings.Cut(pkg.Version, ".")
	if major, err := strconv.Atoi(majorText); err != nil || major != playwrightMajor {
		return fmt.Sprintf("version %q is not major %d", pkg.Version, playwrightMajor)
	}
	return ""
}

// ensurePlaywrightBrowser checks that the chromium builds the resolved
// module wants are present in the shared browser cache, installing them
// through the module's own CLI on a miss. That install is the ONLY browser
// download path (the pinned playwright package has no install script, so npm
// ci never downloads browsers) and is bounded: chromium only, one shared
// cache, no eviction. It runs under the capture stage's context, so
// cancellation and the stage timeout kill the child tree. Any error flows
// into the capture-error path and becomes the screensMissing note — never a
// verification failure.
func ensurePlaywrightBrowser(ctx context.Context, nodeBin, webDir, moduleDir string) (string, error) {
	revisions, err := playwrightChromiumRevisions(moduleDir)
	if err != nil {
		return "", err
	}
	cache, err := playwrightCacheRoot(webDir, moduleDir)
	if err != nil {
		return "", err
	}
	// Resolve/create the owned browser root before recording the installation's
	// required revisions; a first-ever install must be pinned too.
	if err := generatedStoragePinPlaywright(revisions); err != nil {
		return "", fmt.Errorf("pin managed Playwright browser revisions: %w", err)
	}
	if len(missingBrowserDirs(cache, revisions)) == 0 {
		return cache, nil
	}
	cli := filepath.Join(moduleDir, "cli.js")
	if info, statErr := os.Stat(cli); statErr != nil || info.IsDir() {
		return "", fmt.Errorf("Playwright browser is unavailable: chromium %s not found under %s and %s cannot install it; run `npx playwright install chromium` in web/", revisions["chromium"], cache, moduleDir)
	}
	installTail := &tailBuf{limit: 2 * 1024}
	install := exec.Command(nodeBin, cli, "install", "chromium")
	install.Dir = webDir
	// workerEnv strips PLAYWRIGHT_*; inject the cache root explicitly so
	// the CLI installs into the directory the presence check just probed.
	install.Env = envWithValue(workerEnvForRepo(webDir), "PLAYWRIGHT_BROWSERS_PATH", cache)
	install.Stdout = installTail
	install.Stderr = installTail
	if err := runCaptureCmd(ctx, install); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("Playwright browser install failed: %w: %s", err, strings.TrimSpace(installTail.Tail()))
	}
	if still := missingBrowserDirs(cache, revisions); len(still) > 0 {
		return "", fmt.Errorf("Playwright browser install completed but %s still missing under %s", strings.Join(still, ", "), cache)
	}
	return cache, nil
}

// playwrightChromiumRevisions reads the browser registry the module ships
// (browsers.json lives in playwright-core; the full playwright package finds
// it via its playwright-core dependency) and returns the chromium builds a
// headless launch needs, name → revision.
func playwrightChromiumRevisions(moduleDir string) (map[string]string, error) {
	registryPath, err := playwrightRegistryPath(moduleDir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, fmt.Errorf("read browser registry: %w", err)
	}
	var registry struct {
		Browsers []struct {
			Name     string `json:"name"`
			Revision string `json:"revision"`
		} `json:"browsers"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return nil, fmt.Errorf("parse browser registry: %w", err)
	}
	revisions := map[string]string{}
	for _, b := range registry.Browsers {
		if b.Name == "chromium" || b.Name == "chromium-headless-shell" {
			revisions[b.Name] = b.Revision
		}
	}
	if revisions["chromium"] == "" {
		return nil, fmt.Errorf("browser registry near %s lists no chromium", moduleDir)
	}
	return revisions, nil
}

// playwrightRegistryPath returns the first browser registry location used
// by the resolved package. playwright keeps it in playwright-core, which is
// either the resolved module itself, a sibling dependency, or nested below
// the full playwright package.
func playwrightRegistryPath(moduleDir string) (string, error) {
	locations := []string{
		filepath.Join(moduleDir, "browsers.json"),
		filepath.Join(filepath.Dir(moduleDir), "playwright-core", "browsers.json"),
		filepath.Join(moduleDir, "node_modules", "playwright-core", "browsers.json"),
	}
	for _, loc := range locations {
		if info, err := os.Stat(loc); err == nil && !info.IsDir() {
			return loc, nil
		}
	}
	return "", fmt.Errorf("browser registry (browsers.json) not found near %s", moduleDir)
}

// playwrightCacheRoot mirrors playwright-core's registryDirectory logic so
// the daemon's marker check and both Node children agree on one absolute
// location. A relative override is resolved against webDir because INIT_CWD
// is deliberately absent from workerEnv and the children run there. The
// special override "0" selects the resolved playwright-core package's local
// browser directory, exactly as Playwright does.
func playwrightCacheRoot(webDir, moduleDir string) (string, error) {
	result := sharedPlaywrightBrowserDir()
	if result == "" {
		return "", fmt.Errorf("resolve Playwright browser cache: managed storage unavailable")
	}
	if !filepath.IsAbs(result) {
		result = filepath.Join(webDir, result)
	}
	result, err := filepath.Abs(result)
	if err != nil {
		return "", fmt.Errorf("resolve Playwright browser cache: %w", err)
	}
	return result, nil
}

// missingBrowserDirs returns the cache directory names (e.g. chromium-1217,
// chromium_headless_shell-1217) whose INSTALLATION_COMPLETE marker is absent.
func missingBrowserDirs(cache string, revisions map[string]string) []string {
	var missing []string
	for _, name := range []string{"chromium", "chromium-headless-shell"} {
		rev := revisions[name]
		if rev == "" {
			continue
		}
		dir := strings.ReplaceAll(name, "-", "_") + "-" + rev
		if _, err := os.Stat(filepath.Join(cache, dir, "INSTALLATION_COMPLETE")); err != nil {
			missing = append(missing, dir)
		}
	}
	return missing
}

// envWithValue replaces key in an allowlisted child environment. Generated
// storage can be reconfigured by another in-process engine in tests, so a
// multi-step browser operation must keep using the exact owned root it already
// resolved and validated rather than resolving the process-global default
// again between the presence check, install and capture commands.
func envWithValue(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		out = append(out, item)
	}
	return append(out, prefix+value)
}
