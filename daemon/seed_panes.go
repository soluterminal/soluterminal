package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Seeded panes: a side daemon started for a scripted capture opens its panes
// at boot, so nobody has to click to start them.
//
// The person who starts that daemon is the operator: they chose its flags,
// its -data and the file in it. Nothing about running daemons changes. The
// REST and MCP rules for opening panes, typing into them and answering their
// prompts are untouched, and nothing is typed into a seeded pane after it
// starts. The limits (security review, 12 Nov):
//
//   - only with -seed-panes, and only on a side daemon with BOTH its own port
//     and its own -data (daemon_role.go); a daemon on the canonical port or
//     the default data dir refuses the flag with a log line (@secure 21 Nov);
//   - only from <data>/seed-panes.json: a regular file (not a symlink) owned by
//     this user and not writable by group or others;
//   - each pane's argv[0] is exactly "claude" or "codex", followed only by its
//     model (and Claude's effort): no prompt, subcommand or other flag, and
//     nothing that turns off approvals or the sandbox. The arguments are a
//     JSON array passed as argv (each one shell-quoted by the spawn path,
//     never interpreted), and cwd is an existing absolute directory;
//   - only on a fresh data dir: no panes saved or running and never seeded
//     before (<data>/seed-panes.done); the whole file is validated before
//     any pane starts;
//   - each seeded pane carries origin source "seed" and is logged once.
//
// A pane may instead name a WSL distro ({"title","distro"}; ST-253 W2, the
// Windows acceptance rig, @secure 21 Nov): it takes no argv or cwd and opens
// that distro's login shell, exactly what the new-pane picker opens. The
// whole file is refused unless Linux panes are switched on
// (SOLU_LINUX_PANES=1) and wsl.exe lists every named distro.
const (
	seedPanesFileName = "seed-panes.json"
	seedPanesMax      = 12
	seedPaneMaxArgs   = 32
	seedPaneMaxArgLen = 1024
	seedPaneMaxTitle  = 80
	seedPanesMaxBytes = 64 << 10
	seedPaneOriginSrc = "seed"
	// seedPanesDoneName marks a data dir that has been seeded, so closing every
	// seeded pane and restarting does not seed again.
	seedPanesDoneName = "seed-panes.done"
)

type seedPaneSpec struct {
	Title  string   `json:"title"`
	Cwd    string   `json:"cwd,omitempty"`
	Argv   []string `json:"argv,omitempty"`
	Distro string   `json:"distro,omitempty"`
}

type seedPanesDoc struct {
	Panes []seedPaneSpec `json:"panes"`
}

// seedPanesPath is the only file seeding reads.
func seedPanesPath(dataDir string) string { return filepath.Join(dataDir, seedPanesFileName) }

// loadSeedPanes reads and validates <data>/seed-panes.json. Any problem
// refuses the whole file.
func loadSeedPanes(dataDir string) ([]seedPaneSpec, error) {
	path := seedPanesPath(dataDir)
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (symlinks are refused)", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file read must be the file checked: refuse a swap between the two,
	// and check the owner on what was opened.
	st, err := f.Stat()
	if err != nil || !os.SameFile(lst, st) {
		return nil, fmt.Errorf("%s changed while it was read", path)
	}
	if err := seedFileOwnedPrivately(st, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, seedPanesMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > seedPanesMaxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, seedPanesMaxBytes)
	}
	var doc seedPanesDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("%s: trailing data after the JSON object", path)
	}
	if len(doc.Panes) == 0 {
		return nil, fmt.Errorf("%s lists no panes", path)
	}
	if len(doc.Panes) > seedPanesMax {
		return nil, fmt.Errorf("%s lists %d panes; at most %d", path, len(doc.Panes), seedPanesMax)
	}
	for i := range doc.Panes {
		if err := validateSeedPane(&doc.Panes[i]); err != nil {
			return nil, fmt.Errorf("%s: pane %d: %w", path, i+1, err)
		}
	}
	return doc.Panes, nil
}

func validateSeedPane(p *seedPaneSpec) error {
	if p.Distro != "" {
		if len(p.Argv) != 0 || p.Cwd != "" {
			return errors.New("a distro pane takes no argv or cwd: it opens the distro's login shell")
		}
		if !validWSLDistro.MatchString(p.Distro) {
			return fmt.Errorf("distro %q is not a WSL distro name", p.Distro)
		}
		return validateSeedTitle(p)
	}
	if len(p.Argv) == 0 {
		return errors.New("argv is empty")
	}
	if p.Argv[0] != "claude" && p.Argv[0] != "codex" {
		return fmt.Errorf("argv[0] must be \"claude\" or \"codex\", not %q", p.Argv[0])
	}
	if len(p.Argv) > seedPaneMaxArgs {
		return fmt.Errorf("argv has %d entries; at most %d", len(p.Argv), seedPaneMaxArgs)
	}
	for _, a := range p.Argv {
		if len(a) > seedPaneMaxArgLen || strings.IndexFunc(a, unicode.IsControl) >= 0 {
			return errors.New("argv entries must be at most 1024 bytes with no control characters")
		}
	}
	if flag, bad := seedArgvBypassesApprovals(p.Argv[1:]); bad {
		return fmt.Errorf("argv turns off the agent's approvals or sandbox (%s); seeded panes keep their prompts", flag)
	}
	if err := seedArgvAllowed(p.Argv[0], p.Argv[1:]); err != nil {
		return err
	}
	if !filepath.IsAbs(p.Cwd) {
		return fmt.Errorf("cwd %q is not an absolute path", p.Cwd)
	}
	p.Cwd = filepath.Clean(p.Cwd)
	if st, err := os.Stat(p.Cwd); err != nil || !st.IsDir() {
		return fmt.Errorf("cwd %q is not an existing directory", p.Cwd)
	}
	return validateSeedTitle(p)
}

func validateSeedTitle(p *seedPaneSpec) error {
	p.Title = strings.TrimSpace(p.Title)
	if len(p.Title) > seedPaneMaxTitle || strings.IndexFunc(p.Title, unicode.IsControl) >= 0 {
		return errors.New("title must be at most 80 bytes with no control characters")
	}
	return nil
}

// checkSeedDistros refuses a file naming a distro unless Linux panes are on
// and wsl.exe lists every distro it names.
func (a *apiServer) checkSeedDistros(specs []seedPaneSpec) error {
	var named []string
	for _, s := range specs {
		if s.Distro != "" {
			named = append(named, s.Distro)
		}
	}
	if len(named) == 0 {
		return nil
	}
	if a.mgr == nil || a.mgr.linux == nil {
		return errors.New("it names a WSL distro but Linux panes aren't switched on (SOLU_LINUX_PANES=1)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listed, err := listWSLDistros(ctx)
	if err != nil {
		return fmt.Errorf("couldn't list WSL distros: %w", err)
	}
	for _, name := range named {
		if !slices.ContainsFunc(listed, func(d linuxDistro) bool { return d.Name == name }) {
			return fmt.Errorf("WSL distro %q isn't installed", name)
		}
	}
	return nil
}

// seedArgvBypassesApprovals finds an argument that would start the agent
// without its own permission prompts or sandbox (Claude's skip-permissions
// and bypassPermissions mode; Codex's yolo, full-auto, danger-full-access
// sandbox and never-ask approval policy). Both lists apply whatever argv[0]
// is. Flags match exactly or in their "flag=value" form; a value may be the
// next element.
func seedArgvBypassesApprovals(args []string) (string, bool) {
	bare := map[string]bool{
		"--dangerously-skip-permissions":             true,
		"--allow-dangerously-skip-permissions":       true,
		"--yolo":                                     true,
		"--dangerously-bypass-approvals-and-sandbox": true,
		"--full-auto":                                true,
	}
	// valueBad reports whether a flag's value turns approvals or the sandbox off.
	valueBad := func(flag, value string) bool {
		switch flag {
		case "-a", "--ask-for-approval":
			return strings.EqualFold(strings.TrimSpace(value), "never")
		case "--permission-mode":
			return strings.EqualFold(strings.TrimSpace(value), "bypassPermissions")
		case "-s", "--sandbox":
			return strings.EqualFold(strings.TrimSpace(value), "danger-full-access")
		case "-c", "--config":
			v := strings.ToLower(strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "").Replace(value))
			return strings.Contains(v, "sandbox_mode=danger-full-access") || strings.Contains(v, "approval_policy=never")
		}
		return false
	}
	valued := map[string]bool{"--permission-mode": true, "-s": true, "--sandbox": true, "-c": true, "--config": true, "-a": true, "--ask-for-approval": true}
	for i, a := range args {
		name, value, hasValue := strings.Cut(a, "=")
		if bare[name] {
			return name, true
		}
		if !valued[name] {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				continue
			}
			value = args[i+1]
		}
		if valueBad(name, value) {
			return name + " " + value, true
		}
	}
	return "", false
}

// seedArgValue is what a seeded pane's flag value may look like: a model or
// effort name, never a path to a file of settings or a prompt.
var seedArgValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,99}$`)

// seedArgvAllowed is the allowlist behind the bypass check: after argv[0], a
// seeded pane takes only its model (and, for Claude, its effort), as
// "--flag value" or "--flag=value". No other flag, no subcommand (codex exec
// or app-server) and no positional prompt, so a seeded agent starts idle at
// its own prompt with its default permissions.
func seedArgvAllowed(agent string, args []string) error {
	allowed := map[string]bool{"--model": true}
	if agent == "claude" {
		allowed["--effort"] = true
	} else {
		allowed["-m"] = true
	}
	for i := 0; i < len(args); i++ {
		name, value, joined := strings.Cut(args[i], "=")
		if !allowed[name] || (joined && name == "-m") {
			return fmt.Errorf("argument %q is not allowed: a seeded %s pane takes only %s", args[i], agent, seedAllowedFlagsText(agent))
		}
		if !joined {
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if !seedArgValue.MatchString(value) {
			return fmt.Errorf("%s value %q is not a plain model or effort name", name, value)
		}
	}
	return nil
}

func seedAllowedFlagsText(agent string) string {
	if agent == "claude" {
		return "--model and --effort"
	}
	return "--model (or -m)"
}

// seedPanesAllowed decides whether this boot seeds. existing counts the panes
// the data dir already has (saved or running).
func seedPanesAllowed(enabled bool, role daemonRole, existing int, seededBefore bool) (bool, string) {
	switch {
	case !enabled:
		return false, ""
	case role.Primary || role.CanonicalPort || role.DefaultData:
		return false, "seed panes: -seed-panes ignored: this daemon is on the canonical port (8787 or 38787) or the default -data; only a side daemon with its own port and its own -data seeds panes"
	case seededBefore:
		return false, "seed panes: skipped: this -data was already seeded once (" + seedPanesDoneName + "); seeding runs only on a fresh -data"
	case existing > 0:
		return false, fmt.Sprintf("seed panes: skipped: the data dir already has %d pane(s); seeding runs only on a fresh -data", existing)
	}
	return true, ""
}

// seedPanesAtBoot opens the panes in <data>/seed-panes.json when allowed. Runs
// once, after this process owns its port; failures are logged, never fatal.
func (a *apiServer) seedPanesAtBoot(enabled bool, role daemonRole) {
	existing := len(a.mgr.List())
	if a.panes != nil {
		existing = max(existing, len(a.panes.List()))
	}
	done := filepath.Join(a.dataDir, seedPanesDoneName)
	_, statErr := os.Lstat(done)
	ok, why := seedPanesAllowed(enabled, role, existing, statErr == nil || !os.IsNotExist(statErr))
	if why != "" {
		log.Print(why)
	}
	if !ok {
		return
	}
	specs, err := loadSeedPanes(a.dataDir)
	if err != nil {
		log.Printf("seed panes: refused: %v", err)
		return
	}
	if err := a.checkSeedDistros(specs); err != nil {
		log.Printf("seed panes: refused: %s: %v", seedPanesPath(a.dataDir), err)
		return
	}
	// Mark the data dir before the first spawn: a crash mid-seed must not
	// seed again. No marker, no seeding.
	if err := os.WriteFile(done, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		log.Printf("seed panes: refused: cannot record %s: %v", seedPanesDoneName, err)
		return
	}
	now := time.Now().UnixMilli()
	for i, spec := range specs {
		opts := SpawnOpts{
			Title:  spec.Title,
			Origin: &PaneOrigin{Source: seedPaneOriginSrc, Purpose: "seeded at boot from " + seedPanesFileName, At: now},
		}
		what := "WSL " + spec.Distro + " login shell"
		if spec.Distro != "" {
			opts.Distro = spec.Distro
		} else {
			opts.Agent, _ = normalizeAgentKind("", spec.Argv, "")
			opts.Cwd, opts.Cmd = spec.Cwd, append([]string(nil), spec.Argv...)
			what = strings.Join(spec.Argv, " ") + " in " + spec.Cwd
		}
		p, err := a.spawnPaneForRequest(opts)
		if err != nil {
			log.Printf("seed panes: pane %d failed to start: %v", i+1, err)
			continue
		}
		if a.panes != nil {
			if err := a.panes.Put(p); err != nil {
				a.rollbackSpawnedPane(p)
				log.Printf("seed panes: pane %d not saved, closed: %v", i+1, err)
				continue
			}
		}
		log.Printf("seed panes: started pane %s %q: %s", p.ID, spec.Title, what)
	}
}
