// agent-fingerprint hashes the code reachable by the long-lived pane wrapper,
// not the HTTP daemon/UI that happens to share its executable. It uses compiler
// name resolution, so adding a helper automatically brings that helper into the
// fingerprint. Unknown/uninstrumented binaries remain subject to inode checks.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type listedPackage struct {
	Dir, ImportPath, Export                                                   string
	GoFiles, CgoFiles, SFiles, CFiles, HFiles, SysoFiles, EmbedFiles, Imports []string
	Error                                                                     *struct{ Err string }
}

func main() {
	root := flag.String("root", ".", "daemon source directory")
	detail := flag.Bool("detail", false, "print covered declarations on stderr")
	flag.Parse()
	digest, err := fingerprint(*root, *detail)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(digest)
}

func fingerprint(root string, detail bool) (string, error) {
	// -e lets type analysis run before an unrelated web bundle is built. We
	// still type-check all Go code and require every imported export archive;
	// a reachable missing embed is rejected explicitly below.
	cmd := exec.Command("go", "list", "-e", "-export", "-deps", "-json", ".")
	cmd.Dir = root
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GOOS=") && !strings.HasPrefix(kv, "GOARCH=") && !strings.HasPrefix(kv, "CGO_ENABLED=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list agent build inputs: %w: %s", err, stderr.String())
	}
	packages := map[string]listedPackage{}
	var mainPkg listedPackage
	dec := json.NewDecoder(bytes.NewReader(output))
	for dec.More() {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			return "", err
		}
		packages[p.ImportPath] = p
		mainPkg = p
	}
	if len(mainPkg.CgoFiles)+len(mainPkg.SFiles)+len(mainPkg.CFiles)+len(mainPkg.SysoFiles) != 0 {
		return "", fmt.Errorf("agent fingerprint requires an audit for non-Go main-package code")
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range mainPkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(mainPkg.Dir, name), nil, parser.ParseComments)
		if err != nil {
			return "", err
		}
		files = append(files, f)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Sizes: types.SizesFor("gc", "amd64"), Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(packages[path].Export)
	})}
	pkg, err := conf.Check(mainPkg.ImportPath, fset, files, info)
	if err != nil {
		return "", fmt.Errorf("resolve agent dependencies: %w", err)
	}
	return hashClosure(fset, files, pkg, info, packages, detail)
}

func hashClosure(fset *token.FileSet, files []*ast.File, pkg *types.Package, info *types.Info, packages map[string]listedPackage, detail bool) (string, error) {
	for _, name := range []string{"main", "runAgent"} {
		if _, ok := pkg.Scope().Lookup(name).(*types.Func); !ok {
			return "", fmt.Errorf("%s function root is missing; audit fingerprint roots", name)
		}
	}
	// The Muse driver is a pane root, and the Codex terminal bridge a required
	// pane dispatch, while anything in the package refers to them
	// (runMuseDriver, runCodexTerminal): then main must dispatch each through an
	// audited terminating branch, and a dispatch hidden in a closure or a helper
	// is refused. The startup prefix runs through the last required dispatch,
	// which is how the bridge is covered (it is reached from main's prefix, not
	// listed as a root, so today's startup order is unchanged). Once nothing
	// refers to the Muse driver (the Focus removal deletes main's muse-driver
	// dispatch, then the driver), no pane process can start it and it is not a
	// root.
	museRequired, codexRequired := referencedFunc(pkg, info, "runMuseDriver"), referencedFunc(pkg, info, "runCodexTerminal")
	decls := map[types.Object]ast.Node{}
	var roots []ast.Node
	var webMetadata ast.Node
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				decls[info.Defs[d.Name]] = d
				if d.Recv == nil && (d.Name.Name == "runAgent" || (museRequired && d.Name.Name == "runMuseDriver") || d.Name.Name == "initLogging" || d.Name.Name == "init") {
					roots = append(roots, d)
				}
				if d.Recv == nil && d.Name.Name == "main" {
					// Every long-lived pane mode passes through main. Include every
					// statement through the last pane dispatch, including intervening
					// code, but omit the suffix only for audited terminating if
					// branches. Refuse unfamiliar control flow instead of omitting
					// live code.
					foundAgent, foundMuse, foundCodex := false, false, false
					for i, stmt := range d.Body.List {
						if containsGoto(stmt) {
							return "", fmt.Errorf("main pane startup contains goto; audit fingerprint roots")
						}
						callsAgent, callsMuse, callsCodex := paneDispatchCalls(stmt, pkg, info)
						if (callsAgent || callsMuse || callsCodex) && !terminatingPaneDispatch(stmt, pkg, info) {
							return "", fmt.Errorf("main pane dispatch must be a terminating if body; audit fingerprint roots")
						}
						foundAgent, foundMuse, foundCodex = foundAgent || callsAgent, foundMuse || callsMuse, foundCodex || callsCodex
						if foundAgent && (foundMuse || !museRequired) && (foundCodex || !codexRequired) {
							copyFn, copyBody := *d, *d.Body
							copyBody.List = d.Body.List[:i+1]
							copyFn.Body = &copyBody
							roots = append(roots, &copyFn)
							break
						}
					}
					if !foundAgent {
						return "", fmt.Errorf("main must dispatch runAgent; audit fingerprint roots")
					}
					if museRequired && !foundMuse {
						return "", fmt.Errorf("runMuseDriver is referenced, so main must dispatch it; audit fingerprint roots")
					}
					if codexRequired && !foundCodex {
						return "", fmt.Errorf("runCodexTerminal is referenced, so main must dispatch it; audit fingerprint roots")
					}
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						decls[info.Defs[s.Name]] = s
					case *ast.ValueSpec:
						// Package initializers run even for otherwise-unused globals.
						// Compiler-proven constants, a whole-expression validated regexp
						// allocation, and a provably pure data literal can be omitted.
						// Other calls, indexing, division and receives can affect
						// startup without the resulting variable ever being referenced
						// by a pane.
						if d.Tok == token.VAR {
							// This pure parser reads only the daemon's version/source
							// label. Like main.version, it is provenance, not pane code.
							if len(s.Names) == 1 && s.Names[0].Name == "embeddedWebProvenance" {
								webMetadata = s
							}
							for _, value := range s.Values {
								if call, ok := value.(*ast.CallExpr); ok && pureRegexpInitializer(call, info) {
									continue
								}
								if info.Types[value].Value != nil {
									continue
								}
								// A pure composite literal cannot panic, block or
								// observe anything. It only affects a pane when pane
								// code names the variable, which the roots walk below
								// already captures.
								if exprIsPureData(value) {
									continue
								}
								roots = append(roots, s)
								break
							}
						}
						for _, n := range s.Names {
							// Const groups retain iota and implicit-expression semantics.
							if d.Tok == token.CONST {
								decls[info.Defs[n]] = d
							} else {
								decls[info.Defs[n]] = s
							}
						}
					}
				}
			}
		}
	}
	seen := map[ast.Node]bool{}
	external := map[string]bool{}
	// paneReached records external packages named by pane code itself: the pane
	// dispatches, the startup prefix, init functions and initLogging. Those are
	// hashed in full because their functions run inside a pane.
	paneReached := map[string]bool{}
	// A nonconstant global initializer runs before main, so its own code is
	// hashed (it is a root). The data it reads from another package only changes
	// pane behavior when pane code reaches that package too, which the pane roots
	// mark first. A package whose own initialization is impure carries real
	// startup side effects and is added independently below.
	inVarInitializer := false
	var visitObject func(types.Object)
	var visitNode func(ast.Node)
	visitObject = func(obj types.Object) {
		if obj == nil || obj.Pkg() == nil {
			return
		}
		if obj.Pkg() != pkg {
			if !inVarInitializer {
				external[obj.Pkg().Path()] = true
				paneReached[obj.Pkg().Path()] = true
			}
			return
		}
		if n := decls[obj]; n != nil {
			visitNode(n)
		}
		// Interface calls and method values must include implementations, not
		// just explicitly-called methods of a concrete reachable type.
		if tn, ok := obj.(*types.TypeName); ok {
			if named, ok := tn.Type().(*types.Named); ok {
				for i := 0; i < named.NumMethods(); i++ {
					if n := decls[named.Method(i)]; n != nil {
						visitNode(n)
					}
				}
			}
		}
	}
	visitNode = func(n ast.Node) {
		if seen[n] {
			return
		}
		seen[n] = true
		ast.Inspect(n, func(child ast.Node) bool {
			if id, ok := child.(*ast.Ident); ok {
				// Hash the provenance parser and its initializer, but not the
				// unrelated UI bytes supplying its version/source labels.
				if n == webMetadata && id.Name == "webFS" {
					return true
				}
				visitObject(info.Uses[id])
			}
			return true
		})
	}
	// Pane roots first. They decide which packages pane code names, so a global
	// initializer that a pane root also reaches stays attributed to the pane
	// even though the initializer pass runs second.
	for _, n := range roots {
		if _, isVar := n.(*ast.ValueSpec); isVar {
			continue
		}
		inVarInitializer = false
		visitNode(n)
	}
	for _, n := range roots {
		if _, isVar := n.(*ast.ValueSpec); !isVar {
			continue
		}
		inVarInitializer = true
		visitNode(n)
	}
	inVarInitializer = false
	// Go runs the initializers of every transitively imported package before
	// main, so an impure one always contributes to startup. A package whose own
	// initialization is provably pure data cannot affect a pane that never names
	// it and is dropped, which is what keeps a change to an unrelated static
	// catalog from invalidating every pane. Anything uncertain stays in and is
	// hashed exactly as before.
	pure := map[string]bool{}
	for path, p := range packages {
		if path == pkg.Path() || path == "unsafe" {
			continue
		}
		result, ok := pure[path]
		if !ok {
			result = packageInitIsPure(p)
			pure[path] = result
		}
		if !result {
			external[path] = true
		}
	}
	parts := map[string][]byte{}
	var startup bytes.Buffer
	for _, n := range roots {
		if err := format.Node(&startup, fset, n); err != nil {
			return "", err
		}
		startup.WriteByte(0)
	}
	parts["startup-order"] = startup.Bytes()
	for n := range seen {
		var b bytes.Buffer
		if err := format.Node(&b, fset, n); err != nil {
			return "", err
		}
		// Content keys avoid source line numbers/absolute build paths changing
		// a fingerprint when an unrelated daemon function is inserted above it.
		sum := sha256.Sum256(b.Bytes())
		key := "decl/" + filepath.Base(fset.Position(n.Pos()).Filename) + "/" + hex.EncodeToString(sum[:])
		parts[key] = b.Bytes()
		if detail {
			fmt.Fprintln(os.Stderr, key, strings.SplitN(b.String(), "\n", 2)[0])
		}
	}
	// Resolve only the reachable variable's embed patterns. Hashing the entire
	// main-package EmbedFiles set here would pull the unrelated web UI back in.
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Doc == nil {
				continue
			}
			found := false
			for _, c := range g.Doc.List {
				if strings.HasPrefix(c.Text, "//go:embed ") {
					found = true
				}
			}
			if !found {
				continue
			}
			for _, spec := range g.Specs {
				if seen[spec] || seen[g] {
					for _, c := range g.Doc.List {
						if strings.HasPrefix(c.Text, "//go:embed ") {
							for _, pattern := range strings.Fields(strings.TrimPrefix(c.Text, "//go:embed ")) {
								if err := addEmbedPattern(parts, packages[pkg.Path()].Dir, pattern); err != nil {
									return "", err
								}
							}
						}
					}
				}
			}
		}
	}
	var addPackage func(string) error
	added := map[string]bool{}
	addPackage = func(path string) error {
		if added[path] || path == "unsafe" {
			return nil
		}
		added[path] = true
		p, ok := packages[path]
		if !ok {
			return fmt.Errorf("dependency %s unavailable", path)
		}
		// Hash complete imported packages, including embedded data and indirect
		// dependencies. This is intentionally conservative for library changes.
		var names []string
		for _, group := range [][]string{p.GoFiles, p.CgoFiles, p.SFiles, p.CFiles, p.HFiles, p.SysoFiles, p.EmbedFiles} {
			names = append(names, group...)
		}
		for _, name := range names {
			b, err := os.ReadFile(filepath.Join(p.Dir, name))
			if err != nil {
				return err
			}
			parts["package/"+path+"/"+filepath.ToSlash(name)] = b
		}
		for _, dep := range p.Imports {
			if err := addPackage(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for path := range external {
		if err := addPackage(path); err != nil {
			return "", err
		}
	}
	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintln(h, "solu-pane-runtime-v2")
	for _, key := range keys {
		fmt.Fprintf(h, "%d:%s%d:", len(key), key, len(parts[key]))
		h.Write(parts[key])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func paneDispatchCalls(node ast.Node, pkg *types.Package, info *types.Info) (agent, muse, codex bool) {
	if node == nil {
		return false, false, false
	}
	agentFn, museFn, codexFn := pkg.Scope().Lookup("runAgent"), pkg.Scope().Lookup("runMuseDriver"), pkg.Scope().Lookup("runCodexTerminal")
	ast.Inspect(node, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				// A name that is not declared looks up as nil; never match it.
				agent = agent || (agentFn != nil && info.Uses[id] == agentFn)
				muse = muse || (museFn != nil && info.Uses[id] == museFn)
				codex = codex || (codexFn != nil && info.Uses[id] == codexFn)
			}
		}
		return true
	})
	return agent, muse, codex
}

// referencedFunc reports whether the package declares name as a function and
// anything refers to it.
func referencedFunc(pkg *types.Package, info *types.Info, name string) bool {
	fn, ok := pkg.Scope().Lookup(name).(*types.Func)
	if !ok {
		return false
	}
	for _, used := range info.Uses {
		if used == fn {
			return true
		}
	}
	return false
}

// osExitCall reports whether expr is a call of the standard library's os.Exit,
// resolved by the type checker (not by spelling).
func osExitCall(expr ast.Expr, info *types.Info) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == "os" && fn.Name() == "Exit"
}

func containsGoto(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if branch, ok := n.(*ast.BranchStmt); ok && branch.Tok == token.GOTO {
			found = true
		}
		return !found
	})
	return found
}

func terminatingPaneDispatch(stmt ast.Stmt, pkg *types.Package, info *types.Info) bool {
	branch, ok := stmt.(*ast.IfStmt)
	if !ok || branch.Else != nil || len(branch.Body.List) == 0 {
		return false
	}
	switch end := branch.Body.List[len(branch.Body.List)-1].(type) {
	case *ast.ReturnStmt:
		if len(end.Results) != 0 {
			return false
		}
	case *ast.ExprStmt:
		// os.Exit(...) ends the process: `os.Exit(runCodexTerminal())`.
		if !osExitCall(end.X, info) {
			return false
		}
	default:
		return false
	}
	// A pane call in the condition or initializer may execute even when the
	// branch is not taken, and therefore does not establish an early return.
	for _, node := range []ast.Node{branch.Init, branch.Cond} {
		if agent, muse, codex := paneDispatchCalls(node, pkg, info); agent || muse || codex {
			return false
		}
	}
	return true
}

// A valid constant regexp allocates a private value without observable startup
// effects. It still enters the closure if pane code actually references it.
// Resolve the function's package (not its spelling), require a constant, and
// validate it: a dynamic argument or an invalid MustCompile can affect startup.
func pureRegexpInitializer(call *ast.CallExpr, info *types.Info) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fn, ok := info.Uses[selector.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "regexp" || fn.Name() != "MustCompile" {
		return false
	}
	value := info.Types[call.Args[0]].Value
	if value == nil || value.Kind() != constant.String {
		return false
	}
	_, err := regexp.Compile(constant.StringVal(value))
	return err == nil
}

// packageInitIsPure reports whether a package's OWN initialization — its
// package-level variable initializers and its init functions — is provably free
// of observable effects. Such a package cannot change how a pane behaves unless
// pane code names one of its declarations, so when pane code does not reach it
// the package may be left out of the fingerprint entirely.
//
// The analysis fails closed: cgo or assembly inputs, a parse error, an init
// function, a call, an index, a division, a receive, or any construct this
// function does not recognise all report false, and the package is hashed
// exactly as before. An impure package is therefore always covered, no matter
// which root reached it.
func packageInitIsPure(p listedPackage) bool {
	if len(p.CgoFiles)+len(p.SFiles)+len(p.CFiles)+len(p.HFiles)+len(p.SysoFiles) != 0 {
		return false
	}
	fset := token.NewFileSet()
	for _, name := range p.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
		if err != nil {
			return false
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name != nil && d.Name.Name == "init" {
					return false
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						return false
					}
					for _, value := range valueSpec.Values {
						if !exprIsPureData(value) {
							return false
						}
					}
				}
			}
		}
	}
	return true
}

// exprIsPureData reports whether evaluating an expression at package-init time
// yields a value without observing or mutating anything and without any
// possibility of panicking or blocking. It accepts literals, identifiers,
// selector constants, address-of over pure operands, and composite literals of
// pure elements — the shape of a static data table such as the provider model
// catalog. Everything else, including a call, an index, a slice, division, a
// type assertion, a dereference, a receive and a function literal, is rejected.
func exprIsPureData(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.BasicLit, *ast.Ident:
		return true
	case *ast.ParenExpr:
		return exprIsPureData(value.X)
	case *ast.SelectorExpr:
		return exprIsPureData(value.X)
	case *ast.CompositeLit:
		// The literal's type is a type expression, not a runtime value.
		for _, element := range value.Elts {
			if !exprIsPureData(element) {
				return false
			}
		}
		return true
	case *ast.KeyValueExpr:
		return exprIsPureData(value.Key) && exprIsPureData(value.Value)
	case *ast.UnaryExpr:
		switch value.Op {
		case token.ADD, token.SUB, token.XOR, token.NOT:
			return exprIsPureData(value.X)
		case token.AND:
			// Taking the address of an addressable pure expression allocates or
			// aliases a value; it observes and mutates nothing. Composite
			// literals such as &BinarySpec{...} are the common shape here.
			switch inner := value.X.(type) {
			case *ast.CompositeLit:
				return exprIsPureData(inner)
			case *ast.Ident:
				return true
			case *ast.SelectorExpr:
				return exprIsPureData(inner)
			default:
				return false
			}
		default:
			// Receive and anything else are not provably pure.
			return false
		}
	case *ast.BinaryExpr:
		// Division and remainder can panic on a zero divisor; shifts and
		// comparisons cannot.
		if value.Op == token.QUO || value.Op == token.REM {
			return false
		}
		return exprIsPureData(value.X) && exprIsPureData(value.Y)
	default:
		return false
	}
}

func addEmbedPattern(parts map[string][]byte, root, pattern string) error {
	all := strings.HasPrefix(pattern, "all:")
	pattern = strings.TrimPrefix(pattern, "all:")
	// Fail closed on patterns this implementation has not audited, instead of
	// silently omitting source inputs (the current runtime uses simple globs).
	if strings.ContainsAny(pattern, "\"`\\") || strings.Contains(pattern, "..") || filepath.IsAbs(pattern) {
		return fmt.Errorf("unsupported runtime embed pattern %q", pattern)
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("unresolved runtime embed pattern %q", pattern)
	}
	count := 0
	for _, match := range matches {
		err := filepath.WalkDir(match, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path != match && (!all && strings.HasPrefix(entry.Name(), ".") || !all && strings.HasPrefix(entry.Name(), "_") || entry.Name() == ".git") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("non-regular runtime embed input %s", path)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			parts["embed/"+filepath.ToSlash(rel)] = b
			count++
			return nil
		})
		if err != nil {
			return err
		}
	}
	if count == 0 {
		return fmt.Errorf("empty runtime embed pattern %q", pattern)
	}
	return nil
}
