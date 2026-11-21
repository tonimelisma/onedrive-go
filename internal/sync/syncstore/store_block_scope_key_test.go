package syncstore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// foreignKeyViolationsForTest is what PRAGMA foreign_key_check reports for
// the store: one "table -> parent" line per row that references a missing
// parent row.
func foreignKeyViolationsForTest(t *testing.T, store *SyncStore) []string {
	t.Helper()

	rows, err := store.db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()

	var violations []string
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var fkID int
		require.NoError(t, rows.Scan(&table, &rowID, &parent, &fkID))
		violations = append(violations, table+" -> "+parent)
	}
	require.NoError(t, rows.Err())

	return violations
}

func blockedScopeKeyForTest(t *testing.T, store *SyncStore, path string) sql.NullString {
	t.Helper()

	var key sql.NullString
	require.NoError(t, store.db.QueryRowContext(t.Context(),
		`SELECT blocked_scope_key FROM retry_work WHERE path = ?`, path,
	).Scan(&key))

	return key
}

func listRetryWorkForTest(t *testing.T, store *SyncStore) []syncdomain.RetryWorkRow {
	t.Helper()

	rows, err := store.ListRetryWork(t.Context())
	require.NoError(t, err)

	return rows
}

func testServiceScopeForTest(store *SyncStore) *syncdomain.BlockScope {
	return syncfixture.TestBlockScope(syncdomain.SKService(), time.Minute, store.nowFunc().Add(time.Minute))
}

// Validates: R-2.10.33
func TestRetryWorkSchema_BlockedRowsReferenceTheirScopeRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	var foreignKeys int
	require.NoError(t, store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys))
	assert.Equal(t, 1, foreignKeys, "every pooled connection enforces foreign keys")

	rows, err := store.db.QueryContext(ctx, `SELECT "table", "from", "to" FROM pragma_foreign_key_list('retry_work')`)
	require.NoError(t, err)
	var keys []string
	for rows.Next() {
		var parent, from, to string
		require.NoError(t, rows.Scan(&parent, &from, &to))
		keys = append(keys, from+" -> "+parent+"."+to)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{"blocked_scope_key -> block_scopes.scope_key"}, keys)
}

// A scope kept in block_scopes and its blocked work are stored together or
// not at all: a blocked row without its scope row is refused, and a scope row
// is never left without the blocked row it was written for.
//
// Validates: R-2.10.33
func TestRecordBlockedRetryWork_RefusesABlockedRowWithoutItsScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordBlockedRetryWork(ctx,
		syncfixture.TestRetryWorkKey("orphan.txt", "", syncdomain.ActionUpload), syncdomain.SKService(), nil)
	require.Error(t, err, "the database refuses a blocked row whose scope row is missing")
	assert.Empty(t, listRetryWorkForTest(t, store))

	scope := testServiceScopeForTest(store)
	row, err := store.RecordBlockedRetryWork(ctx,
		syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionUpload), syncdomain.SKService(), scope)
	require.NoError(t, err)
	assert.True(t, row.Blocked)
	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "the scope row is written in the blocked row's transaction")
	assert.Equal(t, syncdomain.SKService(), blocks[0].Key)
	assert.Equal(t, sql.NullString{String: syncdomain.SKService().String(), Valid: true},
		blockedScopeKeyForTest(t, store, "blocked.txt"))

	_, err = store.RecordBlockedRetryWork(ctx,
		syncfixture.TestRetryWorkKey("other.txt", "", syncdomain.ActionUpload), syncdomain.SKService(),
		syncfixture.TestBlockScope(syncdomain.SKQuotaOwn(), time.Minute, store.nowFunc().Add(time.Minute)))
	require.Error(t, err, "the scope row written with a blocked row is the row's own scope")
}

// A crash between "scope written" and "blocked row written" cannot happen:
// both are one transaction, so a failure of the blocked row's write leaves
// neither.
//
// Validates: R-2.10.33
func TestRecordBlockedRetryWork_AFailedBlockedRowLeavesNoScopeRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.db.ExecContext(ctx, `CREATE TRIGGER crash_before_blocked_row BEFORE INSERT ON retry_work
		BEGIN SELECT RAISE(ABORT, 'simulated crash before the blocked row'); END`)
	require.NoError(t, err)

	_, err = store.RecordBlockedRetryWork(ctx,
		syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionUpload), syncdomain.SKService(),
		testServiceScopeForTest(store))
	require.ErrorContains(t, err, "simulated crash")

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks, "the scope row rolled back with the blocked row")
}

// A read boundary has no block_scopes row (observation owns it), so work
// blocked under it carries no key, and the start-up repair releases it.
//
// Validates: R-2.10.33
func TestRecordBlockedRetryWork_UnderAReadBoundaryCarriesNoKey(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	readScope := syncdomain.SKPermLocalRead("/docs")

	_, err := store.RecordBlockedRetryWork(ctx,
		syncfixture.TestRetryWorkKey("docs/a.txt", "", syncdomain.ActionUpload), readScope, nil)
	require.NoError(t, err)
	assert.False(t, blockedScopeKeyForTest(t, store, "docs/a.txt").Valid)

	_, err = store.RecordBlockedRetryWork(ctx,
		syncfixture.TestRetryWorkKey("held.txt", "", syncdomain.ActionUpload), syncdomain.SKService(),
		testServiceScopeForTest(store))
	require.NoError(t, err)

	released, err := store.ReleaseOrphanBlockedRetryWork(ctx, store.nowFunc())
	require.NoError(t, err)
	assert.Equal(t, 1, released, "only the row with no scope row to reference is released")
	rows := listRetryWorkForTest(t, store)
	require.Len(t, rows, 2)
	for i := range rows {
		assert.Equal(t, rows[i].Path == "held.txt", rows[i].Blocked, rows[i].Path)
	}
}

// Validates: R-2.10.33
func TestBlockScopeRows_CannotGoWhileBlockedWorkReferencesThem(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	first := syncfixture.TestRetryWorkKey("a.txt", "", syncdomain.ActionUpload)
	second := syncfixture.TestRetryWorkKey("b.txt", "", syncdomain.ActionUpload)
	_, err := store.RecordBlockedRetryWork(ctx, first, syncdomain.SKService(), testServiceScopeForTest(store))
	require.NoError(t, err)
	_, err = store.RecordBlockedRetryWork(ctx, second, syncdomain.SKService(), nil)
	require.NoError(t, err)

	require.Error(t, store.DeleteBlockScope(ctx, syncdomain.SKService()),
		"a scope row its blocked work references cannot be deleted")

	// The upsert updates the referenced row in place.
	require.NoError(t, store.UpsertBlockScope(ctx,
		syncfixture.TestBlockScope(syncdomain.SKService(), 2*time.Minute, store.nowFunc().Add(2*time.Minute))))
	assert.Empty(t, foreignKeyViolationsForTest(t, store))

	released, removed, err := store.ReleaseScopeWork(ctx, syncdomain.SKService(),
		[]syncdomain.RetryWorkKey{first}, store.nowFunc())
	require.NoError(t, err)
	require.Len(t, released, 1)
	assert.False(t, removed)
	assert.False(t, blockedScopeKeyForTest(t, store, "a.txt").Valid, "releasing a row clears its key")
	assert.Empty(t, foreignKeyViolationsForTest(t, store))

	deleted, err := store.DiscardScope(ctx, syncdomain.SKService())
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.RetryWorkKey{second}, deleted)
	assert.Empty(t, foreignKeyViolationsForTest(t, store))
	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// Validates: R-2.5.6
func TestRetryWorkSchema_ChecksRefuseTheStatesTheEngineUsedToAssert(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.UpsertBlockScope(ctx, testServiceScopeForTest(store)))
	service := syncdomain.SKService().String()

	insert := `INSERT INTO retry_work
		(path, action_type, scope_key, blocked, attempt_count, next_retry_at, blocked_scope_key)
		VALUES (?, 'upload', ?, ?, ?, ?, ?)`
	tests := []struct {
		name string
		args []any
	}{
		{"empty path", []any{"", "", 0, 1, 5, nil}},
		{"no attempt", []any{"a.txt", "", 0, 0, 5, nil}},
		{"blocked with retry timing", []any{"a.txt", service, 1, 1, 5, service}},
		{"ready without retry timing", []any{"a.txt", "", 0, 1, 0, nil}},
		{"blocked without a scope", []any{"a.txt", "", 1, 1, 0, nil}},
		{"a key on a ready row", []any{"a.txt", service, 0, 1, 5, service}},
		{"a key that is not the row's scope", []any{"a.txt", syncdomain.SKQuotaOwn().String(), 1, 1, 0, service}},
	}
	for i, tc := range tests {
		if tc.args[0] != "" {
			tc.args[0] = fmt.Sprintf("case-%d.txt", i) // a path of its own: no case fails on another's row
		}
		_, err := store.db.ExecContext(ctx, insert, tc.args...)
		require.Error(t, err, tc.name)
	}

	_, err := store.db.ExecContext(ctx, insert, "ok.txt", service, 1, 1, 0, service)
	require.NoError(t, err, "the valid blocked row the cases vary")
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO block_scopes (scope_key, trial_interval, next_trial_at) VALUES ('', 1, 1)`)
	require.Error(t, err, "a scope row needs a key")
}

// Validates: R-2.5.6
func TestApplySchema_RefusesAGeneration28Store(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", SqliteFileURI(filepath.Join(t.TempDir(), "state.db"), nil))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	_, err = db.ExecContext(t.Context(), CanonicalSchemaSQLForTest())
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, 28)
	require.NoError(t, err)

	err = applySchema(t.Context(), db)
	require.ErrorIs(t, err, errIncompatibleSchema)
	assert.Contains(t, err.Error(), "generation 28 is unsupported")
}
