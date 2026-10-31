package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// nfdShortcutAliasEngine is a parent engine whose shortcut alias folder sits
// on disk under an NFD name (a folder copied from a Mac) while its record
// holds the stored NFC path, on a filesystem that tells the two apart.
func nfdShortcutAliasEngine(t *testing.T, mock *engineMockClient, state ShortcutRootState) (*testEngine, string) {
	t.Helper()

	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	aliasOnDisk := filepath.Join(syncRoot, "Shared", norm.NFD.String("Café"))
	require.NoError(t, os.MkdirAll(aliasOnDisk, 0o700))
	if _, err := os.Lstat(filepath.Join(syncRoot, "Shared", norm.NFC.String("Café"))); err == nil {
		t.Skip("the test filesystem finds an NFD name by its NFC spelling")
	}

	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", norm.NFD.String("Café")))
	require.NoError(t, err)
	stored := "Shared/" + norm.NFC.String("Café")
	require.NoError(t, eng.baseline.replaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: stored,
		LocalAlias:        norm.NFC.String("Café"),
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             state,
		ProtectedPaths:    []string{stored},
		LocalRootIdentity: &identity,
	}}))

	return eng, aliasOnDisk
}

// The alias is where it was, spelled as on disk. Reading it by its stored
// spelling found nothing, so the lifecycle took the NFD-named folder for a
// rename of the alias and renamed the shortcut on OneDrive to the NFD name.
//
// Validates: R-2.4.8, R-2.13.2
func TestEngine_ShortcutAliasWithNFDNameOnDiskIsNotARename(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		moveItemFn: func(context.Context, driveid.ID, string, string, string) (*graph.Item, error) {
			require.FailNow(t, "an alias that did not move must not be renamed online")
			return nil, assert.AnError
		},
		deleteItemFn: func(context.Context, driveid.ID, string) error {
			require.FailNow(t, "an alias that is still there must not be deleted online")
			return nil
		},
	}
	eng, aliasOnDisk := nfdShortcutAliasEngine(t, mock, ShortcutRootStateActive)

	changed, err := eng.reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.False(t, changed, "the alias is unchanged")
	assert.DirExists(t, aliasOnDisk)
	roots, err := eng.baseline.listShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, "Shared/"+norm.NFC.String("Café"), roots[0].RelativeLocalPath)
}

// A removed shortcut's local folder is removed where it is on disk; read by
// its stored spelling it looked already gone and stayed behind.
//
// Validates: R-2.4.8, R-2.13.2
func TestEngine_RemovedShortcutReleaseRemovesTheNFDNamedAliasFolder(t *testing.T) {
	t.Parallel()

	eng, aliasOnDisk := nfdShortcutAliasEngine(t, &engineMockClient{}, ShortcutRootStateRemovedReleasePending)

	_, err := eng.reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.NoDirExists(t, aliasOnDisk, "the alias folder is removed at its spelling on disk")
}
