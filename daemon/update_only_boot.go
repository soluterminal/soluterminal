package main

// The update-only boot: a daemon started only to install this computer's
// desktop update from Cloud launch mode (desktop/src-tauri/src/
// this_computer_update.rs). The desktop app starts it with SOLU_UPDATE_ONLY=1
// in its environment.
//
// It never runs the normal boot sequence. main hands over to runUpdateOnlyBoot
// right after the durable log is attached, and this file starts only what the
// installer update needs:
//
//   - the updater, with its channel and rollback state, this computer's
//     machine token from claim.json and one machine entitlements check (a
//     public build learns the team update path from it);
//   - the auth gate;
//   - four routes on an allow-list router: GET /api/health,
//     GET /api/update/status, POST /api/update/check and
//     POST /api/update/apply-shell (attended only, with the usual relay,
//     downgrade, hold and flavour checks). Every other request gets the
//     update-only refusal; there is no catch-all handler.
//
// No scheduler, cron, catch-up, release train, pane store or restore,
// agent cleanup, MCP/CLI/hook registration, relay, pairing, check-in,
// background update poll, self-update or memory guard: none of their code is
// reachable from here. updateOnlyBootCalls (update_only_boot_test.go) pins the
// exact set of functions this file may call, so adding one is a reviewed,
// deliberate change.
//
// No successor: spawnSuccessor exits instead when this daemon is update-only.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"soluterminal-shared/releases"
)

// updateOnlyEnv is the marker. It is read from the process's own start
// environment only (captured at package init, before ~/.solu/env loads), so
// it always matches /proc/<pid>/environ, which is how the desktop app and
// start-wsl.sh recognise (and only then stop or replace) such a daemon.
const updateOnlyEnv = "SOLU_UPDATE_ONLY"

var bootUpdateOnlyEnv = os.Getenv(updateOnlyEnv)

func updateOnlyDaemon() bool {
	return strings.TrimSpace(bootUpdateOnlyEnv) == "1"
}

const updateOnlyRefusal = "this daemon was started only to install this computer's desktop update (update-only); nothing else is available"

type updateOnlyConfig struct {
	addr, dataDir, relayURL, backendID, shellVersion, shellPlatform string
}

// parseUpdateOnlyFlags accepts the flags the desktop app and start-wsl.sh
// pass to a daemon; the ones that only matter to the normal boot are
// accepted and ignored, and --claim is refused.
func parseUpdateOnlyFlags(args []string) (updateOnlyConfig, error) {
	fs := flag.NewFlagSet("solud", flag.ContinueOnError)
	cfg := updateOnlyConfig{}
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8787", "bind address")
	fs.StringVar(&cfg.dataDir, "data", defaultDataDir(), "data directory")
	fs.StringVar(&cfg.relayURL, "relay", "", "paired cloud relay URL (its HTTPS base is the update source)")
	fs.StringVar(&cfg.backendID, "backend-id", "", "backend id")
	fs.StringVar(&cfg.shellVersion, "shell-version", "", "desktop app version")
	fs.StringVar(&cfg.shellPlatform, "shell-platform", "", "desktop app platform")
	_ = fs.String("relay-token", "", "ignored in update-only mode")
	_ = fs.String("dev-web", "", "ignored in update-only mode")
	claim := fs.Bool("claim", false, "refused in update-only mode")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if *claim {
		return cfg, errors.New("--claim is not available in update-only mode")
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected argument %q in update-only mode", fs.Arg(0))
	}
	if cfg.shellVersion == "" || cfg.shellPlatform == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			fileVer, filePlat, _ := readShellInfo(home)
			if cfg.shellVersion == "" {
				cfg.shellVersion = fileVer
			}
			if cfg.shellPlatform == "" {
				cfg.shellPlatform = filePlat
			}
		}
	}
	return cfg, nil
}

// runUpdateOnlyBoot is the whole life of an update-only daemon. It returns
// the process exit code.
func runUpdateOnlyBoot(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runUpdateOnlyBootContext(ctx, args, nil)
}

// runUpdateOnlyBootContext runs until ctx ends. ready, when set, receives the
// listening address (tests).
func runUpdateOnlyBootContext(ctx context.Context, args []string, ready chan<- string) int {
	cfg, err := parseUpdateOnlyFlags(args)
	if err != nil {
		log.Printf("update-only: %v", err)
		return 2
	}
	// A daemon already owning the data dir keeps it; this one exits.
	holder := dataDirLockHolder{Addr: cfg.addr, Backend: cfg.backendID, Version: version}
	lock := claimDataDirForBoot(cfg.dataDir, holder)
	dataDirLockHeld = lock
	defer func() {
		if lock != nil {
			lock.Release()
		}
	}()
	loadEnvFile(cfg.dataDir)
	// Nothing this daemon launches (the installer, explorer.exe, the app it
	// relaunches) inherits the marker.
	_ = os.Unsetenv(updateOnlyEnv)

	handler, err := newUpdateOnlyHandler(ctx, cfg)
	if err != nil {
		log.Printf("update-only: %v", err)
		return 1
	}
	ln, err := net.Listen(daemonListenerNetwork(cfg.addr, isInsideWSL()), cfg.addr)
	if err != nil {
		log.Print(describeListenConflict(cfg.addr, err))
		return 1
	}
	log.Printf("soluterminal daemon %s listening on http://%s (update-only, data=%s)", version, ln.Addr(), cfg.dataDir)
	if ready != nil {
		ready <- ln.Addr().String()
	}
	return serveUpdateOnly(ctx, ln, handler)
}

func serveUpdateOnly(ctx context.Context, ln net.Listener, handler http.Handler) int {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("update-only: serve: %v", err)
			return 1
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return 0
}

// newUpdateOnlyHandler builds the updater and the allow-list router behind
// the auth gate.
func newUpdateOnlyHandler(ctx context.Context, cfg updateOnlyConfig) (http.Handler, error) {
	updater := NewUpdater(version, cfg.shellVersion, cfg.shellPlatform, nil, nil)
	updater.dataDir = cfg.dataDir
	updater.machineID = installCohortID(cfg.dataDir)
	rollback, _ := newUpdateRollbackStore(cfg.dataDir)
	updater.rollback = rollback

	// This computer's machine token and paired cloud, for the team update
	// endpoint (plain HTTPS; no relay).
	cloudBase, machineToken := "", ""
	if c, err := loadClaimSafe(cfg.dataDir); err == nil && c != nil && c.Token != "" {
		cloudBase, machineToken = httpsBaseFromRelayURL(cfg.relayURL), c.Token
	}
	entitlements := newTeamEntitlements(cfg.dataDir, func() (string, string) { return cloudBase, machineToken })
	updater.channelStore = newUpdateChannelStore(cfg.dataDir)
	updater.channelEvidence = func() channelEvidence { return entitlementChannelEvidence(entitlements) }
	updater.SetCloudTeamUpdates(entitlements.cloudTeamUpdatesPath)
	if cloudBase != "" {
		updater.SetCloudCreds(cloudBase, machineToken)
		refreshCtx, cancel := context.WithTimeout(ctx, 2*entitlementsHTTPTimeout)
		entitlements.refreshMachine(refreshCtx, cloudBase, machineToken)
		cancel()
	}

	token, err := loadOrCreateToken(cfg.dataDir)
	if err != nil {
		return nil, fmt.Errorf("auth token: %w", err)
	}
	api := &apiServer{
		updater:       updater,
		ver:           version,
		addr:          cfg.addr,
		backendID:     cfg.backendID,
		shellVersion:  cfg.shellVersion,
		shellPlatform: cfg.shellPlatform,
		dataDir:       cfg.dataDir,
	}
	gate := &authGate{token: token}
	api.gate = gate
	return gate.middleware(maxRequestBodyMiddleware(defaultJSONBodyBytes, logMiddleware(updateOnlyRouter(api)))), nil
}

// updateOnlyRouter allows exactly four method+path pairs; everything else,
// any other method on those paths included, is refused.
func updateOnlyRouter(api *apiServer) http.Handler {
	routes := map[string]http.HandlerFunc{
		"GET /api/health":              api.updateOnlyHealth,
		"GET /api/update/status":       api.updateStatus,
		"POST /api/update/check":       api.updateCheck,
		"POST /api/update/apply-shell": api.updateOnlyApplyShell,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		writeUpdateOnlyRefusal(w, http.StatusNotFound)
	})
}

func writeUpdateOnlyRefusal(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": "update_only", "message": updateOnlyRefusal})
}

// updateOnlyHealth is the identity, plus two booleans: updateOnly (so no
// launcher reuses this daemon as a normal one) and updaterReady (an update
// check can run: see updaterHasWhatItNeeds). Never values, paths or claim
// data: health is reachable without a token.
func (a *apiServer) updateOnlyHealth(w http.ResponseWriter, _ *http.Request) {
	body := a.liveHealthIdentity()
	body["updateOnly"] = true
	body["updaterReady"] = updaterHasWhatItNeeds(a.updater)
	writeJSON(w, http.StatusOK, body)
}

// updaterHasWhatItNeeds: the team channel is read through the paired cloud,
// so the updater needs its cloud base and machine token for it; the public
// channel needs neither.
func updaterHasWhatItNeeds(u *Updater) bool {
	if u == nil {
		return false
	}
	if base, token := u.cloudCreds(); base != "" && token != "" {
		return true
	}
	return u.channelSnapshot() != releases.FlavorTeam
}

// updateOnlyApplyShell is the attended installer update only: the silent and
// forced variants are refused here. The handler keeps the relay refusal and
// the updater keeps its downgrade, hold and flavour checks.
func (a *apiServer) updateOnlyApplyShell(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "update_only_attended",
			"message": "an update-only daemon installs only the attended desktop update",
		})
		return
	}
	a.updateApplyShell(w, r)
}
