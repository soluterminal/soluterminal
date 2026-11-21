package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// `solud hook-forward --socket <runtimeDir>/hooks.sock` is the Claude Code
// command hook for panes the A′ helper runs in WSL (ST-253 M3). Claude's
// http hooks can't reach a unix socket, and SessionStart only runs command
// hooks anyway. It sends the hook's stdin JSON to the helper's hooks.sock as
// POST /api/hooks/tool-call; the helper attests the caller from the kernel
// and forwards it to the host.
//
// Like today's `curl … || true` SessionStart hook it never fails the hook:
// it prints nothing on stdout, waits at most 4 seconds, and exits 0 whatever
// happens, so a missing helper only loses the fast wake. It carries no pane
// id, token or header: the helper names the pane from the connection.

const (
	hookForwardCommand = "hook-forward"
	// Inside Claude's 5 s hook timeout (linuxClaudeInjectArgs), and long
	// enough for a just-started pane's registration (helper 1.5 s + host 2 s).
	hookForwardTimeout = 4 * time.Second
	hookForwardRoute   = "/api/hooks/tool-call"
)

func runHookForward(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet(hookForwardCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("socket", "", "path of the helper's hooks.sock")
	if err := fs.Parse(args); err != nil || *sock == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: solud hook-forward --socket <path>")
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(stdin, helperFwdMaxHookRequest+1))
	if err != nil || len(body) > helperFwdMaxHookRequest {
		return 0
	}
	if err := hookForwardPost(*sock, body, hookForwardTimeout); err != nil {
		fmt.Fprintf(stderr, "soluterminal hook-forward: %v\n", err)
	}
	return 0
}

func hookForwardPost(sock string, body []byte, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
			Proxy:             nil,
		},
	}
	resp, err := client.Post("http://"+helperFwdHostName+hookForwardRoute, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("helper answered %s", resp.Status)
	}
	return nil
}
