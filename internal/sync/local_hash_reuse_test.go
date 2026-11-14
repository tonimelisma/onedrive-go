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
		{name: "metadata_match_outside_racily_clean_window", nowOffset: int64(racilyCleanWindow) + 1, wantReuse: true},
		{name: "racily_clean_forces_hash", nowOffset: int64(racilyCleanWindow) - 1, wantReuse: false},
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

	assert.False(t, canReuseBaselineHash(info, entry, latest+int64(racilyCleanWindow)+1))
}

// Validates: R-2.2.4
func TestCanReuseBaselineHash_CtimeOrIdentityMismatchRequiresHash(t *testing.T) {
	t.Parallel()

	const born = int64(1_700_000_000_000_000_001)
	tests := []struct {
		name   string
		mutate func(*BaselineEntry)
		// observedBirthTime, when set, is the birth time the file reports.
		observedBirthTime int64
	}{
		{name: "ctime_changed", mutate: func(e *BaselineEntry) { e.LocalCtime-- }},
		{name: "ctime_unknown", mutate: func(e *BaselineEntry) { e.LocalCtime = 0 }},
		{name: "inode_changed", mutate: func(e *BaselineEntry) { e.LocalInode++ }},
		{name: "device_changed", mutate: func(e *BaselineEntry) { e.LocalDevice++ }},
		{name: "identity_unknown", mutate: func(e *BaselineEntry) { e.LocalHasIdentity = false }},
		// A reused inode number: another file holds it now (R-2.17.1).
		{name: "birth_time_changed", mutate: func(e *BaselineEntry) { e.LocalBirthTime = born }, observedBirthTime: born + 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, entry, latest := hashReuseFixture(t, "doc.txt")
			tt.mutate(entry)
			if tt.observedBirthTime != 0 {
				info = reportedBirthTimeInfo{FileInfo: info, birthTime: tt.observedBirthTime}
			}
			assert.False(t, canReuseBaselineHash(info, entry, latest+int64(racilyCleanWindow)+1))
		})
	}
}

// reportedBirthTimeInfo is a FileInfo stating a birth time
// (synctree.BirthTimeReporter), as a filesystem that records one does.
type reportedBirthTimeInfo struct {
	os.FileInfo
	birthTime int64
}

func (i reportedBirthTimeInfo) BirthTime() (int64, bool) {
	return i.birthTime, i.birthTime != 0
}

// Validates: R-2.2.4
func TestCanReuseBaselineHash_MetadataMismatchRequiresHash(t *testing.T) {
	t.Parallel()

	info, entry, latest := hashReuseFixture(t, "changed.txt")
	entry.LocalSize = info.Size() + 1

	assert.False(t, canReuseBaselineHash(info, entry, latest+int64(racilyCleanWindow)+1))
}

// statedModTimeInfo is a FileInfo stating a modification time the test
// chose, whatever the filesystem under the test can hold.
type statedModTimeInfo struct {
	os.FileInfo
	modTime time.Time
}

func (i statedModTimeInfo) ModTime() time.Time {
	return i.modTime
}

// hashReuseFixtureDated is hashReuseFixture for a file whose modification
// time is modTime, recorded in the baseline the way the scanner records it.
func hashReuseFixtureDated(t *testing.T, name string, modTime time.Time) (os.FileInfo, *BaselineEntry, int64) {
	t.Helper()

	info, entry, _ := hashReuseFixture(t, name)
	ctime, ok := synctree.ChangeTimeFromFileInfo(info)
	require.True(t, ok)
	entry.LocalMtime = modTime.UnixNano()

	return statedModTimeInfo{FileInfo: info, modTime: modTime}, entry, ctime
}

// Validates: R-2.2.2, R-2.2.4
func TestCanReuseBaselineHash_AFileDated1601IsNotRacyAndItsHashIsReused(t *testing.T) {
	t.Parallel()

	info, entry, ctime := hashReuseFixtureDated(t, "old.txt", time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC))

	assert.True(t, canReuseBaselineHash(info, entry, ctime+int64(racilyCleanWindow)+1),
		"centuries before the scan is outside the racily-clean window")
}

// Validates: R-2.2.4
func TestCanReuseBaselineHash_AFileDated2300IsRehashedLikeAnyFutureDatedFile(t *testing.T) {
	t.Parallel()

	info, entry, ctime := hashReuseFixtureDated(t, "far.txt", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))

	assert.False(t, canReuseBaselineHash(info, entry, ctime+int64(racilyCleanWindow)+1),
		"a file modified after the scan began is never trusted")
}

// Validates: R-2.2.4
func TestModifiedInRacilyCleanWindow_IsDecidedOnTimes(t *testing.T) {
	t.Parallel()

	scanStart := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	settled := scanStart.Add(-time.Hour).UnixNano()
	tests := []struct {
		name    string
		modTime time.Time
		ctime   int64
		want    bool
	}{
		{name: "modified within the last second", modTime: scanStart.Add(-time.Second + time.Nanosecond), ctime: settled, want: true},
		{name: "modified a second before", modTime: scanStart.Add(-time.Second), ctime: settled, want: false},
		{name: "dated 1601", modTime: time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), ctime: settled, want: false},
		{name: "dated 2300, after the scan began", modTime: time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), ctime: settled, want: true},
		{name: "inode changed within the last second", modTime: scanStart.Add(-time.Hour), ctime: scanStart.Add(-time.Millisecond).UnixNano(), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, modifiedInRacilyCleanWindow(tc.modTime, tc.ctime, scanStart.UnixNano()))
		})
	}
}
