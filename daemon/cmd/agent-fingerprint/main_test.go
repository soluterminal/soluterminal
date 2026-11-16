package main

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPureRegexpInitializerRequiresResolvedValidConstant(t *testing.T) {
	for _, tc := range []struct {
		expression string
		want       bool
	}{
		{`rx.MustCompile("safe")`, true},
		{`rx.MustCompile("sa" + "fe")`, true},
		{`rx.MustCompile("[")`, false},
		{`rx.MustCompile(pattern())`, false},
		{`local.MustCompile("safe")`, false},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", `package fixture
import rx "regexp"
var _ = rx.Compile
var local = struct{MustCompile func(string) int}{func(string) int { return 1 }}
func pattern() string { return "safe" }
var value = `+tc.expression, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
			if _, err := (&types.Config{Importer: importer.Default()}).Check("fixture", fset, []*ast.File{f}, info); err != nil {
				t.Fatal(err)
			}
			decl := f.Decls[len(f.Decls)-1].(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
			if got := pureRegexpInitializer(decl.Values[0].(*ast.CallExpr), info); got != tc.want {
				t.Fatalf("pure = %v, want %v", got, tc.want)
			}
		})
	}
}

func fixtureDigest(t *testing.T, source string) string {
	t.Helper()
	digest, err := fixtureDigestResult(t, source)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func fixtureDigestResult(t *testing.T, source string) (string, error) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	pkg, err := new(types.Config).Check("fixture", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatal(err)
	}
	return hashClosure(fset, []*ast.File{f}, pkg, info, nil, false)
}

func TestFingerprintPureRegexpExceptionPreservesStartupCoverage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := func(source string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		value, err := fingerprint(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	const source = `package main
import rx "regexp"
var mode string
var index = 0
var daemonPattern = INITIALIZER
func runAgent() {}
func runMuseDriver() {}
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return } }
`
	for _, tc := range []struct {
		name, initializer string
		wantChanged       bool
	}{
		{"unused pure allocation", `rx.MustCompile("safe")`, false},
		{"invalid pattern", `rx.MustCompile("[safe")`, true},
		{"surrounding indexing", `[]*rx.Regexp{rx.MustCompile("safe")}[index]`, true},
		{"surrounding builtin", `append([]*rx.Regexp(nil), rx.MustCompile("safe"))`, true},
		{"surrounding method call", `rx.MustCompile("safe").MatchString(mode)`, true},
		{"global callback", `func() *rx.Regexp { return rx.MustCompile("safe") }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := strings.Replace(source, "INITIALIZER", tc.initializer, 1)
			after := strings.Replace(before, "safe", "other", 1)
			if changed := digest(before) != digest(after); changed != tc.wantChanged {
				t.Fatalf("initializer change tracked = %v, want %v", changed, tc.wantChanged)
			}
		})
	}
	used := strings.Replace(source, "INITIALIZER", `rx.MustCompile("safe")`, 1)
	used = strings.Replace(used, "func runAgent() {}", "func runAgent() { daemonPattern.MatchString(mode) }", 1)
	if digest(used) == digest(strings.Replace(used, "safe", "other", 1)) {
		t.Fatal("reachable regexp dependency omitted")
	}
}

func TestFingerprintTracksTransitiveRuntimeNotDaemonHandlers(t *testing.T) {
	const source = `package main
var mode string
type writer struct{}
func (*writer) Write() int { return helper() }
func helper() int { return 42 }
func runAgent() int { var w interface{Write() int} = &writer{}; return w.Write() }
func runMuseDriver() {}
func initLogging() {}
func main() { initLogging(); if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return }; daemon() }
func daemon() int { return 10 }
func init() { helper() }
var initialized = setup()
func setup() int { return 20 }
`
	base := fixtureDigest(t, source)
	if got := fixtureDigest(t, strings.Replace(source, "return 10", "return 11", 1)); got != base {
		t.Fatal("daemon-only handler changed pane fingerprint")
	}
	if got := fixtureDigest(t, "\n\n"+source); got != base {
		t.Fatal("source line movement changed fingerprint")
	}
	for _, change := range [][2]string{{"return 42", "return 43"}, {"return helper()", "return 3"}, {"return 20", "return 21"}, {"initLogging(); if", "initLogging(); helper(); if"}} {
		if fixtureDigest(t, strings.Replace(source, change[0], change[1], 1)) == base {
			t.Fatalf("missed runtime dependency change: %v", change)
		}
	}
}

func TestFingerprintTracksNewHelperAndConstGroup(t *testing.T) {
	source := `package main
const ( a = iota; b )
func runAgent() int { return helper() }
func runMuseDriver() {}
var mode string
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return } }
func helper() int { return b }
`
	base := fixtureDigest(t, source)
	if fixtureDigest(t, strings.Replace(source, "a = iota", "a = iota + 1", 1)) == base {
		t.Fatal("missed implicit const group change")
	}
	next := strings.Replace(source, "return b", "return newHelper()", 1) + "\nfunc newHelper() int { return b }\n"
	if fixtureDigest(t, next) == fixtureDigest(t, strings.Replace(next, "return b", "return a", 1)) {
		t.Fatal("new helper dependency not followed")
	}
}

func TestFingerprintConservativelyTracksGlobalsExceptConstantMetadata(t *testing.T) {
	source := `package main
var webFS = "UI bundle one"
var embeddedWebProvenance = parseWebProvenance(webFS, "version")
func parseWebProvenance(data, name string) string { return data + name }
type unused struct{}
func (*unused) Write() int { return 10 }
var _ interface{ Write() int } = (*unused)(nil)
var callback = func() int { return 20 }
func runAgent() int { return 1 }
func runMuseDriver() {}
var mode string
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return } }
`
	base := fixtureDigest(t, source)
	if fixtureDigest(t, strings.Replace(source, "UI bundle one", "UI bundle two", 1)) != base {
		t.Fatal("constant UI metadata entered runtime")
	}
	for _, change := range [][2]string{{"return 10", "return 11"}, {"return 20", "return 21"}, {"data + name", "data"}} {
		if fixtureDigest(t, strings.Replace(source, change[0], change[1], 1)) == base {
			t.Fatalf("nonconstant global initializer dependency omitted: %v", change)
		}
	}
}

func TestFingerprintTracksBothPaneDispatches(t *testing.T) {
	const source = `package main
var mode string
func runAgent() {}
func runMuseDriver() int { return 10 }
func main() {
 if mode == "agent" { runAgent(); return }
 if mode == "other" { return }
 if mode == "muse-driver" { runMuseDriver(); return }
 daemon()
}
func daemon() {}
`
	base := fixtureDigest(t, source)
	for _, change := range [][2]string{{"muse-driver", "muse-changed"}, {"return 10", "return 11"}, {"other", "muse-driver"}} {
		if fixtureDigest(t, strings.Replace(source, change[0], change[1], 1)) == base {
			t.Fatalf("missed pane dispatch change: %v", change)
		}
	}
	for name, changed := range map[string]string{
		"missing agent dispatch": strings.Replace(source, "runAgent(); return", "return", 1),
		"unexecuted callback":    strings.Replace(source, "runMuseDriver(); return", "_ = func() { runMuseDriver() }; return", 1),
		"dispatch via closure": strings.Replace(strings.Replace(source, "runMuseDriver(); return", "start(); return", 1),
			"func main() {", "func main() { start := func() { runMuseDriver() };", 1),
		"dispatch via helper": strings.Replace(strings.Replace(source, "runMuseDriver(); return", "startMuse(); return", 1),
			"func daemon() {}", "func daemon() {}\nfunc startMuse() { runMuseDriver() }", 1),
		"referenced elsewhere only": strings.Replace(strings.Replace(source, "runMuseDriver(); return", "return", 1),
			"func daemon() {}", "func daemon() { runMuseDriver() }", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixtureDigestResult(t, changed); err == nil {
				t.Fatal("unrecognized pane dispatch accepted")
			}
		})
	}
}

// The Focus removal first deletes main's muse-driver dispatch (the Muse driver
// itself stays one more step), then the driver. Once nothing refers to
// runMuseDriver, no pane process can start it: it is not a root, and the
// startup prefix ends at the runAgent dispatch.
func TestFingerprintMuseRootOnlyWhileReferenced(t *testing.T) {
	const withMuse = `package main
var mode string
func runAgent() int { return 1 }
func runMuseDriver() int { return 10 }
func initLogging() {}
func main() {
 initLogging()
 if mode == "agent" { runAgent(); return }
 if mode == "router" { return }
 if mode == "muse-driver" { runMuseDriver(); return }
 daemon()
}
func daemon() int { return 20 }
`
	// Today's dispatch shape: the driver is a root and the prefix runs through
	// its dispatch.
	base := fixtureDigest(t, withMuse)
	if fixtureDigest(t, strings.Replace(withMuse, "return 10", "return 11", 1)) == base {
		t.Fatal("Muse driver change missed while main dispatches it")
	}
	if fixtureDigest(t, strings.Replace(withMuse, `"router"`, `"router2"`, 1)) == base {
		t.Fatal("startup prefix before the Muse dispatch missed")
	}
	if fixtureDigest(t, strings.Replace(withMuse, "return 20", "return 21", 1)) != base {
		t.Fatal("daemon-only code entered the fingerprint")
	}

	// The step that drops main's dispatch but keeps the driver: no longer a root.
	undispatched := strings.Replace(withMuse, ` if mode == "muse-driver" { runMuseDriver(); return }
`, "", 1)
	dropped := fixtureDigest(t, undispatched)
	if fixtureDigest(t, strings.Replace(undispatched, "return 10", "return 11", 1)) != dropped {
		t.Fatal("unreferenced Muse driver still a root")
	}
	if fixtureDigest(t, strings.Replace(undispatched, "return 1 }", "return 2 }", 1)) == dropped {
		t.Fatal("agent change missed without a Muse dispatch")
	}
	if fixtureDigest(t, strings.Replace(undispatched, `"router"`, `"router2"`, 1)) != dropped {
		t.Fatal("prefix after the agent dispatch entered the fingerprint without a Muse dispatch")
	}

	// The driver deleted too.
	gone := strings.Replace(undispatched, "func runMuseDriver() int { return 10 }\n", "", 1)
	if fixtureDigest(t, gone) != dropped {
		t.Fatal("deleting an unreferenced Muse driver changed the fingerprint")
	}
	if fixtureDigest(t, strings.Replace(gone, "return 1 }", "return 2 }", 1)) == dropped {
		t.Fatal("agent change missed after the Muse driver is gone")
	}

	// runAgent stays mandatory with or without the driver.
	for name, src := range map[string]string{
		"no agent dispatch, driver gone":   strings.Replace(gone, `if mode == "agent" { runAgent(); return }`, "", 1),
		"no agent root, driver gone":       strings.ReplaceAll(gone, "runAgent", "otherAgent"),
		"agent falls through, driver gone": strings.Replace(gone, "runAgent(); return", "runAgent()", 1),
		"Muse dispatch falls through":      strings.Replace(withMuse, "runMuseDriver(); return", "runMuseDriver()", 1),
		"Muse dispatch unconditional":      strings.Replace(withMuse, `if mode == "muse-driver" { runMuseDriver(); return }`, "runMuseDriver()", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixtureDigestResult(t, src); err == nil {
				t.Fatal("pane dispatch accepted without a recognized terminating branch")
			}
		})
	}
}

// The Codex terminal bridge is dispatched between the agent and Muse
// dispatches. Removing the Muse dispatch must not end the startup prefix
// before it: while runCodexTerminal is referenced, main must dispatch it
// through an audited terminating branch (os.Exit counts), and the startup
// prefix runs through that dispatch. It is covered through the prefix, not
// listed as a root.
func TestFingerprintKeepsCodexTerminalBridgeWithoutMuse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result := func(source string) (string, error) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		return fingerprint(dir, false)
	}
	digest := func(source string) string {
		t.Helper()
		value, err := result(source)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	const withMuse = `package main
import "os"
var mode string
func runAgent() int { return 1 }
func runCodexTerminal() int { return 5 }
func runMuseDriver() int { return 10 }
func main() {
	if mode == "agent" { runAgent(); return }
	if mode == "router" { return }
	if mode == "codex-terminal" { os.Exit(runCodexTerminal()) }
	if mode == "muse-driver" { runMuseDriver(); return }
	daemon()
}
func daemon() int { return 20 }
`
	noMuseDispatch := strings.Replace(withMuse, "\tif mode == \"muse-driver\" { runMuseDriver(); return }\n", "", 1)
	noMuse := strings.Replace(noMuseDispatch, "func runMuseDriver() int { return 10 }\n", "", 1)
	for name, source := range map[string]string{"today": withMuse, "Muse dispatch removed": noMuseDispatch, "Muse driver removed": noMuse} {
		t.Run(name, func(t *testing.T) {
			base := digest(source)
			if digest(strings.Replace(source, "return 5", "return 6", 1)) == base {
				t.Fatal("Codex terminal bridge change missed")
			}
			if digest(strings.Replace(source, `"router"`, `"router2"`, 1)) == base {
				t.Fatal("startup prefix before the Codex dispatch missed")
			}
			if digest(strings.Replace(source, "return 20", "return 21", 1)) != base {
				t.Fatal("daemon-only code entered the fingerprint")
			}
		})
	}
	// With no Codex bridge at all, the agent alone is the root.
	if _, err := result(strings.Replace(strings.Replace(noMuse, "func runCodexTerminal() int { return 5 }\n", "", 1),
		"\tif mode == \"codex-terminal\" { os.Exit(runCodexTerminal()) }\n", "\t_ = os.Args\n", 1)); err != nil {
		t.Fatalf("tree without a Codex bridge refused: %v", err)
	}
	for name, source := range map[string]string{
		"Codex dispatch falls through": strings.Replace(noMuse, "os.Exit(runCodexTerminal())", "runCodexTerminal()", 1),
		"Codex dispatch via helper": strings.Replace(strings.Replace(noMuse, "os.Exit(runCodexTerminal())", "os.Exit(startCodex())", 1),
			"func daemon() int { return 20 }", "func daemon() int { return 20 }\nfunc startCodex() int { return runCodexTerminal() }", 1),
		"Codex referenced but not dispatched": strings.Replace(strings.Replace(noMuse, "\tif mode == \"codex-terminal\" { os.Exit(runCodexTerminal()) }\n", "", 1),
			"func daemon() int { return 20 }", "func daemon() int { return runCodexTerminal() }", 1),
		"exit in the condition": strings.Replace(noMuse, `if mode == "codex-terminal" { os.Exit(runCodexTerminal()) }`, `if runCodexTerminal() == 0 { os.Exit(0) }`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := result(source); err == nil {
				t.Fatal("Codex bridge dispatch accepted without a recognized terminating branch")
			}
		})
	}
}

// todayDispatchShape mirrors the order of daemon/main.go's dispatches before
// the Focus removal: a pre-logging exit, initLogging, agent, router, the Codex
// terminal bridge, the Muse driver, then a retired command and the daemon. It
// imports nothing, so its digest depends on this tool alone, not on the Go
// toolchain's standard-library source. (The real tree, where the Codex bridge
// ends in os.Exit, is checked with the real tool in the pull request.)
const todayDispatchShape = `package main
var mode string
func initLogging() {}
func runAgent() int { return 1 }
func runRouter() {}
func runCodexTerminal() int { return 5 }
func runMuseDriver() {}
func main() {
	if mode == "version" { return }
	initLogging()
	if mode == "agent" {
		if code := runAgent(); code != 0 {
			mode = "exit"
		}
		return
	}
	if mode == "router" { runRouter(); return }
	if mode == "codex-terminal" { runCodexTerminal(); return }
	// the long-lived child of a native Muse pane
	if mode == "muse-driver" { runMuseDriver(); return }
	if mode == "gc" { return }
	daemon()
}
func daemon() int { return 20 }
`

// The digest of todayDispatchShape computed by the tool before the Muse root
// and the Codex dispatch became required-while-referenced. Equal means the
// roots and the startup order for today's shape are unchanged.
const todayDispatchShapeDigest = "4f1c12bee234f0457387e6fd528a294d31f803da2d2f4ce307462187e17f3514"

func TestFingerprintTodayDispatchShapeUnchanged(t *testing.T) {
	if got := fixtureDigest(t, todayDispatchShape); got != todayDispatchShapeDigest {
		t.Fatalf("today's dispatch shape digest = %s, want %s", got, todayDispatchShapeDigest)
	}
}

func TestFingerprintRejectsPaneDispatchThatCanFallThrough(t *testing.T) {
	const source = `package main
var mode string
func runAgent() int { return 0 }
func runMuseDriver() {}
func main() {
 if mode == "agent" { runAgent(); return }
 if mode == "muse-driver" { runMuseDriver(); return }
 daemon()
}
func daemon() int { return 10 }
`
	for name, changed := range map[string]string{
		"missing return":     strings.Replace(source, "runMuseDriver(); return", "runMuseDriver()", 1),
		"condition call":     strings.Replace(source, `if mode == "agent" { runAgent(); return }`, "if runAgent() == 1 { return }", 1),
		"unconditional call": strings.Replace(source, `if mode == "muse-driver" { runMuseDriver(); return }`, "runMuseDriver()", 1),
		"escaping goto":      strings.Replace(strings.Replace(source, "runMuseDriver(); return", "runMuseDriver(); goto after; return", 1), " daemon()", "after: daemon()", 1),
		"prefix goto":        strings.Replace(strings.Replace(source, "func main() {", "func main() { goto after;", 1), " daemon()", "after: daemon()", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixtureDigestResult(t, changed); err == nil {
				t.Fatal("pane dispatch accepted without a recognized terminating branch")
			}
		})
	}
}

func TestFingerprintTracksExecutableBuiltinInitializers(t *testing.T) {
	const source = `package main
var size = 0
var unused = make([]byte, size)
var data = []byte{0}
var copied = copy(data, "a")
var appended = append(data, byte(1))
func runAgent() int { return int(data[0]) }
func runMuseDriver() {}
var mode string
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return } }
`
	base := fixtureDigest(t, source)
	for _, change := range [][2]string{{"var size = 0", "var size = -1"}, {`copy(data, "a")`, `copy(data, "b")`}, {"byte(1)", "byte(2)"}} {
		if fixtureDigest(t, strings.Replace(source, change[0], change[1], 1)) == base {
			t.Fatalf("missed executable builtin initializer change: %v", change)
		}
	}
}

func TestFingerprintTracksNonCallInitializers(t *testing.T) {
	const source = `package main
var data = []byte{0}
var index = 0
var unused = data[index]
func runAgent() {}
func runMuseDriver() {}
var mode string
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return } }
`
	if fixtureDigest(t, source) == fixtureDigest(t, strings.Replace(source, "var index = 0", "var index = 1", 1)) {
		t.Fatal("package indexing initializer can panic before main without invalidating runtime")
	}
}

func TestFingerprintTracksInitializationFromDaemonOnlyImport(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "startup"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.22\n")
	write("main.go", `package main
import "fixture/startup"
func runAgent() {}
func runMuseDriver() {}
var mode string
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return }; daemon() }
func daemon() { startup.DaemonOnly() }
`)
	const dependency = `package startup
func init() { println("old-init") }
func DaemonOnly() {}
`
	write("startup/startup.go", dependency)
	before, err := fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	write("startup/startup.go", strings.Replace(dependency, "old-init", "new-init", 1))
	after, err := fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("named dependency initializer changed without invalidating pane runtime")
	}
}

func TestExprIsPureDataClassifiesInitExpressions(t *testing.T) {
	for _, tc := range []struct {
		expression string
		want       bool
	}{
		{`"literal"`, true},
		{`someIdent`, true},
		{`pkg.Constant`, true},
		{`[]string{"a", "b"}`, true},
		{`Model{Name: "claude", Rows: []Row{{Match: "sonnet", Tier: TierStandard}}}`, true},
		{`&Spec{Name: "claude"}`, true},
		{`256 << 10`, true},
		{`1 + 2*3`, true},
		{`-1`, true},
		{`!enabled`, true},
		{`build()`, false},
		{`pkg.MustBuild("x")`, false},
		{`rows[0]`, false},
		{`rows[1:]`, false},
		{`total / count`, false},
		{`total % count`, false},
		{`value.(string)`, false},
		{`*pointer`, false},
		{`<-channel`, false},
		{`func() int { return 1 }`, false},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", `package fixture
var someIdent = 1
var enabled = true
var rows = []int{1, 2, 3}
var total = 4
var count = 2
var pointer = new(int)
var channel = make(chan int)
var pkg = struct{ Constant string; MustBuild func(string) int }{Constant: "c", MustBuild: func(string) int { return 1 }}
type Row struct{ Match string; Tier int }
type Model struct{ Name string; Rows []Row }
type Spec struct{ Name string }
func build() int { return 1 }
var value interface{} = "s"
var probe = `+tc.expression+`
`, 0)
			if err != nil {
				t.Fatal(err)
			}
			decl := f.Decls[len(f.Decls)-1].(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
			if got := exprIsPureData(decl.Values[0]); got != tc.want {
				t.Fatalf("pure data = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPackageInitIsPureFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   bool
	}{
		{"empty package", `package fixture`, true},
		{"pure data table", `package fixture
type Row struct{ Match string }
type Model struct{ Rows []Row }
var catalog = Model{Rows: []Row{{Match: "sonnet"}}}
var names = []string{"a", "b"}
var spec = &Row{Match: "x"}
const limit = 256 << 10
func Lookup(id string) *Row { return spec }
`, true},
		{"init function", `package fixture
var x = 1
func init() { println("side effect") }
`, false},
		{"call initializer", `package fixture
var x = build()
func build() int { return 1 }
`, false},
		{"index initializer", `package fixture
var rows = []int{1}
var first = rows[0]
`, false},
		{"division initializer", `package fixture
var a = 4
var b = 2
var ratio = a / b
`, false},
		{"receive initializer", `package fixture
var ch = make(chan int)
var v = <-ch
`, false},
		{"func literal initializer", `package fixture
var f = func() int { return 1 }
`, false},
		{"parse error", `package fixture
var x = `, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			got := packageInitIsPure(listedPackage{ImportPath: "fixture", Dir: dir, GoFiles: []string{"fixture.go"}})
			if got != tc.want {
				t.Fatalf("packageInitIsPure = %v, want %v", got, tc.want)
			}
		})
	}
	// Non-Go inputs are never audited.
	if packageInitIsPure(listedPackage{ImportPath: "fixture", CgoFiles: []string{"cgo.go"}}) {
		t.Fatal("a package with cgo inputs must not be treated as pure")
	}
	if packageInitIsPure(listedPackage{ImportPath: "fixture", SFiles: []string{"asm.s"}}) {
		t.Fatal("a package with assembly inputs must not be treated as pure")
	}
}

// A pure-data package that only daemon code names cannot change pane behavior:
// its initializers are inert and no pane reaches it. A change to its data must
// therefore leave the pane-runtime digest alone, while the same change must be
// tracked as soon as a pane dispatch names the package.
func TestFingerprintDropsPureUnreachedPackageButKeepsPaneReachable(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "catalog"), 0700); err != nil {
		t.Fatal(err)
	}
	write("go.mod", "module fixture\n\ngo 1.22\n")
	const catalog = `package catalog
var registry = []string{"one"}
func Label() string { return registry[0] }
`
	write("catalog/catalog.go", catalog)
	// The catalog is only named from daemon(), which is past both pane
	// dispatches and therefore outside the pane runtime.
	write("main.go", `package main
import "fixture/catalog"
var mode string
func runAgent() {}
func runMuseDriver() {}
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return }; daemon() }
func daemon() { catalog.Label() }
`)
	before, err := fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	write("catalog/catalog.go", strings.Replace(catalog, `"one"`, `"two"`, 1))
	if after, err := fingerprint(dir, false); err != nil {
		t.Fatal(err)
	} else if before != after {
		t.Fatal("a pure-data package no pane reaches changed the pane-runtime digest")
	}

	// Once a pane dispatch names it, the same data drives pane behavior and the
	// change must be tracked.
	write("main.go", `package main
import "fixture/catalog"
var mode string
func runAgent() int { return len(catalog.Label()) }
func runMuseDriver() {}
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return }; daemon() }
func daemon() { catalog.Label() }
`)
	before, err = fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	write("catalog/catalog.go", strings.Replace(catalog, `"one"`, `"three"`, 1))
	if after, err := fingerprint(dir, false); err != nil {
		t.Fatal(err)
	} else if before == after {
		t.Fatal("a pane-reachable catalog change was omitted from the pane-runtime digest")
	}
}

// TestFingerprintKeepsPaneReachableCatalogEvenWhenTheEditLooksUnrelated pins the
// boundary the pure-initializer omission must not cross, using the real shape: a
// pure-data registry of provider structs that a pane-reachable function selects
// from.
//
// The Muse pane resolves its own binary through providers.ByRuntimeID, so the
// shared/providers catalog is pane code. An edit to one entry ("deepseek"'s
// model list) cannot change the facts another entry ("muse") returns, but seeing
// that needs field-level value tracking through the registry, which this tool
// does not do. Hashing the whole package is therefore the fail-closed answer;
// the alternative silently keeps panes on a catalog they may still read. If this
// test ever starts failing because the edit no longer changes the digest, that
// is a behavior change rather than a cleanup — prove the field-level argument
// first.
func TestFingerprintKeepsPaneReachableCatalogEvenWhenTheEditLooksUnrelated(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "catalog"), 0700); err != nil {
		t.Fatal(err)
	}
	write("go.mod", "module fixture\n\ngo 1.22\n")
	// The registry's initializers are composite literals only, so the package is
	// "pure" under the omission rule.
	const catalog = `package catalog
type Binary struct{ Name string }
type Provider struct {
	Slug   string
	Models []string
	Binary *Binary
}
var registry = []Provider{
	{Slug: "muse", Binary: &Binary{Name: "muse"}},
	{Slug: "deepseek", Models: []string{"deepseek-flash"}},
}
func ByRuntimeID(id string) (Provider, bool) {
	for _, p := range registry {
		if p.Slug == id {
			return p, true
		}
	}
	return Provider{}, false
}
`
	write("catalog/catalog.go", catalog)
	// runMuseDriver is a pane root and reads the catalog, exactly as
	// resolveMuseBin -> resolveProviderBin -> providers.ByRuntimeID does.
	write("main.go", `package main
import "fixture/catalog"
var mode string
func runAgent() {}
func runMuseDriver() {
	p, _ := catalog.ByRuntimeID("muse")
	if p.Binary != nil {
		_ = p.Binary.Name
	}
}
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return } }
`)
	before, err := fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// The edited entry is the one no pane reads; the digest must still move.
	write("catalog/catalog.go", strings.Replace(catalog, "deepseek-flash", "deepseek-v4", 1))
	after, err := fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("a pane-reachable pure-data catalog edit was omitted; fail closed instead")
	}

	// The same catalog, but nothing pane-reachable names it: now it is inert and
	// the edit must leave the digest alone.
	write("catalog/catalog.go", catalog)
	write("main.go", `package main
import "fixture/catalog"
var mode string
func runAgent() {}
func runMuseDriver() {}
func main() { if mode == "agent" { runAgent(); return }; if mode == "muse" { runMuseDriver(); return }; daemon() }
func daemon() { catalog.ByRuntimeID("muse") }
`)
	before, err = fingerprint(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	write("catalog/catalog.go", strings.Replace(catalog, "deepseek-flash", "deepseek-v4", 1))
	if after, err := fingerprint(dir, false); err != nil {
		t.Fatal(err)
	} else if before != after {
		t.Fatal("a pure catalog no pane reaches changed the pane-runtime digest")
	}
}
