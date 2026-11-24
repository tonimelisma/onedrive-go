package sync

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// sourceSite is one function of one product file: the file as the scan finds
// it, and the function's name, with its receiver type for a method.
type sourceSite struct {
	file     string
	function string
}

// contextErrorReaders are the only functions of the engine and the watch
// control plane that may read context.Canceled or context.DeadlineExceeded
// off an error, each with the question it answers. "Did this fail because the
// run is ending" is never one of them: RunEnded answers it from the run's
// context. A transport timeout has the shape of a deadline while the run goes
// on, so the shape alone cannot say the run is ending.
func contextErrorReaders() map[sourceSite]string {
	return map[sourceSite]string{
		{file: "engine_result_classify.go", function: "isCancellationEcho"}: "once the run has ended, tells the cancellation's own " +
			"error from a separate failure that landed during the stop and is still reported",
		{file: "syncdomain/errors.go", function: "CancelCauseError.Is"}: "a cancel's cause answers to context.Canceled, so the " +
			"work it interrupts reads as canceled",
		{file: "syncdomain/domain_graph_error.go", function: "IsRequestTimeout"}: "with the run going on, a deadline is " +
			"a request OneDrive never answered: retry work and outage evidence",
		{file: "engine_result_classify.go", function: "isLiveRunCancel"}: "with the run going on, a cancel came " +
			"from something narrower than the run",
	}
}

// cancellationEchoCallers are the only functions that may call
// isCancellationEcho, each with why its run has ended where it does: the
// echo only narrows within the fact that the run has ended.
func cancellationEchoCallers() map[sourceSite]string {
	return map[sourceSite]string{
		{file: "engine_watch.go", function: "isWatchShutdownError"}:     "behind RunEnded",
		{file: "engine_result_classify.go", function: "classifyResult"}: "behind the completion's RunEnded",
		{file: "engine_runtime_shutdown.go", function: "engineFlow.logSuppressedShutdownCompletionError"}: "reached " +
			"only for a completion of a stopping run",
		{file: "engine_timer_panics.go", function: "withTimerPanic"}: "reached only once a timer callback's panic " +
			"canceled the run",
	}
}

// shapeRead is one place a function reads an error's shape: an errors.Is
// against context.Canceled or context.DeadlineExceeded or a comparison with
// either, or a call of isCancellationEcho (echoCall).
type shapeRead struct {
	function string
	pos      token.Pos
	echoCall bool
}

// shapeReads lists the shape reads in file with the top-level function or
// method that holds each ("" outside any function).
func shapeReads(file *ast.File) []shapeRead {
	var reads []shapeRead
	for _, decl := range file.Decls {
		function := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			function = syncfixture.FuncDeclName(fn)
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			switch {
			case readsContextError(node):
				reads = append(reads, shapeRead{function: function, pos: node.Pos()})
			case callsCancellationEcho(node):
				reads = append(reads, shapeRead{function: function, pos: node.Pos(), echoCall: true})
			}
			return true
		})
	}

	return reads
}

// readsContextError reports an errors.Is(x, context.Canceled) or
// errors.Is(x, context.DeadlineExceeded) call, or an == or != with either.
func readsContextError(node ast.Node) bool {
	switch expr := node.(type) {
	case *ast.CallExpr:
		selector, ok := expr.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Is" || len(expr.Args) != 2 {
			return false
		}
		pkg, isIdent := selector.X.(*ast.Ident)
		return isIdent && pkg.Name == "errors" && isContextErrorValue(expr.Args[1])
	case *ast.BinaryExpr:
		if expr.Op != token.EQL && expr.Op != token.NEQ {
			return false
		}
		return isContextErrorValue(expr.X) || isContextErrorValue(expr.Y)
	default:
		return false
	}
}

// callsCancellationEcho reports a call of isCancellationEcho, the engine's
// own function.
func callsCancellationEcho(node ast.Node) bool {
	call, ok := node.(*ast.CallExpr)

	return ok && nameOf(call.Fun) == "isCancellationEcho"
}

// contextPackage is the name the scanned files import package context by.
const contextPackage = "context"

func isContextErrorValue(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, isIdent := selector.X.(*ast.Ident)

	return isIdent && pkg.Name == contextPackage &&
		(selector.Sel.Name == "Canceled" || selector.Sel.Name == "DeadlineExceeded")
}

// shapeReadViolations returns each shape read in file, the product file at
// path, that no list names, as the sentence a failure prints, and the listed
// sites the file holds.
func shapeReadViolations(fset *token.FileSet, path string, file *ast.File) ([]string, []sourceSite) {
	readers, callers := contextErrorReaders(), cancellationEchoCallers()
	var violations []string
	var sites []sourceSite
	for _, read := range shapeReads(file) {
		site := sourceSite{file: path, function: read.function}
		allowed, what := readers, "reads a context error off an error"
		fix := "ask RunEnded whether the run is ending, or name the question in contextErrorReaders"
		if read.echoCall {
			allowed, what = callers, "calls isCancellationEcho"
			fix = "ask RunEnded first, and name the function in cancellationEchoCallers with why its run has ended there"
		}
		if _, named := allowed[site]; named {
			sites = append(sites, site)
			continue
		}
		violations = append(violations, fmt.Sprintf("%s (in %q) %s: %s", fset.Position(read.pos), read.function, what, fix))
	}

	return violations, sites
}

// Whether work stopped because its run is ending is asked of the run's
// context (RunEnded), never read off the error: every check of an error
// against context.Canceled or context.DeadlineExceeded in the product files
// of the engine and the watch control plane is in a function of a file named
// in contextErrorReaders, every call of isCancellationEcho is in one named in
// cancellationEchoCallers, and each named function still holds one.
//
// Validates: R-6.8.29
func TestContextErrorChecks_OnlyNamedReadersReadTheShape(t *testing.T) {
	t.Parallel()

	seen := make(map[sourceSite]bool)
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
			violations, sites := shapeReadViolations(fset, path, file)
			for _, violation := range violations {
				assert.Fail(t, "the shape of an error read outside a named site", violation)
			}
			for _, site := range sites {
				seen[site] = true
			}
		}
	}
	assert.Positive(t, scanned, "the scan found the production files")
	for _, listed := range []map[sourceSite]string{contextErrorReaders(), cancellationEchoCallers()} {
		for site := range listed {
			assert.True(t, seen[site], "%s %s is named but reads no error's shape", site.file, site.function)
		}
	}
}

// The scan finds both forms of a context error check and the calls of
// isCancellationEcho, and names the function that holds each.
//
// Validates: R-6.8.29
func TestContextErrorChecks_FindsEachFormInItsFunction(t *testing.T) {
	t.Parallel()

	const source = `package p

import (
	"context"
	"errors"
)

func stopped(err error) bool {
	return errors.Is(err, context.Canceled)
}

func (w *worker) timedOut(err error) bool {
	check := func() bool { return err == context.DeadlineExceeded }
	return check() || errors.Is(err, errOther) || isCancellationEcho(err)
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, parser.SkipObjectResolution)
	require.NoError(t, err)

	reads := shapeReads(file)

	require.Len(t, reads, 3)
	assert.Equal(t, shapeRead{function: "stopped", pos: reads[0].pos}, reads[0])
	assert.Equal(t, 9, fset.Position(reads[0].pos).Line)
	assert.Equal(t, shapeRead{function: "worker.timedOut", pos: reads[1].pos}, reads[1])
	assert.Equal(t, 13, fset.Position(reads[1].pos).Line)
	assert.Equal(t, shapeRead{function: "worker.timedOut", pos: reads[2].pos, echoCall: true}, reads[2])
	assert.Equal(t, 14, fset.Position(reads[2].pos).Line)
}

// A site is named by its file and its function: a function of a reader's
// name in another file fails, and so does a new caller of isCancellationEcho,
// each with a sentence that says what to do.
//
// Validates: R-6.8.29
func TestContextErrorChecks_RefusesAnUnnamedSite(t *testing.T) {
	t.Parallel()

	const source = `package sync

import (
	"context"
	"errors"
)

func IsRequestTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

func stopOnEcho(err error) bool {
	return isCancellationEcho(err)
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine_watch_loop.go", source, parser.SkipObjectResolution)
	require.NoError(t, err)

	violations, sites := shapeReadViolations(fset, "engine_watch_loop.go", file)

	assert.Empty(t, sites)
	require.Len(t, violations, 2)
	assert.Contains(t, violations[0], `engine_watch_loop.go:9:9 (in "IsRequestTimeout") reads a context error`)
	assert.Contains(t, violations[0], "ask RunEnded whether the run is ending")
	assert.Contains(t, violations[1], `engine_watch_loop.go:13:9 (in "stopOnEcho") calls isCancellationEcho`)
	assert.Contains(t, violations[1], "name the function in cancellationEchoCallers with why its run has ended there")
}
