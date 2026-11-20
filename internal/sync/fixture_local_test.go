package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed. Returns the absolute path.
func writeTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o700), "MkdirAll(%s)", filepath.Dir(fullPath))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600), "WriteFile(%s)", fullPath)

	return fullPath
}

// findEvent returns the first ChangeEvent with the given path, or nil.
func findEvent(events []ChangeEvent, path string) *ChangeEvent {
	for i := range events {
		if events[i].Path == path {
			return &events[i]
		}
	}

	return nil
}
