package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// notALinkError is what the graph client answers for a saved delta token
// that is not a link.
func notALinkError() error {
	return fmt.Errorf("graph: reading delta token: %w", graph.ErrDeltaTokenNotALink)
}

// A saved drive position that is not a link is refused by the graph client
// and recovered as a cursor that cannot be continued: the drive is
// enumerated again from no token, never read from the initial path as if it
// were the changes since the token.
//
// Validates: R-2.15.2
func TestRunOnce_ASavedPositionThatIsNotALinkIsRecoveredByAFullEnumeration(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	var seen []string
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			seen = append(seen, token)
			if token != "" {
				return nil, notALinkError()
			}

			return syncfixture.DeltaPageWithItems([]graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}}, "fresh-token"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	seedBaseline(t, eng.baseline, ctx, []syncdomain.ActionOutcome{{
		Action: syncdomain.ActionDownload, Success: true, Path: "seed.txt", DriveID: driveID, ItemID: "seed-1",
	}}, "corrupt-token")
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(ctx, driveID, time.Now(), syncdomain.RemoteObservationModeDelta))

	_, err := eng.RunOnce(ctx, syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"corrupt-token", ""}, seen, "the refused position is enumerated again from no token")

	state, err := eng.baseline.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "fresh-token", state.Cursor)
}

// A shared folder's saved position that is not a link is recovered as an
// expired one: the folder is enumerated again from no token in the same
// pass, instead of failing every poll until the periodic full refresh.
//
// Validates: R-2.15.2
func TestRunOnce_AMountRootPositionThatIsNotALinkIsRecoveredByAFullEnumeration(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	var seen []string
	mock := &syncfixture.EngineMockClient{
		FolderDeltaFn: func(_ context.Context, _ driveid.ID, _, token string) ([]graph.Item, string, error) {
			seen = append(seen, token)
			if token != "" {
				return nil, "", notALinkError()
			}

			return nil, "corrupt-token", nil
		},
	}

	tmpDir := t.TempDir()
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:                 filepath.Join(tmpDir, "test.db"),
		SyncRoot:               syncRoot,
		DriveID:                driveID,
		RemoteRootItemID:       "mount-root-root",
		RemoteRootDeltaCapable: true,
		Fetcher:                mock,
		Items:                  mock,
		Downloads:              mock,
		Uploads:                mock,
		FolderDelta:            mock,
		RecursiveLister:        mock,
		PermChecker:            mock,
		Logger:                 syncfixture.TestLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	_, err = eng.RunOnce(t.Context(), syncdomain.SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	_, err = eng.RunOnce(t.Context(), syncdomain.SyncDownloadOnly, RunOptions{})
	require.NoError(t, err, "the refused position is recovered in the same pass")

	refused := slices.Index(seen, "corrupt-token")
	require.GreaterOrEqual(t, refused, 0, "the second pass continues from the saved position: %v", seen)
	require.Greater(t, len(seen), refused+1, "the refused position is followed by a fresh enumeration: %v", seen)
	assert.Empty(t, seen[refused+1], "the folder is enumerated again from no token")
}

// A shared folder's saved position whose change list leads back on itself
// is recovered like one that is not a link: the graph client follows the
// folder's pages itself, its guard ends the listing, and the folder is
// enumerated again from no token in the same pass, instead of failing every
// poll until the periodic full refresh.
//
// Validates: R-2.15.2
func TestRunOnce_AMountRootPositionWhoseChangeListLeadsBackIsRecoveredByAFullEnumeration(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	var seen []string
	mock := &syncfixture.EngineMockClient{
		FolderDeltaFn: func(_ context.Context, _ driveid.ID, _, token string) ([]graph.Item, string, error) {
			seen = append(seen, token)
			if token != "" {
				return nil, "", fmt.Errorf("graph: delta enumeration looped back from page 2 to page 1: %w",
					graph.ErrPagingNeverEnds)
			}

			return nil, "looping-token", nil
		},
	}

	tmpDir := t.TempDir()
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:                 filepath.Join(tmpDir, "test.db"),
		SyncRoot:               syncRoot,
		DriveID:                driveID,
		RemoteRootItemID:       "mount-root-root",
		RemoteRootDeltaCapable: true,
		Fetcher:                mock,
		Items:                  mock,
		Downloads:              mock,
		Uploads:                mock,
		FolderDelta:            mock,
		RecursiveLister:        mock,
		PermChecker:            mock,
		Logger:                 syncfixture.TestLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	_, err = eng.RunOnce(t.Context(), syncdomain.SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	_, err = eng.RunOnce(t.Context(), syncdomain.SyncDownloadOnly, RunOptions{})
	require.NoError(t, err, "the looping position is recovered in the same pass")

	looped := slices.Index(seen, "looping-token")
	require.GreaterOrEqual(t, looped, 0, "the second pass continues from the saved position: %v", seen)
	require.Greater(t, len(seen), looped+1, "the looping position is followed by a fresh enumeration: %v", seen)
	assert.Empty(t, seen[looped+1], "the folder is enumerated again from no token")
}
