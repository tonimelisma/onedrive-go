package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.15.1
func TestRunStateAuditClean(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	var stdout bytes.Buffer
	err = RunStateAudit(t.Context(), StateAuditOptions{
		DBPath: dbPath,
		Stdout: &stdout,
	})
	require.NoError(t, err)
	assert.Equal(t, "state audit: clean\n", stdout.String())
}

// Validates: R-2.15.1
func TestRunStateAuditReportsFindingsInJSON(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	_, err = store.DB().ExecContext(t.Context(), `
		INSERT INTO scope_blocks
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES (?, ?, 'backoff', ?, ?, ?, ?, 2)`,
		synctypes.SKAuthAccount().String(),
		synctypes.IssueUnauthorized,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
		int64(time.Minute),
		time.Date(2026, 4, 3, 10, 1, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 4, 3, 10, 2, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	var stdout bytes.Buffer
	err = RunStateAudit(t.Context(), StateAuditOptions{
		DBPath: dbPath,
		JSON:   true,
		Stdout: &stdout,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finding")

	var output stateAuditOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	assert.False(t, output.Clean)
	require.NotEmpty(t, output.Findings)
	assert.Equal(t, "invalid_auth_scope_timing", output.Findings[0].Code)
}

// Validates: R-2.15.1
func TestRunStateAuditRepairSafe(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	_, err = store.DB().ExecContext(t.Context(), `
		INSERT INTO scope_blocks
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES (?, ?, 'backoff', ?, ?, ?, ?, 1)`,
		synctypes.SKAuthAccount().String(),
		synctypes.IssueUnauthorized,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
		int64(time.Minute),
		time.Date(2026, 4, 3, 10, 1, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 4, 3, 10, 2, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	var stdout bytes.Buffer
	err = RunStateAudit(t.Context(), StateAuditOptions{
		DBPath:     dbPath,
		RepairSafe: true,
		Stdout:     &stdout,
	})
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "clean after")
}
