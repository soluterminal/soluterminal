package main

// The internal command is launched ONLY through a runtime's sandbox. It has no
// daemon, settings, task or approval access and returns closed status records,
// never tool output. Running it manually grants no workflow readiness evidence.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func runCapabilityCommand(args []string, out io.Writer) int {
	if len(args) != 1 || len(args[0]) > 32768 {
		return 2
	}
	raw, err := base64.RawStdEncoding.DecodeString(args[0])
	var requirements []envRequirement
	if err != nil || strictCapabilityJSON(raw, &requirements) != nil || len(requirements) == 0 || len(requirements) > 8 {
		return 2
	}
	for _, r := range requirements {
		if validateCapabilityRequirement(r) != nil {
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), capabilityBatchLimit)
	defer cancel()
	results := make([]capabilityResult, 0, len(requirements))
	for _, r := range requirements {
		results = append(results, executeCapabilityRecipe(ctx, r))
	}
	if json.NewEncoder(out).Encode(results) != nil {
		return 1
	}
	return 0
}

func executeCapabilityRecipe(ctx context.Context, req envRequirement) (result capabilityResult) {
	result = capabilityResult{Name: capabilityResultName(req), Selector: capabilitySelector(req), Status: "ready"}
	defer func() { result.ObservedAt = time.Now().UTC() }()
	if req.Path != "" {
		f, err := os.Open(filepath.FromSlash(req.Path))
		if err != nil {
			capabilityError(&result, err, "missing_path")
			return
		}
		defer f.Close()
		if _, err := f.Stat(); err != nil {
			capabilityError(&result, err, "missing_path")
		}
		return
	}
	recipe, supported := effectiveCapabilityRecipe(req)
	if !supported {
		result.Status = "unsupported_probe"
		return
	}
	bin := req.ToolPath
	if bin == "" {
		var err error
		bin, err = exec.LookPath(req.Exec)
		if err != nil {
			capabilityError(&result, err, "missing_executable")
			return
		}
	}
	run := func(dir, command string, args ...string) bool {
		commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(commandCtx, command, args...)
		// A version check must not trigger Blacksmith's installation updater.
		if recipe.Kind == "exec_version" && filepath.Base(bin) == "blacksmith" {
			cmd.Env = append(os.Environ(), "BLACKSMITH_DISABLE_AUTO_UPDATE=1")
		}
		cmd.Dir = dir
		cmd.SysProcAttr = pipeChildSysProcAttr()
		applyNoWindow(cmd)
		cmd.Cancel = func() error { return killChildTree(cmd.Process) }
		cmd.WaitDelay = time.Second
		// Discard arbitrary stdout/stderr; exit and spawn errno are evidence.
		err := cmd.Run()
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			result.ExitCode = &code
		}
		if commandCtx.Err() != nil {
			err = commandCtx.Err()
		}
		if err != nil {
			capabilityError(&result, err, "missing_dependency")
			return false
		}
		return true
	}
	var scratch string
	if recipe.RequiresScratchWrite || recipe.Kind == "nsis_unicode_smoke" {
		var err error
		scratch, err = os.MkdirTemp(".", ".solu-capability-")
		if err != nil {
			capabilityError(&result, err, "missing_path")
			return
		}
		defer os.RemoveAll(scratch)
		if err := os.WriteFile(filepath.Join(scratch, "payload.txt"), []byte("soluterminal capability probe\n"), 0600); err != nil {
			capabilityError(&result, err, "missing_path")
			return
		}
	}
	if recipe.Kind == "exec_version" {
		run("", bin, recipe.VersionArg)
		return
	}
	if recipe.Kind != "nsis_unicode_smoke" {
		result.Status = "unsupported_probe"
		return
	}
	inspector := recipe.InspectorToolPath
	if inspector == "" {
		var err error
		inspector, err = exec.LookPath(recipe.InspectorExec)
		if err != nil {
			capabilityError(&result, err, "missing_executable")
			return
		}
	}
	// Avoid a relative PATH result being reinterpreted after switching to scratch.
	bin, _ = filepath.Abs(bin)
	inspector, _ = filepath.Abs(inspector)
	const script = "Unicode true\nName \"Capability probe\"\nOutFile \"probe.exe\"\nSilentInstall silent\nSection\nSetOutPath $INSTDIR\nFile \"payload.txt\"\nSystem::Call 'kernel32::GetCurrentProcessId() i .r0'\nSectionEnd\n"
	if err := os.WriteFile(filepath.Join(scratch, "probe.nsi"), []byte(script), 0600); err != nil {
		capabilityError(&result, err, "missing_path")
		return
	}
	if !run(scratch, bin, "-V2", "probe.nsi") {
		return
	}
	if !run(scratch, inspector, "x", "-y", "-oextracted", "probe.exe") {
		return
	}
	payload, err := os.ReadFile(filepath.Join(scratch, "extracted", "payload.txt"))
	if err != nil || string(payload) != "soluterminal capability probe\n" {
		result.Status = "probe_failed"
		return
	}
	plugin, err := os.Stat(filepath.Join(scratch, "extracted", "$PLUGINSDIR", "System.dll"))
	if err != nil || plugin.Size() == 0 {
		result.Status = "probe_failed"
	}
	return
}

func capabilityError(result *capabilityResult, err error, missing string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		result.Status, result.Errno = "timed_out", "ETIMEDOUT"
	case errors.Is(err, os.ErrNotExist), errors.Is(err, exec.ErrNotFound):
		result.Status, result.Errno = missing, "ENOENT"
	case errors.Is(err, syscall.EACCES):
		result.Status, result.Errno = "execution_denied", "EACCES"
	case errors.Is(err, syscall.EPERM):
		result.Status, result.Errno = "execution_denied", "EPERM"
	case errors.Is(err, syscall.EROFS):
		result.Status, result.Errno = "execution_denied", "EROFS"
	default:
		result.Status = "probe_failed"
	}
}

const capabilityCommand = "workflow-capability-probe"

// Explicit env overrides keep command/exec's shell environment equal to the
// selected worker environment instead of inheriting an app-server default.
func capabilityCommandEnv(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			out[k] = v
		}
	}
	return out
}
