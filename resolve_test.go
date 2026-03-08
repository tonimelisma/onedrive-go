package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

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
		errContains string // substring expected in error message
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

// newTestCLIContext creates a minimal CLIContext for testing resolve helpers.
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

// --- newResolveCmd (hidden alias) ---

func TestNewResolveCmd_HiddenAlias(t *testing.T) {
	t.Parallel()

	cmd := newResolveCmd()
	assert.Equal(t, "resolve [path-or-id]", cmd.Use)
	assert.True(t, cmd.Hidden, "resolve should be a hidden alias")

	for _, flag := range []string{"keep-local", "keep-remote", "keep-both", "all", "dry-run"} {
		assert.NotNil(t, cmd.Flags().Lookup(flag), "missing flag %q", flag)
	}
}

// --- conflicts resolve subcommand ---

func TestConflictsResolveCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newConflictsCmd()
	resolveCmd, _, err := cmd.Find([]string{"resolve"})
	require.NoError(t, err)
	assert.Equal(t, "resolve [path-or-id]", resolveCmd.Use)
	assert.False(t, resolveCmd.Hidden)

	for _, flag := range []string{"keep-local", "keep-remote", "keep-both", "all", "dry-run"} {
		assert.NotNil(t, resolveCmd.Flags().Lookup(flag), "missing flag %q", flag)
	}
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

			cmd := newConflictsResolveCmd()
			require.NoError(t, cmd.Flags().Set(tt.flag, "true"))

			got, err := resolveStrategy(cmd)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveStrategy_NoFlag(t *testing.T) {
	t.Parallel()

	cmd := newConflictsResolveCmd()

	_, err := resolveStrategy(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strategy")
}
