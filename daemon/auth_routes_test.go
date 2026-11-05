package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Read registrations without invoking handlers or starting a daemon. Include
// optional features and methodless handlers as well as literal POST patterns.
// Unresolved registration expressions fail rather than silently lose coverage.
func registeredWritePatterns(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var trees []*ast.File
	values := map[string]ast.Expr{}
	ranges := map[*ast.Object][]ast.Expr{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		trees = append(trees, file)
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if i < len(n.Values) {
						values[name.Name] = n.Values[i]
					}
				}
			case *ast.RangeStmt:
				name, ok := n.Value.(*ast.Ident)
				list, literal := n.X.(*ast.CompositeLit)
				if ok && literal && name.Obj != nil {
					ranges[name.Obj] = list.Elts
				}
			}
			return true
		})
	}
	var resolve func(ast.Expr, int) (string, bool)
	resolve = func(expr ast.Expr, depth int) (string, bool) {
		if depth > 10 {
			return "", false
		}
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				value, err := strconv.Unquote(e.Value)
				return value, err == nil
			}
		case *ast.Ident:
			return resolve(values[e.Name], depth+1)
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				left, lok := resolve(e.X, depth+1)
				right, rok := resolve(e.Y, depth+1)
				return left + right, lok && rok
			}
		}
		return "", false
	}
	patterns := map[string]bool{}
	for _, file := range trees {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (method.Sel.Name != "HandleFunc" && method.Sel.Name != "Handle") {
				return true
			}
			// log/slog has an unrelated Handler.Handle(ctx, record).
			if method.Sel.Name == "Handle" {
				if receiver, ok := method.X.(*ast.Ident); !ok || receiver.Name != "mux" {
					return true
				}
			}
			candidates := []ast.Expr{call.Args[0]}
			if name, ok := call.Args[0].(*ast.Ident); ok && len(ranges[name.Obj]) > 0 {
				candidates = ranges[name.Obj]
			}
			for _, candidate := range candidates {
				pattern, ok := resolve(candidate, 0)
				if !ok {
					t.Fatalf("unresolved HTTP registration at offset %d", candidate.Pos())
				}
				if method, _, explicit := strings.Cut(pattern, " "); explicit {
					switch method {
					case "GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE":
					default:
						t.Fatalf("unclassified HTTP method in %q", pattern)
					}
				}
				patterns[pattern] = true
			}
			return true
		})
	}
	if !patterns["/api/panes/{id}/io"] || !patterns["GET /api/panes/{id}/native/ws"] ||
		!patterns["POST /api/file/line"] || !patterns[jevBridgePath] || !patterns[modelGatewayPrefix+"/"] {
		t.Fatal("registration inventory is incomplete")
	}
	result := make([]string, 0, len(patterns))
	for pattern := range patterns {
		result = append(result, pattern)
	}
	slices.Sort(result)
	return result
}

func TestRegisteredWritesUseBrowserOriginGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gate := &authGate{token: "test-master"}
	a := &apiServer{gate: gate}
	mux := http.NewServeMux()
	a.routes(mux)
	// These two mounts are in main.go, outside apiServer.routes.
	mux.Handle("/debug/pprof/", a.diagnosticAccess(http.NotFoundHandler()))
	mux.Handle("/", http.NotFoundHandler())
	for _, pattern := range registeredWritePatterns(t) {
		t.Run(pattern, func(t *testing.T) {
			method, path, explicit := strings.Cut(pattern, " ")
			methods := []string{method}
			if !explicit {
				path = pattern
				methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
			}
			parts := strings.Split(path, "/")
			for i, part := range parts {
				if strings.HasPrefix(part, "{") {
					parts[i] = "origin-test"
				}
			}
			path = strings.Join(parts, "/")
			socket := strings.HasSuffix(path, "/io") || strings.HasSuffix(path, "/native/ws")
			if socket {
				methods = []string{http.MethodGet}
			}
			for _, verb := range methods {
				for _, transport := range []string{"direct", "apiHandler", "relay"} {
					// These are the named, actual-transport preview exceptions. Their
					// real handlers and positive/negative rows are exercised below.
					preview := strings.HasPrefix(path, "/api/proxy/") || strings.HasPrefix(path, "/api/preview-serve/")
					if transport == "relay" && preview {
						continue
					}
					for _, header := range []string{"Origin", "Referer"} {
						for _, site := range []string{"", "same-site", "cross-site"} {
							r := httptest.NewRequest(verb, "http://127.0.0.1:41001"+path, strings.NewReader(`{}`))
							r.RemoteAddr = "127.0.0.1:41003"
							r.Header.Set(header, "http://127.0.0.1:41002")
							r.Header.Set("Sec-Fetch-Site", site)
							r.Header.Set("Content-Type", "text/plain")
							r.Header.Set("Authorization", "Bearer test-master")
							if transport == "relay" && site != "" {
								r.Header.Set("X-Forwarded-Prefix", "/m/test-machine")
							}
							if _, actual := mux.Handler(r); actual != pattern {
								t.Fatalf("%s %s resolved %q, want %q", verb, path, actual, pattern)
							}
							if socket {
								r.Header.Set("Upgrade", "websocket")
								r.Header.Set("Connection", "Upgrade")
							}
							var h http.Handler = a.apiHandler(mux)
							if transport == "direct" {
								h = gate.middleware(h)
							}
							if transport == "relay" {
								h = markRelayTransport(h)
							}
							w := httptest.NewRecorder()
							h.ServeHTTP(w, r)
							if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "cross-origin request rejected") {
								t.Fatalf("%s %s %s %s site=%q status=%d body=%s", transport, verb, path, header, site, w.Code, w.Body.String())
							}
						}
					}
				}
			}
		})
	}
}

func TestBrowserOriginAuthority(t *testing.T) {
	cases := []struct {
		name, host, origin, referer, site string
		want                              bool
	}{
		{"own port", "127.0.0.1:41001", "http://127.0.0.1:41001", "", "same-origin", true},
		{"other port", "127.0.0.1:41001", "http://127.0.0.1:41002", "", "", false},
		{"referer port", "127.0.0.1:41001", "", "http://127.0.0.1:41002/path", "", false},
		{"own referer", "127.0.0.1:41001", "", "http://127.0.0.1:41001/path", "", true},
		{"scheme terminates upstream", "localhost:41001", "https://localhost:41001", "", "", true},
		{"host case", "LOCALHOST:41001", "http://localhost:41001", "", "", true},
		{"ipv6", "[::1]:41001", "http://[::1]:41001", "", "", true},
		{"ipv6 port", "[::1]:41001", "http://[::1]:41002", "", "", false},
		{"missing port", "localhost:41001", "http://localhost", "", "", false},
		{"same-site only", "localhost:41001", "", "", "same-site", false},
		{"cross-site only", "localhost:41001", "", "", "cross-site", false},
		{"headerless", "localhost:41001", "", "", "", true},
		{"opaque", "localhost:41001", "null", "", "", false},
		{"malformed", "localhost:41001", "://localhost:41001", "", "", false},
		{"userinfo", "localhost:41001", "http://user@localhost:41001", "", "", false},
		{"origin wins", "localhost:41001", "http://localhost:41002", "http://localhost:41001/", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/api/health", nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Referer", tc.referer)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if got := originProtected(r); got != tc.want {
				t.Fatalf("originProtected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBrowserOriginRequiresTransportForRelayPrefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:41001/api/health", nil)
	r.Header.Set("Origin", "https://relay.example")
	r.Header.Set("X-Forwarded-Prefix", "/m/test-machine")
	if originProtected(r) {
		t.Fatal("a prefix without relay transport was accepted")
	}
	w := httptest.NewRecorder()
	markRelayTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !originProtected(r) {
			t.Error("relay transport with its prefix was refused")
		}
	})).ServeHTTP(w, r)
}
