package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.4.10
func TestPostSyncHousekeeping_SkipsConfiguredChildRoots(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "root.partial"), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, "child.partial"), []byte("child"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:    mustOpenSyncTree(t, syncRoot),
			logger:      testLogger(t),
			localFilter: LocalFilterConfig{SkipDirs: []string{"Shortcuts/Docs"}},
		},
	}
	flow.postSyncHousekeeping()

	assert.NoFileExists(t, filepath.Join(syncRoot, "root.partial"))
	assert.FileExists(t, filepath.Join(childRoot, "child.partial"))
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_SkipsManagedShortcutRoots(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shared Project")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "root.partial"), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, "shortcut-sentinel.txt.partial"), []byte("child"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree: mustOpenSyncTree(t, syncRoot),
			logger:   testLogger(t),
			localFilter: LocalFilterConfig{
				ManagedRoots: []ManagedRootReservation{{
					Path:      "Shared Project",
					BindingID: "shortcut-binding",
				}},
			},
		},
	}
	flow.postSyncHousekeeping()

	assert.NoFileExists(t, filepath.Join(syncRoot, "root.partial"))
	assert.FileExists(t, filepath.Join(childRoot, "shortcut-sentinel.txt.partial"))
}
