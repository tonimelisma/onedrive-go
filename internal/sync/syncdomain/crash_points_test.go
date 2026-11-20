package syncdomain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Crash-point harness (D-091). A hook at one commit boundary (crash_points.go)
// takes a snapshot of everything a process killed at that instant leaves
// behind: the committed state database, the local tree and data directory,
// and OneDrive as it is. A fresh engine then opens the snapshot and must
// converge with every invariant holding (assertSyncInvariants), no content
// lost.
//
// The lost-WAL variant keeps only the main database file: whatever was
// committed to the write-ahead log since the last checkpoint is gone, as
// after a disk that never honored the fsyncs, or a restore from a copy taken
// without the WAL. The state is then older than both trees, and it has lost
// the one record no tree gives back: the intent a create commits before its
// request (R-5.6.15). After a lost WAL at upload.created_unrecorded the
// create's empty item is a OneDrive file like any other, so the restart keeps
// it beside the local file and downloads it; reopen names such items
// (syncHarness.placeholdersWithoutIntent). A power loss on a disk that honors
// the fsyncs leaves no such state: the store commits with synchronous(FULL),
// so the WAL keeps every commit.
//
// The snapshot is a copy, so every entry has a new inode. The copy records
// which new identity stands for which old one, and the copied database is
// rewritten to match (and its sync folder rebound, as the rebind-root command
// does), so recovery sees the identities a restart on the same disk would.

// Validates: R-6.5.2
//
// allCrashPoints, the list the every-point test crashes at and the seeded
// crash model draws from, holds every crash point crash_points.go declares,
// once, and nothing else: a point declared without being added is named here.
func TestAllCrashPoints_HoldsEveryDeclaredCrashPoint(t *testing.T) {
	t.Parallel()

	points := allCrashPoints()
	declared := declaredCrashPoints(t)
	require.NotEmpty(t, declared, "crash_points.go declares no crash point")
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		assert.Equal(t, 1, countOf(points, declared[name]), "allCrashPoints holds %s (%q) once", name, declared[name])
	}
	values := slices.Collect(maps.Values(declared))
	for _, point := range points {
		assert.Contains(t, values, point, "allCrashPoints holds %q, which crash_points.go does not declare", point)
	}
}

// declaredCrashPoints maps the name of every constant crash_points.go
// declares to its value, read from the source.
func declaredCrashPoints(t *testing.T) map[string]string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), "crash_points.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	declared := map[string]string{}
	for _, decl := range parsed.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			require.True(t, isValue)
			require.Len(t, value.Values, len(value.Names), "every crash point constant has its own value")
			for i, name := range value.Names {
				lit, isLit := value.Values[i].(*ast.BasicLit)
				require.True(t, isLit && lit.Kind == token.STRING, "%s is a string literal", name.Name)
				point, unquoteErr := strconv.Unquote(lit.Value)
				require.NoError(t, unquoteErr)
				declared[name.Name] = point
			}
		}
	}

	return declared
}
