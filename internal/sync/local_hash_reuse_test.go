package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// hashReuseFixture writes a file whose mtime is in the past and returns its
// info, a baseline entry recording exactly that file, and the latest of its
// mtime and ctime (ctime is always "now" and cannot be set back).
func hashReuseFixture(t *testing.T, name string) (os.FileInfo, *BaselineEntry, int64) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, int((200 * time.Millisecond).Nanoseconds()), time.UTC)
	require.NoError(t, os.WriteFile(path, []byte("stable content"), 0o600))
	require.NoError(t, os.Chtimes(path, baseTime, baseTime))

	info, err := os.Lstat(path)
	require.NoError(t, err)
	entry := baselineEntryFromFile(t, dir, name, "cached-hash")
	ctime, ok := synctree.ChangeTimeFromFileInfo(info)
	require.True(t, ok)

	return info, entry, max(info.ModTime().UnixNano(), ctime)
}

// Validates: R-2.2.2, R-2.2.4
func TestCanReuseBaselineHash_MetadataMatchOutsideRacilyCleanWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		nowOffset int64
		wantReuse bool
	}{
		{name: "metadata_match_outside_racily_clean_window", nowOffset: nanosPerSecond + 1, wantReuse: true},
		{name: "racily_clean_forces_hash", nowOffset: nanosPerSecond - 1, wantReuse: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, entry, latest := hashReuseFixture(t, "stable.txt")
			assert.Equal(t, tt.wantReuse, canReuseBaselineHash(info, entry, latest+tt.nowOffset))
		})
	}
}

// Validates: R-2.2.4, R-6.7.15
func TestCanReuseBaselineHash_SubsecondMtimeDifferenceRequiresHash(t *testing.T) {
	t.Parallel()

	info, entry, latest := hashReuseFixture(t, "same-second.txt")
	// Same whole second, different nanoseconds. Local mtimes are compared at
	// full precision; only comparisons with OneDrive values truncate.
	entry.LocalMtime = info.ModTime().Add(700 * time.Millisecond).UnixNano()

	assert.False(t, canReuseBaselineHash(info, entry, latest+nanosPerSecond+1))
}

// Validates: R-2.2.4
func TestCanReuseBaselineHash_CtimeOrIdentityMismatchRequiresHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*BaselineEntry)
	}{
		{name: "ctime_changed", mutate: func(e *BaselineEntry) { e.LocalCtime-- }},
		{name: "ctime_unknown", mutate: func(e *BaselineEntry) { e.LocalCtime = 0 }},
		{name: "inode_changed", mutate: func(e *BaselineEntry) { e.LocalInode++ }},
		{name: "device_changed", mutate: func(e *BaselineEntry) { e.LocalDevice++ }},
		{name: "identity_unknown", mutate: func(e *BaselineEntry) { e.LocalHasIdentity = false }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, entry, latest := hashReuseFixture(t, "doc.txt")
			tt.mutate(entry)
			assert.False(t, canReuseBaselineHash(info, entry, latest+nanosPerSecond+1))
		})
	}
}

func TestCanReuseBaselineHash_MetadataMismatchRequiresHash(t *testing.T) {
	t.Parallel()

	info, entry, latest := hashReuseFixture(t, "changed.txt")
	entry.LocalSize = info.Size() + 1

	assert.False(t, canReuseBaselineHash(info, entry, latest+nanosPerSecond+1))
}
