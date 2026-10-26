package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// Validates: R-2.10.63
func TestIsLocalDiskFull_RecognizesRealSQLiteFull(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "full.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(t.Context(), "CREATE TABLE t (v BLOB)")
	require.NoError(t, err)
	// Cap the database at its current size so the next large write is
	// exactly what a full disk produces: SQLITE_FULL from the real driver.
	var pages int
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages))
	_, err = db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA max_page_count = %d", pages))
	require.NoError(t, err)

	_, writeErr := db.ExecContext(t.Context(), "INSERT INTO t (v) VALUES (?)", []byte(strings.Repeat("x", 1<<20)))
	require.Error(t, writeErr)

	wrapped := fmt.Errorf("sync: commit outcome: %w", writeErr)
	assert.True(t, IsLocalDiskFull(wrapped), "SQLITE_FULL is a full local disk: %v", writeErr)
}

// Validates: R-2.10.60, R-2.10.63
func TestIsLocalDiskFull_LocalErrnosAndOthers(t *testing.T) {
	t.Parallel()

	assert.True(t, IsLocalDiskFull(&os.PathError{Op: "write", Path: "a", Err: syscall.ENOSPC}))
	assert.True(t, IsLocalDiskFull(fmt.Errorf("x: %w", driveops.ErrDiskFull)))
	assert.False(t, IsLocalDiskFull(errors.New("database is locked")))
	assert.False(t, IsLocalDiskFull(nil))
}
