package syncscope

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.4.5
func TestNormalizeConfig_CollapsesAncestorPaths(t *testing.T) {
	t.Parallel()

	cfg, err := NormalizeConfig(Config{
		SyncPaths: []string{"/Docs", "/Docs/report.txt", "/Photos", "/"},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{""}, cfg.SyncPaths)
}

// Validates: R-2.4.5
func TestSnapshot_AllowsPath_ForDirectoryAndExactFileScopes(t *testing.T) {
	t.Parallel()

	snapshot, err := NewSnapshot(Config{
		SyncPaths: []string{"/Docs/report.txt", "/Photos"},
	}, nil)
	require.NoError(t, err)

	assert.True(t, snapshot.ShouldTraverseDir(""))
	assert.True(t, snapshot.ShouldTraverseDir("Docs"))
	assert.True(t, snapshot.AllowsPath("Docs"))
	assert.True(t, snapshot.AllowsPath("Docs/report.txt"))
	assert.False(t, snapshot.AllowsPath("Docs/other.txt"))
	assert.True(t, snapshot.AllowsPath("Photos/raw/cat.jpg"))
	assert.False(t, snapshot.ShouldTraverseDir("Music"))
	assert.False(t, snapshot.AllowsPath("Music/song.mp3"))
}

// Validates: R-2.4.4
func TestSnapshot_IgnoreMarkerExcludesMarkerTreeButKeepsMarkerDirObservable(t *testing.T) {
	t.Parallel()

	snapshot, err := NewSnapshot(Config{
		IgnoreMarker: ".odignore",
	}, []string{"Docs/Private"})
	require.NoError(t, err)

	assert.True(t, snapshot.HasMarkerDir("Docs/Private"))
	assert.True(t, snapshot.ShouldTraverseDir("Docs/Private"), "marker dir stays observable for marker deletion")
	assert.False(t, snapshot.AllowsPath("Docs/Private"), "marker dir itself is excluded from sync")
	assert.False(t, snapshot.AllowsPath("Docs/Private/file.txt"))
	assert.False(t, snapshot.ShouldTraverseDir("Docs/Private/Sub"))
	assert.False(t, snapshot.AllowsPath("Docs/Private/.odignore"), "marker file itself is never synced")
	assert.True(t, snapshot.AllowsPath("Docs/Public/file.txt"))
}

// Validates: R-2.4.4, R-2.4.5
func TestDiffSnapshots_TracksEnteredAndExitedRoots(t *testing.T) {
	t.Parallel()

	oldSnapshot, err := NewSnapshot(Config{
		SyncPaths:    []string{"/Docs/report.txt"},
		IgnoreMarker: ".odignore",
	}, []string{"Docs/Private"})
	require.NoError(t, err)

	newSnapshot, err := NewSnapshot(Config{
		SyncPaths:    []string{"/Docs"},
		IgnoreMarker: ".odignore",
	}, nil)
	require.NoError(t, err)

	diff := DiffSnapshots(oldSnapshot, newSnapshot)
	assert.Equal(t, []string{"Docs"}, diff.EnteredPaths)
	assert.Empty(t, diff.ExitedPaths)
}

// Validates: R-2.4.5
func TestDiffSnapshots_TreatsTransitionToFullScopeAsEnteredRoot(t *testing.T) {
	t.Parallel()

	oldSnapshot, err := NewSnapshot(Config{
		SyncPaths: []string{"/Docs/report.txt"},
	}, nil)
	require.NoError(t, err)

	newSnapshot, err := NewSnapshot(Config{}, nil)
	require.NoError(t, err)

	diff := DiffSnapshots(oldSnapshot, newSnapshot)
	assert.Equal(t, []string{""}, diff.EnteredPaths)
	assert.Empty(t, diff.ExitedPaths)
}

// Validates: R-2.4.4, R-2.4.5
func TestMarshalSnapshot_RoundTrips(t *testing.T) {
	t.Parallel()

	snapshot, err := NewSnapshot(Config{
		SyncPaths:    []string{"/Docs", "/Photos"},
		IgnoreMarker: ".odignore",
	}, []string{"Docs/Private"})
	require.NoError(t, err)

	raw, err := MarshalSnapshot(snapshot)
	require.NoError(t, err)

	decoded, err := UnmarshalSnapshot(raw)
	require.NoError(t, err)

	assert.Equal(t, snapshot.SyncPaths(), decoded.SyncPaths())
	assert.Equal(t, snapshot.IgnoreMarker(), decoded.IgnoreMarker())
	assert.Equal(t, snapshot.MarkerDirs(), decoded.MarkerDirs())
}
