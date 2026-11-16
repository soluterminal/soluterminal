package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func makeContextPack(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("brief"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestValidContextPackDir(t *testing.T) {
	for _, files := range [][]string{{"CLAUDE.md"}, {"AGENTS.md"}, {"CLAUDE.md", "AGENTS.md"}} {
		dir := makeContextPack(t, files...)
		if got, err := validContextPackDir(dir); err != nil || got != filepath.Clean(dir) {
			t.Fatalf("validContextPackDir(%q) = %q, %v", dir, got, err)
		}
	}

	t.Run("home expansion", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := filepath.Join(home, "pack")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("brief"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := validContextPackDir("~/pack")
		if err != nil || got != dir {
			t.Fatalf("home expansion = %q, %v; want %q", got, err, dir)
		}
	})

	regular := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withoutBrief := t.TempDir()
	for name, path := range map[string]string{
		"missing":         filepath.Join(t.TempDir(), "missing"),
		"regular file":    regular,
		"without brief":   withoutBrief,
		"relative":        "relative/pack",
		"single quote":    filepath.Join(t.TempDir(), "bad'pack"),
		"newline":         filepath.Join(t.TempDir(), "bad\npack"),
		"carriage return": filepath.Join(t.TempDir(), "bad\rpack"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validContextPackDir(path); err == nil {
				t.Fatalf("validContextPackDir(%q) unexpectedly succeeded", path)
			}
		})
	}
}

func TestContextPackPointerTextConstraints(t *testing.T) {
	pointer := contextPackPointer("/tmp/pack")
	if !strings.HasPrefix(pointer, contextPackMarker) {
		t.Fatalf("pointer must start with %q", contextPackMarker)
	}
	if strings.ContainsRune(pointer, '\'') {
		t.Fatal("pointer contains an ASCII apostrophe")
	}
	if isHeadlessClaudeCmd([]string{"claude", "--append-system-prompt", pointer}) {
		t.Fatal("pointer makes a terminal argv look headless")
	}
	if strings.Contains(pointer, "'--resume' '") {
		t.Fatal("pointer contains the wrapped resume marker")
	}
}

func TestResolveContextPackStatus(t *testing.T) {
	dir := makeContextPack(t, "CLAUDE.md", "AGENTS.md")
	cases := []struct {
		name, cwd, kind, agent string
		cmd                    []string
		want                   string
	}{
		{"terminal claude", "/tmp", "", "", []string{"claude"}, contextPackInjected},
		{"terminal codex", "/tmp", "", agentCodex, []string{"codex"}, contextPackInjected},
		{"muse", "/tmp", "", agentMuse, []string{"muse"}, contextPackUnsupported},
		{"bash", "/tmp", "", "", []string{"bash"}, contextPackUnsupported},
		{"cwd equal", dir, "", "", []string{"claude"}, contextPackCwd},
		{"cwd inside", filepath.Join(dir, "nested"), "", "", []string{"claude"}, contextPackCwd},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveContextPack(dir, tc.cwd, tc.kind, tc.agent, tc.cmd); got.Status != tc.want {
				t.Fatalf("status = %q, want %q (%+v)", got.Status, tc.want, got)
			}
		})
	}
	missing := resolveContextPack(filepath.Join(t.TempDir(), "gone"), "/tmp", "", "", []string{"claude"})
	if missing.Status != contextPackMissing {
		t.Fatalf("missing status = %q", missing.Status)
	}
	if got := resolveContextPack("", "/tmp", "", "", []string{"claude"}); got != (contextPackInjection{}) {
		t.Fatalf("empty pack = %+v, want zero", got)
	}
}

func TestWithContextPackClaudeArgv(t *testing.T) {
	agentsDir := makeContextPack(t, "AGENTS.md")
	agentsInj := resolveContextPack(agentsDir, "/tmp", "", "", []string{"claude"})
	base := []string{"claude", "--append-system-prompt", orchestratorBrief}
	once := withContextPack(base, agentsInj)
	if countFlag(once, "--add-dir") != 1 || !slices.Contains(once, agentsDir) {
		t.Fatalf("pack dir was not appended once: %v", once)
	}
	prompt := appendedSystemPrompt(t, once)
	if !strings.HasSuffix(prompt, contextPackPointer(agentsDir)) || countFlag(once, "--append-system-prompt") != 1 {
		t.Fatalf("pointer was not joined into the single prompt: %v", once)
	}
	if twice := withContextPack(once, agentsInj); !slices.Equal(once, twice) {
		t.Fatalf("second application changed argv:\n%v\n%v", once, twice)
	}

	stale := []string{"claude", "--add-dir", agentsDir, "--append-system-prompt", contextPackMarker + " old"}
	replaced := withContextPack(stale, agentsInj)
	if countFlag(replaced, "--add-dir") != 1 || strings.Contains(appendedSystemPrompt(t, replaced), "old") {
		t.Fatalf("stale adapter was not replaced: %v", replaced)
	}

	claudeDir := makeContextPack(t, "CLAUDE.md")
	claudeInj := resolveContextPack(claudeDir, "/tmp", "", "", []string{"claude"})
	claudeArgv := withContextPack([]string{"claude"}, claudeInj)
	if countFlag(claudeArgv, "--append-system-prompt") != 1 || countFlag(claudeArgv, "--add-dir") != 1 {
		t.Fatalf("CLAUDE.md pack should use add-dir plus an explicit pointer: %v", claudeArgv)
	}

	for name, cmd := range map[string][]string{
		"codex":   {"codex"},
		"bash":    {"bash"},
		"wrapped": {"bash", "-lc", "'claude'"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := withContextPack(cmd, agentsInj); !slices.Equal(got, cmd) {
				t.Fatalf("argv changed: %v", got)
			}
		})
	}
	empty := []string{"claude", "--model", "x"}
	if got := withContextPack(empty, contextPackInjection{}); !slices.Equal(got, empty) {
		t.Fatalf("empty injection changed argv: %v", got)
	}
}

func TestWithContextPackClaudeMdHasExplicitPointer(t *testing.T) {
	dir := makeContextPack(t, "CLAUDE.md")
	inj := resolveContextPack(dir, "/tmp", "", "", []string{"claude"})
	got := withContextPack([]string{"claude"}, inj)
	if countFlag(got, "--append-system-prompt") != 1 {
		t.Fatalf("CLAUDE.md pack prompt count = %d, want 1: %v", countFlag(got, "--append-system-prompt"), got)
	}
	prompt := appendedSystemPrompt(t, got)
	if !strings.HasPrefix(prompt, contextPackMarker) || !strings.Contains(prompt, dir+"/CLAUDE.md") {
		t.Fatalf("CLAUDE.md pack prompt = %q, want explicit pointer to %s/CLAUDE.md", prompt, dir)
	}
	if twice := withContextPack(got, inj); !slices.Equal(twice, got) {
		t.Fatalf("second CLAUDE.md application changed argv:\n%v\n%v", got, twice)
	}
}

func TestResolveContextPackWrappedClaudeIsUnsupported(t *testing.T) {
	dir := makeContextPack(t, "CLAUDE.md")
	cmd := []string{"bash", "-lc", "exec 'claude'"}
	inj := resolveContextPack(dir, "/tmp", "", "", cmd)
	if inj.Status != contextPackUnsupported {
		t.Fatalf("wrapped Claude status = %q, want %q", inj.Status, contextPackUnsupported)
	}
	if got := withContextPack(cmd, inj); !slices.Equal(got, cmd) {
		t.Fatalf("unsupported wrapped argv changed: %v", got)
	}
}

func TestContextPackEnv(t *testing.T) {
	for _, status := range []string{contextPackInjected, contextPackUnsupported} {
		got := contextPackEnv(contextPackInjection{Dir: "/pack", Status: status})
		if !slices.Equal(got, []string{"SOLU_CONTEXT_PACK=/pack"}) {
			t.Fatalf("status %q env = %v", status, got)
		}
	}
	for _, status := range []string{"", contextPackCwd, contextPackMissing} {
		if got := contextPackEnv(contextPackInjection{Dir: "/pack", Status: status}); got != nil {
			t.Fatalf("status %q env = %v, want nil", status, got)
		}
	}
}

func TestContextPackNoContentLeak(t *testing.T) {
	const secret = "PACK-SECRET-7f3a"
	dir := t.TempDir()
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	for _, tc := range []struct {
		kind, agent string
		cmd         []string
	}{
		{"", "", []string{"claude"}},
		{"", agentCodex, []string{"codex"}},
	} {
		inj := resolveContextPack(dir, "/tmp", tc.kind, tc.agent, tc.cmd)
		argv := withContextPack(tc.cmd, inj)
		env := contextPackEnv(inj)
		if strings.Contains(strings.Join(argv, "\x00"), secret) || strings.Contains(strings.Join(env, "\x00"), secret) {
			t.Fatalf("pack content leaked through argv/env: argv=%v env=%v", argv, env)
		}
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("pack content leaked through logs")
	}
}

func TestCreatePaneContextPackValidationAndInheritance(t *testing.T) {
	pack := makeContextPack(t, "AGENTS.md")
	workspaceStore, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceStore.Put(&Workspace{Name: "strategy", PaneIds: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceStore.SetContextPack("strategy", pack); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, body string
		wantStatus int
		wantPack   string
	}{
		{"invalid", `{"cmd":["claude"],"contextPack":"/does/not/exist"}`, http.StatusBadRequest, ""},
		{"explicit", `{"cmd":["claude"],"contextPack":"` + pack + `"}`, http.StatusCreated, pack},
		{"inherits", `{"cmd":["claude"],"workspace":"strategy"}`, http.StatusCreated, pack},
		{"opts out", `{"cmd":["claude"],"workspace":"strategy","contextPack":"none"}`, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got SpawnOpts
			server := &apiServer{
				ws: workspaceStore,
				spawnPane: func(opts SpawnOpts) (*Pane, error) {
					got = opts
					p := transactionTestPane("pack-created-" + strings.ReplaceAll(tc.name, " ", "-"))
					p.Cmd = []string{"claude"}
					p.ContextPack = opts.ContextPack
					if opts.ContextPack != "" {
						p.ContextPackStatus = contextPackInjected
					}
					return p, nil
				},
			}
			rec := httptest.NewRecorder()
			server.createPane(rec, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(tc.body)))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusBadRequest {
				if !strings.HasPrefix(rec.Body.String(), "invalid context pack:") {
					t.Fatalf("body = %q", rec.Body.String())
				}
				return
			}
			if got.ContextPack != tc.wantPack {
				t.Fatalf("SpawnOpts.ContextPack = %q, want %q", got.ContextPack, tc.wantPack)
			}
		})
	}
}

func TestCreatePaneStaleInheritedContextPackDegradesToMissing(t *testing.T) {
	pack := makeContextPack(t, "AGENTS.md")
	workspaceStore, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceStore.Put(&Workspace{Name: "strategy", PaneIds: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceStore.SetContextPack("strategy", pack); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(pack); err != nil {
		t.Fatal(err)
	}

	var got SpawnOpts
	server := &apiServer{
		ws: workspaceStore,
		spawnPane: func(opts SpawnOpts) (*Pane, error) {
			got = opts
			p := transactionTestPane("stale-pack-created")
			p.Cmd = []string{"claude"}
			p.ContextPack = opts.ContextPack
			p.ContextPackStatus = resolveContextPack(opts.ContextPack, "/tmp", "", "", p.Cmd).Status
			return p, nil
		},
	}
	rec := httptest.NewRecorder()
	server.createPane(rec, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(`{"cmd":["claude"],"workspace":"strategy","cwd":"/tmp"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	if got.ContextPack != pack {
		t.Fatalf("SpawnOpts.ContextPack = %q, want stale inherited path %q", got.ContextPack, pack)
	}
	var dto paneDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.ContextPackStatus != contextPackMissing {
		t.Fatalf("contextPackStatus = %q, want %q", dto.ContextPackStatus, contextPackMissing)
	}
}

func TestValidContextPackDirRejectsTrailingNewlineBeforeTrim(t *testing.T) {
	dir := makeContextPack(t, "AGENTS.md")
	if _, err := validContextPackDir(dir + "\n"); err == nil {
		t.Fatal("valid pack path with a trailing newline unexpectedly succeeded")
	}
}

func TestValidContextPackDirSecurityAndNormalizationEdges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("brief"), 0o600); err != nil {
		t.Fatal(err)
	}
	pack := makeContextPack(t, "AGENTS.md")

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"nul", pack + "\x00evil", "unsupported character"},
		{"single quote", pack + "'evil", "unsupported character"},
		{"newline middle", pack + "\nevil", "unsupported character"},
		{"newline end", pack + "\n", "unsupported character"},
		{"carriage return end", pack + "\r", "unsupported character"},
		{"sentinel", "none", "absolute or start with ~"},
		{"dot sentinel", "./none", "absolute or start with ~"},
		{"sentinel child", "none/", "absolute or start with ~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validContextPackDir(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validContextPackDir(%q) error = %v, want containing %q", tc.raw, err, tc.want)
			}
		})
	}

	for _, raw := range []string{"~", "~/"} {
		if got, err := validContextPackDir(raw); err != nil || got != home {
			t.Fatalf("validContextPackDir(%q) = %q, %v; want %q", raw, got, err, home)
		}
	}
	if got, err := validContextPackDir(pack + string(filepath.Separator)); err != nil || got != pack {
		t.Fatalf("trailing separator = %q, %v; want %q", got, err, pack)
	}
	symlink := filepath.Join(t.TempDir(), "pack-link")
	if err := os.Symlink(pack, symlink); err != nil {
		t.Fatal(err)
	}
	if got, err := validContextPackDir(symlink); err != nil || got != symlink {
		t.Fatalf("symlink = %q, %v; want %q", got, err, symlink)
	}
}

func TestWithContextPackDropsMarkerOnlyPromptBeforeReplacement(t *testing.T) {
	dir := makeContextPack(t, "AGENTS.md")
	inj := resolveContextPack(dir, "/tmp", "", "", []string{"claude"})

	markerOnly := []string{"claude", "--append-system-prompt", contextPackMarker + " old"}
	got := withContextPack(markerOnly, inj)
	if countFlag(got, "--append-system-prompt") != 1 || strings.Contains(appendedSystemPrompt(t, got), "old") {
		t.Fatalf("marker-only prompt was not replaced by one prompt: %v", got)
	}

	const other = "keep this narration"
	mixed := []string{"claude", "--append-system-prompt", contextPackMarker + " old\n\n" + other}
	got = withContextPack(mixed, inj)
	prompt := appendedSystemPrompt(t, got)
	if countFlag(got, "--append-system-prompt") != 1 || !strings.HasPrefix(prompt, other+"\n\n"+contextPackMarker) || strings.Contains(prompt, "old") {
		t.Fatalf("mixed prompt cleanup = %q; argv=%v", prompt, got)
	}
	if twice := withContextPack(got, inj); !slices.Equal(twice, got) {
		t.Fatalf("second application changed cleaned argv:\n%v\n%v", got, twice)
	}
}
