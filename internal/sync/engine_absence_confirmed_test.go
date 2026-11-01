package sync

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// A complete enumeration returned the item, but the converter dropped it (it
// now carries a package facet the client does not sync). Building the full
// refresh's seen set from converted events read that as "gone online" and
// synthesized a local delete (D-008).
//
// Validates: R-6.2.2
func TestFullRefresh_ItemTheConverterSkipsIsNotSynthesizedAsDelete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	keepHash := hashContentQuickXor(t, "keep")
	notesHash := hashContentQuickXor(t, "my notes")
	reportAsPackage := false
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID},
				{
					ID: "item-keep", Name: "keep.txt", ParentID: "root", DriveID: driveID, Size: 4,
					QuickXorHash: keepHash, ETag: "etag-keep", ModifiedAt: modified,
				},
				{
					ID: "item-pkg", Name: "notes.txt", ParentID: "root", DriveID: driveID, Size: 8,
					QuickXorHash: notesHash, IsPackage: reportAsPackage, ETag: "etag-pkg", ModifiedAt: modified,
				},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	writeLocalFile(t, syncRoot, "keep.txt", "keep")
	writeLocalFile(t, syncRoot, "notes.txt", "my notes")
	seedBaseline(t, eng.baseline, ctx, []actionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "keep.txt", DriveID: driveID, ItemID: "item-keep",
			ItemType: ItemTypeFile, LocalHash: keepHash, RemoteHash: keepHash,
			LocalSize: 4, LocalSizeKnown: true, RemoteSize: 4, RemoteSizeKnown: true, ETag: "etag-keep",
		},
		{
			Action: ActionDownload, Success: true, Path: "notes.txt", DriveID: driveID, ItemID: "item-pkg",
			ItemType: ItemTypeFile, LocalHash: notesHash, RemoteHash: notesHash,
			LocalSize: 8, LocalSizeKnown: true, RemoteSize: 8, RemoteSizeKnown: true, ETag: "etag-pkg",
		},
	}, "")
	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	reportAsPackage = true
	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{FullReconcile: true})
	require.NoError(t, err)

	assert.Zero(t, report.LocalDeletes, "an enumerated item is not absent")
	data, readErr := os.ReadFile(filepath.Join(syncRoot, "notes.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, readErr)
	assert.Equal(t, "my notes", string(data))
}

// Validates: R-6.2.2
func TestConfirmMountRootOrphans_ListingThatSkippedALiveItemDeletesNothing(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	eng := &Engine{
		driveID:          driveID,
		remoteRootItemID: "mount-root",
		logger:           testLogger(t),
		itemsClient: &engineMockClient{
			getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
				switch itemID {
				case "skipped":
					return &graph.Item{ID: itemID, Name: "a.txt", ParentID: "sub", DriveID: driveID}, nil
				case "moved-out":
					return &graph.Item{ID: itemID, Name: "b.txt", ParentID: "elsewhere", DriveID: driveID}, nil
				case "elsewhere":
					return &graph.Item{ID: itemID, Name: "elsewhere", ParentID: "drive-root", IsFolder: true, DriveID: driveID}, nil
				case "drive-root":
					return &graph.Item{ID: itemID, IsRoot: true, IsFolder: true, DriveID: driveID}, nil
				case "no-parent":
					return &graph.Item{ID: itemID, Name: "f.txt", DriveID: driveID}, nil
				case "unresolvable":
					return &graph.Item{ID: itemID, Name: "g.txt", ParentID: "parent-lookup-fails", DriveID: driveID}, nil
				case "parent-lookup-fails":
					return nil, assert.AnError
				case "trashed":
					return &graph.Item{ID: itemID, Name: "c.txt", IsDeleted: true, DriveID: driveID}, nil
				case "unknown":
					return nil, assert.AnError
				default:
					return nil, graph.ErrNotFound
				}
			},
		},
	}
	listed := []graph.Item{{ID: "sub", Name: "sub", ParentID: "mount-root", IsFolder: true}}
	orphans := []changeEvent{
		{ItemID: "skipped", Path: "sub/a.txt", Type: ChangeDelete, IsDeleted: true},
		{ItemID: "moved-out", Path: "b.txt", Type: ChangeDelete, IsDeleted: true},
		{ItemID: "trashed", Path: "c.txt", Type: ChangeDelete, IsDeleted: true},
		{ItemID: "gone", Path: "d.txt", Type: ChangeDelete, IsDeleted: true},
		{ItemID: "unknown", Path: "e.txt", Type: ChangeDelete, IsDeleted: true},
		{ItemID: "no-parent", Path: "f.txt", Type: ChangeDelete, IsDeleted: true},
		{ItemID: "unresolvable", Path: "g.txt", Type: ChangeDelete, IsDeleted: true},
	}

	confirmed, _ := eng.confirmMountRootOrphans(t.Context(), listed, orphans)

	var ids []string
	for i := range confirmed {
		ids = append(ids, confirmed[i].ItemID)
	}
	assert.ElementsMatch(t, []string{"moved-out", "trashed", "gone"}, ids,
		"only a 404, a deleted facet, or a provable move out of the mount is proof of absence")
}

// Validates: R-6.2.2
func TestValidateMountRootListingParents_ChildUnderUnlistedFolderAbortsListing(t *testing.T) {
	t.Parallel()

	ok := []graph.Item{
		{ID: "sub", Name: "sub", ParentID: "mount-root", IsFolder: true},
		{ID: "a", Name: "a.txt", ParentID: "sub"},
	}
	require.NoError(t, validateMountRootListingParents(ok, "mount-root"))

	bad := append(append([]graph.Item(nil), ok...), graph.Item{ID: "x", Name: "x.txt", ParentID: "some-other-folder"})
	err := validateMountRootListingParents(bad, "mount-root")
	require.ErrorIs(t, err, errRemoteObservationIncomplete)
}

// Validates: R-2.4.3, R-6.2.2
func TestApplyShortcutRootCompleteOmission_ConfirmedLiveBindingStaysActive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		live  func(itemID string) (*graph.Item, error)
		state ShortcutRootState
	}{
		{
			name: "still a live shortcut to the same folder",
			live: func(itemID string) (*graph.Item, error) {
				return &graph.Item{ID: itemID, Name: "Docs", RemoteDriveID: "drive-1", RemoteItemID: "target-1", RemoteIsFolder: true}, nil
			},
			state: ShortcutRootStateActive,
		},
		{
			name:  "gone",
			live:  func(string) (*graph.Item, error) { return nil, graph.ErrNotFound },
			state: ShortcutRootStateRemovedFinalDrain,
		},
		{
			name: "now points at another folder",
			live: func(itemID string) (*graph.Item, error) {
				return &graph.Item{ID: itemID, Name: "Docs", RemoteDriveID: "drive-1", RemoteItemID: "target-2", RemoteIsFolder: true}, nil
			},
			state: ShortcutRootStateRemovedFinalDrain,
		},
		{
			name:  "lookup failed",
			live:  func(string) (*graph.Item, error) { return nil, assert.AnError },
			state: ShortcutRootStateTargetUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, _ := newTestEngine(t, &engineMockClient{
				getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
					return tc.live(itemID)
				},
			})
			require.NoError(t, eng.baseline.replaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
				NamespaceID:       shortcutNamespaceTestID,
				BindingItemID:     "binding-1",
				RelativeLocalPath: "Shared/Docs",
				LocalAlias:        "Docs",
				RemoteDriveID:     driveid.New("drive-1"),
				RemoteItemID:      "target-1",
				RemoteIsFolder:    true,
				State:             ShortcutRootStateActive,
				ProtectedPaths:    []string{"Shared/Docs"},
			}}))

			// A complete enumeration that omitted the placeholder.
			topology := shortcutTopologyBatch{NamespaceID: shortcutNamespaceTestID, Kind: shortcutTopologyObservationComplete}
			require.NoError(t, eng.confirmShortcutCompleteOmissions(t.Context(), &topology))
			_, err := eng.baseline.applyShortcutTopology(t.Context(), topology)
			require.NoError(t, err)

			roots, err := eng.baseline.listShortcutRoots(t.Context())
			require.NoError(t, err)
			require.Len(t, roots, 1)
			assert.Equal(t, tc.state, roots[0].State)
		})
	}
}

// Validates: R-6.2.2
func TestFullRefresh_OrphanStillLiveOnOneDriveIsNotDeleted(t *testing.T) {
	t.Parallel()

	const fileID = "item-a"
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFile(t, fileID, "root", "a.txt", "alpha\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// A complete enumeration that transiently omits a.txt, which GetItem
	// still returns.
	remote.mu.Lock()
	omitted := remote.items[fileID]
	delete(remote.items, fileID)
	remote.mu.Unlock()
	client := remote.client(t)
	baseGetItem := client.getItemFn
	client.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		if itemID == fileID {
			copied := *omitted
			return &copied, nil
		}
		return baseGetItem(ctx, driveID, itemID)
	}
	eng.fetcher = client
	eng.itemsClient = client

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{FullReconcile: true})
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(syncRoot, "a.txt"), "an item a live lookup still finds is not deleted")
}

// Validates: R-2.15.3, R-6.2.2
//
// A full rescan whose lookup of a missing item failed keeps the item, as it
// must: absence is not proof. But the rescan also saves a fresh delta
// position, which never reports that item again, so only the next full rescan
// can judge it, and that was a day away: until then a file OneDrive replaced
// by a folder stayed in the remote mirror, the folder was shown as a name
// twin, and its contents were not synced (chaos Graph layer, D-092, seed 4).
// An undecided orphan pulls the next full rescan forward.
func TestFullRefresh_UndecidedOrphanPullsTheNextRescanForward(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	const fileID = "item-a"
	remote.addFile(t, fileID, "root", "a.txt", "file\n")
	client := remote.client(t)
	getItem := client.getItemFn
	failLookup := false
	client.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		if failLookup && itemID == fileID {
			return nil, &graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError}
		}
		return getItem(ctx, driveID, itemID)
	}
	h := newSyncHarnessWithClient(t, remote, client)
	clock := &modelClock{}
	h.eng.clock = clock
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	remote.deleteAt(t, "a.txt")
	remote.mkdirAt(t, "a.txt")
	remote.putFileAt(t, "a.txt/inside.txt", "inside\n")
	failLookup = true
	_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{FullReconcile: true})
	require.NoError(t, err)
	failLookup = false

	clock.advance(fullRefreshAfterFolderDeleteDelay + time.Minute)
	for range 2 {
		runScenarioPass(t, h.eng)
	}

	requireScenarioLocalFile(t, h.syncRoot, "a.txt/inside.txt", "inside\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.15.3, R-6.2.2
//
// An orphan whose lookup never succeeds must not cost a whole-drive
// enumeration every 15 minutes forever: the pulled-forward refresh backs off
// while the orphan stays undecided (doubling from 15 minutes towards the
// daily cadence), and once a refresh decides it the normal cadence returns.
func TestFullRefresh_PermanentlyUndecidedOrphanBacksOff(t *testing.T) {
	skipInShort(t)
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	const fileID = "item-a"
	remote.addFile(t, fileID, "root", "a.txt", "file\n")
	// Other content, so the one delete is not an emptied side the
	// delete-safety hold would keep for confirmation.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	client := remote.client(t)
	getItem := client.getItemFn
	failLookup := false
	client.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		if failLookup && itemID == fileID {
			return nil, &graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError}
		}
		return getItem(ctx, driveID, itemID)
	}
	h := newSyncHarnessWithClient(t, remote, client)
	clock := &modelClock{}
	h.eng.clock = clock
	runScenarioPass(t, h.eng)

	remote.deleteAt(t, "a.txt")
	failLookup = true
	_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{FullReconcile: true})
	require.NoError(t, err)

	// Passes every 16 minutes: refreshes are due 15, 30, 60, 120 and 240
	// minutes apart, so six and a half hours (24 passes) run at most five
	// and an hour and a half (6, the -race run, where a pass costs seconds)
	// at most three; without the backoff, one every pass.
	before := remote.fullDeltaCount()
	passes, minRefreshes, maxRefreshes := 24, 3, 5
	if raceDetectorEnabled {
		passes, minRefreshes, maxRefreshes = 6, 2, 3
	}
	for range passes {
		clock.advance(fullRefreshAfterFolderDeleteDelay + time.Minute)
		_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}
	backedOff := remote.fullDeltaCount() - before
	assert.GreaterOrEqual(t, backedOff, minRefreshes, "the undecided orphan is still re-checked")
	assert.LessOrEqual(t, backedOff, maxRefreshes, "the re-checks back off")

	// The lookup recovers: the next due refresh (at most twice the last
	// delay on) decides the orphan, and the daily cadence returns.
	failLookup = false
	for range 2 * passes {
		clock.advance(fullRefreshAfterFolderDeleteDelay + time.Minute)
		_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}
	assert.NoFileExists(t, filepath.Join(h.syncRoot, "a.txt"), "the decided orphan is applied")
	settled := remote.fullDeltaCount()
	for range passes {
		clock.advance(fullRefreshAfterFolderDeleteDelay + time.Minute)
		_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}
	assert.Equal(t, settled, remote.fullDeltaCount(), "no early refresh once nothing is undecided")
}

// Validates: R-2.15.3
func TestUndecidedRefreshDelay_DoublesToTheDailyCadence(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 15*time.Minute, undecidedRefreshDelay(1))
	assert.Equal(t, 30*time.Minute, undecidedRefreshDelay(2))
	assert.Equal(t, 2*time.Hour, undecidedRefreshDelay(4))
	assert.Equal(t, fullRemoteRefreshInterval, undecidedRefreshDelay(8))
	assert.Equal(t, fullRemoteRefreshInterval, undecidedRefreshDelay(1000))
}
