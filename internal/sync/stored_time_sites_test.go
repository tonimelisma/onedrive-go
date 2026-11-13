package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stored time is written by one of two encodings and read back by name.
// Times compared by order and remote modification times go through
// storedTimeNano, which saturates; local modification, change and birth
// times keep UnixNano, are compared only with themselves and are never turned
// back into a time. These tests read the product source: a new conversion
// fails them until it is declared below with what it is, so a new stored time
// has to choose its encoder and its decoder.

const (
	// What a time.Unix(0, n) call decodes.
	decodesOrderedTime   = "an ordered stored time"
	decodesRemoteModTime = "a stored remote modification time"
	decodesRangeBound    = "a bound of the stored range"

	// What a plain t.UnixNano() call encodes.
	encodesThroughTheSaturatingEncoder = "storedTimeNano itself"
	encodesLocalTimeForEquality        = "a local time compared for equality"

	// A value that lives in memory only, either way: a clock reading, an
	// activity stamp, a time shown in an error.
	inMemoryValue = "an in-memory value"
)

// Validates: R-6.7.34, R-2.10.7
func TestStoredTimes_EveryDecodeIsDeclared(t *testing.T) {
	t.Parallel()

	declared := map[string]map[string]int{
		"internal/sync/store_sql_encoding.go":       {decodesRangeBound: 2, decodesRemoteModTime: 1},
		"internal/sync/block_scope_rows.go":         {decodesOrderedTime: 1},
		"internal/sync/engine_runtime_lifecycle.go": {decodesOrderedTime: 1},
		"internal/sync/engine_runtime_owners.go":    {decodesOrderedTime: 1},
		"internal/sync/engine_watch_maintenance.go": {decodesOrderedTime: 3},
		"internal/sync/store_pending_creates.go":    {decodesOrderedTime: 1},
		"internal/sync/local_hash_reuse.go":         {inMemoryValue: 1},
		"internal/sync/observer_local.go":           {inMemoryValue: 1},
		"internal/sync/observer_remote.go":          {inMemoryValue: 1},
		"internal/sync/engine_housekeeping.go":      {inMemoryValue: 1},
	}

	found := countProductCalls(t, isTimeUnixFromNanoseconds, "internal/sync", "internal/driveops")
	assert.Equal(t, declaredTotals(declared), found,
		"every time.Unix(0, n) in internal/sync and internal/driveops is declared with what it decodes")
}

// Validates: R-6.7.34, R-2.10.7
func TestStoredTimes_EveryPlainUnixNanoIsDeclared(t *testing.T) {
	t.Parallel()

	declared := map[string]map[string]int{
		"internal/sync/store_sql_encoding.go":      {encodesThroughTheSaturatingEncoder: 1},
		"internal/sync/scanner.go":                 {encodesLocalTimeForEquality: 7, inMemoryValue: 1},
		"internal/sync/observer_local_handlers.go": {encodesLocalTimeForEquality: 4, inMemoryValue: 1},
		"internal/sync/executor_preconditions.go":  {encodesLocalTimeForEquality: 3},
		"internal/sync/executor_delete.go":         {encodesLocalTimeForEquality: 1, inMemoryValue: 2},
		"internal/sync/executor_transfer.go":       {encodesLocalTimeForEquality: 2},
		"internal/sync/local_hash_reuse.go":        {encodesLocalTimeForEquality: 1},
		"internal/sync/scanner_hash_cache.go":      {encodesLocalTimeForEquality: 1},
		"internal/sync/engine_shortcut_release.go": {encodesLocalTimeForEquality: 1},
		"internal/sync/observer_local.go":          {inMemoryValue: 1},
		"internal/sync/observer_remote.go":         {inMemoryValue: 1},
		"internal/sync/engine_housekeeping.go":     {inMemoryValue: 1},
	}

	found := countProductCalls(t, isPlainUnixNano, "internal/sync")
	assert.Equal(t, declaredTotals(declared), found,
		"every plain UnixNano() in internal/sync is declared with what it encodes; a stored ordered or remote time uses storedTimeNano")
}

func declaredTotals(declared map[string]map[string]int) map[string]int {
	totals := make(map[string]int, len(declared))
	for file, kinds := range declared {
		for _, count := range kinds {
			totals[file] += count
		}
	}

	return totals
}

// countProductCalls counts, per non-test Go file in the repository-relative
// directories, the calls match accepts.
func countProductCalls(t *testing.T, match func(*ast.CallExpr) bool, dirs ...string) map[string]int {
	t.Helper()

	root := filepath.Join("..", "..")
	counts := map[string]int{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		require.NoError(t, err)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, filepath.Join(root, dir, name), nil, parser.SkipObjectResolution)
			require.NoError(t, parseErr, name)
			ast.Inspect(file, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok && match(call) {
					counts[dir+"/"+name]++
				}
				return true
			})
		}
	}

	return counts
}

func isTimeUnixFromNanoseconds(call *ast.CallExpr) bool {
	if !isSelector(call.Fun, "time", "Unix") || len(call.Args) != 2 {
		return false
	}
	seconds, ok := call.Args[0].(*ast.BasicLit)

	return ok && seconds.Kind == token.INT && seconds.Value == "0"
}

func isPlainUnixNano(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)

	return ok && selector.Sel.Name == "UnixNano" && len(call.Args) == 0
}
