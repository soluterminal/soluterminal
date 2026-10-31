package main

import (
	"context"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// Extension points for optional features.
//
// The core daemon (panes, workspaces, previews, schedules, the single active
// provider login, MCP pane coordination) names none of its optional
// features. A feature lives in its own files and plugs in here from init():
// it adds HTTP routes, MCP tools and checks without the core calling it by
// name. Build the daemon without a feature's files and the feature is simply
// absent; nothing else changes.
//
// Registration happens only in init(), before main or any test runs, so the
// slices are read without locking.

// apiRouteHooks add routes to the daemon's HTTP API. They run inside
// apiServer.routes, on the same mux and behind the same auth gate as the core
// routes. http.ServeMux matches by specificity, not registration order, so a
// hook cannot shadow a core route (a duplicate pattern panics at startup, as
// it would in routes itself).
var apiRouteHooks []func(a *apiServer, mux *http.ServeMux)

func registerAPIRoutes(fn func(a *apiServer, mux *http.ServeMux)) {
	apiRouteHooks = append(apiRouteHooks, fn)
}

func (a *apiServer) registerExtensionRoutes(mux *http.ServeMux) {
	for _, fn := range apiRouteHooks {
		fn(a, mux)
	}
}

// mcpToolCall is what an extension MCP tool handler receives.
type mcpToolCall struct {
	Args       map[string]any
	Scope      *mcpScope
	CallerPane string
	// Ident is the provenance stamped on artifacts the call creates
	// ("agent" for an interactive pane session, else the scope's identity).
	Ident string
}

type mcpExtensionTool struct {
	tool mcpTool
	// after names the core tool this one is listed after (empty: at the end),
	// so the advertised order does not depend on which features are built in.
	after string
	call  func(a *apiServer, c mcpToolCall) any
}

var mcpToolHooks []mcpExtensionTool

// registerMCPTool adds a tool to the MCP server. It is advertised to every
// scope that allows its name (mcpToolsForScope) and dispatched after the
// call guards below, exactly like a core tool.
func registerMCPTool(after string, tool mcpTool, call func(a *apiServer, c mcpToolCall) any) {
	for _, core := range mcpCoreTools() {
		if core.Name == tool.Name {
			panic("registerMCPTool: " + tool.Name + " is a core tool")
		}
	}
	if _, dup := extensionMCPTool(tool.Name); dup {
		panic("registerMCPTool: " + tool.Name + " registered twice")
	}
	mcpToolHooks = append(mcpToolHooks, mcpExtensionTool{tool: tool, after: after, call: call})
}

// withExtensionTools merges the registered tools into the core list.
func withExtensionTools(core []mcpTool) []mcpTool {
	out := slices.Clone(core)
	for _, ext := range mcpToolHooks {
		at := len(out)
		if ext.after != "" {
			if i := slices.IndexFunc(out, func(t mcpTool) bool { return t.Name == ext.after }); i >= 0 {
				at = i + 1
			}
		}
		out = slices.Insert(out, at, ext.tool)
	}
	return out
}

func extensionMCPTool(name string) (func(a *apiServer, c mcpToolCall) any, bool) {
	for _, ext := range mcpToolHooks {
		if ext.tool.Name == name {
			return ext.call, true
		}
	}
	return nil, false
}

// mcpCallGuards run on every tool call after the scope check and before the
// mutation policy and the handler. A guard returns either a refusal (a tool
// result, returned to the caller as is) or the arguments to continue with
// (possibly narrowed); returning the args unchanged lets the call through.
var mcpCallGuards []func(a *apiServer, name string, args map[string]any, scope *mcpScope) (map[string]any, any)

func registerMCPCallGuard(fn func(a *apiServer, name string, args map[string]any, scope *mcpScope) (map[string]any, any)) {
	mcpCallGuards = append(mcpCallGuards, fn)
}

func (a *apiServer) runMCPCallGuards(name string, args map[string]any, scope *mcpScope) (map[string]any, any) {
	for _, guard := range mcpCallGuards {
		next, denied := guard(a, name, args, scope)
		if denied != nil {
			return nil, denied
		}
		args = next
	}
	return args, nil
}

// paneShims are commands solud answers when a pane runs it under another name.
// ensureCodexInlineShim links each name into the pane shim directory, which
// pane shells put first on PATH, and main dispatches on argv[0] when solud
// starts as one of them.
type paneShim struct {
	name string
	run  func(args []string) int
}

var paneShims []paneShim

func registerPaneShim(name string, run func(args []string) int) {
	if _, dup := paneShimCommand(name); dup {
		panic("registerPaneShim: " + name + " registered twice")
	}
	paneShims = append(paneShims, paneShim{name: name, run: run})
}

func paneShimCommand(name string) (func(args []string) int, bool) {
	for _, shim := range paneShims {
		if shim.name == name {
			return shim.run, true
		}
	}
	return nil, false
}

// extState holds optional features' state on a core object (apiServer,
// Manager). A feature cannot add a field to a core struct, so it declares an
// extSlot and reaches its state through it.
type extState struct{ m sync.Map }

// extSlot is one feature's state in an extState; reading a slot that was
// never set is nil.
type extSlot[T any] struct{ _ byte }

func (k *extSlot[T]) of(s *extState) *T {
	v, _ := s.m.Load(k)
	p, _ := v.(*T)
	return p
}

// ensure returns the slot's state, creating it on first use.
func (k *extSlot[T]) ensure(s *extState) *T {
	if v, ok := s.m.Load(k); ok {
		return v.(*T)
	}
	v, _ := s.m.LoadOrStore(k, new(T))
	return v.(*T)
}

func (k *extSlot[T]) put(s *extState, v *T) {
	if v == nil {
		s.m.Delete(k)
		return
	}
	s.m.Store(k, v)
}

// get and set address a server's state; get on a nil server is nil.
func (k *extSlot[T]) get(a *apiServer) *T {
	if a == nil {
		return nil
	}
	return k.of(&a.ext)
}

// set attaches state to a server (which must not be nil).
func (k *extSlot[T]) set(a *apiServer, v *T) { k.put(&a.ext, v) }

// bootPhase names a point in func main where optional features run their
// boot steps. Each phase sits where the features' code used to be inline, so
// the boot order is unchanged.
type bootPhase int

const (
	// bootEarly: the schedule store exists; the API server does not yet
	// (bootEnv.api and .updater are nil).
	bootEarly bootPhase = iota
	// bootServer: the API server was just built; nothing else is wired.
	bootServer
	// bootStores: the API server, the schedule store and the credential
	// stores exist; no route is registered and nothing listens yet.
	bootStores
	// bootRelease: just before bootReadiness; the updater is wired.
	bootRelease
	// bootReadiness: release readiness (api.readiness) and the updater are
	// wired.
	bootReadiness
	// bootAccounts: the provider credential stores exist (api.claude,
	// api.codex, api.muse; each nil when it could not be built).
	bootAccounts
	// bootCredentials: the credential stores and runtime health are wired.
	bootCredentials
	// bootLoops: the signal context is live (env.ctx); background loops start.
	bootLoops
	// bootOwned: this process owns the local port; work that must not run in
	// a twin that loses the bind race starts here.
	bootOwned
)

// bootEnv is what boot steps receive: the values func main has built.
type bootEnv struct {
	ctx     context.Context // nil before bootLoops
	dataDir string
	addr    string
	// backendID names this backend to the cloud (dual-backend installs).
	backendID string
	version   string
	bootTeam  bool
	api       *apiServer
	mgr       *Manager
	scheds    *ScheduleStore
	bus       *uiBus
	updater   *Updater
	// restartAdmission is the invocation boundary shared by every feature
	// that starts provider runs.
	restartAdmission *restartAdmissionBarrier
	onExit           []func()
}

// atExit runs fn when func main returns (like a defer at the step's place).
func (b *bootEnv) atExit(fn func()) { b.onExit = append(b.onExit, fn) }

func (b *bootEnv) exit() {
	for i := len(b.onExit) - 1; i >= 0; i-- {
		b.onExit[i]()
	}
}

var bootSteps = map[bootPhase][]func(*bootEnv){}

// registerBootStep runs step at phase, after the steps registered before it.
// TestAutonomyRegistryCoversEveryBootSite scans every registered step as it
// scans func main.
func registerBootStep(phase bootPhase, step func(*bootEnv)) {
	bootSteps[phase] = append(bootSteps[phase], step)
}

func runBootSteps(phase bootPhase, env *bootEnv) {
	for _, step := range bootSteps[phase] {
		step(env)
	}
}

// cloudPusherHooks hand the cloud push channel to optional features when the
// machine is paired (p is the new pusher) and take it back on Disconnect
// (p is nil).
var cloudPusherHooks []func(a *apiServer, p *Pusher)

func registerCloudPusherHook(fn func(a *apiServer, p *Pusher)) {
	cloudPusherHooks = append(cloudPusherHooks, fn)
}

func runCloudPusherHooks(a *apiServer, p *Pusher) {
	for _, fn := range cloudPusherHooks {
		fn(a, p)
	}
}

// System page contributors: optional features add their own state to the
// 15-second health sample, the automation summary and the activity
// interlock.
var (
	systemSampleHooks     []func(a *apiServer, sample *systemSample, cutoff time.Time)
	systemAutomationHooks []func(a *apiServer, out *systemAutomationDTO, cutoff time.Time)
	// systemActivityHooks report running work; ok=false means the feature's
	// store is unavailable, so the host cannot be proven quiet.
	systemActivityHooks []func(a *apiServer) (activity systemActivityCounts, ok bool)
	// systemStatusHooks and systemReleaseHooks add a feature's rows to
	// /api/system/status and its release section.
	systemStatusHooks  []func(a *apiServer, status *systemStatusDTO)
	systemReleaseHooks []func(a *apiServer, out *systemReleaseDTO)
	// systemRuntimeSample folds runtime health into the health sample; nil
	// means this build has no runtime health, which reads as unknown.
	systemRuntimeSample func(a *apiServer, sample *systemSample)
)

// systemActivityCounts is one feature's share of the activity interlock.
type systemActivityCounts struct {
	Invocations int // running model invocations
	Heavy       int // admitted heavy operations
}

func registerSystemSampleHook(fn func(a *apiServer, sample *systemSample, cutoff time.Time)) {
	systemSampleHooks = append(systemSampleHooks, fn)
}

func registerSystemAutomationHook(fn func(a *apiServer, out *systemAutomationDTO, cutoff time.Time)) {
	systemAutomationHooks = append(systemAutomationHooks, fn)
}

func registerSystemActivityHook(fn func(a *apiServer) (systemActivityCounts, bool)) {
	systemActivityHooks = append(systemActivityHooks, fn)
}

func registerSystemStatusHook(fn func(a *apiServer, status *systemStatusDTO)) {
	systemStatusHooks = append(systemStatusHooks, fn)
}

func registerSystemReleaseHook(fn func(a *apiServer, out *systemReleaseDTO)) {
	systemReleaseHooks = append(systemReleaseHooks, fn)
}

// healthHooks add a feature's fields to GET /api/health.
var healthHooks []func(a *apiServer, body map[string]any)

func registerHealthHook(fn func(a *apiServer, body map[string]any)) {
	healthHooks = append(healthHooks, fn)
}

// gitStatusNoteHooks append a feature's note to the git_status MCP tool's
// answer for a repository path ("" adds nothing).
var gitStatusNoteHooks []func(a *apiServer, path string) string

func registerGitStatusNote(fn func(a *apiServer, path string) string) {
	gitStatusNoteHooks = append(gitStatusNoteHooks, fn)
}

// scheduleKind implements a schedule kind beyond the core ones ("prompt",
// "task" and the hidden managed-agent trigger).
type scheduleKind struct {
	// available checks whether this build recognises the schedule target.
	// Nil means every target of this registered kind is supported.
	available func(Schedule) bool
	// validate checks a create/update input of this kind.
	validate func(in ScheduleInput) error
	// title is the default name of a schedule created without one ("" falls
	// back to the prompt's first line).
	title func(in ScheduleInput) string
	// fire runs one occurrence.
	fire func(s *ScheduleStore, schedID string, sched Schedule, apply bool)
	// running reports a daemon-started run of the schedule in flight.
	running func(s *ScheduleStore, sched Schedule) bool
	// runDir is where the kind keeps its run logs.
	runDir func(sched Schedule) (string, error)
}

var scheduleKinds = map[string]scheduleKind{}

// scheduleKindOrder keeps registration order for user-facing kind lists.
var scheduleKindOrder []string

func registerScheduleKind(name string, k scheduleKind) {
	scheduleKinds[name] = k
	scheduleKindOrder = append(scheduleKindOrder, name)
}

// ScheduleChatRef is where a task schedule's reports go (Schedule.DeliverChat).
type ScheduleChatRef struct {
	AgentID  string `json:"agentId,omitempty"` // "" = the feature's first configured sender
	Source   string `json:"source"`            // the chat network
	ChatID   string `json:"chatId"`
	ChatName string `json:"chatName,omitempty"` // display only
}

// scheduleChatDelivery posts a finished task run's report to its schedule's
// DeliverChat. Nil without a chat feature; such runs record that delivery
// isn't available.
var scheduleChatDelivery func(s *ScheduleStore, schedID string, run ScheduleRun, name string, ref ScheduleChatRef)

func registerScheduleChatDelivery(fn func(s *ScheduleStore, schedID string, run ScheduleRun, name string, ref ScheduleChatRef)) {
	scheduleChatDelivery = fn
}

// paneShellBashEnv are bash function definitions (BASH_FUNC_name%%=…) that
// terminal panes pass through the environment, after the core `codex` hook;
// their PowerShell counterparts extend paneShellPowerShellInit.
var paneShellBashEnv []string

func registerPaneShellInit(bashEnv, powerShellInit string) {
	paneShellBashEnv = append(paneShellBashEnv, bashEnv)
	paneShellPowerShellInit += "\n" + powerShellInit
}

// paneBroker routes provider CLIs launched in panes through an optional
// credential broker. Its zero value is "no broker": every pane runs the CLI
// with the user's own login, and the helpers below answer accordingly.
var paneBroker paneBrokerHooks

type paneBrokerHooks struct {
	// prepare plans a pane spawn's route: argv overrides inserted after the
	// CLI executable (brokerChildCommand), and a release for anything minted
	// for the spawn, called when the spawn fails.
	prepare func(m *Manager, opts *SpawnOpts) (overrides []string, release func(), err error)
	// active reports whether a launch environment carries a prepared route.
	active func(env []string) bool
	// accountLabel is the credential label a routed launch reports.
	accountLabel func(label string) string
	// isAccount reports whether a selected credential label is a routed
	// (broker) account rather than the user's own login.
	isAccount func(label string) bool
	// selectedTeam is the team of the provider's selected routed account.
	selectedTeam func(m *Manager, provider string) string
	// agentInheritedEnv and agentEnv prepare a routed child's environment in
	// the pane agent process (before and after its --env entries).
	agentInheritedEnv func(inherited, entries []string) []string
	agentEnv          func(env []string) []string
	// codexShellLaunch plans `codex` typed in a terminal shell; runShellCLI
	// runs the planned CLI and releases what the plan minted.
	codexShellLaunch func(bin string, args, env []string) ([]string, []string, error)
	runShellCLI      func(c *exec.Cmd) error
	// codexLiveConfigValid checks a routed Codex launch's overrides before it
	// may use the live relay.
	codexLiveConfigValid func(overrides []string) bool
}

func paneBrokerActive(env []string) bool { return paneBroker.active != nil && paneBroker.active(env) }

func paneBrokerAccount(label string) bool {
	return paneBroker.isAccount != nil && paneBroker.isAccount(label)
}

func paneBrokerPrepare(m *Manager, opts *SpawnOpts) ([]string, func(), error) {
	if paneBroker.prepare == nil {
		return nil, nil, nil
	}
	return paneBroker.prepare(m, opts)
}

// bodyLimitHooks may raise the request body limit for a feature's own routes
// (maxRequestBodyMiddleware); each sees the limit the previous one chose.
var bodyLimitHooks []func(r *http.Request, limit int64) int64

func registerBodyLimit(fn func(r *http.Request, limit int64) int64) {
	bodyLimitHooks = append(bodyLimitHooks, fn)
}

func requestBodyLimit(r *http.Request, limit int64) int64 {
	for _, fn := range bodyLimitHooks {
		limit = fn(r, limit)
	}
	return limit
}

// harnessTeam supplies native-harness accounts shared by a team. Its zero
// value means a harness uses the user's own keys only.
var harnessTeam struct {
	// sync pulls the provider's shared accounts.
	sync func(a *apiServer, ctx context.Context, provider string) error
	// route gives a shared account a scoped route for one probe: the key and
	// base URL to call, or an HTTP status and error.
	route func(a *apiServer, s *harnessCredStore, name string, acc harnessAccount) (key, base string, status int, err error)
	// release ends a route route minted.
	release func(a *apiServer, key string)
}
