package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store owns the schema and every read the engine plans from
// (sync-store.md, ReadPlanningSnapshot): no production file of the engine
// package carries SQL text, so a read the engine needs is a store method, not
// a query the engine runs.
//
// Validates: R-2.1.3
func TestEngineSources_CarryNoSQL(t *testing.T) {
	t.Parallel()

	// A string literal holds SQL when it carries an upper-case keyword of a
	// statement or of its table clause.
	sqlPattern := regexp.MustCompile(`\b(SELECT|FROM|WHERE|INSERT\s+INTO|DELETE\s+FROM|UPDATE\s+\w+\s+SET|CREATE\s+(TEMP\s+)?TABLE)\b`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		checked++
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && sqlPattern.MatchString(lit.Value) {
				assert.Fail(t, "SQL in the engine", "%s: %s", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	assert.Positive(t, checked, "the engine's sources were checked")
}
