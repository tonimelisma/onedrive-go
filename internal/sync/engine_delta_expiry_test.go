package sync

import (
	"context"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// deltaGone is the 410 OneDrive answers for an expired delta token, with the
// resync flavour as its inner code ("" for a bare 410).
func deltaGone(innerCode string) error {
	graphErr := &graph.GraphError{StatusCode: 410, Code: "resyncRequired", Err: graph.ErrGone}
	if innerCode != "" {
		graphErr.InnerCodes = []string{innerCode}
	}

	return graphErr
}

// expiringScenarioClient answers the next delta call that carries a saved
// token with gone, once, and otherwise serves the scenario remote.
func expiringScenarioClient(t *testing.T, remote *moveScenarioRemote, gone error) (*engineMockClient, *bool) {
	t.Helper()

	client := remote.client(t)
	serve := client.deltaFn
	expire := false
	client.deltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
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

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "kept")
	remote.addFile(t, "item-gone", "root", "gone.txt", "deleted online")
	client, expire := expiringScenarioClient(t, remote, deltaGone("resyncChangesApplyDifferences"))
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
		{name: "upload differences", gone: deltaGone("resyncChangesUploadDifferences")},
		{name: "unknown flavour", gone: deltaGone("")},
		{name: "bare 410", gone: graph.ErrGone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
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
func TestMountRootEvents_ResyncFlavourDecidesWhatAnOrphanMeans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	eng := &Engine{
		driveID:          driveID,
		remoteRootItemID: "mount-root",
		logger:           testLogger(t),
		itemsClient: &engineMockClient{
			getItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
				return nil, graph.ErrNotFound
			},
		},
	}
	bl := baselineWith(&BaselineEntry{
		Path: "gone.txt", DriveID: driveID, ItemID: "gone", ParentID: "mount-root", ItemType: ItemTypeFile,
	})

	for _, tt := range []struct {
		kind         resyncKind
		wantReupload bool
	}{
		{kind: resyncKindApplyDifferences, wantReupload: false},
		{kind: resyncKindUploadDifferences, wantReupload: true},
		{kind: resyncKindUnknown, wantReupload: true},
	} {
		events := eng.mountRootEvents(t.Context(), nil, bl, true, tt.kind)
		require.Len(t, events, 1, "kind %d", tt.kind)
		assert.True(t, events[0].IsDeleted)
		assert.Equal(t, tt.wantReupload, events[0].ReuploadLocal, "kind %d", tt.kind)
	}
}

// Validates: R-2.15.3
func TestNewDeltaExpiredError_ParsesTheResyncFlavour(t *testing.T) {
	t.Parallel()

	assert.Equal(t, resyncKindApplyDifferences, resyncKindOf(newDeltaExpiredError(deltaGone("resyncChangesApplyDifferences"))))
	assert.Equal(t, resyncKindUploadDifferences, resyncKindOf(newDeltaExpiredError(deltaGone("resyncChangesUploadDifferences"))))
	assert.Equal(t, resyncKindUnknown, resyncKindOf(newDeltaExpiredError(deltaGone(""))))
	assert.Equal(t, resyncKindUnknown, resyncKindOf(newDeltaExpiredError(graph.ErrGone)))

	err := newDeltaExpiredError(deltaGone("resyncChangesApplyDifferences"))
	require.ErrorIs(t, err, errDeltaExpired)
	require.ErrorIs(t, err, graph.ErrGone)
}

// tokenRecordingFetcher answers the first delta call with gone and every
// later one with an empty window, recording the tokens it was asked for.
type tokenRecordingFetcher struct {
	mu     stdsync.Mutex // guards tokens
	gone   error
	tokens []string
}

func (f *tokenRecordingFetcher) Delta(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	if len(f.tokens) == 1 {
		return nil, f.gone
	}

	return &graph.DeltaPage{DeltaLink: token}, nil
}

func (f *tokenRecordingFetcher) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.tokens...)
}

// Validates: R-2.15.3
func TestWatch_DeltaExpiredRecoversThroughTheFullRefresh(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	fetcher := &tokenRecordingFetcher{gone: deltaGone("resyncChangesUploadDifferences")}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveID, testLogger(t))
	// Every watch sleep lasts until the test cancels: the loop stops after
	// the recovery instead of polling on.
	obs.clock = &observerTestClock{syncClock: realClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}}

	recovered := make(chan resyncKind, 1)
	orphan := changeEvent{Source: sourceRemote, Type: ChangeDelete, ItemID: "gone", Path: "gone.txt", IsDeleted: true, ReuploadLocal: true}
	obs.SetDeltaExpiredRecovery(func(_ context.Context, kind resyncKind) (remoteObservationBatch, error) {
		recovered <- kind
		return remoteObservationBatch{
			emitted:               []changeEvent{orphan},
			cursorToken:           "token-after-full-refresh",
			markFullRemoteRefresh: true,
		}, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batches := make(chan remoteObservationBatch, 4)
	events := startTestRemoteBatchDrain(ctx, batches, nil)
	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, "expired-token", batches, time.Hour, nil, nil)
	}()

	assert.Equal(t, resyncKindUploadDifferences, <-recovered, "the resync flavour reaches the full refresh")
	emitted := <-events
	assert.True(t, emitted.ReuploadLocal, "the full refresh's batch, orphans included, is what watch emits")
	require.Eventually(t, func() bool { return obs.CurrentDeltaToken() == "token-after-full-refresh" },
		time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, []string{"expired-token"}, fetcher.seen(),
		"watch never re-enumerates on its own, without orphan detection")
}

// Validates: R-2.15.2
func TestRunOnce_SavedTokenOneDriveKeepsRefusingIsReenumerated(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "kept")
	remote.addFile(t, "item-gone", "root", "gone.txt", "deleted online")
	refused := &graph.GraphError{StatusCode: 400, Code: "invalidRequest", Err: graph.ErrBadRequest}
	client, refuse := expiringScenarioClient(t, remote, refused)
	serve := client.deltaFn
	refusals := 0
	client.deltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
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
