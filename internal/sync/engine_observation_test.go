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
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "start" && sel.Sel.Name != "SetDeltaExpiredRecovery") {
				return true
			}
			for _, arg := range call.Args {
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
	assert.GreaterOrEqual(t, checked, 6, "the observation goroutines and the delta-expiry callback were found")
}
