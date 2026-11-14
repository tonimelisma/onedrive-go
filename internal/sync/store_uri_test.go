package sync

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// uriDelimiterFolder names a data folder with the characters a `file:` URI
// reads as the start of its query and its fragment.
const uriDelimiterFolder = "data?v=1#part"

// Validates: R-2.5.2
func TestSyncStore_ReopensAStoreInAFolderNamedWithURIDelimiters(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	parent := t.TempDir()
	dbPath := filepath.Join(parent, uriDelimiterFolder, "state.db")
	logger := newTestLogger(t)
	block := testBlockScope(SKService(), time.Minute, time.Date(2025, 6, 15, 12, 1, 0, 0, time.UTC))

	store, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	var journalMode string
	require.NoError(t, store.rawDB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode))
	assert.Equal(t, "wal", journalMode, "every pragma the URI carries is applied")
	var busyTimeout, journalSizeLimit int64
	require.NoError(t, store.rawDB().QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout))
	require.NoError(t, store.rawDB().QueryRowContext(ctx, "PRAGMA journal_size_limit").Scan(&journalSizeLimit))
	assert.Equal(t, storeBusyTimeout.Milliseconds(), busyTimeout)
	assert.Equal(t, int64(storeJournalSizeLimitBytes), journalSizeLimit)
	require.NoError(t, store.UpsertBlockScope(ctx, block))
	require.NoError(t, store.Close(ctx))

	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Equal(t, []string{uriDelimiterFolder}, names, "nothing is written beside the data folder")
	require.FileExists(t, dbPath)

	reopened, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	blocks, err := reopened.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, block.Key, blocks[0].Key)
	require.NoError(t, reopened.Close(context.Background()))

	inspector, err := openStoreInspector(dbPath, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})
	var inspected int
	require.NoError(t, inspector.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM block_scopes").Scan(&inspected))
	assert.Equal(t, 1, inspected, "the read-only inspector opens the same file")
}

// Validates: R-2.5.2
func TestSQLiteFileURI_EscapesTheDatabasePath(t *testing.T) {
	t.Parallel()

	query := url.Values{sqlitePragmaParam: {"busy_timeout(5000)"}}

	assert.Equal(t,
		"file:///srv/data%3Fv=1%23part%25/state.db?_pragma=busy_timeout%285000%29",
		sqliteFileURI("/srv/data?v=1#part%/state.db", query),
		"an absolute path keeps an empty authority and escapes what a URI would read")
	assert.Equal(t, "file:state.db", sqliteFileURI("state.db", nil),
		"a relative path has no authority SQLite could read as a host")
}

// Tests outside this package open their databases through synctest's
// builder; it must build the URI the store does.
//
// Validates: R-2.5.2
func TestSQLiteFileURI_TheTestBuilderMatchesTheStoreBuilder(t *testing.T) {
	t.Parallel()

	queries := []url.Values{nil, {sqlitePragmaParam: {"busy_timeout(5000)", "foreign_keys(ON)"}, "mode": {"ro"}}}
	for _, path := range []string{
		"/srv/state/state.db",
		"state.db",
		"data/state.db",
		"/srv/data?v=1/state.db",
		"/srv/data#part/state.db",
		"/srv/100%/state.db",
		"/srv/my data/state.db",
		"my data?#%/state.db",
	} {
		for _, query := range queries {
			assert.Equal(t, sqliteFileURI(path, query), synctest.SQLiteFileURI(path, query), "%s %v", path, query)
		}
	}
}

// Validates: R-2.5.2
func TestSQLiteDSNs_AreBuiltOnlyByTheTwoBuilders(t *testing.T) {
	t.Parallel()

	var violations []string
	files := walkModuleGoFiles(t, func(path, _ string) error {
		violations = append(violations, sqliteDSNViolations(t, path)...)
		return nil
	})
	require.Greater(t, files, 100, "the walk reads the module's Go files")
	assert.Empty(t, violations,
		"a SQLite DSN is built by sqliteFileURI or synctest.SQLiteFileURI, never by joining \"file:\" and a path, "+
			"and sql.Open takes one of them (or \":memory:\"), never a bare path")
}

// The check above can fail: it reports each shape it exists to refuse and
// passes the two it allows.
//
// Validates: R-2.5.2
func TestSQLiteDSNViolations_ReportsAJoinedSchemeAndAnOpenWithoutABuilder(t *testing.T) {
	t.Parallel()

	const sample = `package sample

import "database/sql"

func open(path string) {
	_, _ = sql.Open("sqlite", "file:"+path)
	_, _ = sql.Open("sqlite", path)
	_, _ = sql.Open("sqlite", sqliteFileURI(path, nil))
	_, _ = sql.Open("sqlite", ":memory:")
}
`
	path := filepath.Join(t.TempDir(), "sample.go")
	require.NoError(t, os.WriteFile(path, []byte(sample), 0o600))

	assert.Equal(t, []string{
		path + ":6:9: sql.Open without a DSN builder",
		path + ":6:28: \"file:\" joined to a path",
		path + ":7:9: sql.Open without a DSN builder",
	}, sqliteDSNViolations(t, path))
}

// sqliteDSNViolations lists the places in the Go file at path that join a
// "file:" literal to something, or hand sql.Open anything but a builder call.
func sqliteDSNViolations(t *testing.T, path string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, path)

	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BinaryExpr:
			if n.Op == token.ADD && isFileSchemeLiteral(n.X) {
				violations = append(violations, fset.Position(n.Pos()).String()+": \"file:\" joined to a path")
			}
		case *ast.CallExpr:
			if isSelector(n.Fun, "sql", "Open") && len(n.Args) == 2 && !isSQLiteDSNBuilderCall(n.Args[1]) {
				violations = append(violations, fset.Position(n.Pos()).String()+": sql.Open without a DSN builder")
			}
		}
		return true
	})

	return violations
}

func isFileSchemeLiteral(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)

	return err == nil && strings.HasPrefix(value, "file:")
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)

	return ok && ident.Name == pkg
}

func isSQLiteDSNBuilderCall(expr ast.Expr) bool {
	if literal, ok := expr.(*ast.BasicLit); ok {
		return literal.Kind == token.STRING && literal.Value == `":memory:"`
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	if ident, isIdent := call.Fun.(*ast.Ident); isIdent {
		return ident.Name == "sqliteFileURI"
	}

	return isSelector(call.Fun, "synctest", "SQLiteFileURI")
}
