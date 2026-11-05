package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"soluterminal-shared/machineauth"
)

const (
	previewModeAdopt         = "adopt"
	previewModeStatic        = "static"
	previewModeSpawn         = "spawn"
	previewStateStarting     = "starting"
	previewStateRunning      = "running"
	previewStateIdle         = "idleStopped"
	previewStateExpired      = "expired"
	previewStateError        = "error"
	previewIdleTimeout       = 30 * time.Minute
	previewDefaultExpiry     = 7 * 24 * time.Hour
	previewRevokeRetryWindow = 24 * time.Hour
	previewMaximumTargets    = 3
	previewMaxRunningOwned   = 2
)

type PreviewTarget struct {
	ID                   string             `json:"id"`
	TaskID               string             `json:"taskId,omitempty"`
	BackendID            string             `json:"backendId"`
	Mode                 string             `json:"mode"`
	TargetPort           uint16             `json:"targetPort"`
	State                string             `json:"state"`
	URL                  string             `json:"url"`
	CreatedAt            time.Time          `json:"createdAt"`
	LastActivityAt       time.Time          `json:"lastActivityAt"`
	ExpiresAt            time.Time          `json:"expiresAt"`
	ActiveStreams        int                `json:"-"`
	RecipeArtifactID     string             `json:"recipeArtifactId,omitempty"`
	AcceptedDigest       string             `json:"acceptedDigest,omitempty"`
	BundleDir            string             `json:"bundleDir,omitempty"`
	AppDir               string             `json:"appDir,omitempty"`
	BuildDir             string             `json:"buildDir,omitempty"`
	BuildArgv            []string           `json:"buildArgv,omitempty"`
	ServeArgv            []string           `json:"serveArgv,omitempty"`
	Env                  map[string]string  `json:"env,omitempty"`
	HealthPath           string             `json:"healthPath,omitempty"`
	SpaFallback          bool               `json:"spaFallback,omitempty"`
	PublicBase           string             `json:"publicBase,omitempty"`
	PID                  int                `json:"pid,omitempty"`
	ProcessStartIdentity string             `json:"processStartIdentity,omitempty"`
	FreshAcceptance      *browserAcceptance `json:"-"`
	FreshScreenshot      []byte             `json:"-"`
}

type previewOwnedRuntime struct {
	static *previewStaticServer
	spawn  *previewSpawnProcess
}

type previewStartCall struct {
	done chan struct{}
	err  error
}

var previewStartSemaphore = make(chan struct{}, 1)

func previewAcquireGlobalStart(ctx context.Context) (func(), error) {
	select {
	case previewStartSemaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-previewStartSemaphore }) }, nil
}

type previewHTTPError struct {
	Status int
	Err    error
}

func (e *previewHTTPError) Error() string { return e.Err.Error() }
func previewConflict(msg string) error {
	return &previewHTTPError{Status: http.StatusConflict, Err: errors.New(msg)}
}

type previewManager struct {
	retirement     *retirementAuthorityStore
	mu             sync.Mutex
	createMu       sync.Mutex
	targets        map[string]*PreviewTarget
	pendingRevokes map[string]*PreviewTarget
	active         map[string]map[uint64]context.CancelFunc
	nextLease      uint64
	path           string
	backendID      string
	client         *http.Client
	now            func() time.Time
	cloudCreds     func() (string, string)
	onState        func(PreviewTarget)
	idleAfter      time.Duration
	sweepEvery     time.Duration
	dataDir        string
	cacheRoot      string
	admitHeavy     func(context.Context, string) (func(), error)
	runtimes       map[string]*previewOwnedRuntime
	starting       map[string]*previewStartCall
	runningOwned   int
	startingOwned  int
	// rebuildBasePath is retained as a narrow test indirection for existing
	// lifecycle tests. Production uses rebuildBasePathArgv so the exact
	// engine-authored command that passed acceptance is repeated.
	rebuildBasePath     func(context.Context, string, map[string]string, string) (string, error)
	rebuildBasePathArgv func(context.Context, string, string, []string, map[string]string, string) (string, error)
	probeMounted        func(context.Context, string, string, string, uint16, string, string, string, string) (*browserAcceptance, []byte, error)
}

func previewsLocallyEnabled() bool { return strings.TrimSpace(os.Getenv("SOLU_PREVIEWS")) == "1" }

func validLocalPreviewState(state string) bool {
	return state == previewStateStarting || state == previewStateRunning || state == previewStateIdle || state == previewStateExpired || state == previewStateError
}

func validDaemonPreviewID(id string) bool {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	if len(id) != 29 || !strings.HasPrefix(id, "pv_") {
		return false
	}
	for _, ch := range id[3:] {
		if !strings.ContainsRune(alphabet, ch) {
			return false
		}
	}
	return true
}

func validStablePreviewURL(raw, id string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return false
	}
	return u.Path == "/preview/"+id+"/"
}

func newPreviewManager(dataDir, backendID string, cloudCreds func() (string, string), cacheRoots ...string) (*previewManager, error) {
	retirement := retirementCleanupAuthority.Load()
	if retirement == nil || retirement.root != filepath.Join(dataDir, "workflow", "retirement") {
		retirement, _ = loadRetirementAuthority(dataDir)
	}
	return newPreviewManagerWithRetirement(dataDir, backendID, cloudCreds, retirement, cacheRoots...)
}

func newPreviewManagerWithRetirement(dataDir, backendID string, cloudCreds func() (string, string), retirement *retirementAuthorityStore, cacheRoots ...string) (*previewManager, error) {
	cacheRoot := filepath.Join(dataDir, "preview-cache")
	if len(cacheRoots) > 0 && strings.TrimSpace(cacheRoots[0]) != "" {
		cacheRoot = cacheRoots[0]
	}
	m := &previewManager{
		retirement: retirement,
		targets:    map[string]*PreviewTarget{}, pendingRevokes: map[string]*PreviewTarget{}, active: map[string]map[uint64]context.CancelFunc{},
		path: filepath.Join(dataDir, "previews.json"), backendID: backendID,
		client: &http.Client{Timeout: 15 * time.Second}, now: time.Now,
		cloudCreds: cloudCreds, idleAfter: previewIdleTimeout, sweepEvery: time.Minute,
		dataDir: dataDir, cacheRoot: cacheRoot, runtimes: map[string]*previewOwnedRuntime{},
		starting:            map[string]*previewStartCall{},
		rebuildBasePathArgv: previewRebuildForBasePath, probeMounted: previewProbeMountedForPublicBase,
	}
	if retirement != nil && retirement.loadErr != nil {
		return m, retirement.loadErr
	}
	if err := m.load(); err != nil {
		return m, err
	}
	return m, nil
}

func (m *previewManager) load() error {
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read previews registry: %w", err)
	}
	var entries []*PreviewTarget
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("parse previews registry: %w", err)
	}
	now := m.now()
	dirty := false
	queueRevoke := func(target *PreviewTarget) {
		dirty = true
		if target == nil || !validDaemonPreviewID(target.ID) {
			return
		}
		cp := *target
		cp.State = previewStateExpired
		cp.ActiveStreams = 0
		if cp.ExpiresAt.After(now) {
			cp.ExpiresAt = now
		}
		if now.Sub(cp.ExpiresAt) > previewRevokeRetryWindow {
			return
		}
		m.pendingRevokes[cp.ID] = &cp
	}
	for _, target := range entries {
		if target == nil || !validDaemonPreviewID(target.ID) {
			dirty = true
			continue
		}
		if !validStablePreviewURL(target.URL, target.ID) || !validLocalPreviewState(target.State) || target.State == previewStateExpired || !target.ExpiresAt.After(now) {
			queueRevoke(target)
			continue
		}
		target.ActiveStreams = 0
		switch target.Mode {
		case previewModeAdopt:
			if target.TargetPort == 0 {
				queueRevoke(target)
				continue
			}
		case previewModeStatic:
			if !previewDigestRE.MatchString(target.AcceptedDigest) || target.BundleDir == "" || !filepath.IsAbs(target.BundleDir) || !previewPathWithin(m.cacheRoot, target.BundleDir) {
				queueRevoke(target)
				continue
			}
			if previewVerifyBundle(target.BundleDir, target.AcceptedDigest) != nil {
				target.State = previewStateError
			} else {
				target.State = previewStateIdle
			}
			target.TargetPort = 0
			target.PID = 0
			target.ProcessStartIdentity = ""
			dirty = true
		case previewModeSpawn:
			// AppDir was added after the first slice-2 checkpoint. An old record
			// used BuildDir as both cwd and digest root; preserve replay without
			// weakening newly created output-scoped targets.
			if target.AppDir == "" {
				target.AppDir = target.BuildDir
				dirty = true
			}
			if validatePreviewArgv("serveArgv", target.ServeArgv, true) != nil || validatePreviewEnv(target.Env) != nil || validatePreviewHealthPath(target.HealthPath) != nil || target.AppDir == "" || !filepath.IsAbs(target.AppDir) || target.BuildDir == "" || !filepath.IsAbs(target.BuildDir) || !previewDigestRE.MatchString(target.AcceptedDigest) {
				queueRevoke(target)
				continue
			}
			if target.PID > 0 && processAlive(target.PID) {
				liveIdentity := previewProcessStartIdentity(target.PID)
				if liveIdentity != "" && target.ProcessStartIdentity != "" && liveIdentity == target.ProcessStartIdentity {
					proc, findErr := os.FindProcess(target.PID)
					if findErr == nil && target.TargetPort != 0 {
						target.State = previewStateRunning
						m.runtimes[target.ID] = &previewOwnedRuntime{spawn: &previewSpawnProcess{Process: proc, LogPath: filepath.Join(m.dataDir, "preview-logs", target.ID+".log")}}
						m.runningOwned++
						break
					}
				}
				target.State = previewStateError
				dirty = true
			} else {
				target.State = previewStateIdle
				target.TargetPort = 0
				target.PID = 0
				target.ProcessStartIdentity = ""
				dirty = true
			}
		default:
			queueRevoke(target)
			continue
		}
		m.targets[target.ID] = target
	}
	if dirty {
		return m.saveLocked()
	}
	return nil
}

func (m *previewManager) saveLocked() error {
	entries := make([]*PreviewTarget, 0, len(m.targets)+len(m.pendingRevokes))
	for _, target := range m.targets {
		cp := *target
		cp.ActiveStreams = 0
		entries = append(entries, &cp)
	}
	for _, target := range m.pendingRevokes {
		cp := *target
		cp.ActiveStreams = 0
		entries = append(entries, &cp)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt.Before(entries[j].CreatedAt) })
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomicSynced(m.path, b, 0o600)
}

func validateAdoptPort(ctx context.Context, port uint16) error {
	if port == 0 {
		return errors.New("an explicit port is required")
	}
	host := fmt.Sprintf("127.0.0.1:%d", port)
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("port %d is not listening on loopback", port)
	}
	_ = conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/", nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		Transport:     &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("port %d is not an HTTP service", port)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

type previewCloudRegistration struct {
	PreviewID string    `json:"previewId"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (m *previewManager) cloudRequest(ctx context.Context, method, route string, body any, out any) error {
	if m.cloudCreds == nil {
		return errors.New("cloud preview service is unavailable")
	}
	base, token := m.cloudCreds()
	if strings.TrimSpace(base) == "" || strings.TrimSpace(token) == "" {
		return errors.New("pair this machine with the cloud before creating a preview")
	}
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+route, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	machineauth.SignRequest(req, token, raw, time.Now())
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloud preview request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		// A fleet-disabled cloud returns the same 404 as a missing route. For
		// DELETE that is already the desired security state, so local teardown
		// may continue and "stop" cannot be wedged by the kill switch.
		if method == http.MethodDelete && resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode == http.StatusNotFound {
			return errors.New("previews are not enabled by the cloud")
		}
		return fmt.Errorf("cloud preview request failed (%d)", resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(out); err != nil {
			return fmt.Errorf("decode cloud preview response: %w", err)
		}
	}
	return nil
}

func (m *previewManager) Create(ctx context.Context, port uint16, taskID string, ttlHours *int) (*PreviewTarget, error) {
	if !previewsLocallyEnabled() {
		return nil, &previewHTTPError{Status: http.StatusNotFound, Err: errors.New("not found")}
	}
	m.createMu.Lock()
	defer m.createMu.Unlock()
	if err := validateAdoptPort(ctx, port); err != nil {
		return nil, err
	}
	now := m.now().UTC()
	m.mu.Lock()
	count := 0
	taskAlreadyPreviewed := false
	for _, target := range m.targets {
		if target.ExpiresAt.After(now) && target.State != previewStateExpired {
			count++
			if taskID != "" && target.TaskID == taskID {
				taskAlreadyPreviewed = true
			}
		}
	}
	m.mu.Unlock()
	if taskAlreadyPreviewed {
		return nil, previewConflict("task already has a preview")
	}
	if count >= previewMaximumTargets {
		return nil, previewConflict("maximum of three concurrent previews reached")
	}
	body := map[string]any{"backendId": m.backendID, "targetHint": "manual", "state": "active"}
	if taskID != "" {
		body["taskId"] = taskID
	}
	if ttlHours != nil {
		if *ttlHours <= 0 || *ttlHours > 168 {
			return nil, errors.New("ttlHours must be between 1 and 168")
		}
		body["ttlHours"] = *ttlHours
	}
	var registration previewCloudRegistration
	if err := m.cloudRequest(ctx, http.MethodPost, "/api/internal/previews", body, &registration); err != nil {
		return nil, err
	}
	if !validDaemonPreviewID(registration.PreviewID) || !validStablePreviewURL(registration.URL, registration.PreviewID) || !registration.ExpiresAt.After(now) {
		if validDaemonPreviewID(registration.PreviewID) {
			_ = m.cloudRequest(context.Background(), http.MethodDelete, "/api/internal/previews/"+registration.PreviewID, nil, nil)
		}
		return nil, errors.New("cloud returned an invalid preview registration")
	}
	target := &PreviewTarget{
		ID: registration.PreviewID, TaskID: taskID, BackendID: m.backendID, Mode: previewModeAdopt,
		TargetPort: port, State: previewStateRunning, URL: registration.URL,
		CreatedAt: now, LastActivityAt: now, ExpiresAt: registration.ExpiresAt,
	}
	m.mu.Lock()
	m.targets[target.ID] = target
	if err := m.saveLocked(); err != nil {
		delete(m.targets, target.ID)
		m.mu.Unlock()
		_ = m.cloudRequest(context.Background(), http.MethodDelete, "/api/internal/previews/"+target.ID, nil, nil)
		return nil, fmt.Errorf("save preview registry: %w", err)
	}
	cp := *target
	m.mu.Unlock()
	return &cp, nil
}

func (m *previewManager) cloudAvailable() bool {
	if m == nil || m.cloudCreds == nil {
		return false
	}
	base, token := m.cloudCreds()
	return strings.TrimSpace(base) != "" && strings.TrimSpace(token) != ""
}

func (m *previewManager) hasCapacity(taskID string) (bool, string) {
	if m == nil {
		return false, "preview manager unavailable"
	}
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, target := range m.targets {
		if target.ExpiresAt.After(now) && target.State != previewStateExpired {
			count++
			if taskID != "" && target.TaskID == taskID {
				return false, "task already has a preview"
			}
		}
	}
	if count >= previewMaximumTargets {
		return false, "maximum of three concurrent previews reached"
	}
	return true, ""
}

func (m *previewManager) hasOwnedRunCapacity() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningOwned+m.startingOwned < previewMaxRunningOwned
}

func (m *previewManager) reserveOwnedStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runningOwned+m.startingOwned >= previewMaxRunningOwned {
		return previewConflict("maximum of two daemon-owned previews are already running")
	}
	m.startingOwned++
	return nil
}

func (m *previewManager) releaseOwnedStart(started bool) {
	m.mu.Lock()
	if m.startingOwned > 0 {
		m.startingOwned--
	}
	if started {
		m.runningOwned++
	}
	m.mu.Unlock()
}

func (m *previewManager) releaseRunningOwned() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.runningOwned > 0 {
		m.runningOwned--
	}
	m.mu.Unlock()
}

func (m *previewManager) acquireStartAdmission(ctx context.Context) (func(), error) {
	if err := m.reserveOwnedStart(); err != nil {
		return nil, err
	}
	releaseHeavy := func() {}
	if m.admitHeavy != nil {
		var err error
		releaseHeavy, err = m.admitHeavy(ctx, "preview")
		if err != nil {
			m.releaseOwnedStart(false)
			return nil, err
		}
	}
	// The workflow acceptance path already holds the same heavy slot before it
	// reaches the package-wide start semaphore, so manager starts take locks in
	// that order too. A global->heavy order here would deadlock against an
	// acceptance run holding heavy->global.
	releaseGlobal, err := previewAcquireGlobalStart(ctx)
	if err != nil {
		releaseHeavy()
		m.releaseOwnedStart(false)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseGlobal()
			releaseHeavy()
		})
	}, nil
}

func previewRecipeBuildDir(recipe previewRecipe) string {
	if recipe.BuildDir != "" {
		return recipe.BuildDir
	}
	// Replay compatibility for recipes emitted by the pre-repair checkpoint.
	return recipe.AppDir
}

func (m *previewManager) verifyRecipeOutput(worktree string, recipe previewRecipe) (bundleDir, appDir, buildDir string, err error) {
	if err = validatePreviewRecipe(recipe); err != nil {
		return "", "", "", err
	}
	switch recipe.Kind {
	case previewModeStatic:
		bundleDir = filepath.Join(m.cacheRoot, strings.TrimPrefix(recipe.AcceptedDigest, "sha256:"))
		if !previewPathWithin(m.cacheRoot, bundleDir) {
			return "", "", "", errors.New("sealed preview bundle path is outside the preview cache")
		}
		if err = previewVerifyBundle(bundleDir, recipe.AcceptedDigest); err != nil {
			return "", "", "", err
		}
	case previewModeSpawn:
		appDir, err = previewResolveBeneath(worktree, filepath.Join(worktree, filepath.FromSlash(recipe.AppDir)))
		if err != nil {
			return "", "", "", errors.New("preview app directory escapes the task worktree")
		}
		buildDir, err = previewResolveBeneath(worktree, filepath.Join(worktree, filepath.FromSlash(previewRecipeBuildDir(recipe))))
		if err != nil {
			return "", "", "", errors.New("preview build directory escapes the task worktree")
		}
		actual, digestErr := previewBuildDigest(buildDir)
		if digestErr != nil {
			return "", "", "", digestErr
		}
		if subtle.ConstantTimeCompare([]byte(actual), []byte(recipe.AcceptedDigest)) != 1 {
			return "", "", "", fmt.Errorf("accepted preview digest mismatch: accepted %s, found %s", recipe.AcceptedDigest, actual)
		}
	}
	return bundleDir, appDir, buildDir, nil
}

func (m *previewManager) startOwnedAdmitted(ctx context.Context, target PreviewTarget) (*previewOwnedRuntime, uint16, int, string, error) {
	switch target.Mode {
	case previewModeStatic:
		if err := previewVerifyBundle(target.BundleDir, target.AcceptedDigest); err != nil {
			return nil, 0, 0, "", err
		}
		server, err := newPreviewStaticServer(target.BundleDir, target.SpaFallback, target.AcceptedDigest)
		if err != nil {
			return nil, 0, 0, "", err
		}
		_ = previewTouchBundle(target.BundleDir)
		return &previewOwnedRuntime{static: server}, server.Port(), 0, "", nil
	case previewModeSpawn:
		actual, err := previewBuildDigest(target.BuildDir)
		if err != nil {
			return nil, 0, 0, "", err
		}
		if subtle.ConstantTimeCompare([]byte(actual), []byte(target.AcceptedDigest)) != 1 {
			return nil, 0, 0, "", fmt.Errorf("accepted preview digest mismatch: accepted %s, found %s", target.AcceptedDigest, actual)
		}
		port, err := previewAllocatePort()
		if err != nil {
			return nil, 0, 0, "", err
		}
		basePath := ""
		if target.PublicBase == "basePath" {
			basePath = "/preview/" + target.ID
		}
		appDir := target.AppDir
		if appDir == "" {
			appDir = target.BuildDir
		}
		proc, err := previewSpawnWithOptions(ctx, appDir, target.ServeArgv, target.Env, port, filepath.Join(m.dataDir, "preview-logs"), basePath, target.HealthPath)
		if err != nil {
			return nil, 0, 0, "", err
		}
		postStartDigest, digestErr := previewBuildDigest(target.BuildDir)
		if digestErr != nil || subtle.ConstantTimeCompare([]byte(postStartDigest), []byte(target.AcceptedDigest)) != 1 {
			_ = previewStopTree(proc)
			if digestErr != nil {
				return nil, 0, 0, "", digestErr
			}
			return nil, 0, 0, "", fmt.Errorf("preview process mutated accepted output during startup: accepted %s, found %s", target.AcceptedDigest, postStartDigest)
		}
		identity := previewProcessStartIdentity(proc.Process.Pid)
		if identity == "" {
			_ = previewStopTree(proc)
			return nil, 0, 0, "", errors.New("preview process start identity is unavailable; refusing an unkillable owned process")
		}
		return &previewOwnedRuntime{spawn: proc}, port, proc.Process.Pid, identity, nil
	default:
		return nil, 0, 0, "", errors.New("preview target is not daemon-owned")
	}
}

func (m *previewManager) startOwned(ctx context.Context, target PreviewTarget) (*previewOwnedRuntime, uint16, int, string, error) {
	release, err := m.acquireStartAdmission(ctx)
	if err != nil {
		return nil, 0, 0, "", err
	}
	started := false
	defer func() {
		release()
		m.releaseOwnedStart(started)
	}()
	runtime, port, pid, identity, err := m.startOwnedAdmitted(ctx, target)
	if err == nil {
		started = true
	}
	return runtime, port, pid, identity, err
}

func (m *previewManager) CreateFromRecipe(ctx context.Context, taskID, worktree string, recipe previewRecipe) (*PreviewTarget, error) {
	if !previewsLocallyEnabled() {
		return nil, &previewHTTPError{Status: http.StatusNotFound, Err: errors.New("not found")}
	}
	m.createMu.Lock()
	defer m.createMu.Unlock()
	if ok, reason := m.hasCapacity(taskID); !ok {
		return nil, previewConflict(reason)
	}
	bundleDir, appDir, buildDir, err := m.verifyRecipeOutput(worktree, recipe)
	if err != nil {
		return nil, err
	}
	releaseBundlePin := func() {}
	if bundleDir != "" {
		releaseBundlePin = previewPinBundle(bundleDir)
	}
	defer releaseBundlePin()
	now := m.now().UTC()
	cloudState := "active"
	if recipe.PublicBase == "basePath" {
		cloudState = "dormant"
	}
	body := map[string]any{"backendId": m.backendID, "taskId": taskID, "targetHint": recipe.Kind, "state": cloudState}
	var registration previewCloudRegistration
	if err := m.cloudRequest(ctx, http.MethodPost, "/api/internal/previews", body, &registration); err != nil {
		return nil, err
	}
	rollback := true
	defer func() {
		if rollback && validDaemonPreviewID(registration.PreviewID) {
			_ = m.cloudRequest(context.Background(), http.MethodDelete, "/api/internal/previews/"+registration.PreviewID, nil, nil)
		}
	}()
	if !validDaemonPreviewID(registration.PreviewID) || !validStablePreviewURL(registration.URL, registration.PreviewID) || !registration.ExpiresAt.After(now) {
		return nil, errors.New("cloud returned an invalid preview registration")
	}
	target := PreviewTarget{
		ID: registration.PreviewID, TaskID: taskID, BackendID: m.backendID, Mode: recipe.Kind,
		State: previewStateStarting, URL: registration.URL, CreatedAt: now, LastActivityAt: now,
		ExpiresAt: registration.ExpiresAt, AcceptedDigest: recipe.AcceptedDigest,
		BundleDir: bundleDir, AppDir: appDir, BuildDir: buildDir,
		BuildArgv: append([]string(nil), recipe.BuildArgv...), ServeArgv: append([]string(nil), recipe.ServeArgv...),
		Env: clonePreviewEnv(recipe.Env), HealthPath: recipe.HealthPath, SpaFallback: recipe.SpaFallback,
		PublicBase: recipe.PublicBase,
	}
	var runtime *previewOwnedRuntime
	var port uint16
	var pid int
	var identity string
	if recipe.PublicBase == "basePath" {
		// The ID-specific production rebuild and its start are one admitted
		// operation. This keeps builds behind the same heavy floor/slot and the
		// package-wide single-start semaphore as wakes and ordinary starts.
		release, admissionErr := m.acquireStartAdmission(ctx)
		if admissionErr != nil {
			return nil, admissionErr
		}
		started := false
		func() {
			defer func() {
				release()
				m.releaseOwnedStart(started)
			}()
			var newDigest string
			if m.rebuildBasePath != nil {
				newDigest, err = m.rebuildBasePath(ctx, target.BuildDir, target.Env, "/preview/"+target.ID)
			} else if len(target.BuildArgv) == 0 {
				err = errors.New("preview recipe does not retain the accepted basePath build command")
			} else {
				newDigest, err = m.rebuildBasePathArgv(ctx, target.AppDir, target.BuildDir, target.BuildArgv, target.Env, "/preview/"+target.ID)
			}
			if err != nil {
				return
			}
			target.AcceptedDigest = newDigest
			runtime, port, pid, identity, err = m.startOwnedAdmitted(ctx, target)
			started = err == nil
		}()
	} else {
		runtime, port, pid, identity, err = m.startOwned(ctx, target)
	}
	if err != nil {
		return nil, err
	}
	cleanupRuntime := true
	defer func() {
		if cleanupRuntime {
			_ = m.stopOwnedRuntime(target, runtime)
			m.releaseRunningOwned()
		}
	}()
	target.TargetPort, target.PID, target.ProcessStartIdentity = port, pid, identity
	if recipe.PublicBase == "basePath" {
		accepted, screenshot, acceptErr := m.probeMounted(ctx, worktree, recipe.AppDir, target.ID, target.TargetPort, target.HealthPath, target.AcceptedDigest, recipe.SourceCommit, recipe.PublicBase)
		if acceptErr != nil || accepted == nil || !accepted.Pass {
			if acceptErr == nil {
				acceptErr = errors.New("mounted-path browser acceptance failed after the basePath rebuild")
			}
			return nil, acceptErr
		}
		postAcceptDigest, digestErr := previewBuildDigest(target.BuildDir)
		if digestErr != nil || subtle.ConstantTimeCompare([]byte(postAcceptDigest), []byte(target.AcceptedDigest)) != 1 {
			if digestErr != nil {
				return nil, digestErr
			}
			return nil, fmt.Errorf("preview output changed during mounted-path acceptance: accepted %s, found %s", target.AcceptedDigest, postAcceptDigest)
		}
		target.FreshAcceptance = accepted
		target.FreshScreenshot = append([]byte(nil), screenshot...)
		if err := m.cloudRequest(ctx, http.MethodPatch, "/api/internal/previews/"+target.ID, map[string]string{"state": "active"}, nil); err != nil {
			return nil, err
		}
	}
	target.State = previewStateRunning
	m.mu.Lock()
	m.targets[target.ID] = &target
	m.runtimes[target.ID] = runtime
	if err := m.saveLocked(); err != nil {
		delete(m.targets, target.ID)
		delete(m.runtimes, target.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("save preview registry: %w", err)
	}
	cp := target
	m.mu.Unlock()
	rollback = false
	cleanupRuntime = false
	return &cp, nil
}

func clonePreviewEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func (m *previewManager) SetRecipeArtifactID(id, artifactID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target := m.targets[id]
	if target == nil {
		return errors.New("unknown preview")
	}
	target.RecipeArtifactID = artifactID
	return m.saveLocked()
}

func (m *previewManager) List() []PreviewTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PreviewTarget, 0, len(m.targets))
	for _, target := range m.targets {
		out = append(out, *target)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *previewManager) Get(id string) (PreviewTarget, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target := m.targets[id]
	if target == nil {
		return PreviewTarget{}, false
	}
	return *target, true
}

func (m *previewManager) detachOwnedLocked(id string) *previewOwnedRuntime {
	runtime := m.runtimes[id]
	if runtime != nil {
		delete(m.runtimes, id)
		if m.runningOwned > 0 {
			m.runningOwned--
		}
	}
	return runtime
}

func (m *previewManager) stopOwnedRuntime(target PreviewTarget, runtime *previewOwnedRuntime) error {
	if runtime == nil {
		return nil
	}
	if runtime.static != nil {
		return runtime.static.Close()
	}
	if runtime.spawn == nil || runtime.spawn.Process == nil {
		return nil
	}
	pid := runtime.spawn.Process.Pid
	if !processAlive(pid) {
		return nil
	}
	liveIdentity := previewProcessStartIdentity(pid)
	if target.PID != pid || target.ProcessStartIdentity == "" || liveIdentity == "" || liveIdentity != target.ProcessStartIdentity {
		return fmt.Errorf("preview process identity mismatch for pid %d; process was not killed", pid)
	}
	return previewStopTree(runtime.spawn)
}

func (m *previewManager) triggerWake(id string) (*previewStartCall, error) {
	m.mu.Lock()
	if call := m.starting[id]; call != nil {
		m.mu.Unlock()
		return call, nil
	}
	target := m.targets[id]
	if target == nil || (target.Mode != previewModeStatic && target.Mode != previewModeSpawn) {
		m.mu.Unlock()
		return nil, errors.New("preview cannot be relaunched")
	}
	if target.State == previewStateExpired || !target.ExpiresAt.After(m.now()) {
		m.mu.Unlock()
		return nil, errors.New("preview has expired")
	}
	if target.State == previewStateRunning {
		call := &previewStartCall{done: make(chan struct{})}
		close(call.done)
		m.mu.Unlock()
		return call, nil
	}
	call := &previewStartCall{done: make(chan struct{})}
	m.starting[id] = call
	previousState := target.State
	target.State = previewStateStarting
	cp := *target
	saveErr := m.saveLocked()
	m.mu.Unlock()
	if saveErr != nil {
		m.mu.Lock()
		delete(m.starting, id)
		if current := m.targets[id]; current != nil && current.State == previewStateStarting {
			current.State = previousState
		}
		call.err = saveErr
		close(call.done)
		m.mu.Unlock()
		return call, saveErr
	}
	if m.onState != nil {
		m.onState(cp)
	}
	go m.finishWake(id, call, cp)
	return call, nil
}

func previewWakeCleanupTarget(target PreviewTarget, port uint16, pid int, identity string) PreviewTarget {
	target.TargetPort = port
	target.PID = pid
	target.ProcessStartIdentity = identity
	return target
}

func (m *previewManager) finishWake(id string, call *previewStartCall, target PreviewTarget) {
	ctx, cancel := context.WithTimeout(context.Background(), previewHealthGateTimeout+time.Minute)
	defer cancel()
	runtime, port, pid, identity, err := m.startOwned(ctx, target)
	var cleanupRuntime *previewOwnedRuntime
	var cleanupTarget PreviewTarget
	m.mu.Lock()
	current := m.targets[id]
	if err == nil && current != nil && current.State == previewStateStarting {
		current.TargetPort = port
		current.PID = pid
		current.ProcessStartIdentity = identity
		current.State = previewStateRunning
		current.LastActivityAt = m.now().UTC()
		m.runtimes[id] = runtime
		cp := *current
		err = m.saveLocked()
		if err == nil && m.onState != nil {
			defer m.onState(cp)
		} else if err != nil {
			cleanupRuntime = m.detachOwnedLocked(id)
			cleanupTarget = *current
			current.TargetPort = 0
			current.PID = 0
			current.ProcessStartIdentity = ""
		}
	} else if err == nil {
		// The preview was stopped while its wake was in flight. startOwned has
		// charged one running-owned slot; return it and tear down only this
		// identity-verified child/listener.
		if m.runningOwned > 0 {
			m.runningOwned--
		}
		cleanupTarget := previewWakeCleanupTarget(target, port, pid, identity)
		go func() { _ = m.stopOwnedRuntime(cleanupTarget, runtime) }()
		err = errors.New("preview was stopped while waking")
	}
	if err != nil && current != nil {
		current.State = previewStateError
		cp := *current
		_ = m.saveLocked()
		if m.onState != nil {
			defer m.onState(cp)
		}
	}
	call.err = err
	delete(m.starting, id)
	close(call.done)
	m.mu.Unlock()
	if cleanupRuntime != nil {
		_ = m.stopOwnedRuntime(cleanupTarget, cleanupRuntime)
	}
}

func (m *previewManager) Wake(id string) error {
	_, err := m.triggerWake(id)
	return err
}

func (m *previewManager) Relaunch(ctx context.Context, id string) (*PreviewTarget, error) {
	call, err := m.triggerWake(id)
	if err != nil {
		return nil, err
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if call.err != nil {
		return nil, call.err
	}
	target, ok := m.Get(id)
	if !ok || target.State != previewStateRunning {
		return nil, errors.New("preview did not relaunch")
	}
	return &target, nil
}

func (m *previewManager) markOwnedError(id string) {
	m.mu.Lock()
	target := m.targets[id]
	if target == nil || (target.Mode != previewModeStatic && target.Mode != previewModeSpawn) {
		m.mu.Unlock()
		return
	}
	runtime := m.detachOwnedLocked(id)
	cp := *target
	target.State = previewStateError
	target.TargetPort = 0
	_ = m.saveLocked()
	stateCopy := *target
	m.mu.Unlock()
	_ = m.stopOwnedRuntime(cp, runtime)
	if m.onState != nil {
		m.onState(stateCopy)
	}
}

func (m *previewManager) stop(ctx context.Context, id string, stateEvent bool, hardExpired bool) error {
	m.mu.Lock()
	target := m.targets[id]
	if target == nil {
		m.mu.Unlock()
		return nil
	}
	cp := *target
	m.mu.Unlock()
	// Stop invariant: make the internet route unusable before touching the
	// loopback lease or durable registry.
	revokeErr := m.cloudRequest(ctx, http.MethodDelete, "/api/internal/previews/"+id, nil, nil)
	if revokeErr != nil {
		if !hardExpired {
			return revokeErr
		}
		// Hard expiry still closes local streams immediately, but retain an
		// unroutable tombstone so the sweeper retries the cloud revoke instead
		// of forgetting an orphaned row after a transient cloud failure.
		m.mu.Lock()
		current := m.targets[id]
		notifyState := false
		var runtime *previewOwnedRuntime
		if current != nil {
			for _, cancel := range m.active[id] {
				cancel()
			}
			delete(m.active, id)
			current.ActiveStreams = 0
			notifyState = stateEvent && current.State != previewStateExpired
			current.State = previewStateExpired
			cp = *current
			runtime = m.detachOwnedLocked(id)
		}
		saveErr := m.saveLocked()
		m.mu.Unlock()
		_ = m.stopOwnedRuntime(cp, runtime)
		if notifyState && m.onState != nil {
			m.onState(cp)
		}
		if saveErr != nil {
			return fmt.Errorf("persist hard-expiry revoke tombstone: %v (cloud revoke: %w)", saveErr, revokeErr)
		}
		return revokeErr
	}
	m.mu.Lock()
	for _, cancel := range m.active[id] {
		cancel()
	}
	delete(m.active, id)
	runtime := m.detachOwnedLocked(id)
	delete(m.targets, id)
	err := m.saveLocked()
	m.mu.Unlock()
	_ = m.stopOwnedRuntime(cp, runtime)
	if cp.BundleDir != "" {
		_ = previewCacheSweepToLimitContext(retirementWithAuthority(context.Background(), m.retirement), m.cacheRoot, m.pinnedBundles(), previewCacheMaxBytes)
	}
	if stateEvent && cp.State != previewStateExpired && m.onState != nil {
		cp.State = previewStateExpired
		m.onState(cp)
	}
	return err
}

func (m *previewManager) Stop(ctx context.Context, id string) error {
	return m.stop(ctx, id, false, false)
}

func (m *previewManager) drainPendingRevokes(ctx context.Context) (revoked int) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.pendingRevokes))
	for id := range m.pendingRevokes {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := m.cloudRequest(ctx, http.MethodDelete, "/api/internal/previews/"+id, nil, nil); err != nil {
			continue
		}
		m.mu.Lock()
		delete(m.pendingRevokes, id)
		_ = m.saveLocked()
		m.mu.Unlock()
		revoked++
	}
	return revoked
}

func (m *previewManager) expireAtBoot(ctx context.Context, id string) error {
	m.mu.Lock()
	target := m.targets[id]
	if target == nil {
		m.mu.Unlock()
		return nil
	}
	now := m.now().UTC()
	if target.ExpiresAt.After(now) {
		target.ExpiresAt = now
	}
	_ = m.saveLocked()
	m.mu.Unlock()
	return m.stop(ctx, id, true, true)
}

type previewBootReconcile struct {
	Kept                int
	ExpiredAdopt        int
	ExpiredUnverifiable int
	Revoked             int
	PendingRevoke       int
}

func (m *previewManager) reconcileBoot(ctx context.Context) previewBootReconcile {
	m.mu.Lock()
	targets := make([]PreviewTarget, 0, len(m.targets))
	for _, target := range m.targets {
		targets = append(targets, *target)
	}
	m.mu.Unlock()

	var summary previewBootReconcile
	for _, target := range targets {
		switch target.Mode {
		case previewModeAdopt:
			summary.ExpiredAdopt++
			_ = m.expireAtBoot(ctx, target.ID)
			continue
		case previewModeStatic:
			if target.State == previewStateError {
				summary.ExpiredUnverifiable++
				_ = m.expireAtBoot(ctx, target.ID)
				continue
			}
		case previewModeSpawn:
			appInfo, appErr := os.Stat(target.AppDir)
			buildInfo, buildErr := os.Stat(target.BuildDir)
			if appErr != nil || !appInfo.IsDir() || buildErr != nil || !buildInfo.IsDir() {
				summary.ExpiredUnverifiable++
				_ = m.expireAtBoot(ctx, target.ID)
				continue
			}
		}
		summary.Kept++
		if target.TaskID != "" && m.onState != nil {
			m.onState(target)
		}
	}

	m.mu.Lock()
	pending := make([]PreviewTarget, 0, len(m.pendingRevokes))
	for _, target := range m.pendingRevokes {
		pending = append(pending, *target)
	}
	m.mu.Unlock()
	for _, target := range pending {
		if target.TaskID != "" && m.onState != nil {
			m.onState(target)
		}
	}
	summary.Revoked = m.drainPendingRevokes(ctx)
	m.mu.Lock()
	summary.PendingRevoke = len(m.pendingRevokes)
	m.mu.Unlock()
	return summary
}

func (m *previewManager) MintLink(ctx context.Context, id string, rotate bool, ttlHours *int) (string, error) {
	m.mu.Lock()
	target := m.targets[id]
	if target == nil || target.State == previewStateExpired {
		m.mu.Unlock()
		return "", errors.New("unknown preview")
	}
	m.mu.Unlock()
	body := map[string]any{}
	if ttlHours != nil {
		body["ttlHours"] = *ttlHours
	}
	route := "/api/internal/previews/" + id + "/link"
	if rotate {
		route = "/api/internal/previews/" + id + "/rotate-link"
	}
	var response struct {
		URL string `json:"url"`
	}
	if err := m.cloudRequest(ctx, http.MethodPost, route, body, &response); err != nil {
		return "", err
	}
	if response.URL == "" {
		return "", errors.New("cloud returned an invalid preview link")
	}
	return response.URL, nil
}

func (m *previewManager) beginRequest(parent context.Context, id string) (PreviewTarget, context.Context, func(), bool) {
	now := m.now().UTC()
	m.mu.Lock()
	target := m.targets[id]
	if target == nil || !target.ExpiresAt.After(now) {
		m.mu.Unlock()
		return PreviewTarget{}, nil, nil, false
	}
	if target.State != previewStateRunning {
		if target.Mode == previewModeStatic || target.Mode == previewModeSpawn {
			cp := *target
			m.mu.Unlock()
			return cp, parent, func() {}, true
		}
		m.mu.Unlock()
		return PreviewTarget{}, nil, nil, false
	}
	target.LastActivityAt = now
	target.ActiveStreams++
	m.nextLease++
	lease := m.nextLease
	ctx, cancel := context.WithCancel(parent)
	if m.active[id] == nil {
		m.active[id] = map[uint64]context.CancelFunc{}
	}
	m.active[id][lease] = cancel
	cp := *target
	m.mu.Unlock()
	var once sync.Once
	return cp, ctx, func() {
		once.Do(func() {
			m.mu.Lock()
			if current := m.targets[id]; current != nil {
				if current.ActiveStreams > 0 {
					current.ActiveStreams--
				}
				current.LastActivityAt = m.now().UTC()
			}
			delete(m.active[id], lease)
			if len(m.active[id]) == 0 {
				delete(m.active, id)
			}
			m.mu.Unlock()
			cancel()
		})
	}, true
}

func (m *previewManager) run(ctx context.Context) {
	interval := m.sweepEvery
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.shutdownOwned()
			return
		case <-ticker.C:
			m.sweep(ctx)
		}
	}
}

func (m *previewManager) pinnedBundles() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pinned := map[string]bool{}
	now := m.now().UTC()
	for _, target := range m.targets {
		if target.BundleDir != "" && target.ExpiresAt.After(now) && target.State != previewStateExpired {
			pinned[target.BundleDir] = true
		}
	}
	return pinned
}

// pruneCache bounds sealed static-preview bundles without touching anything
// referenced by a live target or an in-flight server/build pin.
func (m *previewManager) pruneCache(capBytes int64) (removed int64) {
	removed, _ = m.pruneCacheContext(context.Background(), capBytes)
	return removed
}

func (m *previewManager) shutdownOwned() {
	m.mu.Lock()
	type owned struct {
		target  PreviewTarget
		runtime *previewOwnedRuntime
	}
	var runtimes []owned
	for id, runtime := range m.runtimes {
		if target := m.targets[id]; target != nil && target.Mode != previewModeAdopt {
			runtimes = append(runtimes, owned{target: *target, runtime: runtime})
		}
	}
	m.runtimes = map[string]*previewOwnedRuntime{}
	m.runningOwned = 0
	m.mu.Unlock()
	for _, item := range runtimes {
		_ = m.stopOwnedRuntime(item.target, item.runtime)
	}
}

func (m *previewManager) idleStopOwned(id string, now time.Time) {
	m.mu.Lock()
	target := m.targets[id]
	if target == nil || (target.Mode != previewModeStatic && target.Mode != previewModeSpawn) || target.State != previewStateRunning || target.ActiveStreams != 0 || now.Sub(target.LastActivityAt) < m.idleAfter {
		m.mu.Unlock()
		return
	}
	if m.starting[id] != nil {
		m.mu.Unlock()
		return
	}
	call := &previewStartCall{done: make(chan struct{})}
	m.starting[id] = call
	cp := *target
	runtime := m.detachOwnedLocked(id)
	target.State = previewStateIdle
	target.TargetPort = 0
	target.PID = 0
	target.ProcessStartIdentity = ""
	stateCopy := *target
	_ = m.saveLocked()
	m.mu.Unlock()
	stopErr := m.stopOwnedRuntime(cp, runtime)
	m.mu.Lock()
	if stopErr != nil {
		if current := m.targets[id]; current != nil {
			current.State = previewStateError
			current.PID = cp.PID
			current.ProcessStartIdentity = cp.ProcessStartIdentity
			stateCopy = *current
			_ = m.saveLocked()
		}
	}
	call.err = stopErr
	delete(m.starting, id)
	close(call.done)
	m.mu.Unlock()
	if m.onState != nil {
		m.onState(stateCopy)
	}
}

func (m *previewManager) sweep(ctx context.Context) {
	now := m.now().UTC()
	type candidate struct {
		id   string
		hard bool
	}
	var candidates []candidate
	var idleOwned []string
	m.mu.Lock()
	for id, target := range m.targets {
		if !target.ExpiresAt.After(now) {
			candidates = append(candidates, candidate{id: id, hard: true})
			continue
		}
		if target.Mode == previewModeAdopt && target.ActiveStreams == 0 && now.Sub(target.LastActivityAt) >= m.idleAfter {
			candidates = append(candidates, candidate{id: id})
		} else if (target.Mode == previewModeStatic || target.Mode == previewModeSpawn) && target.State == previewStateRunning && target.ActiveStreams == 0 && now.Sub(target.LastActivityAt) >= m.idleAfter {
			idleOwned = append(idleOwned, id)
		}
	}
	m.mu.Unlock()
	for _, item := range candidates {
		_ = m.stop(ctx, item.id, true, item.hard)
	}
	for _, id := range idleOwned {
		m.idleStopOwned(id, now)
	}
	m.drainPendingRevokes(ctx)
	_ = previewCacheSweepToLimitContext(retirementWithAuthority(context.Background(), m.retirement), m.cacheRoot, m.pinnedBundles(), previewCacheMaxBytes)
}
