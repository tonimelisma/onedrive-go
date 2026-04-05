package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func TestFormatNanoTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nanos int64
		want  string
	}{
		{name: "zero returns empty", nanos: 0, want: ""},
		{name: "unix epoch", nanos: 1, want: "1970-01-01T00:00:00Z"},
		{name: "known timestamp", nanos: 1704067200_000000000, want: "2024-01-01T00:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, formatNanoTimestamp(tt.nanos))
		})
	}
}

func TestNewIssuesCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newIssuesCmd()
	assert.Equal(t, "issues", cmd.Use)
	assert.False(t, cmd.Hidden)
	assert.Nil(t, cmd.Flags().Lookup("history"))

	subcommands := cmd.Commands()
	require.Len(t, subcommands, 1)
	assert.Equal(t, "force-deletes", subcommands[0].Name())
}

func TestIssuesCmd_RejectsUnexpectedPositionalArgs(t *testing.T) {
	t.Parallel()

	cmd := newIssuesCmd()
	cmd.SetArgs([]string{"unexpected"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestPrintGroupedIssuesText_ReadOnlySectionsOnly(t *testing.T) {
	t.Parallel()

	snapshot := syncstore.IssuesSnapshot{
		Conflicts: []synctypes.ConflictRecord{
			{ID: "conflict-1", Path: "/conflict.txt", ConflictType: "edit_edit", DetectedAt: 1},
		},
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryInvalidFilename,
				PrimaryIssueType: synctypes.IssueInvalidFilename,
				Paths:            []string{"docs/CON"},
				Count:            1,
			},
		},
		HeldDeletes: []syncstore.HeldDeleteSnapshot{
			{Path: "delete/a.txt", LastSeenAt: 1700000000000000000},
		},
		PendingRetries: []syncstore.PendingRetrySnapshot{
			{ScopeLabel: "retry", Count: 1, EarliestNext: time.Now()},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, snapshot, false))

	output := buf.String()
	assert.Contains(t, output, "INVALID FILENAME")
	assert.Contains(t, output, "HELD DELETES")
	assert.NotContains(t, output, "CONFLICTS")
	assert.NotContains(t, output, "PENDING RETRIES")
	assert.NotContains(t, output, "Recheck:")
	assert.NotContains(t, output, "Retry trial requested")
}

// Validates: R-2.3.10
func TestPrintGroupedIssuesJSON_ReadOnlyOutput(t *testing.T) {
	t.Parallel()

	snapshot := syncstore.IssuesSnapshot{
		Conflicts: []synctypes.ConflictRecord{
			{ID: "ignored-conflict", Path: "/conflict.txt", ConflictType: "edit_edit", DetectedAt: 1},
		},
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryQuotaExceeded,
				PrimaryIssueType: synctypes.IssueQuotaExceeded,
				ScopeLabel:       "your OneDrive storage",
				Paths:            []string{"/a.txt", "/b.txt"},
				Count:            2,
			},
		},
		HeldDeletes: []syncstore.HeldDeleteSnapshot{
			{Path: "/deleted.txt", LastSeenAt: 2000000000},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesJSON(&buf, snapshot))

	var out issuesOutputJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	require.Len(t, out.FailureGroups, 1)
	assert.Equal(t, "QUOTA EXCEEDED", out.FailureGroups[0].Title)
	require.Len(t, out.HeldDeletes, 1)
	assert.Equal(t, "/deleted.txt", out.HeldDeletes[0].Path)
	assert.NotContains(t, buf.String(), "\"conflicts\"")
}

// Validates: R-2.3.3, R-2.3.10, R-2.10.45
func TestIssuesService_RunList_SurfacesAuthScopeWithoutFakePaths(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	canonicalID, err := driveid.NewCanonicalID("personal:user@example.com")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o750))

	dbPath := config.DriveStatePath(canonicalID)
	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	require.NoError(t, mgr.UpsertScopeBlock(t.Context(), &synctypes.ScopeBlock{
		Key:          synctypes.SKAuthAccount(),
		IssueType:    synctypes.IssueUnauthorized,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
	}))

	t.Run("text", func(t *testing.T) {
		var buf bytes.Buffer
		svc := newIssuesService(&CLIContext{
			Logger:       logger,
			OutputWriter: &buf,
			Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
		})

		require.NoError(t, svc.runList(t.Context()))

		output := buf.String()
		assert.Contains(t, output, "AUTHENTICATION REQUIRED")
		assert.Contains(t, output, "Scope: your OneDrive account authorization")
		assert.NotContains(t, output, "No issues.")
		assert.NotContains(t, output, "  /")
	})

	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		svc := newIssuesService(&CLIContext{
			Logger:       logger,
			OutputWriter: &buf,
			Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
			Flags:        CLIFlags{JSON: true},
		})

		require.NoError(t, svc.runList(t.Context()))

		var out issuesOutputJSON
		require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
		require.Len(t, out.FailureGroups, 1)
		assert.Equal(t, synctypes.IssueUnauthorized, out.FailureGroups[0].IssueType)
		assert.Equal(t, "your OneDrive account authorization", out.FailureGroups[0].Scope)
		assert.Empty(t, out.FailureGroups[0].Paths)
	})
}

// Validates: R-6.10.5
func TestIssuesService_RunList_UsesReadOnlyInspector(t *testing.T) {
	setTestDriveHome(t)

	canonicalID, err := driveid.NewCanonicalID("personal:readonly@example.com")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o750))

	dbPath := config.DriveStatePath(canonicalID)
	logger := slog.New(slog.DiscardHandler)
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	require.NoError(t, store.RecordFailure(t.Context(), &synctypes.SyncFailureParams{
		Path:       "docs/CON",
		DriveID:    driveid.New("drive-1"),
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		IssueType:  synctypes.IssueInvalidFilename,
		Category:   synctypes.CategoryActionable,
		ErrMsg:     "reserved name",
	}, nil))
	require.NoError(t, store.Close(t.Context()))

	require.NoError(t, os.Chmod(dbPath, 0o400))

	var buf bytes.Buffer
	svc := newIssuesService(&CLIContext{
		Logger:       logger,
		OutputWriter: &buf,
		Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
	})

	require.NoError(t, svc.runList(t.Context()))
	assert.Contains(t, buf.String(), "INVALID FILENAME")
}

func newHeldDeleteIssuesCmd(t *testing.T) (*cobra.Command, string, *bytes.Buffer) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-issues.db")
	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	ctx := t.Context()
	driveID := driveid.New("drive-1")
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "delete/a.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionDelete,
		ActionType: synctypes.ActionRemoteDelete,
		IssueType:  synctypes.IssueBigDeleteHeld,
		Category:   synctypes.CategoryActionable,
		ErrMsg:     "held delete",
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "docs/CON",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		IssueType:  synctypes.IssueInvalidFilename,
		Category:   synctypes.CategoryActionable,
		ErrMsg:     "reserved name",
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs/a.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		Category:   synctypes.CategoryTransient,
		ErrMsg:     "shared folder is read-only",
		ScopeKey:   synctypes.SKPermRemote("Shared/Docs"),
	}, nil))
	require.NoError(t, mgr.Close(ctx))

	xdgDir := filepath.Join(tmpDir, "xdg-data")
	require.NoError(t, os.MkdirAll(filepath.Join(xdgDir, "onedrive-go"), 0o700))
	t.Setenv("XDG_DATA_HOME", xdgDir)

	cid := driveid.MustCanonicalID("personal:test@example.com")
	expectedPath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(expectedPath), 0o700))
	require.NoError(t, os.Symlink(dbPath, expectedPath))

	var buf bytes.Buffer
	cc := &CLIContext{
		StatusWriter: &buf,
		OutputWriter: &buf,
		Logger:       logger,
		Cfg:          &config.ResolvedDrive{CanonicalID: cid},
	}

	cmd := newIssuesCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	return cmd, expectedPath, &buf
}

func TestIssuesForceDeletes_ClearsHeldDeletesOnly(t *testing.T) {
	cmd, dbPath, out := newHeldDeleteIssuesCmd(t)

	cmd.SetArgs([]string{"force-deletes"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Approved all held deletes for this drive.")

	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	rows, err := mgr.ListSyncFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"docs/CON", "Shared/Docs/a.txt"}, []string{rows[0].Path, rows[1].Path})
}
