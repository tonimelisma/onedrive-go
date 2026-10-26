package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type fakePutTargetResolver map[string]*graph.Item

func (f fakePutTargetResolver) ResolveItem(_ context.Context, remotePath string) (*graph.Item, error) {
	if item, ok := f[remotePath]; ok {
		return item, nil
	}

	return nil, graph.ErrNotFound
}

// Validates: R-1.3.7
func TestResolvePutFileTarget_UploadsIntoFolders(t *testing.T) {
	t.Parallel()

	resolver := fakePutTargetResolver{
		"/docs":       {IsFolder: true},
		"/docs/a.txt": {},
	}
	for _, tt := range []struct{ target, want string }{
		{"/docs/", "/docs/one.txt"},
		{"/docs", "/docs/one.txt"},
		{"/", "/one.txt"},
		{"/docs/a.txt", "/docs/a.txt"},
		{"/docs/new.txt", "/docs/new.txt"},
	} {
		got, err := resolvePutFileTarget(t.Context(), resolver, tt.target, "one.txt")
		require.NoError(t, err, tt.target)
		assert.Equal(t, tt.want, got, tt.target)
	}
}

// Validates: R-1.2.6
func TestLocalFileTarget_DownloadsIntoDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sep := string(filepath.Separator)
	assert.Equal(t, filepath.Join(dir, "one.txt"), localFileTarget(dir, "one.txt"))
	assert.Equal(t, filepath.Join(dir, "one.txt"), localFileTarget(dir+sep, "one.txt"))
	assert.Equal(t, "one.txt", localFileTarget("."+sep, "one.txt"))
	assert.Equal(t, filepath.Join(dir, "renamed.txt"), localFileTarget(filepath.Join(dir, "renamed.txt"), "one.txt"))

	file := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	assert.Equal(t, file, localFileTarget(file, "one.txt"), "an existing file is overwritten, not entered")
}

// Validates: R-1.7.3
func TestMoveTargetChecksAndDisplayPaths(t *testing.T) {
	t.Parallel()

	assert.True(t, isWithinRemotePath("/a/b/c", "/a"))
	assert.True(t, isWithinRemotePath("/A/b", "/a"), "OneDrive paths compare case-insensitively")
	assert.True(t, isWithinRemotePath("/a", "/a/"))
	assert.False(t, isWithinRemotePath("/ab", "/a"))
	assert.False(t, isWithinRemotePath("/b/a", "/a"))

	assert.Equal(t, "/a/b/copy.txt", joinDisplayPath("/a/b/", "copy.txt"))
	assert.Equal(t, "/a/b/copy.txt", joinDisplayPath("a/b", "copy.txt"))
	assert.Equal(t, "/copy.txt", joinDisplayPath("/", "copy.txt"))
	assert.Equal(t, "/a/c", moveTargetPath("/a", destInfo{destIsDir: true, newName: "c"}))
	assert.Equal(t, "/a/x", moveTargetPath("a/x", destInfo{}))
}

// Validates: R-6.6.11
func TestRemoteFailureText_NotImplementedPassesOnOneDriveMessage(t *testing.T) {
	t.Parallel()

	err := newRemoteOpError("uploading", "/docs", &graph.GraphError{
		StatusCode: 501,
		Message:    "Cannot upload content to an item representing a folder.",
		Err:        graph.ErrServerError,
	})
	assert.Equal(t, "OneDrive cannot do this for /docs: Cannot upload content to an item representing a folder.", remoteFailureText(err))

	outage := newRemoteOpError("uploading", "/docs", &graph.GraphError{StatusCode: 503, Err: graph.ErrServerError})
	assert.Contains(t, remoteFailureText(outage), "temporary server problem")
	assert.False(t, graph.IsNotSupported(errors.New("plain")))
}
