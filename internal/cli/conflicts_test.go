package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

func TestTruncateID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "longer than prefix", id: "abcdefghijklmnop", want: "abcdefgh"},
		{name: "exact prefix length", id: "abcdefgh", want: "abcdefgh"},
		{name: "shorter than prefix", id: "abc", want: "abc"},
		{name: "empty string", id: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, truncateID(tt.id))
		})
	}
}

func TestToConflictJSON(t *testing.T) {
	t.Parallel()

	c := &synctypes.ConflictRecord{
		ID:           "abc123",
		Path:         "/foo.txt",
		ConflictType: "edit_edit",
		DetectedAt:   1700000000000000000,
		LocalHash:    "aaa",
		RemoteHash:   "bbb",
		Resolution:   "keep_local",
		ResolvedBy:   "user",
		ResolvedAt:   1700000001000000000,
	}

	j := toConflictJSON(c)
	assert.Equal(t, "abc123", j.ID)
	assert.Equal(t, "/foo.txt", j.Path)
	assert.Equal(t, "edit_edit", j.ConflictType)
	assert.NotEmpty(t, j.DetectedAt)
	assert.Equal(t, "aaa", j.LocalHash)
	assert.Equal(t, "bbb", j.RemoteHash)
	assert.Equal(t, "keep_local", j.Resolution)
	assert.Equal(t, "user", j.ResolvedBy)
	assert.NotEmpty(t, j.ResolvedAt)
}

func TestNewConflictsCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newConflictsCmd()
	assert.Equal(t, "conflicts", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("history"))

	resolveCmd, _, err := cmd.Find([]string{"resolve"})
	require.NoError(t, err)
	assert.Equal(t, "resolve [path-or-id]", resolveCmd.Use)
}

func newEmptyConflictsCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	setTestDriveHome(t)

	var buf bytes.Buffer
	cc := &CLIContext{
		StatusWriter: &buf,
		OutputWriter: &buf,
		Logger:       slog.New(slog.DiscardHandler),
		Cfg:          &config.ResolvedDrive{CanonicalID: driveid.MustCanonicalID("personal:test@example.com")},
	}

	cmd := newConflictsCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	return cmd, &buf
}

func TestConflictsCmd_RejectsUnexpectedPositionalArgs(t *testing.T) {
	cmd, _ := newEmptyConflictsCmd(t)
	cmd.SetArgs([]string{"unexpected"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestConflictsCmd_HistoryFlagStillWorksFromRoot(t *testing.T) {
	cmd, out := newEmptyConflictsCmd(t)
	cmd.SetArgs([]string{"--history"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "No conflicts in history.")
}

func TestConflictsCmd_ResolveSubcommandStillExecutesFromRoot(t *testing.T) {
	cmd, _ := newEmptyConflictsCmd(t)
	cmd.SetArgs([]string{"resolve", "--keep-both"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "specify a conflict path or ID, or use --all to resolve all conflicts")
}

// Validates: R-2.3.4
func TestPrintConflictsJSON(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{
			ID:           "conflict-001",
			Path:         "/docs/readme.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printConflictsJSON(&buf, conflicts))

	var result conflictsOutputJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Conflicts, 1)
	assert.Equal(t, "conflict-001", result.Conflicts[0].ID)
}

// Validates: R-2.3.4
func TestPrintConflictsTable(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "abcdefghijklmnop", Path: "/test.txt", ConflictType: "edit_edit", DetectedAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	require.NoError(t, printConflictsTable(&buf, conflicts, false))
	output := buf.String()
	assert.Contains(t, output, "abcdefgh")
	assert.Contains(t, output, "/test.txt")
	assert.Contains(t, output, "edit_edit")
}

func TestPrintConflictsTable_WithHistory(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{
			ID:           "abcdefghijklmnop",
			Path:         "/resolved.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
			Resolution:   "keep_local",
			ResolvedBy:   "user",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printConflictsTable(&buf, conflicts, true))

	output := buf.String()
	assert.Contains(t, output, "RESOLUTION")
	assert.Contains(t, output, "RESOLVED BY")
	assert.Contains(t, output, "keep_local")
	assert.Contains(t, output, "user")
}

func TestFindConflict(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "aabb1122-dead-beef-cafe-000000000001", Path: "/foo/bar.txt"},
		{ID: "aabb1122-dead-beef-cafe-000000000002", Path: "/baz/qux.txt"},
		{ID: "ccdd3344-dead-beef-cafe-000000000003", Path: "/other/file.txt"},
	}

	got, found, err := findConflict(conflicts, "/foo/bar.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, got)
	assert.Equal(t, "aabb1122-dead-beef-cafe-000000000001", got.ID)

	_, _, err = findConflict(conflicts, "aabb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"aabb"`)
}

func newTestCLIContext(w io.Writer) *CLIContext {
	return &CLIContext{
		StatusWriter: w,
		Logger:       slog.New(slog.DiscardHandler),
	}
}

func TestResolveEachConflict_ResolvesAll(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "id-1", Path: "/foo.txt"},
		{ID: "id-2", Path: "/bar.txt"},
	}

	var resolved []string
	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveEachConflict(cc, conflicts, "keep_both", false, func(id, _ string) (string, error) {
		resolved = append(resolved, id)
		return "queued", nil
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"id-1", "id-2"}, resolved)
	assert.Contains(t, buf.String(), "Queued /foo.txt as keep_both (engine will resolve on the next sync pass)")
	assert.Contains(t, buf.String(), "Queued /bar.txt as keep_both (engine will resolve on the next sync pass)")
}

// Validates: R-2.3.4, R-2.3.12
func TestResolveSingleConflict_AlreadyResolvedIsReplaySafe(t *testing.T) {
	t.Parallel()

	resolvedConflicts := []synctypes.ConflictRecord{
		{ID: "id-1", Path: "/foo.txt", Resolution: synctypes.ResolutionKeepBoth},
	}

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveSingleConflict(cc, "/foo.txt", "keep_local", false,
		func() ([]synctypes.ConflictRecord, error) { return nil, nil },
		func() ([]synctypes.ConflictRecord, error) { return resolvedConflicts, nil },
		func(_, _ string) (string, error) {
			require.Fail(t, "resolveFn should not be called for already resolved conflicts")
			return "", nil
		},
	)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "already resolved as keep_both")
}

func TestResolveStrategy(t *testing.T) {
	t.Parallel()

	cmd := newConflictsResolveCmd()
	require.NoError(t, cmd.Flags().Set("keep-both", "true"))
	got, err := resolveStrategy(cmd)
	require.NoError(t, err)
	assert.Equal(t, resolutionKeepBoth, got)
}

// Validates: R-2.3.4
func TestConflictsService_RunList_UsesDedicatedCommandSurface(t *testing.T) {
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

	_, err = mgr.DB().ExecContext(t.Context(), `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
		('c1', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved'),
		('c2', ?, 'item-2', '/resolved.txt', 'edit_edit', 2, 'keep_local')`,
		canonicalID.String(), canonicalID.String(),
	)
	require.NoError(t, err)

	t.Run("text unresolved only", func(t *testing.T) {
		var buf bytes.Buffer
		svc := newConflictsService(&CLIContext{
			Logger:       logger,
			OutputWriter: &buf,
			Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
		})

		require.NoError(t, svc.runList(context.Background(), false))
		assert.Contains(t, buf.String(), "/conflict.txt")
		assert.NotContains(t, buf.String(), "/resolved.txt")
	})

	t.Run("json history", func(t *testing.T) {
		var buf bytes.Buffer
		svc := newConflictsService(&CLIContext{
			Logger:       logger,
			OutputWriter: &buf,
			Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
			Flags:        CLIFlags{JSON: true},
		})

		require.NoError(t, svc.runList(context.Background(), true))

		var out conflictsOutputJSON
		require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
		require.Len(t, out.Conflicts, 2)
	})
}

// Validates: R-6.10.5
func TestConflictsService_RunList_UsesReadOnlyProjectionHelper(t *testing.T) {
	setTestDriveHome(t)

	canonicalID := driveid.MustCanonicalID("personal:readonly-conflicts@example.com")
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o750))

	dbPath := config.DriveStatePath(canonicalID)
	logger := slog.New(slog.DiscardHandler)
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`,
		canonicalID.String(),
	)
	require.NoError(t, err)

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	require.Eventually(t, func() bool {
		_, walErr := os.Stat(walPath)
		_, shmErr := os.Stat(shmPath)
		return walErr == nil && shmErr == nil
	}, time.Second, 10*time.Millisecond, "WAL sidecars were not created")

	dbDir := filepath.Dir(dbPath)
	require.NoError(t, os.Chmod(dbPath, 0o400))
	// #nosec G302 -- test intentionally makes the directory read-only to prove conflict listing stays on the read-only path.
	require.NoError(t, os.Chmod(dbDir, 0o500))
	t.Cleanup(func() {
		// #nosec G302 -- cleanup restores the temp state dir so the writable store can close.
		assert.NoError(t, os.Chmod(dbDir, 0o700))
		assert.NoError(t, os.Chmod(dbPath, 0o600))
		assert.NoError(t, store.Close(context.Background()))
	})

	var buf bytes.Buffer
	svc := newConflictsService(&CLIContext{
		Logger:       logger,
		OutputWriter: &buf,
		Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
	})

	require.NoError(t, svc.runList(t.Context(), false))
	assert.Contains(t, buf.String(), "/conflict.txt")
}

func TestConflictsService_RunResolve_AllDryRun(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	svc := newConflictsService(&CLIContext{
		StatusWriter: &buf,
		Logger:       slog.New(slog.DiscardHandler),
	})

	err := resolveEachConflict(svc.cc, []synctypes.ConflictRecord{{ID: "id-1", Path: "/foo.txt"}}, "keep_local", true, func(_, _ string) (string, error) {
		return "", fmt.Errorf("should not be called")
	})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "Would resolve /foo.txt")
}

// Validates: R-2.3.12
func TestConflictsService_RequestConflictResolutionConcurrentCLIsFirstWriterWins(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	canonicalID := driveid.MustCanonicalID("personal:concurrent-conflicts@example.com")
	logger := slog.New(slog.DiscardHandler)
	store, err := syncstore.NewSyncStore(t.Context(), config.DriveStatePath(canonicalID), logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	_, err = store.DB().ExecContext(t.Context(), `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
			('conflict-concurrent-cli', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`,
		canonicalID.String(),
	)
	require.NoError(t, err)

	svc := newConflictsService(&CLIContext{
		Logger: logger,
		Cfg:    &config.ResolvedDrive{CanonicalID: canonicalID},
	})

	const requestCount = 16
	statuses := make(chan syncstore.ConflictRequestStatus, requestCount)
	errorsCh := make(chan error, requestCount)
	start := make(chan struct{})
	ctx := t.Context()
	var wg sync.WaitGroup

	for i := range requestCount {
		strategy := synctypes.ResolutionKeepLocal
		if i%2 == 1 {
			strategy = synctypes.ResolutionKeepRemote
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, requestErr := svc.requestConflictResolution(
				ctx,
				store,
				"conflict-concurrent-cli",
				strategy,
			)
			if requestErr != nil {
				errorsCh <- requestErr
				return
			}
			statuses <- result.Status
		}()
	}

	close(start)
	wg.Wait()
	close(statuses)
	close(errorsCh)

	require.Empty(t, errorsCh)

	queued := 0
	for status := range statuses {
		switch status {
		case syncstore.ConflictRequestQueued:
			queued++
		case syncstore.ConflictRequestAlreadyQueued, syncstore.ConflictRequestDifferentStrategy:
		case syncstore.ConflictRequestAlreadyResolving, syncstore.ConflictRequestAlreadyResolved:
			require.Failf(t, "unexpected terminal conflict request status", "status=%s", status)
		default:
			require.Failf(t, "unexpected conflict request status", "status=%s", status)
		}
	}
	assert.Equal(t, 1, queued)

	conflict, err := store.GetConflictRequest(t.Context(), "conflict-concurrent-cli")
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateResolutionRequested, conflict.State)
	assert.Contains(t, []string{synctypes.ResolutionKeepLocal, synctypes.ResolutionKeepRemote}, conflict.RequestedResolution)
}
