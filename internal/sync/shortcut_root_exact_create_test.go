package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// caseInsensitiveShortcutEngine is a parent engine on the emulated
// case-insensitive volume holding one shortcut alias record at Shared/X2 that
// has no local folder yet.
func caseInsensitiveShortcutEngine(t *testing.T) (*testEngine, string) {
	t.Helper()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		withTestShortcutNamespace(inputs)
		inputs.hooks.interposeSyncTree = synctest.CaseInsensitive
	})
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/X2",
		LocalAlias:        "X2",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/X2"},
	}}))

	return eng, syncRoot
}

func onlyShortcutRoot(t *testing.T, eng *testEngine) syncshortcut.ShortcutRootRecord {
	t.Helper()

	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)

	return roots[0]
}

// A user's folder x2 stands where a shortcut alias X2 is to be made, on a
// volume that folds case. The alias used to take x2 for its own folder (the
// lstat of X2 found it, and MkdirAllNoFollow called it done), so the shared
// folder's content would have been synced into the user's folder. The alias
// adopts nothing it did not make: its path is blocked, and x2 stays as it is.
//
// Validates: R-2.4.8, R-2.12.10
func TestEngine_ShortcutAliasCreateDoesNotAdoptACaseVariantFolder(t *testing.T) {
	t.Parallel()

	eng, syncRoot := caseInsensitiveShortcutEngine(t)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "Shared", "x2"), 0o700))

	_, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	root := onlyShortcutRoot(t, eng)
	assert.Equal(t, syncshortcut.ShortcutRootStateBlockedPath, root.State)
	assert.Nil(t, root.LocalRootIdentity, "the user's folder is not the alias's")
	assert.Equal(t, []string{"x2"}, syncfixture.LocalNames(t, filepath.Join(syncRoot, "Shared")))
	assert.Empty(t, syncfixture.LocalNames(t, filepath.Join(syncRoot, "Shared", "x2")))
}

// The alias folder a crashed materialize left under its exact name is the
// alias's: it is adopted.
//
// Validates: R-2.4.8, R-2.5.1, R-2.12.10
func TestEngine_ShortcutAliasCreateAdoptsItsExactlySpelledFolder(t *testing.T) {
	t.Parallel()

	eng, syncRoot := caseInsensitiveShortcutEngine(t)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "Shared", "X2"), 0o700))

	_, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	root := onlyShortcutRoot(t, eng)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, root.State)
	require.NotNil(t, root.LocalRootIdentity)
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "X2"))
	require.NoError(t, err)
	assert.Equal(t, identity, *root.LocalRootIdentity)
}

// With nothing at the alias's name, the alias folder is made under the
// stored spelling.
//
// Validates: R-2.4.8, R-2.12.10
func TestEngine_ShortcutAliasCreateMakesItsFolder(t *testing.T) {
	t.Parallel()

	eng, syncRoot := caseInsensitiveShortcutEngine(t)

	_, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	root := onlyShortcutRoot(t, eng)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, root.State)
	require.NotNil(t, root.LocalRootIdentity)
	assert.Equal(t, []string{"X2"}, syncfixture.LocalNames(t, filepath.Join(syncRoot, "Shared")))
}
