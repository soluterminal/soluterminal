package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var supportBundleDial = net.DialTimeout

func runSupportBundleCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	var diagnostics bytes.Buffer
	flags.SetOutput(&diagnostics)
	out := flags.String("out", "", "destination file (must not already exist)")
	data := flags.String("data", defaultDataDir(), "workspace data directory to inspect, without modifying it")
	jsonOut := flags.Bool("json", false, "save the sectioned bundle as JSON instead of text")
	addr := flags.String("addr", defaultDaemonAddr, "loopback address the daemon listens on (a side daemon's -addr), used to find its log and check its port")
	if err := flags.Parse(args); err != nil {
		fmt.Fprint(stderr, redactSensitive(diagnostics.String()))
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "support-bundle: unexpected positional arguments")
		return 2
	}
	dataDir, err := filepath.Abs(*data)
	if err != nil {
		fmt.Fprintln(stderr, "support-bundle: cannot resolve data directory")
		return 1
	}
	daemonAddr, ok := supportBundleLoopbackAddr(*addr)
	if !ok {
		fmt.Fprintln(stderr, "support-bundle: --addr must be a loopback host:port, such as 127.0.0.1:8787")
		return 2
	}
	shellDir, _ := soluShellDataDir()
	exe, _ := os.Executable()
	offline := &supportBundleOffline{dataDir: dataDir, shellDir: shellDir, executable: exe, addr: daemonAddr}
	bundle := buildSupportBundle(nil, "", offline)
	// Separate the timestamp with a dot so the generated name cannot look
	// like one long opaque credential to the shared conservative redactor.
	bundle.Filename = strings.Replace(bundle.Filename, "soluterminal-support-", "soluterminal-support.", 1)
	if *jsonOut {
		bundle.Filename = strings.TrimSuffix(bundle.Filename, ".txt") + ".json"
	}
	body, err := encodeSupportBundleFile(&bundle, *jsonOut)
	if err != nil {
		fmt.Fprintln(stderr, "support-bundle: could not encode diagnostics")
		return 1
	}
	if *out == "" {
		*out = bundle.Filename
	}
	path, err := filepath.Abs(*out)
	if err != nil {
		fmt.Fprintln(stderr, "support-bundle: cannot resolve output path")
		return 1
	}
	// Exclusive creation also prevents following an existing output symlink or
	// accidentally overwriting a diagnostic input, executable, or credential file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "support-bundle: cannot create output file (use a new path in a writable directory)")
		return 1
	}
	_, writeErr := f.Write(body)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path) // Only the new file this invocation created.
		fmt.Fprintln(stderr, "support-bundle: could not write output file")
		return 1
	}
	fmt.Fprintln(stdout, *out) // the user chose this path; print it as given
	return 0
}

// encodeSupportBundleFile includes framing/JSON escaping in the existing 1 MiB
// budget. Shrink complete, already-redacted sections, never serialized bytes.
func encodeSupportBundleFile(bundle *supportBundleResponse, asJSON bool) ([]byte, error) {
	for {
		var body []byte
		if asJSON {
			var err error
			body, err = json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				return nil, err
			}
			body = append(body, '\n')
		} else {
			var text strings.Builder
			fmt.Fprintf(&text, "soluterminal support bundle\nGenerated: %s\n\n", bundle.GeneratedAt)
			for _, section := range bundle.Sections {
				fmt.Fprintf(&text, "== %s ==\n%s\n", section.Title, section.Text)
				if section.Note != "" {
					fmt.Fprintf(&text, "Note: %s\n", section.Note)
				}
				if section.Truncated {
					text.WriteString("[truncated]\n")
				}
				text.WriteByte('\n')
			}
			text.WriteString("Omitted: " + strings.Join(bundle.Omitted, "; ") + "\n")
			text.WriteString("Log paths: " + strings.Join(bundle.LogPaths, ", ") + "\n")
			body = []byte(text.String())
		}
		if len(body) <= supportBundleMaxTotalBytes {
			return body, nil
		}
		largest := -1
		for i, section := range bundle.Sections {
			if section.Bytes > 0 && (largest < 0 || section.Bytes > bundle.Sections[largest].Bytes) {
				largest = i
			}
		}
		if largest < 0 {
			return nil, errors.New("bundle metadata exceeds size limit")
		}
		section := bundle.Sections[largest]
		bundle.Sections[largest] = finalizeSupportSection(section, section.Bytes/2)
	}
}

type supportBundleOffline struct {
	dataDir, shellDir, executable string
	// addr is the daemon's listen address (defaultDaemonAddr unless --addr
	// names a side daemon's): it decides where the durable log lives and which
	// port is checked.
	addr string
}

// supportBundleLoopbackAddr normalises --addr to host:port on a loopback
// host. The offline command never reaches beyond this machine.
func supportBundleLoopbackAddr(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || port == "" {
		return "", false
	}
	if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
		return "", false
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", false
		}
	}
	return net.JoinHostPort(host, port), true
}

func (o *supportBundleOffline) daemonAddr() string {
	if o.addr == "" {
		return defaultDaemonAddr
	}
	return o.addr
}

func (o *supportBundleOffline) sections() []supportBundleSection {
	versions := supportBundleKV("version", map[string]any{
		"binary": version, "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"go": runtime.Version(), "executable": o.executable,
		"dataDir": o.dataDir, "shellDataDir": o.shellDir,
	})
	var shell shellInfo
	versions += "\n" + supportBundleReadJSON(filepath.Join(o.dataDir, "shell-info.json"), &shell)
	markers := []string{filepath.Join(o.dataDir, "solud.version")}
	if o.shellDir != "" {
		markers = append(markers, filepath.Join(o.shellDir, "solud.version"))
	}
	if o.executable != "" {
		markers = append(markers, filepath.Join(filepath.Dir(o.executable), "solud.version"), o.executable+".version")
	}
	for _, path := range supportBundleUniquePaths(markers...) {
		body, note := supportBundleReadSmallFile(path)
		versions += "\n" + supportBundleKV("marker", map[string]string{"path": path, "version": string(body), "note": note})
	}
	var rollback updateRollbackState
	var settings updateSettings
	updates := supportBundleReadJSON(filepath.Join(o.dataDir, "update_rollback.json"), &rollback) + "\n" +
		supportBundleReadJSON(filepath.Join(o.dataDir, "update_settings.json"), &settings)
	// A TCP connect reports occupancy only; never mistake an unrelated service
	// for solud, and never start a daemon or send a mutating request.
	addr := o.daemonAddr()
	port := addr + ": no TCP connection (daemon may be stopped or use another port; pass --addr for a side daemon)"
	conn, err := supportBundleDial("tcp", addr, 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		port = addr + ": accepting TCP connections (service identity not checked)"
	}
	return []supportBundleSection{
		{ID: "versions", Title: "Offline versions and paths", Text: versions, SelectedByDefault: true},
		{ID: "updates", Title: "Persisted update state", Text: updates, Note: "Live updater status is unavailable; recent errors may also appear in the log tails.", SelectedByDefault: true},
		{ID: "system", Title: "OS and launch diagnostics", Text: supportBundlePlatformInfo(), SelectedByDefault: true},
		{ID: "connection", Title: "Local port status", Text: port, SelectedByDefault: true},
	}
}

func (o *supportBundleOffline) logPaths() []string {
	paths := []string{}
	envDir := strings.TrimSpace(os.Getenv("SOLU_DURABLE_LOG_DIR"))
	if !strings.EqualFold(envDir, "off") {
		durableDir, _ := resolveDurableLogDir(runtime.GOOS, envDir, o.daemonAddr(), o.dataDir, defaultDataDir(), func() (string, error) { return o.shellDir, nil })
		if durableDir != "" {
			paths = append(paths, filepath.Join(durableDir, durableLogName))
		}
	}
	if o.shellDir != "" {
		for _, name := range []string{"daemon.log", "launch.log", "successor.log"} {
			paths = append(paths, filepath.Join(o.shellDir, name))
		}
	}
	if o.executable != "" {
		paths = append(paths, filepath.Join(filepath.Dir(o.executable), "successor.log"))
	}
	return supportBundleUniquePaths(paths...)
}

func supportBundleUniquePaths(paths ...string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, path := range paths {
		if path != "" && !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}

func supportBundleReadSmallFile(path string) ([]byte, string) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, "missing or not a regular file"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "unreadable"
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, supportBundleFileTailBytes+1))
	if err != nil {
		return nil, "unreadable"
	}
	if len(body) > supportBundleFileTailBytes {
		return nil, "omitted: file exceeds diagnostic read limit"
	}
	return body, ""
}

func supportBundleReadJSON(path string, target any) string {
	body, note := supportBundleReadSmallFile(path)
	if note != "" {
		return supportBundleKV(filepath.Base(path), map[string]string{"status": note})
	}
	// Decode only the known diagnostic fields; unknown fields are never copied.
	if json.Unmarshal(body, target) != nil {
		return filepath.Base(path) + ": invalid JSON (contents omitted)"
	}
	return supportBundleKV(filepath.Base(path), target)
}
