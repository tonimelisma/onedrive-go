package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testOwnedPartialName(base string) string {
	return ".onedrive-go." + base + ".partial"
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_SkipsConfiguredChildRoots(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, testOwnedPartialName("root.txt")), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, testOwnedPartialName("child.txt")), []byte("child"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:      mustOpenSyncTree(t, syncRoot),
			logger:        testLogger(t),
			contentFilter: ContentFilterConfig{IgnoredDirs: []string{"Shortcuts/Docs"}},
		},
	}
	flow.postSyncHousekeeping(t.Context())

	assert.NoFileExists(t, filepath.Join(syncRoot, testOwnedPartialName("root.txt")))
	assert.FileExists(t, filepath.Join(childRoot, testOwnedPartialName("child.txt")))
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_SkipsProtectedShortcutRoots(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shared Project")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, testOwnedPartialName("root.txt")), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, testOwnedPartialName("shortcut-sentinel.txt")), []byte("child"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:      mustOpenSyncTree(t, syncRoot),
			logger:        testLogger(t),
			contentFilter: ContentFilterConfig{},
			protectedRoots: []ProtectedRoot{{
				Path:      "Shared Project",
				BindingID: "shortcut-binding",
			}},
		},
	}
	flow.postSyncHousekeeping(t.Context())

	assert.NoFileExists(t, filepath.Join(syncRoot, testOwnedPartialName("root.txt")))
	assert.FileExists(t, filepath.Join(childRoot, testOwnedPartialName("shortcut-sentinel.txt")))
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_RespectsIncludedDirs(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	includedRoot := filepath.Join(syncRoot, "Projects")
	excludedRoot := filepath.Join(syncRoot, "Archive")
	require.NoError(t, os.MkdirAll(includedRoot, 0o700))
	require.NoError(t, os.MkdirAll(excludedRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(includedRoot, testOwnedPartialName("owned.txt")), []byte("included"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(excludedRoot, "user.partial"), []byte("excluded"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "root.partial"), []byte("root"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:      mustOpenSyncTree(t, syncRoot),
			logger:        testLogger(t),
			contentFilter: ContentFilterConfig{IncludedDirs: []string{"Projects"}},
		},
	}
	flow.postSyncHousekeeping(t.Context())

	assert.NoFileExists(t, filepath.Join(includedRoot, testOwnedPartialName("owned.txt")))
	assert.FileExists(t, filepath.Join(excludedRoot, "user.partial"))
	assert.FileExists(t, filepath.Join(syncRoot, "root.partial"))
}
