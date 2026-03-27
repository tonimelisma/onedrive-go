package driveops

import (
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeTestResponsef(t *testing.T, w io.Writer, format string, args ...any) {
	t.Helper()

	_, err := fmt.Fprintf(w, format, args...)
	require.NoError(t, err)
}

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}
