package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync"
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

	c := &sync.ConflictRecord{
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

	// Has resolve, clear, and retry subcommands.
	resolveCmd, _, err := cmd.Find([]string{"resolve"})
	require.NoError(t, err)
	assert.Equal(t, "resolve [path-or-id]", resolveCmd.Use)

	clearCmd, _, err := cmd.Find([]string{"clear"})
	require.NoError(t, err)
	assert.Equal(t, "clear [path]", clearCmd.Use)

	retryCmd, _, err := cmd.Find([]string{"retry"})
	require.NoError(t, err)
	assert.Equal(t, "retry [path]", retryCmd.Use)
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

// --- printConflictsJSON ---

func TestPrintConflictsJSON_EmptyList(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printConflictsJSON(&buf, nil)
	require.NoError(t, err)

	var result []conflictJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result)
}

func TestPrintConflictsJSON_WithConflicts(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "conflict-001",
			Path:         "/docs/readme.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
			LocalHash:    "local-hash",
			RemoteHash:   "remote-hash",
		},
		{
			ID:           "conflict-002",
			Path:         "/photos/cat.jpg",
			ConflictType: "delete_edit",
			DetectedAt:   1700000001000000000,
			Resolution:   "keep_local",
			ResolvedBy:   "user",
			ResolvedAt:   1700000002000000000,
		},
	}

	var buf bytes.Buffer
	err := printConflictsJSON(&buf, conflicts)
	require.NoError(t, err)

	var result []conflictJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result, 2)
	assert.Equal(t, "conflict-001", result[0].ID)
	assert.Equal(t, "edit_edit", result[0].ConflictType)
	assert.Equal(t, "keep_local", result[1].Resolution)
}

// --- printConflictsTable ---

func TestPrintConflictsTable(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "abcdefghijklmnop",
			Path:         "/test.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printConflictsTable(&buf, conflicts, false)

	output := buf.String()
	assert.Contains(t, output, "abcdefgh") // truncated ID
	assert.Contains(t, output, "/test.txt")
	assert.Contains(t, output, "edit_edit")
}

// --- failure output ---

func TestToFailureJSON(t *testing.T) {
	t.Parallel()

	row := &sync.SyncFailureRow{
		Path:         "docs/CON",
		DriveID:      driveid.New("test-drive-id"),
		Direction:    "upload",
		Category:     "actionable",
		IssueType:    "invalid_filename",
		ItemID:       "item-123",
		FailureCount: 1,
		LastError:    "file name is not valid for OneDrive: CON",
		HTTPStatus:   0,
		FileSize:     1024,
		FirstSeenAt:  1700000000000000000,
		LastSeenAt:   1700000001000000000,
	}

	j := toFailureJSON(row)
	assert.Equal(t, "docs/CON", j.Path)
	assert.Equal(t, "upload", j.Direction)
	assert.Equal(t, "actionable", j.Category)
	assert.Equal(t, "invalid_filename", j.IssueType)
	assert.Equal(t, 1, j.FailureCount)
	assert.Equal(t, "file name is not valid for OneDrive: CON", j.LastError)
	assert.Equal(t, int64(1024), j.FileSize)
	assert.NotEmpty(t, j.FirstSeenAt)
	assert.NotEmpty(t, j.LastSeenAt)
}

func TestPrintFailuresJSON_EmptyList(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printFailuresJSON(&buf, nil)
	require.NoError(t, err)

	var result []failureJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result)
}

func TestPrintFailuresJSON_WithFailures(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{
			Path:         "docs/CON",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "actionable",
			IssueType:    "invalid_filename",
			FailureCount: 1,
			LastError:    "reserved name",
			FirstSeenAt:  1700000000000000000,
			LastSeenAt:   1700000000000000000,
		},
		{
			Path:         "data/huge.bin",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "actionable",
			IssueType:    "file_too_large",
			FailureCount: 1,
			LastError:    "exceeds 250 GB",
			FileSize:     300 * 1024 * 1024 * 1024,
			FirstSeenAt:  1700000001000000000,
			LastSeenAt:   1700000001000000000,
		},
	}

	var buf bytes.Buffer
	err := printFailuresJSON(&buf, failures)
	require.NoError(t, err)

	var result []failureJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result, 2)
	assert.Equal(t, "docs/CON", result[0].Path)
	assert.Equal(t, "invalid_filename", result[0].IssueType)
	assert.Equal(t, "file_too_large", result[1].IssueType)
	assert.Equal(t, int64(300*1024*1024*1024), result[1].FileSize)
}

func TestPrintFailuresTable(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{
			Path:         "docs/CON",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "actionable",
			IssueType:    "invalid_filename",
			FailureCount: 1,
			LastError:    "reserved name",
			LastSeenAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printFailuresTable(&buf, failures)

	output := buf.String()
	assert.Contains(t, output, "PATH")
	assert.Contains(t, output, "DIRECTION")
	assert.Contains(t, output, "docs/CON")
	assert.Contains(t, output, "upload")
}

func TestPrintFailuresTable_TruncatesLongErrors(t *testing.T) {
	t.Parallel()

	longErr := "this is a very long error message that should be truncated to sixty characters total for table display purposes"
	failures := []sync.SyncFailureRow{
		{
			Path:         "file.txt",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "transient",
			IssueType:    "upload_failed",
			FailureCount: 3,
			LastError:    longErr,
			LastSeenAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printFailuresTable(&buf, failures)

	output := buf.String()
	assert.Contains(t, output, longErr[:maxFailureErrorLen-3]+"...")
	assert.NotContains(t, output, longErr) // full message should not appear
}

// --- findConflict ---

func TestFindConflict(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
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

			got, err := findConflict(conflicts, tt.idOrPath)
			if tt.wantErr {
				require.Error(t, err)

				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}

				return
			}

			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, tt.wantID, got.ID)
		})
	}
}

// --- resolve helpers ---

func newTestCLIContext(w io.Writer) *CLIContext {
	return &CLIContext{
		StatusWriter: w,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestResolveEachConflict_ResolvesAll(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
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

	conflicts := []sync.ConflictRecord{
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

	conflicts := []sync.ConflictRecord{
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

	conflicts := []sync.ConflictRecord{
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
		func() ([]sync.ConflictRecord, error) { return conflicts, nil },
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
		func() ([]sync.ConflictRecord, error) { return nil, nil },
		func(_, _ string) error { return nil },
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflict not found")
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

func TestPrintIssuesJSON_MixedOutput(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "conflict-001",
			Path:         "/docs/readme.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
		},
	}

	failures := []sync.SyncFailureRow{
		{
			Path:        "docs/CON",
			Direction:   "upload",
			Category:    "actionable",
			IssueType:   "invalid_filename",
			LastError:   "reserved name",
			FirstSeenAt: 1700000000000000000,
			LastSeenAt:  1700000000000000000,
		},
	}

	var buf bytes.Buffer
	err := printIssuesJSON(&buf, conflicts, failures)
	require.NoError(t, err)

	var result []issueJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result, 2)
	assert.Equal(t, "conflict", result[0].Kind)
	assert.Equal(t, "conflict-001", result[0].ID)
	assert.Equal(t, "failure", result[1].Kind)
	assert.Equal(t, "docs/CON", result[1].Path)
}

// --- unified issues text ---

func TestPrintIssuesText_BothSections(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{ID: "abcdefghijklmnop", Path: "/test.txt", ConflictType: "edit_edit", DetectedAt: 1700000000000000000},
	}

	failures := []sync.SyncFailureRow{
		{Path: "docs/CON", Direction: "upload", LastError: "reserved name", LastSeenAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	printIssuesText(&buf, conflicts, failures, false)

	output := buf.String()
	assert.Contains(t, output, "CONFLICTS")
	assert.Contains(t, output, "/test.txt")
	assert.Contains(t, output, "FILE ISSUES")
	assert.Contains(t, output, "docs/CON")
}

func TestPrintIssuesText_OnlyConflicts(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{ID: "abcdefghijklmnop", Path: "/test.txt", ConflictType: "edit_edit", DetectedAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	printIssuesText(&buf, conflicts, nil, false)

	output := buf.String()
	assert.Contains(t, output, "CONFLICTS")
	assert.NotContains(t, output, "FILE ISSUES")
}

func TestPrintIssuesText_OnlyFailures(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{Path: "docs/CON", Direction: "upload", LastError: "reserved name", LastSeenAt: 1700000000000000000},
	}

	var buf bytes.Buffer
	printIssuesText(&buf, nil, failures, false)

	output := buf.String()
	assert.NotContains(t, output, "CONFLICTS")
	assert.Contains(t, output, "FILE ISSUES")
}
