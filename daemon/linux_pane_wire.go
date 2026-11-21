package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"soluterminal/helperproto"
)

// linuxPanesEnv switches Linux panes on for a Windows host (ST-253, until
// the Settings switch lands): SOLU_LINUX_PANES=1 in %USERPROFILE%\.solu\env.
const linuxPanesEnv = "SOLU_LINUX_PANES"

// helperHostInputs locates what the Windows host needs to place the helper:
// the cached signed manifest's folder and the bundled solud-linux-amd64.gz.
type helperHostInputs struct {
	manifestDir string
	gzPath      string
}

// newHelperSupervisorForDistro wires one distro's supervisor to its inputs:
// before each start, the expected hash is re-read from the signed manifest
// and the bundled binary is checked against it before it is placed.
// linuxPaneHooks plugs later milestones into each distro's supervisor (W3:
// fwd stream handlers and hello observation). Zero value: v1 behaviour.
type linuxPaneHooks struct {
	StreamHandlers func(distro string) map[string]helperproto.HostStreamHandler
	OnHello        func(distro string, hello helperproto.HelloResult, p helperPlacement)
}

func newHelperSupervisorForDistro(distro string, in helperHostInputs, hooks linuxPaneHooks, logf func(string, ...any)) *helperSupervisor {
	run := wslDistroRunner(distro)
	cfg := helperSupervisorConfig{
		Distro:  distro,
		Version: version,
		Place: func(ctx context.Context) (helperPlacement, error) {
			sha, err := helperExpectedSHA(in.manifestDir, version, currentBuildFlavor())
			if err != nil {
				return helperPlacement{}, err
			}
			return placeHelper(ctx, run, version, sha, helperGzSource(in.gzPath, sha))
		},
		Command: func(p helperPlacement) *exec.Cmd { return wslHelperCommand(distro, p) },
		Logf:    logf,
	}
	if hooks.StreamHandlers != nil {
		cfg.StreamHandlers = func() map[string]helperproto.HostStreamHandler { return hooks.StreamHandlers(distro) }
	}
	if hooks.OnHello != nil {
		cfg.OnHello = func(h helperproto.HelloResult, p helperPlacement) { hooks.OnHello(distro, h, p) }
	}
	return newHelperSupervisor(cfg)
}

// linuxPaneBackendFromEnv returns the Linux-pane backend when this host has
// Linux panes switched on, else nil (Manager.linux stays off).
func linuxPaneBackendFromEnv(hooks linuxPaneHooks) linuxPaneBackend {
	if os.Getenv(linuxPanesEnv) != "1" {
		return nil
	}
	in, err := helperHostInputsForThisHost()
	if err != nil {
		log.Printf("linux panes: off: %v", err)
		return nil
	}
	return newHelperBackend(func(distro string) *helperSupervisor {
		return newHelperSupervisorForDistro(distro, in, hooks, log.Printf)
	})
}

// helperGzBeside finds the bundled helper beside the canonical solud.exe.
func helperGzBeside() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), helperSourceName), nil
}
