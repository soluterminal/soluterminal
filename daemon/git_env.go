package main

// Every git the daemon runs goes through hardenedGitCommand (or, for the
// release and preview checkouts' own `fetch origin main`, gitFetchCommand,
// which differs only in allowing that transport). The repositories are not
// trusted: an agent or worker can edit .git/config, .gitattributes and
// .git/info/* of any checkout it works in, and the daemon runs git outside any
// agent sandbox. TestDaemonGitCallsUseHardenedCommand fails when new code
// starts git another way.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// gitNoTransportEnv forbids every transport and remote helper for a git the
// daemon runs over a repository it does not trust. GIT_ALLOW_PROTOCOL=none,
// unlike `-c protocol.allow`, cannot be overridden by repository config
// (protocol.<name>.allow) and reaches the lazy-fetch child git, so a partial
// clone (extensions.partialClone) cannot run remote.*.uploadpack,
// core.sshCommand or ext:: to fetch a missing object. GIT_NO_LAZY_FETCH
// (git ≥ 2.44) additionally stops the fetch from being attempted at all.
var gitNoTransportEnv = []string{"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=none"}

// gitNoUserConfigEnv drops the system config and attributes. The global
// config (~/.gitconfig, XDG) is replaced by GIT_CONFIG_GLOBAL: a file the
// daemon writes with only the person's safe.directory values (and, for turn
// git, their excludes file), or os.DevNull. Only the repository's own config
// applies otherwise, and the options below override everything in it that
// could start a program.
var gitNoUserConfigEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1"}

// gitFixedEnv never prompts, never takes optional locks (no index refresh is
// written) and ignores refs/replace, so a graft cannot change what history a
// check sees.
var gitFixedEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1"}

// hardenedGitOptions is the one option set. Command-line config outranks the
// repository's, so each entry switches off a program the repository could
// name for the commands the daemon runs. Diffing subcommands additionally get
// --no-ext-diff --no-textconv (hardenedGitArgv): `-c diff.external=` does not
// unset an external diff, it makes git run an empty command.
var hardenedGitOptions = []string{
	"--no-pager", "--no-optional-locks",
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.pager=cat",
	"-c", "core.attributesFile=" + os.DevNull,
	// diff.submodule=diff would run a nested `git diff` inside a submodule,
	// which reads THAT repo's config (diff.external …) without our flags.
	"-c", "diff.submodule=short",
	// submodule.recurse=true would make checkout and merge recurse into
	// submodules under THEIR config.
	"-c", "submodule.recurse=false",
	// No transport may run: a partial clone would otherwise lazily fetch a
	// missing object through whatever remote/program .git/config names.
	"-c", "protocol.allow=never",
	// Verifying a signature runs gpg.program / gpg.ssh.program — any program
	// the repository names.
	"-c", "log.showSignature=false",
	"-c", "merge.verifySignatures=false",
	// An automatic gc or maintenance run after a writing command (the release
	// fast-forward) would start more git processes.
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// gitDiffingCommands accept, and get, --no-ext-diff --no-textconv.
var gitDiffingCommands = map[string]bool{"diff": true, "diff-files": true, "diff-index": true, "diff-tree": true, "log": true, "show": true}

// hardenedGitEnv is the daemon's environment for such a git: WITHOUT any
// inherited GIT_* variable (GIT_DIR, GIT_CONFIG_*, GIT_EXEC_PATH, … must not
// redirect these commands; case-insensitively, as Windows names are), with
// no transport, no system config and globalConfig as the global config.
func hardenedGitEnv(globalConfig string) []string {
	return slices.Concat(gitEnvWithoutInherited(), gitNoTransportEnv, gitNoUserConfigEnv, []string{"GIT_CONFIG_GLOBAL=" + globalConfig}, gitFixedEnv)
}

func gitEnvWithoutInherited() []string {
	env := make([]string, 0, len(os.Environ())+16)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

// hardenedGitArgv is `git [-C dir] <hardenedGitOptions> <args>`, with
// --no-ext-diff --no-textconv after a diffing subcommand. args may start with
// global options in their =form (--git-dir=…, --work-tree=…;
// TestDaemonGitCallsUseHardenedCommand refuses the separate form).
func hardenedGitArgv(dir string, args []string) []string {
	argv := make([]string, 0, len(hardenedGitOptions)+len(args)+4)
	if dir != "" {
		argv = append(argv, "-C", dir)
	}
	argv = append(argv, hardenedGitOptions...)
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "-C" || gitSeparateValueOptions[args[i]] {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		if gitDiffingCommands[args[i]] {
			argv = append(argv, args[:i+1]...)
			argv = append(argv, "--no-ext-diff", "--no-textconv")
			return append(argv, args[i+1:]...)
		}
		break
	}
	return append(argv, args...)
}

// gitSeparateValueOptions are global options whose value may follow as its
// own argument. Callers must use the =form; the argv scan still skips the
// value so it is never taken for the subcommand.
var gitSeparateValueOptions = map[string]bool{"--git-dir": true, "--work-tree": true, "--namespace": true, "--super-prefix": true, "--exec-path": true, "--config-env": true, "--list-cmds": true, "--attr-source": true}

// gitUserConfigValues are read from the person's own global and system git
// config only, never from a repository: safe.directory and core.excludesFile
// (pattern data, for turn git only). The read runs outside any repository with
// every inherited GIT_* variable dropped and is cached for gitUserConfigTTL. A
// failed read passes no safe.directory, so a dubious-ownership repository
// fails closed. git honours safe.directory only from protected (global or
// system) config, and `-c` only on newer versions, so the values reach git
// through the GIT_CONFIG_GLOBAL files writeGitGlobalConfigs keeps:
// globalConfig (safe.directory only) for every hardened git, turnGlobalConfig
// (plus the excludes file) for turnGit and git_status. Either is os.DevNull
// when it could not be written.
type gitUserConfigValues struct {
	safeDirectories []string
	excludesFile    string

	globalConfig, turnGlobalConfig string
}

const gitUserConfigTTL = 60 * time.Second

var gitUserConfigCache struct {
	sync.Mutex
	at     time.Time
	values gitUserConfigValues
}

var gitSafeDirectoryWildcardLogged sync.Once

func gitUserConfig() gitUserConfigValues {
	gitUserConfigCache.Lock()
	defer gitUserConfigCache.Unlock()
	if !gitUserConfigCache.at.IsZero() && time.Since(gitUserConfigCache.at) < gitUserConfigTTL {
		return gitUserConfigCache.values
	}
	values := readGitUserConfig()
	values.globalConfig, values.turnGlobalConfig = writeGitGlobalConfigs(values)
	gitUserConfigCache.values = values
	gitUserConfigCache.at = time.Now()
	return gitUserConfigCache.values
}

func readGitUserConfig() gitUserConfigValues {
	read := func(args ...string) ([]byte, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"config"}, args...)...)
		cmd.Dir = string(filepath.Separator)
		cmd.Env = gitEnvWithoutInherited()
		applyNoWindow(cmd)
		out, err := cmd.Output()
		if processExitCode(err) == 1 && ctx.Err() == nil {
			return nil, true // the key is not set
		}
		return out, err == nil
	}
	var values gitUserConfigValues
	// System config is read before global, so its values come first.
	for _, scope := range []string{"--system", "--global"} {
		out, ok := read(scope, "--includes", "-z", "--get-all", "safe.directory")
		if !ok {
			values.safeDirectories = nil
			break
		}
		for _, value := range bytes.SplitAfter(out, []byte{0}) {
			if len(value) > 0 {
				values.safeDirectories = append(values.safeDirectories, string(bytes.TrimSuffix(value, []byte{0})))
			}
		}
	}
	if slices.Contains(values.safeDirectories, "*") {
		gitSafeDirectoryWildcardLogged.Do(func() {
			log.Printf("git: the user's config sets safe.directory=*; daemon git trusts every repository owner")
		})
	}
	out, ok := read("--global", "--includes", "--path", "--get", "core.excludesFile")
	excludes := strings.TrimRight(string(out), "\r\n")
	if ok && excludes == "" {
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			excludes = filepath.Join(xdg, "git", "ignore")
		} else if home, err := os.UserHomeDir(); err == nil {
			excludes = filepath.Join(home, ".config", "git", "ignore")
		}
	}
	if ok && filepath.IsAbs(excludes) {
		values.excludesFile = excludes
	}
	return values
}

// gitGlobalConfigContents is the GIT_CONFIG_GLOBAL file for values: the
// safe.directory values in order (empty resets included) and, withExcludes,
// core.excludesFile. Never an include. A value git config could not carry as
// plain data (a newline, NUL or other control character) is refused: the
// whole safe.directory list, since dropping one reset entry would widen
// trust, or the excludes file.
func gitGlobalConfigContents(values gitUserConfigValues, withExcludes bool) string {
	var b strings.Builder
	b.WriteString(gitGlobalConfigHeader)
	if len(values.safeDirectories) > 0 && !slices.ContainsFunc(values.safeDirectories, gitConfigValueRefused) {
		b.WriteString("[safe]\n")
		for _, value := range values.safeDirectories {
			b.WriteString("\tdirectory = " + gitConfigQuote(value) + "\n")
		}
	}
	if withExcludes && values.excludesFile != "" && !gitConfigValueRefused(values.excludesFile) {
		b.WriteString("[core]\n\texcludesFile = " + gitConfigQuote(values.excludesFile) + "\n")
	}
	return b.String()
}

func gitConfigValueRefused(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 })
}

// gitConfigQuote is value as a git config string: quoted, so leading and
// trailing spaces, '#' and ';' stay data, with '\' and '"' escaped.
func gitConfigQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

const gitGlobalConfigHeader = "# Written by the soluterminal daemon from the user's own git config.\n"

const (
	gitGlobalConfigName     = "global.gitconfig"
	gitTurnGlobalConfigName = "turn-global.gitconfig"
)

// writeGitGlobalConfigs keeps both GIT_CONFIG_GLOBAL files in the view root
// (gitViewRoot: daemon-owned, 0700) and returns their paths, or os.DevNull
// for one that could not be written.
func writeGitGlobalConfigs(values gitUserConfigValues) (global, turn string) {
	global, turn = os.DevNull, os.DevNull
	root, err := gitViewRoot()
	if err != nil {
		log.Printf("git: global config files: %v", err)
		return global, turn
	}
	if slices.ContainsFunc(values.safeDirectories, gitConfigValueRefused) || gitConfigValueRefused(values.excludesFile) {
		log.Printf("git: the user's safe.directory or core.excludesFile holds a control character; not passed to daemon git")
	}
	if path, err := writeGitConfigFile(root, gitGlobalConfigName, gitGlobalConfigContents(values, false)); err == nil {
		global = path
	} else {
		log.Printf("git: global config file: %v", err)
	}
	if path, err := writeGitConfigFile(root, gitTurnGlobalConfigName, gitGlobalConfigContents(values, true)); err == nil {
		turn = path
	} else {
		log.Printf("git: turn global config file: %v", err)
	}
	return global, turn
}

// writeGitConfigFile leaves root/name holding exactly contents as a regular
// 0600 file owned by the daemon, replacing it atomically when it differs.
func writeGitConfigFile(root, name, contents string) (string, error) {
	path := filepath.Join(root, name)
	if gitPrivateFile(path) {
		if current, err := os.ReadFile(path); err == nil && string(current) == contents {
			return path, nil
		}
	}
	tmp, err := os.CreateTemp(root, "."+name+"-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(contents)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return "", err
	}
	if !gitPrivateFile(path) {
		return "", errors.New("git config file is not a private file owned by the daemon: " + path)
	}
	return path, nil
}

func gitPrivateFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && fileOwnedByCurrentUser(info) && (runtime.GOOS == "windows" || info.Mode().Perm() == 0o600)
}

// gitViewDir is the daemon-private parent of status views (setGitViewDir at
// startup). Never the temp directory: sandboxed agents can write there, and a
// config or info/attributes planted in a view would define the very filter
// drivers the view exists to avoid.
var gitViewDir atomic.Pointer[string]

func setGitViewDir(dataDir string) {
	dir := filepath.Join(dataDir, "git-views")
	gitViewDir.Store(&dir)
}

func gitViewRoot() (string, error) {
	root := filepath.Join(defaultDataDir(), "git-views")
	if dir := gitViewDir.Load(); dir != nil {
		root = *dir
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || !fileOwnedByCurrentUser(info) {
		return "", errors.New("git view directory is not a directory owned by the daemon: " + root)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(root, 0o700); err != nil {
			return "", err
		}
	}
	return root, nil
}

// hardenedGitCommand is the only way daemon code starts git over a checkout.
// Callers add to cmd.Env (never replace it) and set pipes; ctx may be
// context.Background() for a runner that bounds the process itself. The
// options cannot switch off the repository's filter drivers, so a command
// that compares the working tree with the index (status, diff HEAD,
// diff-index, diff-files) must use hardenedGitStatusView instead.
func hardenedGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return newHardenedGitCommand(ctx, gitUserConfig().globalConfig, dir, args)
}

// hardenedGitCommandWithExcludes is hardenedGitCommand with the person's
// global excludes file as well, so files they ignore stay out of turn
// snapshots and the git_status tool. Only turnGit uses it: worker and release
// checks stay strict.
func hardenedGitCommandWithExcludes(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return newHardenedGitCommand(ctx, gitUserConfig().turnGlobalConfig, dir, args)
}

func newHardenedGitCommand(ctx context.Context, globalConfig, dir string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", hardenedGitArgv(dir, args)...)
	cmd.Env = hardenedGitEnv(globalConfig)
	// The daemon may run detached on Windows, with no console for git to
	// inherit; without this each git opens a console window.
	applyNoWindow(cmd)
	return cmd
}

// gitFetchCommand is `git fetch <remote> <refspec>` for the release and preview
// checkouts (the owner's own clone of this project): hardenedGitCommand
// without the transport ban, and with the user's config, because the fetch
// needs their credential helper, url rewrites and SSH settings. It is the only
// git that may reach a remote; nothing else can be passed to it.
func gitFetchCommand(ctx context.Context, dir, remote, refspec string) *exec.Cmd {
	argv := []string{"-C", dir}
	for i := 0; i < len(hardenedGitOptions); i++ {
		if hardenedGitOptions[i] == "-c" && hardenedGitOptions[i+1] == "protocol.allow=never" {
			i++
			continue
		}
		argv = append(argv, hardenedGitOptions[i])
	}
	cmd := exec.CommandContext(ctx, "git", append(argv, "fetch", "-q", "--no-write-fetch-head", "--no-recurse-submodules", "--", remote, refspec)...)
	cmd.Env = slices.Concat(gitEnvWithoutInherited(), gitFixedEnv)
	applyNoWindow(cmd)
	return cmd
}

// hardenedGitStatusView runs `git status --porcelain=v1 -z` for the checkout
// at top against a private, empty git directory that borrows only the
// repository's objects and index. Status reads the working tree, which runs
// clean filters (and, racily, even diff-index does): with no repository
// config beyond gitViewConfig's literals and no user config, no filter,
// fsmonitor or other driver is defined, even if the checkout's config changes
// concurrently. Local info/* rules are deliberately omitted. head is the
// commit to compare against.
func hardenedGitStatusView(ctx context.Context, objects, index, top, head string, args ...string) ([]byte, error) {
	config, err := gitViewConfig(ctx, top)
	if err != nil {
		return nil, err
	}
	root, err := gitViewRoot()
	if err != nil {
		return nil, err
	}
	view, err := os.MkdirTemp(root, "view-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(view)
	for _, dir := range []string{"objects", "refs"} {
		if err := os.Mkdir(filepath.Join(view, dir), 0700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(view, "HEAD"), []byte(head+"\n"), 0600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(view, "config"), []byte(config), 0600); err != nil {
		return nil, err
	}
	if gitViewCreatedForTest != nil {
		gitViewCreatedForTest(view)
	}
	argv := append([]string{"--git-dir=" + view, "--work-tree=" + top, "status", "--porcelain=v1", "-z", "--no-renames", "--ignore-submodules=all"}, args...)
	cmd := hardenedGitCommand(ctx, "", argv...)
	cmd.Dir = top
	cmd.Env = append(cmd.Env, "GIT_OBJECT_DIRECTORY="+objects, "GIT_INDEX_FILE="+index)
	return cmd.Output()
}

var gitViewCreatedForTest func(view string)

// gitViewBoolConfig and gitViewEnumConfig are the repository settings the
// status view copies: they decide whether a file counts as changed (mode bits,
// symlinks, case, Unicode normalisation, line endings), and without them a
// clean checkout with filemode=false or autocrlf reads as dirty.
var (
	gitViewBoolConfig = []string{"core.filemode", "core.symlinks", "core.ignorecase", "core.precomposeunicode"}
	gitViewEnumConfig = []struct {
		key    string
		values []string
	}{{"core.autocrlf", []string{"true", "false", "input"}}, {"core.eol", []string{"lf", "crlf", "native"}}}
)

// gitViewConfig is the status view's config: the checkout's own values of
// those settings, read from its local config without includes, as normalised
// literals and nothing else. git normalises the booleans (--type=bool); an
// enum value outside its list, or any read failure, is an error.
func gitViewConfig(ctx context.Context, top string) (string, error) {
	read := func(typed bool, keys []string) (map[string]string, error) {
		args := []string{"config", "--local", "--no-includes", "-z"}
		if typed {
			args = append(args, "--type=bool")
		}
		pattern := `^(` + strings.ReplaceAll(strings.Join(keys, "|"), ".", `\.`) + `)$`
		out, err := hardenedGitCommand(ctx, top, append(args, "--get-regexp", pattern)...).Output()
		if processExitCode(err) == 1 && ctx.Err() == nil {
			return nil, nil // none set
		}
		if err != nil {
			return nil, errors.New("read repository config: " + gitErrorText(err))
		}
		values := map[string]string{}
		for _, entry := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
			key, value, hasValue := strings.Cut(entry, "\n")
			if !hasValue {
				value = "true" // a bare key is boolean true
			}
			values[key] = strings.ToLower(value) // the last one wins, as in git
		}
		return values, nil
	}
	bools, err := read(true, gitViewBoolConfig)
	if err != nil {
		return "", err
	}
	var enumKeys []string
	for _, enum := range gitViewEnumConfig {
		enumKeys = append(enumKeys, enum.key)
	}
	enums, err := read(false, enumKeys)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[core]\n")
	for _, key := range gitViewBoolConfig {
		if value, ok := bools[key]; ok {
			if value != "true" && value != "false" {
				return "", errors.New("repository config " + key + " is not a boolean")
			}
			b.WriteString("\t" + strings.TrimPrefix(key, "core.") + " = " + value + "\n")
		}
	}
	for _, enum := range gitViewEnumConfig {
		if value, ok := enums[enum.key]; ok {
			if !slices.Contains(enum.values, value) {
				return "", errors.New("repository config " + enum.key + " has an unsupported value")
			}
			b.WriteString("\t" + strings.TrimPrefix(enum.key, "core.") + " = " + value + "\n")
		}
	}
	return b.String(), nil
}

// hardenedGitTrackedChanges lists tracked paths whose index or working-tree
// content differs from HEAD (the contract `git diff --name-only HEAD` had),
// without letting the repository's config run anything.
func hardenedGitTrackedChanges(ctx context.Context, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout())
	defer cancel()
	run := func(args ...string) (string, error) {
		out, err := hardenedGitCommand(ctx, dir, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	head, err := run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, errors.New("read HEAD: " + gitErrorText(err))
	}
	paths, err := run("rev-parse", "--path-format=absolute", "--git-path", "objects", "--git-path", "index", "--show-toplevel")
	locations := strings.Split(paths, "\n")
	if err != nil || len(locations) != 3 {
		return nil, errors.New("resolve repository paths: " + gitErrorText(err))
	}
	// The view ignores submodules: reading one runs git under ITS config.
	// Refuse rather than report a changed submodule as clean.
	index, err := run("ls-files", "--stage", "-z")
	if err != nil {
		return nil, errors.New("read index: " + gitErrorText(err))
	}
	for _, entry := range strings.Split(index, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return nil, errors.New("submodule changes cannot be read safely")
		}
	}
	out, err := hardenedGitStatusView(ctx, locations[0], locations[1], locations[2], head, "--untracked-files=no")
	if err != nil {
		return nil, errors.New("status: " + gitErrorText(err))
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(entry) > 3 {
			files = append(files, entry[3:])
		}
	}
	return files, nil
}

func gitErrorText(err error) string {
	if err == nil {
		return "unexpected output"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return gitFailureReason(string(exitErr.Stderr))
	}
	return gitFailureReason(err.Error())
}

// gitFailureReason makes git's stderr fit for a reason a person reads: the
// ownership refusal in plain words, otherwise at most gitReasonMaxBytes with
// control characters (a repository path can carry escape sequences) replaced.
func gitFailureReason(stderr string) string {
	if strings.Contains(stderr, "detected dubious ownership") {
		return "repository owned by another user (git's safe.directory check refused it)"
	}
	stderr = strings.ToValidUTF8(stderr, "\uFFFD")
	if len(stderr) > gitReasonMaxBytes {
		stderr = stderr[:gitReasonMaxBytes]
		for !utf8.ValidString(stderr) {
			stderr = stderr[:len(stderr)-1]
		}
	}
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 || r == utf8.RuneError {
			return ' '
		}
		return r
	}, stderr)), " ")
}

const gitReasonMaxBytes = 2048
