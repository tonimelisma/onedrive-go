package syncobserve

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func TestCanReuseBaselineHash_MetadataMatchOutsideRacilyCleanWindow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "stable.txt")
	content := []byte("stable content")
	oldTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, os.WriteFile(path, content, 0o644))
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))

	info, err := os.Stat(path)
	require.NoError(t, err)

	assert.True(t, CanReuseBaselineHash(info, &synctypes.BaselineEntry{
		Path:      "stable.txt",
		DriveID:   driveid.New("d"),
		ItemID:    "i1",
		ItemType:  synctypes.ItemTypeFile,
		LocalHash: "cached-hash",
		Size:      info.Size(),
		Mtime:     info.ModTime().UnixNano(),
	}, info.ModTime().UnixNano()+nanosPerSecond+1))
}

func TestCanReuseBaselineHash_RacilyCleanForcesHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "racy.txt")
	content := []byte("stable content")
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, os.WriteFile(path, content, 0o644))
	require.NoError(t, os.Chtimes(path, baseTime, baseTime))

	info, err := os.Stat(path)
	require.NoError(t, err)

	assert.False(t, CanReuseBaselineHash(info, &synctypes.BaselineEntry{
		Path:      "racy.txt",
		DriveID:   driveid.New("d"),
		ItemID:    "i1",
		ItemType:  synctypes.ItemTypeFile,
		LocalHash: "cached-hash",
		Size:      info.Size(),
		Mtime:     info.ModTime().UnixNano(),
	}, info.ModTime().UnixNano()+nanosPerSecond-1))
}

func TestCanReuseBaselineHash_MetadataMismatchRequiresHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "changed.txt")
	content := []byte("stable content")
	oldTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, os.WriteFile(path, content, 0o644))
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))

	info, err := os.Stat(path)
	require.NoError(t, err)

	assert.False(t, CanReuseBaselineHash(info, &synctypes.BaselineEntry{
		Path:      "changed.txt",
		DriveID:   driveid.New("d"),
		ItemID:    "i1",
		ItemType:  synctypes.ItemTypeFile,
		LocalHash: "cached-hash",
		Size:      info.Size() + 1,
		Mtime:     info.ModTime().UnixNano(),
	}, info.ModTime().UnixNano()+nanosPerSecond+1))
}
