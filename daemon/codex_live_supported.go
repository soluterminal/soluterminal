//go:build linux || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// codexLiveCopy is what a live Codex pane tells its user when its login needs
// attention. Public builds follow the user's own Codex login, so they point at
// `codex login`; a build with saved accounts replaces these from its init
// (codex_team_copy.go) to point at its account settings.
var codexLiveCopy = struct {
	signedOut, loginChanged, refreshFailed, refreshFailedCodex string
}{
	signedOut:          "Signed out. Run codex login in a shell, then restart this pane.",
	loginChanged:       "This pane is using a different Codex login. Restart it to use your normal one.",
	refreshFailed:      "Codex sign-in expired. Run codex login in a shell, then restart this pane.",
	refreshFailedCodex: "Codex sign-in expired. Run codex login in a shell, then restart this pane.",
}

type codexLiveRPC struct {
	conn           *websocket.Conn
	mu             sync.Mutex
	writeMu        sync.Mutex
	next           int
	pending        map[int]chan codexRPCLine
	requests       chan codexRPCLine
	accountUpdates chan codexRPCLine
	done           chan struct{}
}

func newCodexLiveRPC(ctx context.Context, socket string) (*codexLiveRPC, error) {
	d := websocket.Dialer{HandshakeTimeout: 2 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	c, _, err := d.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(16 << 20)
	r := &codexLiveRPC{conn: c, pending: map[int]chan codexRPCLine{}, requests: make(chan codexRPCLine, 8), accountUpdates: make(chan codexRPCLine, 8), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			var line codexRPCLine
			if err := c.ReadJSON(&line); err != nil {
				return
			}
			if line.Method != "" && len(line.ID) > 0 {
				select {
				case r.requests <- line:
				default:
					return
				}
				continue
			}
			if line.Method == "account/updated" {
				select {
				case r.accountUpdates <- line:
				default:
				}
				continue
			}
			var id int
			if json.Unmarshal(line.ID, &id) != nil {
				continue
			}
			r.mu.Lock()
			ch := r.pending[id]
			r.mu.Unlock()
			if ch != nil {
				select {
				case ch <- line:
				default:
				}
			}
		}
	}()
	return r, nil
}

func (r *codexLiveRPC) write(value any) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_ = r.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return r.conn.WriteJSON(value)
}

func (r *codexLiveRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	r.next++
	id := r.next
	ch := make(chan codexRPCLine, 1)
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	if err := r.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case line := <-ch:
		if line.Error != nil {
			return nil, errors.New("Codex rejected " + method)
		} // never log provider response/token echoes
		return line.Result, nil
	case <-r.done:
		return nil, errors.New("Codex authentication connection closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type codexLiveToken struct {
	AccessToken string `json:"accessToken"`
	AccountID   string `json:"chatgptAccountId"`
	Revision    string `json:"-"`
	Subject     string `json:"-"`
}

func readCodexLiveToken(path string) (codexLiveToken, error) {
	f, err := os.Open(path)
	if err != nil {
		return codexLiveToken{}, errors.New("Codex login file unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCodexAuthBytes+1))
	if err != nil || len(b) > maxCodexAuthBytes {
		return codexLiveToken{}, errors.New("invalid Codex login file")
	}
	access, _, id, ok := codexAuthTokens(b)
	if !ok || id == "" {
		return codexLiveToken{}, errors.New("live switching requires a ChatGPT file login")
	}
	return codexLiveToken{AccessToken: access, AccountID: id, Revision: codexAuthFingerprint(b), Subject: codexAuthSubject(b)}, nil
}

func codexLiveLogin(ctx context.Context, r *codexLiveRPC, token codexLiveToken) error {
	_, err := r.call(ctx, "account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": token.AccessToken, "chatgptAccountId": token.AccountID})
	return err
}

func codexLiveRefresh(ctx context.Context, daemonURL, dir, revision string) error {
	u, err := url.Parse(daemonURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid local daemon address")
	}
	b, _ := json.Marshal(map[string]string{"revision": revision})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(daemonURL, "/")+"/api/codex/live-auth/refresh", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token, err := os.ReadFile(filepath.Join(dir, "token")); err == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return errors.New("credential daemon unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("Codex login needs attention in Credentials")
	}
	return nil
}

// Only config overrides/features belong to the app-server process; TUI flags
// (including permission/model choices) remain verbatim on the remote TUI.
// Apply -c to BOTH sides so developer instructions and security overrides are
// not silently lost when the UI no longer hosts the engine in-process.
func codexLiveServerArgs(args []string, socket string) []string {
	out := []string{"app-server", "--listen", "unix://" + socket}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "-c" || a == "--config" || a == "--enable" || a == "--disable" {
			if i+1 < len(args) {
				out = append(out, a, args[i+1])
				i++
			}
		} else if strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "--enable=") || strings.HasPrefix(a, "--disable=") || (strings.HasPrefix(a, "-c") && len(a) > 2) {
			out = append(out, a)
		}
	}
	// No disk writes/refresh grants from Codex itself, including initial boot.
	return append(out, "-c", `cli_auth_credentials_store="ephemeral"`)
}

func codexLiveClientArgs(args []string, socket string) []string {
	// The private app-server provides isolation. Shell wrappers add no-daemon
	// for ordinary launches, but Codex rejects it alongside an explicit remote.
	// Put transport flags before -- so prompt text stays literal.
	out := []string{"--remote", "unix://" + socket}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return append(out, args[i:]...)
		}
		if args[i] == "--no-daemon" {
			continue
		}
		out = append(out, args[i])
		if codexShellOptionTakesValue(args[i]) && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

func stopCodexLiveProcess(c *exec.Cmd, done <-chan error) {
	if c == nil || c.Process == nil {
		return
	}
	_ = c.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.Process.Kill()
		<-done
	}
}

func runCodexTerminal() int {
	f := flag.NewFlagSet("codex-terminal", flag.ContinueOnError)
	dir := f.String("data-dir", defaultDataDir(), "SoluTerminal data directory")
	daemonURL := f.String("daemon-url", "http://127.0.0.1:8787", "local credential daemon")
	brokerConfig := f.String("broker-config", "", "generated nonsecret team provider overrides")
	if err := f.Parse(os.Args[2:]); err != nil {
		return 2
	}
	cmd := f.Args()
	paneID := os.Getenv("SOLU_PANE_ID")
	if len(cmd) == 0 || !codexLiveTerminalArgs(cmd) || paneID == "" || filepath.Base(paneID) != paneID {
		fmt.Fprintln(os.Stderr, "[solu] Invalid Codex live-auth launch")
		return 2
	}
	var overrides []string
	if *brokerConfig != "" && json.Unmarshal([]byte(*brokerConfig), &overrides) != nil {
		return 2
	}
	if err := runCodexLiveTerminal(*dir, *daemonURL, paneID, cmd, overrides); err != nil {
		fmt.Fprintln(os.Stderr, "[solu] Codex live authentication:", err)
		return 1
	}
	return 0
}

func runCodexLiveTerminal(dir, daemonURL, paneID string, argv []string, brokerOverrides ...[]string) error {
	team := paneBrokerActive(os.Environ())
	var overrides []string
	if len(brokerOverrides) > 0 {
		overrides = brokerOverrides[0]
	}
	if team != (len(overrides) > 0) {
		return errors.New("team broker launch configuration missing or unexpected")
	}
	if team && (paneBroker.codexLiveConfigValid == nil || !paneBroker.codexLiveConfigValid(overrides)) {
		return errors.New("invalid team broker recovery configuration")
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return errors.New("Codex CLI not found")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	codexDir := os.Getenv("CODEX_HOME")
	if codexDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		codexDir = filepath.Join(home, ".codex")
	}
	authPath := filepath.Join(codexDir, "auth.json")
	var token codexLiveToken
	if !team {
		if err := codexRequireFileStorage(codexDir); err != nil {
			return err
		}
		token, err = readCodexLiveToken(authPath)
		if err != nil {
			return err
		}
	}
	socketDir, err := os.MkdirTemp("", "solu-codex-live-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(socketDir) // exact private directory minted by this process
	if err = codexLivePrivateDirectory(socketDir); err != nil {
		return err
	}
	socket := filepath.Join(socketDir, "app.sock")
	if err = os.MkdirAll(filepath.Join(dir, "codex-live"), 0700); err != nil {
		return err
	}
	statusPath := codexLiveStatusPath(dir, paneID)
	st := codexLiveStatus{PaneID: paneID, PID: os.Getpid(), State: "starting"}
	if team {
		st.Mode = "broker"
	}
	writeStatus := func() {
		st.UpdatedAt = time.Now().UnixMilli()
		b, _ := json.Marshal(st)
		_ = writeFileAtomicSynced(statusPath, b, 0600)
	}
	writeStatus()
	defer func() { st.State = "stopped"; writeStatus() }()
	engineArgs := append(append([]string{}, argv[1:]...), overrides...)
	server := exec.Command(bin, codexLiveServerArgs(engineArgs, socket)...)
	relayEnv := append(os.Environ(), "SOLU_CODEX_RELAY_DIR="+socketDir)
	server.Env = providerChildEnvForBin(bin, relayEnv)
	server.Stdin = nil
	server.Stdout = io.Discard
	server.Stderr = io.Discard // never mix RPC diagnostics into the TUI
	if err = server.Start(); err != nil {
		return errors.New("could not start Codex app-server")
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	defer stopCodexLiveProcess(server, serverDone)
	st.ServerPID = server.Process.Pid
	var rpc *codexLiveRPC
	bootCtx, bootCancel := context.WithTimeout(ctx, 20*time.Second)
	defer bootCancel()
	for rpc == nil {
		rpc, err = newCodexLiveRPC(bootCtx, socket)
		if err == nil {
			break
		}
		select {
		case <-bootCtx.Done():
			return errors.New("Codex app-server did not become ready (CLI 0.154+ required)")
		case e := <-serverDone:
			serverDone <- e
			return errors.New("Codex app-server exited before startup (CLI 0.154+ required)")
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer rpc.conn.Close()
	if _, err = rpc.call(bootCtx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "soluterminal_live_auth", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return err
	}
	if err = rpc.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	if !team {
		err = codexLiveLogin(bootCtx, rpc, token)
	}
	if err != nil {
		return err
	}
	st.State, st.AccountID, st.Revision = "live", token.AccountID, token.Revision
	recovery := newCodexLiveRecovery()
	recovery.inputPath = filepath.Join(dir, "codex-live", paneID+".input")
	if err := writeFileAtomicSynced(recovery.inputPath, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0600); err != nil {
		return errors.New("could not initialize terminal input coordination")
	}
	relaySocket := filepath.Join(socketDir, "terminal.sock")
	closeRelay, err := startCodexLiveRelay(ctx, relaySocket, socket, recovery)
	if err != nil {
		return errors.New("could not start Codex terminal relay")
	}
	defer closeRelay()
	clientArgs := codexLiveClientArgs(engineArgs, relaySocket)
	client := exec.Command(bin, clientArgs...)
	client.Env = providerChildEnvForBin(bin, relayEnv)
	client.Stdin = os.Stdin
	client.Stdout = os.Stdout
	client.Stderr = os.Stderr
	if err = client.Start(); err != nil {
		return errors.New("could not start Codex terminal")
	}
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Wait() }()
	defer stopCodexLiveProcess(client, clientDone)
	st.ClientPID = client.Process.Pid
	writeStatus()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	missingReads := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-clientDone:
			clientDone <- e
			if exitErr := codexLivePendingEngineExit(serverDone); exitErr != nil {
				st.Error = exitErr.Error()
				return exitErr
			}
			return e
		case e := <-serverDone:
			serverDone <- e
			exitErr := codexLiveEngineExit(e)
			st.Error = exitErr.Error()
			return exitErr
		case <-rpc.done:
			exitErr := codexLiveClosedChannel(serverDone)
			st.Error = exitErr.Error()
			return exitErr
		case update := <-rpc.accountUpdates:
			if team {
				continue
			} // broker-only: never adopt a personal login event
			var event struct {
				AuthMode *string `json:"authMode"`
			}
			if json.Unmarshal(update.Params, &event) == nil && event.AuthMode == nil {
				// Respect an explicit TUI logout. Do not immediately sign it back
				// in with the same file; a new host credential revision can reconnect.
				recovery.userInput()
				st.State, st.Error = "signed-out", codexLiveCopy.signedOut
				writeStatus()
			} else if event.AuthMode != nil && *event.AuthMode == "chatgptAuthTokens" && token.AccessToken != "" {
				st.State, st.Error = "live", ""
				writeStatus()
			} else if event.AuthMode != nil {
				st.State, st.Error = "needs-attention", codexLiveCopy.loginChanged
				writeStatus()
			}
		case request := <-rpc.requests:
			if team {
				_ = rpc.write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "Authentication is managed by the team broker"}})
				continue
			}
			if request.Method != "account/chatgptAuthTokens/refresh" {
				_ = rpc.write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "Unsupported host request"}})
				continue
			}
			var previous struct {
				AccountID string `json:"previousAccountId"`
			}
			_ = json.Unmarshal(request.Params, &previous)
			refreshCtx, refreshCancel := context.WithTimeout(ctx, 8*time.Second)
			next, refreshErr := readCodexLiveToken(authPath)
			if refreshErr == nil && ((previous.AccountID != "" && previous.AccountID != token.AccountID) || !codexLiveSameOwner(token, next)) {
				refreshErr = errors.New("the Codex sign-in changed; this turn cannot refresh with a different sign-in")
			}
			if refreshErr == nil && next.Revision == token.Revision {
				refreshErr = codexLiveRefresh(refreshCtx, daemonURL, dir, token.Revision)
				if refreshErr == nil {
					next, refreshErr = readCodexLiveToken(authPath)
				}
			}
			refreshCancel()
			if refreshErr == nil && !codexLiveSameOwner(token, next) {
				refreshErr = errors.New("account changed during refresh")
			}
			if refreshErr != nil {
				_ = rpc.write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": codexLiveCopy.refreshFailedCodex}})
				st.State, st.Error = "needs-attention", codexLiveCopy.refreshFailed
			} else {
				if err = rpc.write(map[string]any{"id": request.ID, "result": next}); err != nil {
					return err
				}
				token = next
				st.State, st.AccountID, st.Revision, st.Error = "live", token.AccountID, token.Revision, ""
			}
			writeStatus()
		case <-ticker.C:
			if team {
				recovery.gate.Lock()
				_, retryAt, _ := recovery.status()
				if retryAt > 0 && time.Now().UnixMilli() >= retryAt && !recovery.busy() {
					checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
					idle := codexLiveThreadsIdle(checkCtx, rpc)
					checkCancel()
					if idle && recovery.recover(time.Now()) != nil {
						recovery.userInput()
						st.Error = "Automatic continuation could not be confirmed; check this conversation"
					}
				}
				recovery.gate.Unlock()
				st.Recovery, st.RetryAt, st.RetryAttempts = recovery.status()
				writeStatus()
				continue
			}
			next, readErr := readCodexLiveToken(authPath)
			if readErr != nil {
				missingReads++
				recovery.userInput()
				if missingReads == 2 {
					logoutCtx, logoutCancel := context.WithTimeout(ctx, 5*time.Second)
					_, logoutErr := rpc.call(logoutCtx, "account/logout", map[string]any{})
					logoutCancel()
					if logoutErr != nil {
						return errors.New("could not clear the unavailable login from Codex")
					}
					token = codexLiveToken{}
				}
				st.State, st.Error = "needs-attention", "Current Codex login unavailable"
				writeStatus()
				continue
			}
			missingReads = 0
			recovery.gate.Lock()
			// The relay blocks new submissions while we inspect the server and
			// install credentials. Existing turns keep their original identity.
			_, retryAt, _ := recovery.status()
			needsSwitch := next.Revision != token.Revision
			needsRecovery := retryAt > 0 && time.Now().UnixMilli() >= retryAt
			if needsSwitch || needsRecovery {
				checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
				idle := !recovery.busy() && codexLiveThreadsIdle(checkCtx, rpc)
				checkCancel()
				if needsSwitch && !idle {
					st.State = "switch-pending"
				} else if needsSwitch {
					// Coalesce selections changed while waiting for the idle check.
					latest, latestErr := readCodexLiveToken(authPath)
					if latestErr != nil {
						recovery.gate.Unlock()
						continue
					}
					next = latest
					loginCtx, loginCancel := context.WithTimeout(ctx, 5*time.Second)
					err = codexLiveLogin(loginCtx, rpc, next)
					loginCancel()
					if err != nil {
						st.State, st.Error = "needs-attention", "Live account handoff failed"
					} else {
						token = next
						st.State, st.AccountID, st.Revision, st.Error = "live", token.AccountID, token.Revision, ""
					}
				}
				if idle && st.State == "live" && token.Revision == next.Revision {
					if recovery.recover(time.Now()) != nil {
						recovery.userInput()
						st.Error = "Automatic continuation could not be confirmed; check this conversation"
					}
				}
			} else if st.State == "switch-pending" {
				st.State, st.Error = "live", "" // selection returned to the adopted account
			}
			recovery.gate.Unlock()
			st.Recovery, st.RetryAt, st.RetryAttempts = recovery.status()
			writeStatus()
		}
	}
}

// Unknown identity cannot qualify for same-owner refresh. Never substitute the
// newly selected account into an old account's in-flight 401 retry.
func codexLiveSameOwner(a, b codexLiveToken) bool {
	return a.AccountID != "" && a.AccountID == b.AccountID && a.Subject != "" && a.Subject == b.Subject
}
