package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"soluterminal-shared/daemonfeatures"
	"time"

	"soluterminal-shared/relaymux"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// NOTE: the old startRelay() retry/backoff loop and the serveRelay() wrapper
// were removed when the relaySupervisor (relay_supervisor.go) became the SOLE
// owner of the relay lifecycle. Reconnect/backoff is now the supervisor's
// responsibility (it owns the dial loop, the token-change debounce, and the
// exactly-one-tunnel invariant). The single live dial entry point is
// serveRelayClassified below.

// liveShellInfo, when set, returns the daemon's CURRENT shell (Tauri installer)
// version and platform, stamped onto every relay handshake as the sv/sp params.
// It's a func (not a captured value) so each handshake reads the LIVE value —
// the updater can refresh the shell version mid-process, and we want the fresh
// one on reconnect. main.go wires it from the apiServer right after the relay
// supervisor is built; it stays nil in unit tests that dial directly, and older
// cloud simply ignores the unknown params (forward-safe, exactly like `v`).
var liveShellInfo func() (shellVersion, shellPlatform string)

// serveRelayClassified runs one relay connection attempt end-to-end and
// classifies its outcome for the supervisor. It returns when the tunnel is
// torn down for any reason (network drop, cloud restart, context cancel), so
// the supervisor can decide whether to redial. The passed handler serves every
// stream the cloud opens — the daemon's normal request mux, so
// browsers-via-cloud hit the same code paths as browsers-via-LAN. Auth for
// those requests is conveyed by the tunnel having been authenticated with the
// machine token; the handler must not apply the local authGate.
//
// Two behaviors the supervisor relies on:
//   - It CLASSIFIES the dial outcome: a 401/403 handshake rejection returns a
//     *relayAuthError (isRelayAuthError true) so the supervisor can stop
//     retrying a permanently-bad token instead of looping forever. Network
//     errors return a plain error and keep the caller's backoff/retry.
//   - onConnected (if non-nil) fires once the tunnel is live, so the
//     supervisor can flip its RelayState to "online". It carries the cloud
//     instance named in the handshake response (relaymux.InstanceHeader), or
//     "" for clouds that don't send it.
func serveRelayClassified(ctx context.Context, relayURL, token, backendID string, handler http.Handler, onConnected func(instance string)) error {
	u, err := url.Parse(relayURL)
	if err != nil {
		return fmt.Errorf("parse relay url: %w", err)
	}
	q := u.Query()
	// Empty backendID ("legacy single-backend") is fine to omit — cloud
	// treats absence as "" anyway. Setting it explicitly makes logs
	// clearer.
	if backendID != "" {
		q.Set("backend", backendID)
	}
	// Stamp the daemon build version on the relay handshake so the cloud can
	// build a fleet-version view (older cloud ignores the unknown param, so this
	// is forward-safe to ship before the cloud reads it).
	if version != "" {
		q.Set("v", version)
	}
	// Advertise this build's named features (daemon_features.go) so the cloud
	// gates on capability, not version. Older clouds ignore the param.
	if f := daemonfeatures.Encode(daemonFeatures); f != "" {
		q.Set("f", f)
	}
	// Stamp the shell/installer version + platform too, so the cloud can show a
	// per-machine INSTALLER version next to the daemon version — the shell and
	// daemon update on independent channels, so they diverge. Read live (see
	// liveShellInfo). Omit either when empty (CLI-only install / older build);
	// older cloud ignores the unknown params.
	if liveShellInfo != nil {
		if sv, sp := liveShellInfo(); sv != "" {
			q.Set("sv", sv)
			if sp != "" {
				q.Set("sp", sp)
			}
		}
	}
	// The remote-access policy and platform, so the cloud can apply the
	// account's strictest policy (device_policy_report.go). Older clouds
	// ignore both.
	if liveDevicePolicy != nil {
		if dp := liveDevicePolicy(); dp != "" {
			q.Set("dp", dp)
			if pl := devicePlatform(); pl != "" {
				q.Set("pl", pl)
			}
		}
	}
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	// Capture the handshake *http.Response (previously discarded as `_`): on a
	// failed upgrade the cloud answers 401 "unknown machine token" / 403, which
	// is a PERMANENT condition the supervisor must not retry. Distinguish it
	// from a network error so only the network case keeps backing off.
	ws, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return &relayAuthError{status: resp.StatusCode}
		}
		return fmt.Errorf("dial: %w", err)
	}

	adapter := relaymux.NewWebSocketAdapter(ws)
	session, err := yamux.Server(adapter, relaymux.YamuxConfig())
	if err != nil {
		_ = ws.Close()
		return fmt.Errorf("yamux: %w", err)
	}

	instance := ""
	if resp != nil {
		instance = resp.Header.Get(relaymux.InstanceHeader)
	}
	if instance != "" {
		log.Printf("relay: connected to %s (instance %s)", relayURL, instance)
	} else {
		log.Printf("relay: connected to %s", relayURL)
	}
	if onConnected != nil {
		onConnected(instance)
	}

	srv := &http.Server{Handler: handler}
	// Close the session when the context is cancelled so http.Serve
	// returns promptly on daemon shutdown. The `done` channel stops
	// this goroutine on normal termination so we don't leak one
	// watcher per reconnect.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-done:
		}
	}()

	err = srv.Serve(&relaymux.Listener{Session: session})
	// Treat the two "normal" close paths as nil so we don't log "EOF"
	// on every reconnect.
	if errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
