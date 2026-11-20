package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	twinSyncedName    = nfcCafe + ".txt"
	twinSecondName    = nfdCafe + ".txt"
	twinSyncedContent = "synced content\n"
)

// skipUnlessBothUnicodeFormsCoexist creates both spellings of one name in a
// fresh directory and skips when the filesystem folds them into one entry.
func skipUnlessBothUnicodeFormsCoexist(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, twinSyncedName), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, twinSecondName), []byte("b"), 0o600))
	if len(localNames(t, dir)) != 2 {
		t.Skip("the filesystem folds Unicode forms into one name; the two spellings cannot both exist")
	}
}
