package syncobserve

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}
