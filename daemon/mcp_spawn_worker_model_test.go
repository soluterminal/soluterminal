package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ST-283: a Claude worker started by spawn_pane without a model takes the
// person's worker-model setting; everything else keeps today's behaviour.
func TestSpawnPaneModelDecision(t *testing.T) {
	claude, codex := []string{"claude"}, []string{"codex"}
	for _, tt := range []struct {
		name               string
		cmd                []string
		caller, setting    string
		worker             bool
		wantModel, wantSrc string
	}{
		{name: "setting empty: no flag, as before", cmd: claude, worker: true, wantSrc: spawnModelFromNone},
		{name: "setting blank: no flag", cmd: claude, setting: "  ", worker: true, wantSrc: spawnModelFromNone},
		{name: "setting present: claude worker takes it", cmd: claude, setting: "opus", worker: true, wantModel: "opus", wantSrc: spawnModelFromSetting},
		{name: "caller names a model: caller wins", cmd: claude, caller: " sonnet ", setting: "opus", worker: true, wantModel: "sonnet", wantSrc: spawnModelFromCaller},
		{name: "lead: setting ignored", cmd: claude, setting: "opus", wantSrc: spawnModelFromNone},
		{name: "lead naming a model keeps it", cmd: claude, caller: "haiku", setting: "opus", wantModel: "haiku", wantSrc: spawnModelFromCaller},
		{name: "codex worker: setting ignored", cmd: codex, setting: "opus", worker: true, wantSrc: spawnModelFromNone},
		{name: "shell: setting ignored", cmd: []string{"bash"}, setting: "opus", worker: true, wantSrc: spawnModelFromNone},
		{name: "default shell: setting ignored", setting: "opus", worker: true, wantSrc: spawnModelFromNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model, src := spawnPaneModel(tt.cmd, tt.caller, tt.worker, tt.setting)
			if model != tt.wantModel || src != tt.wantSrc {
				t.Fatalf("got (%q, %q), want (%q, %q)", model, src, tt.wantModel, tt.wantSrc)
			}
		})
	}
}

func TestPaneStartModel(t *testing.T) {
	for _, tt := range []struct {
		cmd      []string
		want     string
		wantNote bool
	}{
		{cmd: []string{"claude"}, wantNote: true},
		{cmd: []string{"claude", "--model", "opus", "--effort", "high"}, want: "opus"},
		{cmd: []string{"/usr/local/bin/claude", "--model=claude-fable-5-1[1m]"}, want: "claude-fable-5-1[1m]"},
		{cmd: []string{"claude", "--model", "opus", "--resume", "abc", "--model", "sonnet"}, want: "sonnet"},
		{cmd: []string{"claude", "--model"}, wantNote: true},
		{cmd: []string{"claude", "-m", "opus"}, wantNote: true}, // claude has no -m
		{cmd: []string{"codex"}, wantNote: true},
		{cmd: []string{"codex", "-m", "gpt-5.5-codex", "-c", "model_reasoning_effort=high"}, want: "gpt-5.5-codex"},
		{cmd: []string{"codex", "--model", "gpt-5.4"}, want: "gpt-5.4"},
		{cmd: []string{"codex", "-c", `model="o3"`}, want: "o3"},
		{cmd: []string{"codex", "-c", "model_reasoning_effort=high"}, wantNote: true},
		{cmd: []string{"bash"}},
		{cmd: []string{"bash", "--model", "opus"}},
		{cmd: nil},
	} {
		got, note := paneStartModel(tt.cmd), paneStartModelNote(tt.cmd)
		if got != tt.want || (note != "") != tt.wantNote {
			t.Errorf("%q: model %q note %q; want model %q, note %v", tt.cmd, got, note, tt.want, tt.wantNote)
		}
	}
}

// A claude or codex not on the daemon's PATH runs inside the login-shell
// wrapper, which is what Pane.Cmd then holds; its model still reads.
func TestPaneStartModelLoginShellWrapped(t *testing.T) {
	for _, tt := range []struct {
		cmd      []string
		want     string
		wantNote bool
	}{
		{cmd: []string{"claude", "--model", "opus", "--effort", "high"}, want: "opus"},
		{cmd: []string{"claude"}, wantNote: true},
		{cmd: []string{"claude", "--resume", "abc", "--model=sonnet"}, want: "sonnet"},
		{cmd: []string{"codex", "-m", "gpt-5.5-codex"}, want: "gpt-5.5-codex"},
		{cmd: []string{"codex", "app-server"}, wantNote: true},
		{cmd: []string{"claude", "--append-system-prompt", "it's --model x", "--model", "haiku"}, want: "haiku"},
		{cmd: []string{"vim", "--model", "opus"}},
	} {
		wrapped, ok := tryLoginShellWrap(tt.cmd, "", nil)
		if !ok {
			t.Skip("no login-shell wrapper on this platform")
		}
		got, note := paneStartModel(wrapped), paneStartModelNote(wrapped)
		if got != tt.want || (note != "") != tt.wantNote {
			t.Errorf("%q wrapped as %q: model %q note %q; want model %q, note %v", tt.cmd, wrapped, got, note, tt.want, tt.wantNote)
		}
	}
	if got := singleQuotedWords(`'a'\''b' 'c'; exec bash -i`); !slices.Equal(got, []string{"a'b", "c"}) {
		t.Errorf("singleQuotedWords = %q", got)
	}
}

func workerModelSpawnServer(t *testing.T, setting string) (*apiServer, *[]SpawnOpts) {
	t.Helper()
	var captured []SpawnOpts
	mgr := NewManager(t.TempDir())
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Put(&Workspace{Name: "Default"}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if setting != "" {
		s := defaultWorkerSettings()
		s.WorkerModel = setting
		raw, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(dataDir, "worker-settings.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	api := &apiServer{
		mgr:     mgr,
		ws:      ws,
		dataDir: dataDir,
		panes:   &failingPaneStore{},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			captured = append(captured, o)
			p := transactionTestPane("pane_worker_model_" + string(rune('a'+len(captured))))
			p.Cmd = append([]string(nil), o.Cmd...)
			mgr.panes[p.ID] = p
			return p, nil
		},
	}
	return api, &captured
}

func TestMCPSpawnPaneWorkerModel(t *testing.T) {
	for _, tt := range []struct {
		name     string
		setting  string
		args     map[string]any
		wantCmd  []string
		wantResp map[string]any // model, model_source; model_note checked for presence
		wantNote bool
	}{
		{name: "setting empty: no flag", args: map[string]any{"cmd": "claude"}, wantCmd: []string{"claude"}, wantResp: map[string]any{"model_source": "none"}, wantNote: true},
		{name: "setting present: worker gets it", setting: "opus", args: map[string]any{"cmd": "claude", "effort": "high"}, wantCmd: []string{"claude", "--model", "opus", "--effort", "high"}, wantResp: map[string]any{"model": "opus", "model_source": "worker_model_setting"}},
		{name: "caller names a model", setting: "opus", args: map[string]any{"cmd": "claude", "model": "sonnet"}, wantCmd: []string{"claude", "--model", "sonnet"}, wantResp: map[string]any{"model": "sonnet", "model_source": "caller"}},
		{name: "lead: no flag", setting: "opus", args: map[string]any{"cmd": "claude", "lead": "security"}, wantCmd: []string{"claude"}, wantResp: map[string]any{"model_source": "none"}, wantNote: true},
		{name: "codex: no flag", setting: "opus", args: map[string]any{"cmd": "codex"}, wantCmd: []string{"codex"}, wantResp: map[string]any{"model_source": "none"}, wantNote: true},
		{name: "shell: no model fields", setting: "opus", args: map[string]any{"cmd": "bash"}, wantCmd: []string{"bash"}, wantResp: map[string]any{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api, captured := workerModelSpawnServer(t, tt.setting)
			// These test panes never show an input box to wait for.
			tt.args["wait_ready"] = false
			text, isErr := mcpResultText(t, api.mcpToolSpawnPane(tt.args, ""))
			if isErr || len(*captured) != 1 {
				t.Fatalf("spawn: %s (error %v, spawned %d)", text, isErr, len(*captured))
			}
			if got := (*captured)[0].Cmd; !slices.Equal(got, tt.wantCmd) {
				t.Fatalf("argv %#v, want %#v", got, tt.wantCmd)
			}
			var resp map[string]any
			if err := json.Unmarshal([]byte(text), &resp); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"model", "model_source"} {
				if resp[k] != tt.wantResp[k] {
					t.Errorf("%s = %v, want %v (result %s)", k, resp[k], tt.wantResp[k], text)
				}
			}
			if _, has := resp["model_note"]; has != tt.wantNote {
				t.Errorf("model_note present %v, want %v (result %s)", has, tt.wantNote, text)
			}
		})
	}
}

// The setting is read at spawn time: a change in Settings reaches the next
// worker without a daemon restart.
func TestMCPSpawnPaneWorkerModelReadAtSpawn(t *testing.T) {
	api, captured := workerModelSpawnServer(t, "")
	api.mcpToolSpawnPane(map[string]any{"cmd": "claude", "wait_ready": false}, "")
	q := api.workers()
	q.mu.Lock()
	q.settings.WorkerModel = "opus"
	q.mu.Unlock()
	api.mcpToolSpawnPane(map[string]any{"cmd": "claude", "wait_ready": false}, "")
	if len(*captured) != 2 || !slices.Equal((*captured)[0].Cmd, []string{"claude"}) || !slices.Equal((*captured)[1].Cmd, []string{"claude", "--model", "opus"}) {
		t.Fatalf("argvs = %#v", *captured)
	}
}

func TestMCPListPanesStartModel(t *testing.T) {
	api, _ := workerModelSpawnServer(t, "")
	for id, cmd := range map[string][]string{
		"pane_lm_opus":  {"claude", "--model", "opus"},
		"pane_lm_none":  {"claude"},
		"pane_lm_codex": {"codex", "-m", "gpt-5.5-codex"},
		"pane_lm_shell": {"bash"},
	} {
		p := transactionTestPane(id)
		p.Cmd = cmd
		api.mgr.panes[id] = p
	}
	text, isErr := mcpResultText(t, api.mcpToolListPanes(map[string]any{}, ""))
	if isErr {
		t.Fatal(text)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]any{}
	for _, r := range rows {
		got[r["id"].(string)] = [2]any{r["model"], r["model_note"] != nil}
	}
	want := map[string][2]any{
		"pane_lm_opus":  {"opus", false},
		"pane_lm_none":  {nil, true},
		"pane_lm_codex": {"gpt-5.5-codex", false},
		"pane_lm_shell": {nil, false},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: (model, note) = %v, want %v", id, got[id], w)
		}
	}
}

// Only the person changes the worker model: a pane can neither set nor clear
// it, and a value that isn't a model id is refused.
func TestWorkerSettingsWorkerModelIsPersonOnly(t *testing.T) {
	a := authorityTestServer(t)
	a.dataDir, a.metrics = t.TempDir(), NewMetrics()
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	pane := transactionTestPane("agent")
	pane.Pid = startSleeper(t).Process.Pid
	a.mgr.panes[pane.ID] = pane
	stubPeerPID(t, pane.Pid, true)
	mux := http.NewServeMux()
	a.routes(mux)
	put := func(body string, human bool) int {
		t.Helper()
		if human {
			stubPeerPID(t, 0, false)
		} else {
			stubPeerPID(t, pane.Pid, true)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/worker-settings", strings.NewReader(body))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	model := func() string { return a.workerModelSetting() }
	if code := put(`{"workerModel":"opus"}`, false); code != http.StatusForbidden || model() != "" {
		t.Fatalf("pane set: status %d, model %q", code, model())
	}
	for _, bad := range []string{`{"workerModel":"--dangerously-skip-permissions"}`, `{"workerModel":"opus; rm -rf ~"}`} {
		if code := put(bad, true); code != http.StatusBadRequest || model() != "" {
			t.Fatalf("%s: status %d, model %q", bad, code, model())
		}
	}
	if code := put(`{"workerModel":" opus "}`, true); code != http.StatusOK || model() != "opus" {
		t.Fatalf("person set: status %d, model %q", code, model())
	}
	if code := put(`{"workerModel":""}`, false); code != http.StatusForbidden || model() != "opus" {
		t.Fatalf("pane clear: status %d, model %q", code, model())
	}
	// A pane may still narrow other worker settings without touching it.
	if code := put(`{"maxRunning":2}`, false); code != http.StatusOK || model() != "opus" {
		t.Fatalf("pane narrow: status %d, model %q", code, model())
	}
	reloaded := &apiServer{dataDir: a.dataDir}
	if got := reloaded.workerModelSetting(); got != "opus" {
		t.Fatalf("persisted model %q, want opus", got)
	}
	if code := put(`{"workerModel":""}`, true); code != http.StatusOK || model() != "" {
		t.Fatalf("person clear: status %d, model %q", code, model())
	}
}
