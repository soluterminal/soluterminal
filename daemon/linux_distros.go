package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// linuxDistro is one WSL distro the new-pane picker can open a Linux pane in.
type linuxDistro struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// listWSLDistros asks wsl.exe for the installed distros and the default
// one (wsl_list.go reads its UTF-16 output and never localized text). A
// seam for tests; nil off Windows.
var listWSLDistros = func(ctx context.Context) ([]linuxDistro, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "wsl.exe", args...)
		cmd.WaitDelay = time.Second // a child holding the pipe can't outlast the bound
		applyNoWindow(cmd)
		cmd.Env = helperWSLEnv(os.Environ())
		out, err := cmd.Output()
		return decodeWSLListing(out), err
	}
	quiet, err := run("--list", "--quiet")
	if err != nil {
		return nil, err
	}
	verbose, _ := run("--list", "--verbose")
	def, _ := wslDefaultDistro(verbose, quiet)
	var out []linuxDistro
	for _, name := range wslDistroNames(quiet) {
		if !validWSLDistro.MatchString(name) {
			continue // a name a Linux pane couldn't use
		}
		out = append(out, linuxDistro{Name: name, Default: name == def})
	}
	return out, nil
}

// linuxDistros is GET /api/linux/distros: whether this machine opens Linux
// panes (SOLU_LINUX_PANES=1 on a Windows host) and in which distros.
func (a *apiServer) linuxDistros(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		Enabled bool          `json:"enabled"`
		Distros []linuxDistro `json:"distros"`
		Error   string        `json:"error,omitempty"`
	}{Distros: []linuxDistro{}}
	if a.mgr != nil && a.mgr.linux != nil {
		resp.Enabled = true
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		distros, err := listWSLDistros(ctx)
		if err != nil {
			resp.Error = "couldn't list the WSL distros"
		}
		if distros != nil {
			resp.Distros = distros
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
