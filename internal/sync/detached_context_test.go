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
// control plane bounds: a store write detached from cancellation and left
// without a deadline is a wait nothing bounds, under a stop bound that
// assumes every such wait has one.
func detachedContextPackages() []string {
	return []string{".", filepath.Join("..", "multisync")}
}

// unboundedDetachedContexts returns the context.WithoutCancel calls in file
// that are not the first argument of a context.WithTimeout or
// context.WithDeadline call: the one form that detaches from cancellation and
// bounds the result in the same expression.
func unboundedDetachedContexts(file *ast.File) []token.Pos {
	bounded := make(map[*ast.CallExpr]bool)
	var detached []*ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if (isContextCall(call, "WithTimeout") || isContextCall(call, "WithDeadline")) && len(call.Args) > 0 {
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
	unbounded := context.WithoutCancel(ctx)
	_ = unbounded
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, parser.SkipObjectResolution)
	require.NoError(t, err)

	unbounded := unboundedDetachedContexts(file)

	require.Len(t, unbounded, 1)
	assert.Equal(t, 15, fset.Position(unbounded[0]).Line)
}
