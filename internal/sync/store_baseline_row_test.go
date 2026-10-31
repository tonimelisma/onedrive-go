package sync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.5.2
func TestUpsertBaselineRowTx_RoundTripsEveryColumn(t *testing.T) {
	t.Parallel()

	ctx := synctest.TestContext(t)
	store := newTestStore(t)

	// Every field set to a distinct non-zero value, so a column the helper
	// drops, swaps or binds out of order changes what Load reads back.
	written := &BaselineEntry{
		Path:             "docs/report.txt",
		ItemID:           "item-1",
		ParentID:         "parent-1",
		ItemType:         ItemTypeFile,
		LocalHash:        "local-hash",
		RemoteHash:       "remote-hash",
		LocalSize:        11,
		LocalSizeKnown:   true,
		RemoteSize:       12,
		RemoteSizeKnown:  true,
		LocalMtime:       13,
		RemoteMtime:      14,
		LocalCtime:       15,
		LocalDevice:      16,
		LocalInode:       17,
		LocalBirthTime:   18,
		LocalHasIdentity: true,
		ETag:             "etag-1",
	}
	require.NoError(t, upsertBaselineRowTx(ctx, store.rawDB(), written))

	baseline, err := store.Load(ctx)
	require.NoError(t, err)
	read, ok := baseline.GetByPath(written.Path)
	require.True(t, ok, "the upserted row must load")
	assert.Equal(t, written, read)

	// The upsert replaces every column of the item's existing row.
	rewritten := *written
	rewritten.Path = "docs/renamed.txt"
	rewritten.LocalHash = "local-hash-2"
	rewritten.LocalBirthTime = 28
	rewritten.ETag = "etag-2"
	require.NoError(t, upsertBaselineRowTx(ctx, store.rawDB(), &rewritten))

	require.NoError(t, store.reloadBaselineCache(ctx))
	baseline, err = store.Load(ctx)
	require.NoError(t, err)
	_, stale := baseline.GetByPath(written.Path)
	assert.False(t, stale, "the upsert keys on the item, so the old path is gone")
	read, ok = baseline.GetByPath(rewritten.Path)
	require.True(t, ok)
	assert.Equal(t, &rewritten, read)
}

// Validates: R-2.5.2
func TestSQLUpsertBaseline_WritesEveryBaselineColumn(t *testing.T) {
	t.Parallel()

	ctx := synctest.TestContext(t)
	store := newTestStore(t)

	rows, err := store.rawDB().QueryContext(ctx, `SELECT name FROM pragma_table_info('baseline')`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()

	insertColumns, _, found := strings.Cut(sqlUpsertBaseline, "VALUES")
	require.True(t, found)
	columns := 0
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns++
		assert.Contains(t, insertColumns, column,
			"sqlUpsertBaseline and baselineRowArgs must write every baseline column")
	}
	require.NoError(t, rows.Err())
	assert.Len(t, baselineRowArgs(&BaselineEntry{}), columns,
		"baselineRowArgs binds one value per baseline column")
}
