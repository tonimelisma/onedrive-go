package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// expiringScenarioClient answers the next delta call that carries a saved
// token with gone, once, and otherwise serves the scenario remote.
func expiringScenarioClient(t *testing.T, remote *moveScenarioRemote, gone error) (*syncfixture.EngineMockClient, *bool) {
	t.Helper()

	client := remote.client(t)
	serve := client.DeltaFn
	expire := false
	client.DeltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
		if expire && token != "" {
			expire = false
			return nil, gone
		}

		return serve(ctx, driveID, token)
	}

	return client, &expire
}

// Validates: R-2.15.3
func TestRunOnce_DeltaExpiredRecoverySynthesizesMissedDeletes(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "kept")
	remote.addFile(t, "item-gone", "root", "gone.txt", "deleted online")
	client, expire := expiringScenarioClient(t, remote, syncfixture.DeltaGone("resyncChangesApplyDifferences"))
	eng, syncRoot := newTestEngine(t, client)
	require.Equal(t, 2, runScenarioPass(t, eng).Downloads)

	// The delete happened while the token aged out: no delta window will
	// ever report it.
	remote.removeUnreported("item-gone")
	*expire = true

	report := runScenarioPass(t, eng)
	assert.Equal(t, 1, report.LocalDeletes, "the missed delete is applied: %+v", report)
	assert.NoFileExists(t, filepath.Join(syncRoot, "gone.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "keep.txt"))
	requireScenarioQuietPlan(t, eng)

	state, err := eng.baseline.ReadObservationState(t.Context())
	require.NoError(t, err)
	assert.NotZero(t, state.NextFullRemoteRefreshAt, "the recovery counts as a full refresh")
}

// Validates: R-2.15.3
func TestRunOnce_DeltaExpiredUploadDifferencesKeepsAndReuploadsLocal(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		gone error
	}{
		{name: "upload differences", gone: syncfixture.DeltaGone("resyncChangesUploadDifferences")},
		{name: "unknown flavor", gone: syncfixture.DeltaGone("")},
		{name: "bare 410", gone: graph.ErrGone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFile(t, "item-keep", "root", "keep.txt", "kept")
			remote.addFile(t, "item-gone", "root", "gone.txt", "missing online")
			client, expire := expiringScenarioClient(t, remote, tt.gone)
			eng, syncRoot := newTestEngine(t, client)
			require.Equal(t, 2, runScenarioPass(t, eng).Downloads)

			remote.removeUnreported("item-gone")
			*expire = true

			// OneDrive asked for the items it did not return to be uploaded
			// again; it never asked for them to be deleted here.
			report := runScenarioPass(t, eng)
			assert.Zero(t, report.LocalDeletes, "%+v", report)
			assert.Equal(t, 1, report.Uploads, "the local copy is uploaded again: %+v", report)
			got, err := localpath.ReadFile(filepath.Join(syncRoot, "gone.txt"))
			require.NoError(t, err)
			assert.Equal(t, "missing online", string(got))
			_, online := remote.itemAt("gone.txt")
			assert.True(t, online)
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.15.3
func TestMountRootEvents_ResyncFlavorDecidesWhatAnOrphanMeans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	eng := &Engine{
		driveID:          driveID,
		remoteRootItemID: "mount-root",
		logger:           syncfixture.TestLogger(t),
		itemsClient: &syncfixture.EngineMockClient{
			GetItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
				return nil, graph.ErrNotFound
			},
		},
	}
	bl := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "gone.txt", DriveID: driveID, ItemID: "gone", ParentID: "mount-root", ItemType: syncdomain.ItemTypeFile,
	})

	for _, tt := range []struct {
		kind         syncdomain.ResyncKind
		wantReupload bool
	}{
		{kind: syncdomain.ResyncKindApplyDifferences, wantReupload: false},
		{kind: syncdomain.ResyncKindUploadDifferences, wantReupload: true},
		{kind: syncdomain.ResyncKindUnknown, wantReupload: true},
	} {
		events, _ := eng.remoteSource().mountRootItems(t.Context(), nil, bl, true, tt.kind)
		require.Len(t, events, 1, "kind %d", tt.kind)
		assert.True(t, events[0].IsDeleted)
		assert.Equal(t, tt.wantReupload, events[0].ReuploadLocal, "kind %d", tt.kind)
	}
}

// Validates: R-2.15.3
func TestNewDeltaExpiredError_ParsesTheResyncFlavor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, syncdomain.ResyncKindApplyDifferences, syncdomain.ResyncKindOf(syncdomain.NewDeltaExpiredError(syncfixture.DeltaGone("resyncChangesApplyDifferences"))))
	assert.Equal(t, syncdomain.ResyncKindUploadDifferences, syncdomain.ResyncKindOf(syncdomain.NewDeltaExpiredError(syncfixture.DeltaGone("resyncChangesUploadDifferences"))))
	assert.Equal(t, syncdomain.ResyncKindUnknown, syncdomain.ResyncKindOf(syncdomain.NewDeltaExpiredError(syncfixture.DeltaGone(""))))
	assert.Equal(t, syncdomain.ResyncKindUnknown, syncdomain.ResyncKindOf(syncdomain.NewDeltaExpiredError(graph.ErrGone)))

	err := syncdomain.NewDeltaExpiredError(syncfixture.DeltaGone("resyncChangesApplyDifferences"))
	require.ErrorIs(t, err, syncdomain.ErrDeltaExpired)
	require.ErrorIs(t, err, graph.ErrGone)
}

// Validates: R-2.15.2
func TestRunOnce_SavedTokenOneDriveKeepsRefusingIsReenumerated(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "kept")
	remote.addFile(t, "item-gone", "root", "gone.txt", "deleted online")
	refused := &graph.GraphError{StatusCode: 400, Code: "invalidRequest", Err: graph.ErrBadRequest}
	client, refuse := expiringScenarioClient(t, remote, refused)
	serve := client.DeltaFn
	refusals := 0
	client.DeltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
		if *refuse && token != "" {
			refusals++
			return nil, refused
		}
		return serve(ctx, driveID, token)
	}
	eng, syncRoot := newTestEngine(t, client)
	require.Equal(t, 2, runScenarioPass(t, eng).Downloads)

	remote.removeUnreported("item-gone")
	*refuse = true

	// The token used to be retried on every run and the drive never synced
	// again; now the second refusal re-enumerates, orphans included.
	report := runScenarioPass(t, eng)
	assert.Equal(t, 2, refusals, "refused twice, then not asked again")
	assert.Equal(t, 1, report.LocalDeletes, "%+v", report)
	assert.NoFileExists(t, filepath.Join(syncRoot, "gone.txt"))

	*refuse = false
	requireScenarioQuietPlan(t, eng)
}
