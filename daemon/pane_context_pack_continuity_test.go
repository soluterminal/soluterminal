package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextPackCopySitesStayPairedWithRole(t *testing.T) {
	for _, tc := range []struct {
		file     string
		typeName string
	}{
		{"pane_resume.go", "SpawnOpts"},
		{"api_migrate.go", "SpawnOpts"},
		{"api_migrate.go", "PaneDef"},
		{"mcp.go", "PaneDef"},
	} {
		t.Run(tc.file+"/"+tc.typeName, func(t *testing.T) {
			parsed, err := parser.ParseFile(token.NewFileSet(), tc.file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			paired := 0
			ast.Inspect(parsed, func(node ast.Node) bool {
				lit, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				ident, ok := lit.Type.(*ast.Ident)
				if !ok || ident.Name != tc.typeName {
					return true
				}
				fields := map[string]bool{}
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							fields[key.Name] = true
						}
					}
				}
				if !fields["Role"] {
					return true
				}
				paired++
				if !fields["ContextPack"] {
					t.Errorf("%s literal carrying Role does not carry ContextPack", tc.typeName)
				}
				return true
			})
			if paired == 0 {
				t.Fatalf("found no %s literal carrying Role", tc.typeName)
			}
		})
	}
}

func TestContextPackPreservedAcrossRespawnPaths(t *testing.T) {
	const pack = "/packs/strategy"

	t.Run("pure resume builder", func(t *testing.T) {
		snap := PaneSnapshot{
			ID:              "pane-builders",
			Cwd:             "/tmp",
			ClaudeSessionID: "session-builders",
			ContextPack:     pack,
		}
		if got := resumeSpawnOpts(snap, 0, 0).ContextPack; got != pack {
			t.Fatalf("resumeSpawnOpts ContextPack = %q, want %q", got, pack)
		}
	})

	t.Run("clean migration", func(t *testing.T) {
		var got SpawnOpts
		api := &apiServer{spawnPane: func(opts SpawnOpts) (*Pane, error) {
			got = opts
			return transactionTestPane("pane-migrated-pack"), nil
		}}
		p := transactionTestPane("pane-migrate-pack")
		p.Cmd = []string{"claude"}
		p.ContextPack = pack
		if _, err := api.doMigrateSibling(p); err != nil {
			t.Fatal(err)
		}
		if got.ContextPack != pack {
			t.Fatalf("migration ContextPack = %q, want %q", got.ContextPack, pack)
		}
	})
}

func TestCreatePaneSpecialWorkspacePackInheritanceIsExplicitlyAbsent(t *testing.T) {
	pack := makeContextPack(t, "AGENTS.md")
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"strategy", "_current"} {
		if err := store.Put(&Workspace{Name: name, PaneIds: []string{}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetContextPack(name, pack); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"omitted workspace", `{"cmd":["claude"]}`},
		{"autosave scratchpad", `{"cmd":["claude"],"workspace":"_current"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got SpawnOpts
			api := &apiServer{ws: store, spawnPane: func(opts SpawnOpts) (*Pane, error) {
				got = opts
				return transactionTestPane("pane-special-workspace"), nil
			}}
			rec := httptest.NewRecorder()
			api.createPane(rec, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(tc.body)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d; body=%q", rec.Code, rec.Body.String())
			}
			if got.ContextPack != "" {
				t.Fatalf("ContextPack = %q, want empty without a named workspace target", got.ContextPack)
			}
		})
	}
}
