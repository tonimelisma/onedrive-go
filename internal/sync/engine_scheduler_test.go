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

// Validates: R-6.3.10
//
// The engine's clock is the sync tree's scheduler as well as its time: every
// goroutine the sync tree's product code starts is started by SyncClock.Go,
// so a test that owns the clock owns when the watch's goroutines, the
// workers, the scan's hash workers, the debounce loop and the socket.io
// reader run. The one go statement is the real clock's own; a goroutine
// group's Go is no way around it, since depguard admits no errgroup.
func TestSyncTree_StartsGoroutinesOnlyThroughTheClock(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	var statements []string
	for _, dir := range syncTreePackageDirs() {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			ast.Inspect(file, func(node ast.Node) bool {
				if stmt, ok := node.(*ast.GoStmt); ok {
					statements = append(statements, filepath.ToSlash(fset.Position(stmt.Pos()).Filename))
				}
				return true
			})
		}
	}

	assert.Equal(t, []string{"syncdomain/domain_clock.go"}, statements,
		"start a goroutine through the engine's SyncClock.Go, not a go statement")
}
