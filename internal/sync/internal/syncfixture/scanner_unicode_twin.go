package syncfixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	TwinSyncedName    = NfcCafe + ".txt"
	TwinSecondName    = NfdCafe + ".txt"
	TwinSyncedContent = "synced content\n"
)

// SkipUnlessBothUnicodeFormsCoexist creates both spellings of one name in a
// fresh directory and skips when the filesystem folds them into one entry.
func SkipUnlessBothUnicodeFormsCoexist(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, TwinSyncedName), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, TwinSecondName), []byte("b"), 0o600))
	if len(LocalNames(t, dir)) != 2 {
		t.Skip("the filesystem folds Unicode forms into one name; the two spellings cannot both exist")
	}
}
