package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// detachedContextPackages are the packages whose shutdown paths the watch
// control plane bounds, the sync tree's and multisync: a store write detached from cancellation and left
// without a deadline is a wait nothing bounds, under a stop bound that
// assumes every such wait has one.
func detachedContextPackages() []string {
	return append(syncTreePackageDirs(), filepath.Join("..", "multisync"))
}

// syncTreePackageDirs are the directories of the sync tree's product
// packages, from this one: the engine's first.
func syncTreePackageDirs() []string {
	return []string{
		".", "syncdomain", "syncshortcut", "syncstore",
		filepath.Join("internal", "syncplan"), filepath.Join("internal", "syncobserve"), filepath.Join("internal", "syncexec"),
	}
}

// unboundedDetachedContexts returns the context.WithoutCancel calls in file
// that are not the first argument of a context.WithTimeout,
// context.WithTimeoutCause, context.WithDeadline or context.WithDeadlineCause
// call: the one form that detaches from cancellation and bounds the result in
// the same expression.
func unboundedDetachedContexts(file *ast.File) []token.Pos {
	bounded := make(map[*ast.CallExpr]bool)
	var detached []*ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isBoundingContextCall(call) && len(call.Args) > 0 {
			if inner, isCall := call.Args[0].(*ast.CallExpr); isCall && isContextCall(inner, "WithoutCancel") {
				bounded[inner] = true
			}
		}
		if isContextCall(call, "WithoutCancel") {
			detached = append(detached, call)
		}
		return true
	})

	var unbounded []token.Pos
	for _, call := range detached {
		if !bounded[call] {
			unbounded = append(unbounded, call.Pos())
		}
	}
	return unbounded
}

// isBoundingContextCall reports a call that gives its first argument a
// deadline.
func isBoundingContextCall(call *ast.CallExpr) bool {
	for _, name := range []string{"WithTimeout", "WithTimeoutCause", "WithDeadline", "WithDeadlineCause"} {
		if isContextCall(call, name) {
			return true
		}
	}

	return false
}

func isContextCall(call *ast.CallExpr, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "context"
}

// Every context the engine and the watch control plane detach from
// cancellation carries a deadline, so the bound on a drive's stop rests on
// waits that are themselves bounded.
//
// Validates: R-6.3.9
func TestDetachedContexts_AlwaysCarryADeadline(t *testing.T) {
	t.Parallel()

	scanned := 0
	for _, dir := range detachedContextPackages() {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			require.NoError(t, parseErr)
			scanned++
			for _, pos := range unboundedDetachedContexts(file) {
				assert.Failf(t, "detached context without a deadline",
					"%s: wrap context.WithoutCancel in context.WithTimeout or context.WithDeadline", fset.Position(pos))
			}
		}
	}
	assert.Positive(t, scanned, "the scan found the production files")
}

// The rule above accepts only the bounded form.
//
// Validates: R-6.3.9
func TestUnboundedDetachedContexts_FlagsOnlyTheUnboundedForm(t *testing.T) {
	t.Parallel()

	const source = `package p

import (
	"context"
	"time"
)

func f(ctx context.Context) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = bounded
	byDeadline, stop := context.WithDeadline(context.WithoutCancel(ctx), time.Now())
	defer stop()
	_ = byDeadline
	withCause, end := context.WithTimeoutCause(context.WithoutCancel(ctx), time.Second, errFixture)
	defer end()
	_ = withCause
	unbounded := context.WithoutCancel(ctx)
	_ = unbounded
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, parser.SkipObjectResolution)
	require.NoError(t, err)

	unbounded := unboundedDetachedContexts(file)

	require.Len(t, unbounded, 1)
	assert.Equal(t, 18, fset.Position(unbounded[0]).Line)
}

// detachSiteAllowed names, by file and function, the places in the sync
// tree's product code that may detach from cancellation by hand: the one
// helper every store wait after a run's context ended goes through
// (syncexec.DetachedStoreContext, under RunDetachedStoreAccess and
// RunDetachedStoreAccessUntil), and the store's own close, which runs after
// every run is over and checkpoints under its own bound. Every other detach
// takes the helper, so a stopping run's waits share the stop's one deadline.
func detachSiteAllowed() map[string]bool {
	return map[string]bool{
		filepath.Join("internal", "syncexec", "worker.go") + ":DetachedStoreContext": true,
		filepath.Join("syncstore", "store.go") + ":Close":                            true,
	}
}

// handMadeDetaches returns, as "<function>", the functions of file that call
// context.WithoutCancel, once per call.
func handMadeDetaches(file *ast.File) []string {
	var sites []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if isCall && isContextCall(call, "WithoutCancel") {
				sites = append(sites, fn.Name.Name)
			}
			return true
		})
	}

	return sites
}

// Every store wait the sync tree detaches from cancellation goes through the
// one helper, so once a run's context ended its waits share the stop's one
// deadline instead of each taking a bound of its own.
//
// Validates: R-6.3.9
func TestDetachedContexts_TheSyncTreeDetachesOnlyThroughTheOneHelper(t *testing.T) {
	t.Parallel()

	allowed := detachSiteAllowed()
	seen := make(map[string]bool, len(allowed))
	for _, dir := range syncTreePackageDirs() {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			require.NoError(t, parseErr)
			for _, function := range handMadeDetaches(file) {
				site := filepath.Clean(path) + ":" + function
				seen[site] = true
				assert.True(t, allowed[site],
					"%s detaches by hand: run the store wait through syncexec.RunDetachedStoreAccess or, after the run's context ended, the stop's deadline (runStoppedStoreAccess)", site)
			}
		}
	}
	for site := range allowed {
		assert.True(t, seen[site], "the allowed site %s no longer detaches: remove it from the list", site)
	}
}

// The scan above finds a hand-made detach in the function that makes it.
//
// Validates: R-6.3.9
func TestHandMadeDetaches_NamesTheFunctionOfEachDetach(t *testing.T) {
	t.Parallel()

	const source = `package p

import (
	"context"
	"time"
)

func helper(ctx context.Context) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = bounded
}

func clean(ctx context.Context) {
	_ = ctx
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, parser.SkipObjectResolution)
	require.NoError(t, err)

	assert.Equal(t, []string{"helper"}, handMadeDetaches(file))
}
