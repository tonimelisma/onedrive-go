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

// --- truncateID ---

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
		{name: "one char", id: "x", want: "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateID(tt.id)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- formatNanoTimestamp ---

func TestFormatNanoTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nanos int64
		want  string
	}{
		{name: "zero returns empty", nanos: 0, want: ""},
		{name: "unix epoch", nanos: 0 + 1, want: "1970-01-01T00:00:00Z"},
		{name: "known timestamp", nanos: 1704067200_000000000, want: "2024-01-01T00:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatNanoTimestamp(tt.nanos)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- toConflictJSON ---

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

// --- command structure ---

func TestNewIssuesCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newIssuesCmd()
	assert.Equal(t, "issues", cmd.Use)
	assert.False(t, cmd.Hidden)

	// Has resolve, clear, retry, and recheck subcommands.
	resolveCmd, _, err := cmd.Find([]string{"resolve"})
	require.NoError(t, err)
	assert.Equal(t, "resolve [path-or-id]", resolveCmd.Use)

	clearCmd, _, err := cmd.Find([]string{"clear"})
	require.NoError(t, err)
	assert.Equal(t, "clear [path]", clearCmd.Use)

	retryCmd, _, err := cmd.Find([]string{"retry"})
	require.NoError(t, err)
	assert.Equal(t, "retry [path]", retryCmd.Use)

	recheckCmd, _, err := cmd.Find([]string{"recheck"})
	require.NoError(t, err)
	assert.Equal(t, "recheck <boundary>", recheckCmd.Use)
}

func TestIssuesResolveCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newIssuesCmd()
	resolveCmd, _, err := cmd.Find([]string{"resolve"})
	require.NoError(t, err)
	assert.Equal(t, "resolve [path-or-id]", resolveCmd.Use)
	assert.False(t, resolveCmd.Hidden)

	for _, flag := range []string{"keep-local", "keep-remote", "keep-both", "all", "dry-run"} {
		assert.NotNil(t, resolveCmd.Flags().Lookup(flag), "missing flag %q", flag)
	}
}

// --- printConflictsTable ---

// Validates: R-2.3.3
func TestPrintConflictsTable(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{
			ID:           "abcdefghijklmnop",
			Path:         "/test.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printConflictsTable(&buf, conflicts, false))

	output := buf.String()
	assert.Contains(t, output, "abcdefgh") // truncated ID
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
	assert.Contains(t, output, "/resolved.txt")
}

// --- findConflict ---

func TestFindConflict(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "aabb1122-dead-beef-cafe-000000000001", Path: "/foo/bar.txt"},
		{ID: "aabb1122-dead-beef-cafe-000000000002", Path: "/baz/qux.txt"},
		{ID: "ccdd3344-dead-beef-cafe-000000000003", Path: "/other/file.txt"},
	}

	tests := []struct {
		name        string
		idOrPath    string
		wantID      string
		wantNil     bool
		wantErr     bool
		errContains string
	}{
		{name: "exact ID match", idOrPath: "aabb1122-dead-beef-cafe-000000000001", wantID: "aabb1122-dead-beef-cafe-000000000001"},
		{name: "exact path match", idOrPath: "/foo/bar.txt", wantID: "aabb1122-dead-beef-cafe-000000000001"},
		{name: "unique prefix", idOrPath: "ccdd", wantID: "ccdd3344-dead-beef-cafe-000000000003"},
		{name: "ambiguous prefix", idOrPath: "aabb", wantErr: true, errContains: `"aabb"`},
		{name: "no match", idOrPath: "zzzz", wantNil: true},
		{name: "full ID exact takes priority over prefix", idOrPath: "aabb1122-dead-beef-cafe-000000000002", wantID: "aabb1122-dead-beef-cafe-000000000002"},
		{name: "empty string returns nil", idOrPath: "", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, found, err := findConflict(conflicts, tt.idOrPath)
			if tt.wantErr {
				require.Error(t, err)

				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}

				return
			}

			require.NoError(t, err)

			if tt.wantNil {
				assert.False(t, found)
				assert.Nil(t, got)

				return
			}

			assert.True(t, found)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantID, got.ID)
		})
	}
}

// --- resolve helpers ---

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
	resolveFn := func(id, _ string) error {
		resolved = append(resolved, id)
		return nil
	}

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveEachConflict(cc, conflicts, "keep_both", false, resolveFn)
	require.NoError(t, err)

	assert.Equal(t, []string{"id-1", "id-2"}, resolved)
	assert.Contains(t, buf.String(), "Resolved /foo.txt as keep_both")
	assert.Contains(t, buf.String(), "Resolved /bar.txt as keep_both")
}

func TestResolveEachConflict_DryRun(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "id-1", Path: "/foo.txt"},
	}

	resolveCalled := false
	resolveFn := func(_, _ string) error {
		resolveCalled = true
		return nil
	}

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveEachConflict(cc, conflicts, "keep_local", true, resolveFn)
	require.NoError(t, err)

	assert.False(t, resolveCalled, "resolveFn should not be called in dry-run mode")
	assert.Contains(t, buf.String(), "Would resolve")
}

func TestResolveEachConflict_EmptyConflicts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveEachConflict(cc, nil, "keep_both", false, func(_, _ string) error {
		require.Fail(t, "should not be called")
		return nil
	})
	require.NoError(t, err)

	assert.Contains(t, buf.String(), "No unresolved conflicts")
}

func TestResolveEachConflict_ErrorPropagation(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "id-1", Path: "/foo.txt"},
	}

	resolveFn := func(_, _ string) error {
		return fmt.Errorf("db error")
	}

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveEachConflict(cc, conflicts, "keep_both", false, resolveFn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolving /foo.txt")
	assert.Contains(t, err.Error(), "db error")
}

func TestResolveSingleConflict_ExactMatch(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "id-1", Path: "/foo.txt"},
		{ID: "id-2", Path: "/bar.txt"},
	}

	var resolvedID string
	resolveFn := func(id, _ string) error {
		resolvedID = id
		return nil
	}

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveSingleConflict(cc, "/bar.txt", "keep_local", false,
		func() ([]synctypes.ConflictRecord, error) { return conflicts, nil },
		func() ([]synctypes.ConflictRecord, error) { return conflicts, nil },
		resolveFn,
	)
	require.NoError(t, err)
	assert.Equal(t, "id-2", resolvedID)
}

func TestResolveSingleConflict_NotFound(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cc := newTestCLIContext(&buf)

	err := resolveSingleConflict(cc, "nonexistent", "keep_both", false,
		func() ([]synctypes.ConflictRecord, error) { return nil, nil },
		func() ([]synctypes.ConflictRecord, error) { return nil, nil },
		func(_, _ string) error { return nil },
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflict not found")
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
		func(_, _ string) error {
			require.Fail(t, "resolveFn should not be called for already resolved conflicts")
			return nil
		},
	)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "already resolved as keep_both")
}

// --- resolveStrategy ---

func TestResolveStrategy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flag string
		want string
	}{
		{"keep-local", "keep-local", resolutionKeepLocal},
		{"keep-remote", "keep-remote", resolutionKeepRemote},
		{"keep-both", "keep-both", resolutionKeepBoth},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := newIssuesResolveCmd()
			require.NoError(t, cmd.Flags().Set(tt.flag, "true"))

			got, err := resolveStrategy(cmd)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveStrategy_NoFlag(t *testing.T) {
	t.Parallel()

	cmd := newIssuesResolveCmd()

	_, err := resolveStrategy(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strategy")
}

// --- unified issues JSON ---

// Validates: R-2.3.10
func TestPrintGroupedIssuesJSON_MixedOutput(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{
			ID:           "conflict-001",
			Path:         "/docs/readme.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
		},
	}

	snapshot := syncstore.IssuesSnapshot{
		Conflicts: conflicts,
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryInvalidFilename,
				PrimaryIssueType: synctypes.IssueInvalidFilename,
				Paths:            []string{"docs/CON"},
				Count:            1,
			},
		},
	}

	var buf bytes.Buffer
	err := printGroupedIssuesJSON(&buf, snapshot)
	require.NoError(t, err)

	var result issuesOutputJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Conflicts, 1)
	assert.Equal(t, "conflict-001", result.Conflicts[0].ID)
	require.Len(t, result.FailureGroups, 1)
	assert.Equal(t, "INVALID FILENAME", result.FailureGroups[0].Title)
	assert.Contains(t, result.FailureGroups[0].Paths, "docs/CON")
}

// --- grouped issues text ---

// Validates: R-2.3.7, R-2.3.8
func TestPrintGroupedIssuesText_BothSections(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "abcdefghijklmnop", Path: "/test.txt", ConflictType: "edit_edit", DetectedAt: 1700000000000000000},
	}

	snapshot := syncstore.IssuesSnapshot{
		Conflicts: conflicts,
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryInvalidFilename,
				PrimaryIssueType: synctypes.IssueInvalidFilename,
				Paths:            []string{"docs/CON"},
				Count:            1,
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, snapshot, false, false))

	output := buf.String()
	assert.Contains(t, output, "CONFLICTS")
	assert.Contains(t, output, "/test.txt")
	assert.Contains(t, output, "INVALID FILENAME")
	assert.Contains(t, output, "docs/CON")
}

func TestPrintGroupedIssuesText_OnlyConflicts(t *testing.T) {
	t.Parallel()

	conflicts := []synctypes.ConflictRecord{
		{ID: "abcdefghijklmnop", Path: "/test.txt", ConflictType: "edit_edit", DetectedAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, syncstore.IssuesSnapshot{Conflicts: conflicts}, false, false))

	output := buf.String()
	assert.Contains(t, output, "CONFLICTS")
}

func TestPrintGroupedIssuesText_OnlyFailures(t *testing.T) {
	t.Parallel()

	snapshot := syncstore.IssuesSnapshot{
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryInvalidFilename,
				PrimaryIssueType: synctypes.IssueInvalidFilename,
				Paths:            []string{"docs/CON"},
				Count:            1,
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, snapshot, false, false))

	output := buf.String()
	assert.NotContains(t, output, "CONFLICTS")
	assert.Contains(t, output, "INVALID FILENAME")
}

func TestPrintGroupedIssuesText_HeldDeletes(t *testing.T) {
	t.Parallel()

	heldDeletes := []syncstore.HeldDeleteSnapshot{
		{Path: "file1.txt", LastSeenAt: 1700000000000000000},
		{Path: "file2.txt", LastSeenAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, syncstore.IssuesSnapshot{HeldDeletes: heldDeletes}, false, false))

	output := buf.String()
	assert.Contains(t, output, "HELD DELETES")
	assert.Contains(t, output, "2 files")
	assert.Contains(t, output, "file1.txt")
	assert.Contains(t, output, "file2.txt")
}

func TestPrintGroupedIssuesText_MixedHeldAndOther(t *testing.T) {
	t.Parallel()

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummaryInvalidFilename,
			PrimaryIssueType: synctypes.IssueInvalidFilename,
			Paths:            []string{"docs/CON"},
			Count:            1,
		},
	}

	heldDeletes := []syncstore.HeldDeleteSnapshot{
		{Path: "file1.txt", LastSeenAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, syncstore.IssuesSnapshot{
		Groups:      groups,
		HeldDeletes: heldDeletes,
	}, false, false))

	output := buf.String()
	assert.Contains(t, output, "HELD DELETES")
	assert.Contains(t, output, "1 files")
	assert.Contains(t, output, "file1.txt")
	assert.Contains(t, output, "INVALID FILENAME")
	assert.Contains(t, output, "docs/CON")
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

		require.NoError(t, svc.runList(t.Context(), false))

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

		require.NoError(t, svc.runList(t.Context(), false))

		var out issuesOutputJSON
		require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
		require.Len(t, out.FailureGroups, 1)
		assert.Equal(t, synctypes.IssueUnauthorized, out.FailureGroups[0].IssueType)
		assert.Equal(t, "your OneDrive account authorization", out.FailureGroups[0].Scope)
		assert.Empty(t, out.FailureGroups[0].Paths)
	})
}

// --- issues clear / retry behavioral tests ---

// newSeededIssuesCmd creates a cobra command with a CLIContext backed by a
// real SyncStore in a temp directory, pre-seeded with actionable and transient
// failures. Returns the command and the DB path (for post-assertions).
func newSeededIssuesCmd(t *testing.T) (*cobra.Command, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-issues.db")

	logger := slog.New(slog.DiscardHandler)

	// Create and seed the DB.
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Actionable failure (invalid filename — will be targeted by "clear").
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "docs/CON",
		Direction: synctypes.DirectionUpload,
		IssueType: "invalid_filename",
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil)
	require.NoError(t, err)

	// Transient failure (upload_failed — will be targeted by "retry").
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "data/report.xlsx",
		Direction:  synctypes.DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "connection reset",
		HTTPStatus: 500,
		FileSize:   1024,
	}, nil)
	require.NoError(t, err)

	// Prefix-adjacent transient failure should survive single-path retry.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "data/report.xlsx.bak",
		Direction:  synctypes.DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "connection reset",
		HTTPStatus: 500,
		FileSize:   2048,
	}, nil)
	require.NoError(t, err)

	// Second actionable failure for testing --all.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "docs/NUL.txt",
		Direction: synctypes.DirectionUpload,
		IssueType: "invalid_filename",
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil)
	require.NoError(t, err)

	// Prefix-adjacent actionable failure should survive single-path clear.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "docs/CON-copy",
		Direction: synctypes.DirectionUpload,
		IssueType: "invalid_filename",
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil)
	require.NoError(t, err)

	require.NoError(t, mgr.Close(t.Context()))

	// Build a CLIContext whose Cfg.StatePath() returns our temp DB path.
	// We override XDG_DATA_HOME and pick a CanonicalID that resolves to our path.
	// Simpler: directly set Cfg with the known StatePath by using a custom
	// ResolvedDrive. StatePath() calls DriveStatePath which uses DefaultDataDir,
	// so we set XDG_DATA_HOME to make it predictable.
	xdgDir := filepath.Join(tmpDir, "xdg-data")
	require.NoError(t, os.MkdirAll(filepath.Join(xdgDir, "onedrive-go"), 0o700))
	t.Setenv("XDG_DATA_HOME", xdgDir)

	// Compute the canonical ID that maps to our DB path.
	// DriveStatePath produces: $XDG_DATA_HOME/onedrive-go/state_<sanitized>.db
	// We need to create a symlink or copy the DB to that location.
	cid := driveid.MustCanonicalID("personal:test@example.com")
	expectedPath := config.DriveStatePath(cid)

	require.NoError(t, os.MkdirAll(filepath.Dir(expectedPath), 0o700))
	// Symlink our seeded DB to the expected path.
	require.NoError(t, os.Symlink(dbPath, expectedPath))

	var buf bytes.Buffer
	cc := &CLIContext{
		StatusWriter: &buf,
		Logger:       logger,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
		},
	}

	cmd := newIssuesCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	return cmd, expectedPath
}

func newSeededRemoteBlockedIssuesCmd(t *testing.T) (*cobra.Command, string, *bytes.Buffer) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-remote-blocked.db")
	logger := slog.New(slog.DiscardHandler)

	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs/a.txt",
		DriveID:    driveid.New("d!1"),
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "shared folder is read-only",
		ScopeKey:   synctypes.SKPermRemote("Shared/Docs"),
	}, nil))
	require.NoError(t, mgr.Close(t.Context()))

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
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
		},
	}

	cmd := newIssuesCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	return cmd, expectedPath, &buf
}

// Validates: R-2.3.5
func TestIssuesClear_SinglePath(t *testing.T) {
	cmd, dbPath := newSeededIssuesCmd(t)

	// Execute "issues clear docs/CON" via the parent command.
	cmd.SetArgs([]string{"clear", "docs/CON"})
	require.NoError(t, cmd.Execute())

	// Verify: "docs/CON" is gone, other failures remain.
	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	ctx := context.Background()
	actionable, err := mgr.ListActionableFailures(ctx)
	require.NoError(t, err)

	require.Len(t, actionable, 2)
	assert.ElementsMatch(t, []string{"docs/NUL.txt", "docs/CON-copy"}, []string{
		actionable[0].Path,
		actionable[1].Path,
	})
}

// Validates: R-2.3.5, R-2.3.12
func TestIssuesClear_SinglePath_ReplaySafe(t *testing.T) {
	cmd, dbPath := newSeededIssuesCmd(t)

	cmd.SetArgs([]string{"clear", "docs/CON"})
	require.NoError(t, cmd.Execute())

	cmd.SetArgs([]string{"clear", "docs/CON"})
	require.NoError(t, cmd.Execute())

	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	actionable, err := mgr.ListActionableFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, actionable, 2)
	assert.ElementsMatch(t, []string{"docs/NUL.txt", "docs/CON-copy"}, []string{
		actionable[0].Path,
		actionable[1].Path,
	})
}

// Validates: R-2.3.5
func TestIssuesClear_All(t *testing.T) {
	cmd, dbPath := newSeededIssuesCmd(t)

	cmd.SetArgs([]string{"clear", "--all"})
	require.NoError(t, cmd.Execute())

	// Verify: all actionable failures gone, transient remains.
	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	ctx := context.Background()
	actionable, err := mgr.ListActionableFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, actionable, "all actionable failures should be cleared")

	// Transient failure should still exist.
	all, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.ElementsMatch(t, []string{"data/report.xlsx", "data/report.xlsx.bak"}, []string{
		all[0].Path,
		all[1].Path,
	})
}

// Validates: R-2.3.6
func TestIssuesRetry_SinglePath(t *testing.T) {
	cmd, dbPath := newSeededIssuesCmd(t)

	cmd.SetArgs([]string{"retry", "data/report.xlsx"})
	require.NoError(t, cmd.Execute())

	// Verify: the transient failure for "data/report.xlsx" is cleared.
	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	ctx := context.Background()
	all, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{
		"docs/CON",
		"docs/NUL.txt",
		"docs/CON-copy",
		"data/report.xlsx.bak",
	}, []string{
		all[0].Path,
		all[1].Path,
		all[2].Path,
		all[3].Path,
	})
}

// Validates: R-2.3.6, R-2.3.12
func TestIssuesRetry_SinglePath_ReplaySafe(t *testing.T) {
	cmd, dbPath := newSeededIssuesCmd(t)

	cmd.SetArgs([]string{"retry", "data/report.xlsx"})
	require.NoError(t, cmd.Execute())

	cmd.SetArgs([]string{"retry", "data/report.xlsx"})
	require.NoError(t, cmd.Execute())

	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	rows, err := mgr.ListSyncFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.ElementsMatch(t, []string{
		"docs/CON",
		"docs/NUL.txt",
		"docs/CON-copy",
		"data/report.xlsx.bak",
	}, []string{
		rows[0].Path,
		rows[1].Path,
		rows[2].Path,
		rows[3].Path,
	})
}

// Validates: R-2.3.6
func TestIssuesRetry_All(t *testing.T) {
	cmd, dbPath := newSeededIssuesCmd(t)

	cmd.SetArgs([]string{"retry", "--all"})
	require.NoError(t, cmd.Execute())

	// Verify: transient failures are cleared; actionable remain.
	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	ctx := context.Background()
	all, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)

	// Only actionable failures should remain.
	for _, f := range all {
		assert.Equal(t, synctypes.CategoryActionable, f.Category,
			"only actionable failures should remain after retry --all")
	}
}

// Validates: R-2.3.11
func TestIssuesRecheck_Boundary(t *testing.T) {
	cmd, dbPath, out := newSeededRemoteBlockedIssuesCmd(t)

	cmd.SetArgs([]string{"recheck", "Shared/Docs"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Queued permission recheck for Shared/Docs.")

	logger := slog.New(slog.DiscardHandler)
	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	keys, err := mgr.ListRequestedScopeRechecks(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []synctypes.ScopeKey{synctypes.SKPermRemote("Shared/Docs")}, keys)
}

// Validates: R-2.3.11
func TestIssuesRecheck_MissingBoundary(t *testing.T) {
	cmd, _, _ := newSeededRemoteBlockedIssuesCmd(t)

	cmd.SetArgs([]string{"recheck", "Shared/Missing"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared-folder write block not found")
}

// Validates: R-2.3.11
func TestIssuesRecheck_RootBoundarySlashAlias(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-remote-blocked-root.db")
	logger := slog.New(slog.DiscardHandler)

	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordFailure(t.Context(), &synctypes.SyncFailureParams{
		Path:       "blocked.txt",
		DriveID:    driveid.New("d!1"),
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "shared folder is read-only",
		ScopeKey:   synctypes.SKPermRemote(""),
	}, nil))
	require.NoError(t, mgr.Close(t.Context()))

	xdgDir := filepath.Join(tmpDir, "xdg-data")
	require.NoError(t, os.MkdirAll(filepath.Join(xdgDir, "onedrive-go"), 0o700))
	t.Setenv("XDG_DATA_HOME", xdgDir)

	cid := driveid.MustCanonicalID("personal:test@example.com")
	expectedPath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(expectedPath), 0o700))
	require.NoError(t, os.Symlink(dbPath, expectedPath))

	var out bytes.Buffer
	cc := &CLIContext{
		StatusWriter: &out,
		OutputWriter: &out,
		Logger:       logger,
		Cfg:          &config.ResolvedDrive{CanonicalID: cid},
	}

	cmd := newIssuesCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"recheck", "/"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Queued permission recheck for /.")
}

// Validates: R-2.10.47
func TestIssuesService_RunList_DoesNotClearPersistedAuthScope(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	seedAuthScope(t, cid)

	var out bytes.Buffer
	svc := newIssuesService(&CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			SyncDir:     t.TempDir(),
		},
	})

	require.NoError(t, svc.runList(t.Context(), false))
	assert.True(t, hasPersistedAuthScope(t.Context(), cid.Email(), testDriveLogger(t)))
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
		Path:      "docs/CON",
		DriveID:   driveid.New("drive-1"),
		Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueInvalidFilename,
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil))
	require.NoError(t, store.Close(t.Context()))

	require.NoError(t, os.Chmod(dbPath, 0o400))

	var buf bytes.Buffer
	svc := newIssuesService(&CLIContext{
		Logger:       logger,
		OutputWriter: &buf,
		Cfg:          &config.ResolvedDrive{CanonicalID: canonicalID},
	})

	require.NoError(t, svc.runList(t.Context(), false))
	assert.Contains(t, buf.String(), "INVALID FILENAME")
}
