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

// Every goroutine the watch starts for observation, and every callback an
// observer runs on its own goroutine, reaches the run through an observation
// and its send-only outputs: none of them names the runtime, whose
// scheduler, ledgers and timers belong to the loop goroutine.
func TestObservation_OffLoopGoroutinesNeverHoldTheRuntime(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("engine_*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			for _, arg := range offLoopCallbacks(n) {
				lit, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				checked++
				ast.Inspect(lit.Body, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok {
						assert.NotContains(t, []string{"rt", "flow"}, id.Name,
							"%s: an off-loop goroutine names %q", fset.Position(id.Pos()), id.Name)
					}
					return true
				})
			}
			return true
		})
	}
	assert.GreaterOrEqual(t, checked, 7,
		"the observation goroutines, the delta-expiry callback and the protected-root event sink were found")
}

// offLoopCallbacks are the expressions of n that run off the loop goroutine:
// the arguments of a goroutine start, and the observer callbacks the
// observers' inputs carry.
func offLoopCallbacks(n ast.Node) []ast.Expr {
	switch node := n.(type) {
	case *ast.CallExpr:
		if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "start" {
			return node.Args
		}
	case *ast.KeyValueExpr:
		if key, ok := node.Key.(*ast.Ident); ok &&
			(key.Name == "DeltaExpiredRecovery" || key.Name == "ProtectedRootEvents") {
			return []ast.Expr{node.Value}
		}
	}
	return nil
}
