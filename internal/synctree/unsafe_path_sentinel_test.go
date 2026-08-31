package synctree

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.4.8
//
// Containment failures are the definition of an unsafe path, and callers
// classify on ErrUnsafePath to tell a permanent path problem from a transient
// one. Without the sentinel, a path that escapes the root is reported as
// merely unavailable and retried forever instead of surfacing as blocked.
func TestRootOperationsReportContainmentFailuresAsUnsafePath(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	escapes := []struct {
		name string
		path string
	}{
		{name: "ParentTraversal", path: ".." + string(filepath.Separator) + "outside.txt"},
		{name: "BareParent", path: ".."},
		{name: "Absolute", path: string(filepath.Separator) + "etc" + string(filepath.Separator) + "passwd"},
		{name: "NestedTraversal", path: filepath.Join("a", "..", "..", "outside.txt")},
	}

	for _, tt := range escapes {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, statErr := root.Stat(tt.path)
			require.Error(t, statErr)
			require.ErrorIs(t, statErr, ErrUnsafePath,
				"a containment failure must be identifiable by sentinel, not by message text")

			removeErr := root.Remove(tt.path)
			require.Error(t, removeErr)
			require.ErrorIs(t, removeErr, ErrUnsafePath)
		})
	}
}

// Validates: R-6.4.8
//
// Ordinary paths inside the tree must not be flagged.
func TestRootOperationsDoNotFlagContainedPaths(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	_, statErr := root.Stat(filepath.Join("nested", "file.txt"))
	require.Error(t, statErr, "the file does not exist")
	assert.NotErrorIs(t, statErr, ErrUnsafePath,
		"a missing file inside the tree is not a containment failure")
}
