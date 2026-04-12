package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestCanReuseBaselineHash_MetadataMatchOutsideRacilyCleanWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		nowOffset int64
		wantReuse bool
	}{
		{
			name:      "metadata_match_outside_racily_clean_window",
			path:      "stable.txt",
			nowOffset: nanosPerSecond + 1,
			wantReuse: true,
		},
		{
			name:      "racily_clean_forces_hash",
			path:      "racy.txt",
			nowOffset: nanosPerSecond - 1,
			wantReuse: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.path)
			content := []byte("stable content")
			baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

			require.NoError(t, os.WriteFile(path, content, 0o600))
			require.NoError(t, os.Chtimes(path, baseTime, baseTime))

			info, err := os.Stat(path)
			require.NoError(t, err)

			assert.Equal(t, tt.wantReuse, CanReuseBaselineHash(info, &BaselineEntry{
				Path:           tt.path,
				DriveID:        driveid.New("d"),
				ItemID:         "i1",
				ItemType:       ItemTypeFile,
				LocalHash:      "cached-hash",
				LocalSize:      info.Size(),
				LocalSizeKnown: true,
				LocalMtime:     info.ModTime().UnixNano(),
			}, info.ModTime().UnixNano()+tt.nowOffset))
		})
	}
}

// Validates: R-6.7.15
func TestCanReuseBaselineHash_SameSecondSubsecondDifferenceStillMatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "same-second.txt")
	content := []byte("stable content")
	fileTime := time.Date(2025, 1, 1, 0, 0, 0, int((200 * time.Millisecond).Nanoseconds()), time.UTC)

	require.NoError(t, os.WriteFile(path, content, 0o600))
	require.NoError(t, os.Chtimes(path, fileTime, fileTime))

	info, err := os.Stat(path)
	require.NoError(t, err)

	baselineTime := fileTime.Add(700 * time.Millisecond)
	assert.True(t, CanReuseBaselineHash(info, &BaselineEntry{
		Path:           "same-second.txt",
		DriveID:        driveid.New("d"),
		ItemID:         "i1",
		ItemType:       ItemTypeFile,
		LocalHash:      "cached-hash",
		LocalSize:      info.Size(),
		LocalSizeKnown: true,
		LocalMtime:     baselineTime.UnixNano(),
	}, info.ModTime().UnixNano()+nanosPerSecond+1))
}

func TestCanReuseBaselineHash_MetadataMismatchRequiresHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "changed.txt")
	content := []byte("stable content")
	oldTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, os.WriteFile(path, content, 0o600))
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))

	info, err := os.Stat(path)
	require.NoError(t, err)

	assert.False(t, CanReuseBaselineHash(info, &BaselineEntry{
		Path:           "changed.txt",
		DriveID:        driveid.New("d"),
		ItemID:         "i1",
		ItemType:       ItemTypeFile,
		LocalHash:      "cached-hash",
		LocalSize:      info.Size() + 1,
		LocalSizeKnown: true,
		LocalMtime:     info.ModTime().UnixNano(),
	}, info.ModTime().UnixNano()+nanosPerSecond+1))
}
