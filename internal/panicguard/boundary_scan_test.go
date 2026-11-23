package panicguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot is the module root, seen from this package's directory.
const repoRoot = "../.."

// minimumGoroutineLaunches keeps the scan honest: the product starts a few
// dozen goroutines, and a scan that finds far fewer is reading the wrong tree.
const minimumGoroutineLaunches = 20

// excludedPackages are the directories under internal/ and pkg/ the scans
// skip, each with the reason. Everything else is product code, including a
// package added later.
func excludedPackages() map[string]string {
	return map[string]string{
		"internal/devtool":                     "the developer tool behind cmd/devtool; onedrive-go never links it",
		"internal/synctest":                    "test helpers; only test binaries link them",
		"internal/sync/internal/syncfixture":   "test helpers; only test binaries link them",
		"internal/sync/internal/syncstoretest": "test helpers; only test binaries link them",
	}
}

// goroutineBoundaryExceptions are the functions a go statement may start
// without a boundary of the common form, keyed "<package dir>.<function>",
// each with the reason it needs none. An entry no go statement uses fails the
// scan, so the list cannot outlive its code.
func goroutineBoundaryExceptions() map[string]string {
	return map[string]string{
		"internal/sync/internal/syncexec.worker": "a worker runs every action through safeExecuteAction, the boundary " +
			"that makes a panic that action's failure (R-6.3.8); the loop around it only receives from the dispatch channel",
		"internal/sync/internal/syncexec.closeCompletionsWhenWorkersExit": "it waits for the workers and closes the completions " +
			"channel through a sync.Once; neither can panic",
	}
}

// goPackage is the parsed non-test files of one directory.
type goPackage struct {
	dir   string
	fset  *token.FileSet
	files []*ast.File
}

// productPackages parses main.go and every package under internal/ and pkg/
// that excludedPackages does not name. Test files and testdata are left out:
// they are not linked into the binary.
func productPackages(t *testing.T) []goPackage {
	t.Helper()

	excluded := excludedPackages()
	for dir := range excluded {
		info, err := os.Stat(filepath.Join(repoRoot, dir))
		require.NoError(t, err, "excluded package %s must exist", dir)
		require.True(t, info.IsDir())
	}

	byDir := map[string][]string{".": {filepath.Join(repoRoot, "main.go")}}
	for _, top := range []string{"internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, top), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return fmt.Errorf("relative path of %s: %w", path, relErr)
			}
			rel = filepath.ToSlash(rel)
			if entry.IsDir() {
				if _, skip := excluded[rel]; skip || entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
				byDir[filepath.ToSlash(filepath.Dir(rel))] = append(byDir[filepath.ToSlash(filepath.Dir(rel))], path)
			}
			return nil
		})
		require.NoError(t, err)
	}

	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	packages := make([]goPackage, 0, len(dirs))
	for _, dir := range dirs {
		pkg := goPackage{dir: dir, fset: token.NewFileSet()}
		for _, path := range byDir[dir] {
			file, err := parser.ParseFile(pkg.fset, path, nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			pkg.files = append(pkg.files, file)
		}
		packages = append(packages, pkg)
	}

	return packages
}

// Validates: R-6.3.8, R-6.3.10
//
// A recovered panic reports what failed but not where, and the goroutine that
// could answer is unwound by the time anyone reads the report. So every
// recover in product code hands its value to Recovered, which logs the stack
// once, at Error: directly, or through a function of its package that calls
// Recovered (for the sync tree, of any of its packages). A recover that only logged the value, or logged nothing, would
// leave a panic that cannot be located.
func TestPanicRecoverySitesCaptureTheStack(t *testing.T) {
	t.Parallel()

	sites := 0
	var findings []string
	for _, scan := range productScans(t) {
		findings = append(findings, scan.recoverFindings()...)
		sites += scan.recoverSites
	}

	assert.Empty(t, findings, "\n"+strings.Join(findings, "\n"))
	assert.NotZero(t, sites, "the scan must find the recovery sites it polices")
}

// Validates: R-6.3.8, R-6.3.10
//
// A goroutine without a recover at its boundary would end the process with
// Go's exit status 2 and nothing in the log file. Every go statement, every Go
// call of a goroutine group, and every time.AfterFunc or context.AfterFunc
// call, whose callback runs on a goroutine of its own, starts a function
// whose first statements are deferred calls, one of which recovers and hands
// the value to Recovered: deferred calls registered before it are the
// goroutine's own cleanup (a WaitGroup's Done, a channel's close), and it
// covers everything after it. A function with no such boundary is allowed
// only by name, with a reason, in goroutineBoundaryExceptions, and a start the
// scan cannot resolve to a function of the package is a finding, never a pass.
func TestEveryGoroutineHasAPanicBoundary(t *testing.T) {
	t.Parallel()

	exceptions := goroutineBoundaryExceptions()
	used := make(map[string]bool)
	launches := 0
	var findings []string
	for _, scan := range productScans(t) {
		launches += scan.launchCount()
		findings = append(findings, scan.goroutineFindings(exceptions, used)...)
	}

	assert.Empty(t, findings, "\n"+strings.Join(findings, "\n"))
	for key := range exceptions {
		assert.True(t, used[key], "goroutineBoundaryExceptions names %s, which no go statement starts", key)
	}
	assert.GreaterOrEqual(t, launches, minimumGoroutineLaunches, "the scan must find the goroutines it polices")
}

// demoPackageDir, demoSource and demoTimerSource are a package the scan
// self-test reads: a goroutine of every kind the scans must accept and of
// every kind they must reject. Line numbers in the test's expectations refer
// to demoSource (demo.go) and demoTimerSource (timers.go).
const (
	demoPackageDir = "internal/demo"
	demoTrivial    = demoPackageDir + ".trivial"
	demoNeverUsed  = demoPackageDir + ".neverUsed"
	demoSource     = `package demo

import "example.com/tools/other"

func noBoundary() { go func() { work() }() }

func literalBoundary() {
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				report(panicguard.Recovered(logger, "in demo", r))
			}
		}()
		work()
	}()
}

func boundaryAfterWork() {
	go func() {
		ready := work()
		defer func() {
			if r := recover(); r != nil {
				report(panicguard.Recovered(logger, "in demo", r))
			}
		}()
		use(ready)
	}()
}

func namedWithBoundary() { go s.loop() }

func (s *server) loop() {
	defer s.recoverLoopPanic()
	work()
}

func (s *server) recoverLoopPanic() {
	r := recover()
	if r == nil {
		return
	}
	s.fail(wrapPanic(r))
}

func wrapPanic(r any) error { return panicguard.Recovered(nil, "in the demo loop", r) }

func namedWithoutBoundary() { go helper() }

func helper() { work() }

func allowed() { go trivial() }

func trivial() { close(done) }

func groupWithoutBoundary() { g.Go(func() error { return work() }) }

func groupNamed() { g.Go(s.loop) }

func groupNamedWithoutBoundary() { g.Go(helper) }

func groupUnnamed() { g.Go(makeWork()) }

func undeclared() { go elsewhere.Start() }

func imported() { go other.Run() }

func Run() {
	defer func() {
		if r := recover(); r != nil {
			report(panicguard.Recovered(logger, "in demo", r))
		}
	}()
}

func silentRecover() {
	defer func() {
		if r := recover(); r != nil {
			log.Println(r)
		}
	}()
}
`
	demoTimerSource = `package demo

import (
	stdcontext "context"
	"time"
)

func timerWithoutBoundary() { time.AfterFunc(delay, func() { work() }) }

func timerWithBoundary() {
	time.AfterFunc(delay, func() {
		defer func() {
			if r := recover(); r != nil {
				report(panicguard.Recovered(logger, "in demo", r))
			}
		}()
		work()
	})
}

func namedTimerWithBoundary() { time.AfterFunc(delay, s.loop) }

func callbackWithoutBoundary() { stdcontext.AfterFunc(ctx, helper) }

func clockTimer() { clock.AfterFunc(delay, helper) }

func clockStartsNamed() { clock.Go(func() { s.loop() }) }

func clockStartsHelper() { clock.Go(func() { helper() }) }

func (realClock) Go(fn func()) { go fn() }

func (g *group) start(ctx stdcontext.Context, fn func(stdcontext.Context)) {
	clock.Go(func() { runGroupMember(ctx, done, fn) })
}

func runGroupMember(ctx stdcontext.Context, done chan struct{}, fn func(stdcontext.Context)) {
	defer close(done)
	fn(ctx)
}

func startsMemberWithoutBoundary() { g.start(ctx, func(stdcontext.Context) { work() }) }

func startsMemberWithBoundary() { g.start(ctx, s.loop) }

func startsMemberThroughForwarder() { g.start(ctx, func(ctx stdcontext.Context) { s.loop(ctx) }) }

func startsHelperThroughForwarder() { g.start(ctx, func(ctx stdcontext.Context) { helper(ctx) }) }
`
)

// Validates: R-6.3.10
//
// The scans prove nothing unless they reject what they are meant to reject.
func TestBoundaryScans_RejectWhatTheyPolice(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "demo.go", demoSource, parser.SkipObjectResolution)
	require.NoError(t, err)
	timerFile, err := parser.ParseFile(fset, "timers.go", demoTimerSource, parser.SkipObjectResolution)
	require.NoError(t, err)
	scan := newPackageScan(goPackage{dir: demoPackageDir, fset: fset, files: []*ast.File{file, timerFile}})

	exceptions := map[string]string{
		demoTrivial:   "closes a channel",
		demoNeverUsed: "names no goroutine",
	}
	used := make(map[string]bool)
	findings := scan.goroutineFindings(exceptions, used)

	assert.Len(t, findings, 13, "%v", findings)
	assertFindingAt(t, findings, "internal/demo/demo.go:5:", "a literal with no boundary")
	assertFindingAt(t, findings, "internal/demo/demo.go:20:", "a boundary registered after work")
	assertFindingAt(t, findings, "internal/demo/demo.go:48:", "a named function with no boundary")
	assertFindingAt(t, findings, "internal/demo/demo.go:56:", "a goroutine group's Go call with no boundary")
	assertFindingAt(t, findings, "internal/demo/demo.go:60:", "a goroutine group's Go call of a named function with no boundary")
	assertFindingAt(t, findings, "internal/demo/demo.go:62:", "a goroutine group's Go call the scan cannot name")
	assertFindingAt(t, findings, "internal/demo/demo.go:64:", "a function this package does not declare")
	assertFindingAt(t, findings, "internal/demo/demo.go:66:", "an imported package's function of a name this package declares")
	assertFindingAt(t, findings, "internal/demo/timers.go:8:", "a timer callback literal with no boundary")
	assertFindingAt(t, findings, "internal/demo/timers.go:23:", "a context callback, under a renamed import, of a named function with no boundary")
	assertFindingAt(t, findings, "internal/demo/timers.go:29:", "a clock's Go call of a literal forwarding to a function with no boundary")
	assertFindingAt(t, findings, "internal/demo/timers.go:42:", "a starter's function argument with no boundary")
	assertFindingAt(t, findings, "internal/demo/timers.go:48:", "a starter's literal forwarding to a function with no boundary")
	assert.True(t, used[demoTrivial])
	assert.False(t, used[demoNeverUsed], "an exception no go statement uses stays unused")

	recoverFindings := scan.recoverFindings()
	require.Len(t, recoverFindings, 1, "only the recover that does not hand its value to Recovered: %v", recoverFindings)
	assert.Contains(t, recoverFindings[0], "internal/demo/demo.go:78:")
	assert.Equal(t, 6, scan.recoverSites)
}

func assertFindingAt(t *testing.T, findings []string, position, what string) {
	t.Helper()

	for _, finding := range findings {
		if strings.Contains(finding, position) {
			return
		}
	}
	assert.Fail(t, "the scan missed "+what, "no finding at %s in %v", position, findings)
}

// packageScan is the declarations of one package the scans resolve names
// against.
type packageScan struct {
	pkg goPackage
	// funcs maps a function or method name to its declarations.
	funcs map[string][]*ast.FuncDecl
	// guards are the functions that call Recovered directly.
	guards map[string]bool
	// recoverers are the functions that call recover and hand the value to
	// Recovered or a guard: a defer of one of them is a boundary.
	recoverers   map[string]bool
	recoverSites int
}

func newPackageScan(pkg goPackage) *packageScan {
	scan := &packageScan{
		pkg:        pkg,
		funcs:      make(map[string][]*ast.FuncDecl),
		guards:     make(map[string]bool),
		recoverers: make(map[string]bool),
	}
	for _, file := range pkg.files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				scan.funcs[fn.Name.Name] = append(scan.funcs[fn.Name.Name], fn)
			}
		}
	}
	for name, decls := range scan.funcs {
		for _, fn := range decls {
			if callsRecovered(directCalls(fn.Body)) {
				scan.guards[name] = true
			}
		}
	}
	for name, decls := range scan.funcs {
		for _, fn := range decls {
			if scan.handsRecoverToGuard(fn.Body) {
				scan.recoverers[name] = true
			}
		}
	}

	return scan
}

// syncTree is the directory of the sync engine's packages. They were one
// package until the engine was split along its authority boundaries, and a
// boundary one of them declares (the clock's timer recover lives with the
// vocabulary) serves the others, so they share their guards and recoverers.
const syncTree = "internal/sync"

// productScans scans every product package, the sync tree's packages sharing
// their guards and recoverers.
func productScans(t *testing.T) []*packageScan {
	t.Helper()

	var scans []*packageScan
	guards, recoverers := map[string]bool{}, map[string]bool{}
	for _, pkg := range productPackages(t) {
		scan := newPackageScan(pkg)
		scans = append(scans, scan)
		if pkg.dir != syncTree && !strings.HasPrefix(pkg.dir, syncTree+"/") {
			continue
		}
		for name := range scan.guards {
			guards[name] = true
		}
		for name := range scan.recoverers {
			recoverers[name] = true
		}
	}
	for _, scan := range scans {
		if scan.pkg.dir == syncTree || strings.HasPrefix(scan.pkg.dir, syncTree+"/") {
			scan.guards, scan.recoverers = guards, recoverers
		}
	}

	return scans
}

// directCalls are the calls a function body makes itself, leaving out those
// of function literals declared inside it.
func directCalls(body *ast.BlockStmt) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			calls = append(calls, n)
		}
		return true
	})

	return calls
}

// calledName is the name a call is made by: f for f(), m for x.m().
func calledName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	default:
		return ""
	}
}

func isRecoverCall(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "recover" && len(call.Args) == 0
}

func isRecoveredCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Recovered" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)

	return ok && pkg.Name == "panicguard"
}

func callsRecovered(calls []*ast.CallExpr) bool {
	for _, call := range calls {
		if isRecoveredCall(call) {
			return true
		}
	}

	return false
}

// handsRecoverToGuard reports a body that calls recover and passes on to
// Recovered, itself or through one of the package's guards.
func (s *packageScan) handsRecoverToGuard(body *ast.BlockStmt) bool {
	calls := directCalls(body)
	recovers, guarded := false, false
	for _, call := range calls {
		switch {
		case isRecoverCall(call):
			recovers = true
		case isRecoveredCall(call), s.guards[calledName(call)]:
			guarded = true
		}
	}

	return recovers && guarded
}

func (s *packageScan) position(node ast.Node) string {
	position := s.pkg.fset.Position(node.Pos())
	return filepath.ToSlash(filepath.Join(s.pkg.dir, filepath.Base(position.Filename))) + ":" +
		strconv.Itoa(position.Line) + ":"
}

// recoverFindings lists every function that calls recover without handing
// the value to Recovered.
func (s *packageScan) recoverFindings() []string {
	var findings []string
	check := func(body *ast.BlockStmt) {
		for _, call := range directCalls(body) {
			if !isRecoverCall(call) {
				continue
			}
			s.recoverSites++
			if !s.handsRecoverToGuard(body) {
				findings = append(findings, s.position(call)+" recover() does not hand its value to panicguard.Recovered")
			}
		}
	}
	for _, file := range s.pkg.files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				if n.Body != nil {
					check(n.Body)
				}
			case *ast.FuncLit:
				check(n.Body)
			}
			return true
		})
	}

	return findings
}

// goroutineStart is one place the package starts a goroutine: the started
// function (a literal, or an expression naming one), where, and the file.
type goroutineStart struct {
	at   ast.Node
	fn   ast.Expr
	file *ast.File
}

// launches are the goroutine starts of the package: go statements, the Go
// calls of goroutine groups (errgroup.Group, sync.WaitGroup) and clocks
// whatever their argument, and the callbacks of time.AfterFunc and
// context.AfterFunc. A clock's Go method that only starts the function it is
// handed is not one of its own: the call of that method is the start, as a
// clock's AfterFunc is.
func (s *packageScan) launches() []goroutineStart {
	var starts []goroutineStart
	for _, file := range s.pkg.files {
		forwarding := forwardingGoStatements(file)
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				if forwarding[n] {
					return true
				}
				starts = append(starts, goroutineStart{at: n.Call, fn: n.Call.Fun, file: file})
			case *ast.CallExpr:
				if selector, ok := n.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == goMethodName && len(n.Args) == 1 {
					starts = append(starts, goroutineStart{at: n, fn: n.Args[0], file: file})
				}
				if callback, ok := afterFuncCallback(n, file); ok {
					starts = append(starts, goroutineStart{at: n, fn: callback, file: file})
				}
			}
			return true
		})
	}

	return starts
}

// goMethodName names the method of a goroutine group or a clock that starts
// a goroutine.
const goMethodName = "Go"

// forwardingGoStatements are the go statements of file that a method named
// Go uses to start its own function parameter: a clock's implementation of
// the start its callers make.
func forwardingGoStatements(file *ast.File) map[*ast.GoStmt]bool {
	forwarding := make(map[*ast.GoStmt]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != goMethodName || fn.Body == nil {
			continue
		}
		params := make(map[string]bool)
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				params[name.Name] = true
			}
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if stmt, isGo := node.(*ast.GoStmt); isGo {
				if ident, isIdent := stmt.Call.Fun.(*ast.Ident); isIdent && params[ident.Name] && len(stmt.Call.Args) == 0 {
					forwarding[stmt] = true
				}
			}
			return true
		})
	}

	return forwarding
}

// forwardedCall is the function a literal whose whole body is one call
// starts: func() { w.loop(ctx) } starts loop.
func forwardedCall(literal *ast.FuncLit) (ast.Expr, bool) {
	if len(literal.Body.List) != 1 {
		return nil, false
	}
	stmt, ok := literal.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	switch call.Fun.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return call.Fun, true
	default:
		return nil, false
	}
}

// afterFuncCallback is the callback of a time.AfterFunc or context.AfterFunc
// call, which runs on a goroutine of its own. A clock's AfterFunc method is
// not one: its implementation's own time.AfterFunc call is.
func afterFuncCallback(call *ast.CallExpr, file *ast.File) (ast.Expr, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "AfterFunc" || len(call.Args) != 2 {
		return nil, false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	for _, path := range []string{"time", "context"} {
		if name, imported := importedAs(file, path); imported && name == pkg.Name {
			return call.Args[1], true
		}
	}

	return nil, false
}

// importedAs is the name file refers to the standard package path by, and
// whether file imports it.
func importedAs(file *ast.File, path string) (string, bool) {
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name, true
		}
		return path, true
	}

	return "", false
}

func (s *packageScan) launchCount() int {
	return len(s.launches())
}

// importNames are the names file refers to its imported packages by.
func importNames(file *ast.File) map[string]bool {
	names := make(map[string]bool, len(file.Imports))
	for _, spec := range file.Imports {
		if spec.Name != nil {
			names[spec.Name.Name] = true
			continue
		}
		parts := strings.Split(strings.Trim(spec.Path.Value, `"`), "/")
		name := parts[len(parts)-1]
		if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
			name = parts[len(parts)-2]
		}
		names[name] = true
	}

	return names
}

// startedName is the name of the function start begins, and "" with the
// finding when the scan cannot resolve it to a function of this package.
func (s *packageScan) startedName(start goroutineStart, where string) (string, string) {
	switch fn := start.fn.(type) {
	case *ast.Ident:
		return fn.Name, ""
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && importNames(start.file)[pkg.Name] {
			return "", where + " goroutine starts " + pkg.Name + "." + fn.Sel.Name +
				", which another package declares; start a function of this package that has a boundary"
		}
		return fn.Sel.Name, ""
	default:
		return "", where + " goroutine starts a function the scan cannot name; start a literal or a named function with a boundary"
	}
}

// goroutineFindings lists every goroutine start whose function has no
// boundary or cannot be resolved, marking in used the exceptions it relied
// on.
func (s *packageScan) goroutineFindings(exceptions map[string]string, used map[string]bool) []string {
	var findings []string
	starters := make(map[string]int)
	for _, start := range s.launches() {
		where := s.position(start.at)
		if literal, ok := start.fn.(*ast.FuncLit); ok {
			if starter, index, isStarter := s.starterOf(start, literal); isStarter {
				// The goroutine only runs a function its starter was
				// handed: the starter's callers are checked instead.
				starters[starter] = index
				continue
			}
			if callee, forwards := forwardedCall(literal); forwards {
				start.fn = callee
			}
		}
		if literal, ok := start.fn.(*ast.FuncLit); ok {
			if problem := s.boundaryProblem(literal.Body); problem != "" {
				findings = append(findings, where+" goroutine literal "+problem)
			}
			continue
		}

		name, finding := s.startedName(start, where)
		if finding != "" {
			findings = append(findings, finding)
			continue
		}
		key := s.pkg.dir + "." + name
		if _, ok := exceptions[key]; ok {
			used[key] = true
			continue
		}
		decls := s.funcs[name]
		if len(decls) == 0 {
			findings = append(findings, where+" goroutine starts "+name+
				", which this package does not declare; start a function of this package that has a boundary")
			continue
		}
		for _, fn := range decls {
			if problem := s.boundaryProblem(fn.Body); problem != "" {
				findings = append(findings, where+" goroutine starts "+name+", which "+problem)
			}
		}
	}

	return append(findings, s.starterArgumentFindings(starters)...)
}

// starterOf reports whether start is the goroutine of a starter: a function
// whose goroutine literal only calls a forwarder (a function of the package
// whose body runs deferred calls and then calls one of its function
// parameters) and hands it one of the starter's own parameters. The function
// that parameter receives is what runs on the goroutine, so it is the
// starter's callers, not the start inside it, that must pass a function with
// a boundary. It returns the starter's name and that parameter's index.
func (s *packageScan) starterOf(start goroutineStart, literal *ast.FuncLit) (string, int, bool) {
	call, ok := forwardedCallExpr(literal)
	if !ok {
		return "", 0, false
	}
	callee, ok := call.Fun.(*ast.Ident)
	if !ok || len(s.funcs[callee.Name]) != 1 {
		return "", 0, false
	}
	forwarded, ok := forwardedParameter(s.funcs[callee.Name][0])
	if !ok || forwarded >= len(call.Args) {
		return "", 0, false
	}
	arg, ok := call.Args[forwarded].(*ast.Ident)
	if !ok {
		return "", 0, false
	}
	enclosing := s.enclosingFunc(start)
	if enclosing == nil {
		return "", 0, false
	}
	for index, name := range parameterNames(enclosing.Type) {
		if name == arg.Name {
			return enclosing.Name.Name, index, true
		}
	}

	return "", 0, false
}

// forwardedCallExpr is the one call a literal whose whole body is that call
// makes.
func forwardedCallExpr(literal *ast.FuncLit) (*ast.CallExpr, bool) {
	if len(literal.Body.List) != 1 {
		return nil, false
	}
	stmt, ok := literal.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := stmt.X.(*ast.CallExpr)

	return call, ok
}

// forwardedParameter is the index of the function parameter fn calls when its
// body is deferred calls, none of them a recover, followed by one call of
// that parameter.
func forwardedParameter(fn *ast.FuncDecl) (int, bool) {
	body := fn.Body.List
	if len(body) == 0 {
		return 0, false
	}
	for _, stmt := range body[:len(body)-1] {
		deferred, ok := stmt.(*ast.DeferStmt)
		if !ok {
			return 0, false
		}
		if _, isLiteral := deferred.Call.Fun.(*ast.FuncLit); isLiteral {
			return 0, false
		}
	}
	last, ok := body[len(body)-1].(*ast.ExprStmt)
	if !ok {
		return 0, false
	}
	call, ok := last.X.(*ast.CallExpr)
	if !ok {
		return 0, false
	}
	called, ok := call.Fun.(*ast.Ident)
	if !ok {
		return 0, false
	}
	for index, name := range parameterNames(fn.Type) {
		if name == called.Name {
			return index, true
		}
	}

	return 0, false
}

// parameterNames are a function type's parameter names in order, "_" for an
// unnamed one.
func parameterNames(fnType *ast.FuncType) []string {
	var names []string
	for _, field := range fnType.Params.List {
		if len(field.Names) == 0 {
			names = append(names, "_")
			continue
		}
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}

	return names
}

// enclosingFunc is the declared function of the package whose body holds
// start.
func (s *packageScan) enclosingFunc(start goroutineStart) *ast.FuncDecl {
	for _, decl := range start.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Body != nil && fn.Body.Pos() <= start.at.Pos() && start.at.End() <= fn.Body.End() {
			return fn
		}
	}

	return nil
}

// starterArgumentFindings checks every call of a starter: the function it
// hands the starter to run on a goroutine must have a boundary, as if the
// call started it.
func (s *packageScan) starterArgumentFindings(starters map[string]int) []string {
	var findings []string
	for _, file := range s.pkg.files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			index, isStarter := starters[calledName(call)]
			if !isStarter || index >= len(call.Args) {
				return true
			}
			where := s.position(call)
			arg := call.Args[index]
			if literal, isLiteral := arg.(*ast.FuncLit); isLiteral {
				// A literal that only forwards to one call starts that call,
				// as a go statement of it does.
				callee, forwards := forwardedCall(literal)
				if !forwards {
					if problem := s.boundaryProblem(literal.Body); problem != "" {
						findings = append(findings, where+" goroutine literal "+problem)
					}
					return true
				}
				arg = callee
			}
			name, finding := s.startedName(goroutineStart{at: call, fn: arg, file: file}, where)
			if finding != "" {
				findings = append(findings, finding)
				return true
			}
			for _, fn := range s.funcs[name] {
				if problem := s.boundaryProblem(fn.Body); problem != "" {
					findings = append(findings, where+" goroutine starts "+name+", which "+problem)
				}
			}
			if len(s.funcs[name]) == 0 {
				findings = append(findings, where+" goroutine starts "+name+
					", which this package does not declare; start a function of this package that has a boundary")
			}
			return true
		})
	}

	return findings
}

// boundaryProblem says what is missing from a goroutine's body, or "" when
// its leading deferred calls include a boundary.
func (s *packageScan) boundaryProblem(body *ast.BlockStmt) string {
	for _, stmt := range body.List {
		deferred, ok := stmt.(*ast.DeferStmt)
		if !ok {
			return "runs a statement before any deferred recover hands a panic to panicguard.Recovered"
		}
		if s.isBoundary(deferred) {
			return ""
		}
	}

	return "has no deferred recover that hands a panic to panicguard.Recovered"
}

// isBoundary reports a deferred call that recovers and hands the value on to
// Recovered: a function literal that does both, or a function of the package
// that does.
func (s *packageScan) isBoundary(deferred *ast.DeferStmt) bool {
	if literal, ok := deferred.Call.Fun.(*ast.FuncLit); ok {
		return s.handsRecoverToGuard(literal.Body)
	}

	return s.recoverers[calledName(deferred.Call)]
}
