package syncfixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// WriteTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed. Returns the absolute path.
func WriteTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o700), "MkdirAll(%s)", filepath.Dir(fullPath))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600), "WriteFile(%s)", fullPath)

	return fullPath
}

// FindEvent returns the first LocalEntry with the given path, or nil.
func FindEvent(events []syncdomain.LocalEntry, path string) *syncdomain.LocalEntry {
	for i := range events {
		if events[i].Path == path {
			return &events[i]
		}
	}

	return nil
}
