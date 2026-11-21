package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Placing the helper binary in a distro (helper-protocol-v1, Files; ruling
// 8.1): ~/.local/lib/soluterminal/helper/solud-<version>, directory and file
// 0700, written to a temp name and renamed, and its sha256 checked against
// the release's signed manifest entry before each start.

// helperDistroRunner runs one of the fixed scripts below in the distro as
// the person's Linux user: `sh -c <script> sh <args…>`. Arguments are passed
// positionally, never spliced into the script.
type helperDistroRunner func(ctx context.Context, script string, args []string, stdin io.Reader) ([]byte, error)

// wslDistroRunner runs scripts through wsl.exe in one distro.
func wslDistroRunner(distro string) helperDistroRunner {
	return func(ctx context.Context, script string, args []string, stdin io.Reader) ([]byte, error) {
		argv := append([]string{"-d", distro, "--exec", "sh", "-c", script, "sh"}, args...)
		cmd := exec.CommandContext(ctx, "wsl.exe", argv...)
		applyNoWindow(cmd)
		cmd.Env = helperWSLEnv(os.Environ())
		cmd.Stdin = stdin
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(decodeWSLListing(stderr.Bytes())))
		}
		return out, nil
	}
}

// helperProbeScript reports the user, arch and home, whether claude and
// codex are on the login shell's PATH (for W3; nvm and ~/.profile set it
// there), and the sha256 and modes of the placed binary for $1.
const helperProbeScript = `printf 'uid=%s\narch=%s\nhome=%s\n' "$(id -u)" "$(uname -m)" "$HOME"
t=; command -v timeout >/dev/null 2>&1 && t="timeout 10"
$t "${SHELL:-/bin/sh}" -lc 'command -v claude >/dev/null 2>&1 && echo claude=1; command -v codex >/dev/null 2>&1 && echo codex=1' 2>/dev/null
d="$HOME/.local/lib/soluterminal/helper"
f="$d/solud-$1"
if [ -L "$d" ]; then echo dirlink=1; elif [ -d "$d" ]; then printf 'dirmode=%s\n' "$(stat -c %a "$d")"; fi
if [ -f "$f" ] && [ ! -L "$f" ]; then printf 'sha256=%s\nmode=%s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$(stat -c %a "$f")"; fi
exit 0`

// helperPlaceScript writes stdin to solud-$1 through a temp name in the same
// 0700 directory, then renames it into place.
const helperPlaceScript = `set -eu
d="$HOME/.local/lib/soluterminal/helper"
mkdir -p "$d"
if [ -L "$d" ] || [ -L "$d/solud-$1" ]; then echo "helper path is a symlink" >&2; exit 3; fi
chmod 700 "$d"
t=$(mktemp "$d/.solud-XXXXXXXX")
trap 'rm -f "$t"' EXIT
cat > "$t"
chmod 700 "$t"
mv -f "$t" "$d/solud-$1"
trap - EXIT`

var (
	helperVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
	sha256HexRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// errHelperArchUnsupported: v1 places x86_64 only.
var errHelperArchUnsupported = errors.New("this distro's architecture isn't supported for Linux panes yet")

type helperProbe struct {
	uid       int
	arch      string
	home      string
	sha256    string
	mode      string // the binary's permission bits, octal
	dirMode   string
	dirLink   bool
	hasClaude bool
	hasCodex  bool
}

func parseHelperProbe(out []byte) (helperProbe, error) {
	var p helperProbe
	uidSeen := false
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		switch k {
		case "uid":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return p, fmt.Errorf("probe: bad uid %q", v)
			}
			p.uid, uidSeen = n, true
		case "arch":
			p.arch = v
		case "home":
			p.home = v
		case "sha256":
			p.sha256 = v
		case "mode":
			p.mode = v
		case "dirmode":
			p.dirMode = v
		case "dirlink":
			p.dirLink = v == "1"
		case "claude":
			p.hasClaude = v == "1"
		case "codex":
			p.hasCodex = v == "1"
		}
	}
	if !uidSeen || p.arch == "" || !path.IsAbs(p.home) || strings.ContainsAny(p.home, "\x00\n") {
		return p, errors.New("probe: incomplete answer from the distro")
	}
	return p, nil
}

func (p helperProbe) placedOK(wantSHA string) bool {
	return p.sha256 == wantSHA && p.mode == "700" && p.dirMode == "700" && !p.dirLink
}

// placeHelper makes sure the distro holds solud-<version> with sha256
// wantSHA, writing it from source when it is missing or differs, and
// returns where it is and who will run it. source opens the release's
// solud-linux bytes (already decompressed).
func placeHelper(ctx context.Context, run helperDistroRunner, version, wantSHA string, source func() (io.ReadCloser, error)) (helperPlacement, error) {
	var none helperPlacement
	if !helperVersionRe.MatchString(version) {
		return none, fmt.Errorf("bad helper version %q", version)
	}
	if !sha256HexRe.MatchString(wantSHA) {
		return none, errors.New("no signed manifest hash for solud-linux")
	}
	probe := func() (helperProbe, error) {
		out, err := run(ctx, helperProbeScript, []string{version}, nil)
		if err != nil {
			return helperProbe{}, fmt.Errorf("probe the distro: %w", err)
		}
		return parseHelperProbe(out)
	}
	p, err := probe()
	if err != nil {
		return none, err
	}
	if p.arch != "x86_64" {
		return none, fmt.Errorf("%w (%s)", errHelperArchUnsupported, p.arch)
	}
	// Placed and right means the hash AND the 0700 file and directory with
	// no symlink: anything else is placed again, and the place script
	// refuses a symlinked directory.
	if !p.placedOK(wantSHA) {
		src, err := source()
		if err != nil {
			return none, fmt.Errorf("open solud-linux: %w", err)
		}
		_, err = run(ctx, helperPlaceScript, []string{version}, src)
		_ = src.Close()
		if err != nil {
			return none, fmt.Errorf("place the helper: %w", err)
		}
		if p, err = probe(); err != nil {
			return none, err
		}
		if !p.placedOK(wantSHA) {
			return none, errors.New("the placed helper doesn't match the signed manifest or isn't private")
		}
	}
	return helperPlacement{
		BinPath:   p.home + "/.local/lib/soluterminal/helper/solud-" + version,
		UID:       p.uid,
		Arch:      p.arch,
		HasClaude: p.hasClaude,
		HasCodex:  p.hasCodex,
	}, nil
}

// wslHelperCommand is the helper's process: wsl.exe -d <distro> --exec
// <bin> helper --proto 1.
func wslHelperCommand(distro string, p helperPlacement) *exec.Cmd {
	cmd := exec.Command("wsl.exe", "-d", distro, "--exec", p.BinPath, "helper", "--proto", "1")
	applyNoWindow(cmd)
	cmd.Env = helperWSLEnv(os.Environ())
	return cmd
}

// helperWSLEnvKeys is all of the Windows environment wsl.exe gets. Above
// all WSLENV is left out: it names host variables for WSL to copy into the
// distro, and so into the helper and every agent it starts.
var helperWSLEnvKeys = map[string]bool{
	"SYSTEMROOT": true, "WINDIR": true, "SYSTEMDRIVE": true, "COMSPEC": true,
	"PATH": true, "PATHEXT": true, "TEMP": true, "TMP": true,
	"USERPROFILE": true, "LOCALAPPDATA": true, "APPDATA": true, "USERNAME": true,
	"HOMEDRIVE": true, "HOMEPATH": true,
	"PROCESSOR_ARCHITECTURE": true, "NUMBER_OF_PROCESSORS": true,
}

// helperWSLEnv keeps only the Windows variables wsl.exe itself needs.
func helperWSLEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && helperWSLEnvKeys[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	return out
}
