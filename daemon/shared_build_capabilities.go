package main

import (
	"bytes"

	"encoding/json"

	"fmt"
	"io"

	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	envFaultBuildCapability = "build_capability"
	capabilityBatchLimit    = 90 * time.Second
)

func strictCapabilityJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing capability declaration")
	}
	return nil
}

func validateCapabilityRequirement(r envRequirement) error {
	selectors := 0
	for _, s := range []string{r.Path, r.Exec, r.ToolPath} {
		if s != "" {
			selectors++
		}
	}
	if selectors != 1 {
		return fmt.Errorf("exactly one of path, exec or toolPath is required")
	}
	if len(r.Name) > 120 || strings.ContainsFunc(r.Name, unicode.IsControl) {
		return fmt.Errorf("name must be at most 120 characters without controls")
	}
	if r.Path != "" {
		if strings.ContainsFunc(r.Path, unicode.IsControl) {
			return fmt.Errorf("path must not contain controls")
		}
		if err := validatePreviewRelativePath("path", r.Path, true); err != nil {
			return err
		}
	}
	if r.Exec != "" && !isBareExecutableName(r.Exec) {
		return fmt.Errorf("exec must be a bare executable name")
	}
	if r.ToolPath != "" && (!filepath.IsAbs(r.ToolPath) || filepath.Clean(r.ToolPath) != r.ToolPath || strings.ContainsFunc(r.ToolPath, unicode.IsControl)) {
		return fmt.Errorf("toolPath must be a clean absolute path without controls")
	}
	if r.Probe == nil {
		return nil
	}
	p := r.Probe
	if r.Path != "" {
		return fmt.Errorf("path uses the fixed existence/readability probe")
	}
	if p.Kind != "exec_version" && p.Kind != "nsis_unicode_smoke" {
		return fmt.Errorf("unsupported probe kind %q", p.Kind)
	}
	if p.Kind == "exec_version" {
		if p.InspectorExec != "" || p.InspectorToolPath != "" {
			return fmt.Errorf("version probe cannot declare an inspector")
		}
		if p.VersionArg != "" && p.VersionArg != "--version" && p.VersionArg != "-V" && p.VersionArg != "version" && p.VersionArg != "-VERSION" && p.VersionArg != "i" {
			return fmt.Errorf("unsupported versionArg")
		}
		return nil
	}
	if p.VersionArg != "" || p.RequiresScratchWrite {
		return fmt.Errorf("NSIS smoke probe has fixed arguments and scratch output")
	}
	if (p.InspectorExec == "") == (p.InspectorToolPath == "") {
		return fmt.Errorf("NSIS probe needs exactly one inspector selector")
	}
	if p.InspectorExec != "" && !isBareExecutableName(p.InspectorExec) {
		return fmt.Errorf("inspectorExec must be bare")
	}
	if p.InspectorToolPath != "" && (!filepath.IsAbs(p.InspectorToolPath) || filepath.Clean(p.InspectorToolPath) != p.InspectorToolPath || strings.ContainsFunc(p.InspectorToolPath, unicode.IsControl)) {
		return fmt.Errorf("inspectorToolPath must be clean and absolute")
	}
	return nil
}

// ExitCode is nil until a process actually exits. A missing response, a path
// lookup, and an untrusted model statement can never produce Status=ready.
type capabilityResult struct {
	Name       string    `json:"name"`
	Selector   string    `json:"selector"`
	Status     string    `json:"status"`
	ExitCode   *int      `json:"exitCode,omitempty"`
	Errno      string    `json:"errno,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

func capabilityResultName(r envRequirement) string {
	if r.Name != "" {
		return r.Name
	}
	return capabilitySelector(r)
}

func capabilitySelector(r envRequirement) string {
	if r.Path != "" {
		return r.Path
	}
	if r.ToolPath != "" {
		return r.ToolPath
	}
	return r.Exec
}

func effectiveCapabilityRecipe(r envRequirement) (capabilityRecipe, bool) {
	if r.Path != "" {
		return capabilityRecipe{}, true
	}
	name := r.Exec
	if name == "" {
		name = filepath.Base(r.ToolPath)
	}
	if r.Probe != nil {
		recipe := *r.Probe
		if recipe.Kind == "exec_version" && recipe.VersionArg == "" {
			recipe.VersionArg = capabilityVersionArgs[name]
		}
		return recipe, recipe.Kind != "exec_version" || recipe.VersionArg != ""
	}
	arg := capabilityVersionArgs[name]
	return capabilityRecipe{Kind: "exec_version", VersionArg: arg}, arg != ""
}

var capabilityVersionArgs = map[string]string{
	"go": "version", "makensis": "-VERSION", "7zz": "i",
	"node": "--version", "pnpm": "--version", "cargo": "--version",
	"cargo-xwin": "--version", "git": "--version", "uv": "--version",
	"python": "--version", "python3": "--version", "npm": "--version",
	"rsync": "--version", "ssh": "-V", "blacksmith": "--version",
}

type capabilityBatch struct {
	ID                string             `json:"id"`
	Stage             string             `json:"stage"`
	Runtime           string             `json:"runtime"`
	Dir               string             `json:"dir"`
	Role              string             `json:"role"`
	Model             string             `json:"model,omitempty"`
	PermissionPreset  string             `json:"permissionPreset"`
	WritableRoots     []string           `json:"writableRoots,omitempty"`
	Account           string             `json:"account,omitempty"`
	ConfigDir         string             `json:"configDir,omitempty"`
	RequirementDigest string             `json:"requirementDigest"`
	ContextDigest     string             `json:"contextDigest"`
	PassiveDigest     string             `json:"passiveDigest,omitempty"`
	WorktreeHead      string             `json:"worktreeHead"`
	StartedAt         time.Time          `json:"startedAt"`
	Deadline          time.Time          `json:"deadline"`
	Results           []capabilityResult `json:"results,omitempty"`
	FinishedAt        *time.Time         `json:"finishedAt,omitempty"`
}

func validateCapabilityResults(reqs []envRequirement, results []capabilityResult) error {
	if len(results) != len(reqs) {
		return fmt.Errorf("incomplete worker capability evidence")
	}
	for i, result := range results {
		if result.Name != capabilityResultName(reqs[i]) || result.Selector != capabilitySelector(reqs[i]) || result.ObservedAt.IsZero() {
			return fmt.Errorf("uncorrelated worker capability evidence")
		}
		switch result.Status {
		case "ready":
			if reqs[i].Path == "" && (result.ExitCode == nil || *result.ExitCode != 0) {
				return fmt.Errorf("ready execution has no successful process exit")
			}
		case "missing_path", "missing_executable", "missing_dependency", "execution_denied", "unsupported_probe", "probe_failed", "timed_out", "unobserved":
		default:
			return fmt.Errorf("unknown worker capability status")
		}
		switch result.Errno {
		case "", "ENOENT", "EACCES", "EPERM", "EROFS", "ETIMEDOUT":
		default:
			return fmt.Errorf("unsupported capability errno")
		}
		if result.Status == "missing_dependency" && result.Errno != "ENOENT" {
			return fmt.Errorf("missing dependency requires observed ENOENT")
		}
		if result.Status == "execution_denied" && result.Errno != "EACCES" && result.Errno != "EPERM" && result.Errno != "EROFS" {
			return fmt.Errorf("execution denial requires observed EACCES, EPERM or EROFS")
		}
		if len(result.Evidence) > 2048 || strings.ContainsFunc(result.Evidence, unicode.IsControl) {
			return fmt.Errorf("unbounded or unsafe capability evidence")
		}
	}
	return nil
}

func cloneCapabilityBatch(src *capabilityBatch) *capabilityBatch {
	if src == nil {
		return nil
	}
	dst := *src
	dst.FinishedAt = cloneValuePtr(src.FinishedAt)
	dst.Results = make([]capabilityResult, len(src.Results))
	dst.WritableRoots = append([]string(nil), src.WritableRoots...)
	for i, result := range src.Results {
		dst.Results[i] = result
		dst.Results[i].ExitCode = cloneValuePtr(result.ExitCode)
	}
	return &dst
}

type capabilityFault struct {
	Batch  capabilityBatch
	Result capabilityResult
}

func capabilityUnsupported(reqs []envRequirement, detail string) []capabilityResult {
	results := make([]capabilityResult, len(reqs))
	for i, r := range reqs {
		results[i] = capabilityResult{Name: capabilityResultName(r), Selector: capabilitySelector(r), Status: "unsupported_probe", Evidence: detail, ObservedAt: time.Now().UTC()}
	}
	return results
}

func (e *capabilityFault) Error() string {
	return fmt.Sprintf("required build capability %q: %s (%s)", e.Result.Name, e.Result.Status, e.Result.Evidence)
}

func validatePreviewRelativePath(name, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
	if filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean relative path", name)
	}
	normalized := strings.ReplaceAll(value, `\`, "/")
	if path.Clean(normalized) != normalized || strings.HasPrefix(normalized, "//") || (len(normalized) >= 2 && normalized[1] == ':') {
		return fmt.Errorf("%s must not be absolute", name)
	}
	for _, segment := range strings.FieldsFunc(normalized, func(r rune) bool { return r == '/' }) {
		if segment == ".." {
			return fmt.Errorf("%s must not contain '..'", name)
		}
	}
	return nil
}

// isBareExecutableName accepts only a name resolvable through PATH. A value
// carrying a separator would be a filesystem lookup relative to the daemon's
// working directory, not the worktree — declare those with `path` instead.
func isBareExecutableName(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) < 0
}
