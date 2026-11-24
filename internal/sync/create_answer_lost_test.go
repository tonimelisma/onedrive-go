package sync

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// newScenarioCreateExecutor returns an executor that creates and uploads on
// remote and records its pending creates in a real store, plus that store and
// the sync root.
func newScenarioCreateExecutor(t *testing.T, remote *moveScenarioRemote) (*syncexec.Executor, *syncstore.SyncStore, string) {
	t.Helper()

	return newScenarioCreateExecutorOn(t, remote, remote.client(t), nil)
}

// newScenarioCreateExecutorOn is newScenarioCreateExecutor through client, a
// client of remote whose functions the test may still replace, with the
// executor and the store on the clock now (nil is the wall clock).
func newScenarioCreateExecutorOn(
	t *testing.T, remote *moveScenarioRemote, client *syncfixture.EngineMockClient, now func() time.Time,
) (*syncexec.Executor, *syncstore.SyncStore, string) {
	t.Helper()

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	_, logger := syncfixture.NewDebugLogBuffer()
	store := syncstoretest.NewTestStoreOn(t, now)
	cfg := mustNewTestExecutorConfig(t, &syncexec.ExecutorInputs{
		Items:          client,
		Downloads:      client,
		Uploads:        client,
		SyncTree:       syncTree,
		DriveID:        remote.driveID,
		Logger:         logger,
		PendingCreates: store,
		Now:            now,
	})

	return syncexec.NewExecution(cfg, syncfixture.EmptyBaseline()), store, syncRoot
}

// loseCreateAnswers turns the lost create answers of remote on or off
// (moveScenarioRemote.loseCreateAnswers).
func loseCreateAnswers(remote *moveScenarioRemote, lose bool) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.loseCreateAnswers = lose
}

// Validates: R-5.6.15
//
// The request that creates a new file reached OneDrive and its answer never
// reached the client (live, 2026-11-11, during a network outage). The attempt
// cleared its intent as if nothing had been created, every retry got 409
// "name already exists" on its own empty file, and the next observation read
// that file as someone else's: a create conflict. The intent now outlives the
// attempt, and the retry's 409 finishes the upload into the empty file by ID.
func TestUploadNewFile_CreateWhoseAnswerWasLostIsFinishedByTheRetry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, content string }{
		{name: "a file with content", content: "my words\n"},
		{name: "an empty file", content: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			e, store, syncRoot := newScenarioCreateExecutor(t, remote)
			local := syncfixture.WriteExecTestFile(t, syncRoot, "doc.txt", tc.content)
			action := &syncdomain.Action{Type: syncdomain.ActionUpload, Path: "doc.txt"}

			loseCreateAnswers(remote, true)
			_, lostErr := e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "doc.txt", local)
			require.ErrorIs(t, lostErr, graph.ErrCreateOutcomeUnknown)
			require.Equal(t, map[string]string{"doc.txt": ""}, remote.tree(), "the create landed: an empty file")
			lost, ok := remote.itemAt("doc.txt")
			require.True(t, ok)
			pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			require.True(t, found, "the intent outlives a create whose outcome is unknown")
			assert.Empty(t, pending.ItemID, "the client never learned the item's ID")

			loseCreateAnswers(remote, false)
			result, err := e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "doc.txt", local)
			require.NoError(t, err, "the retry's 409 is about its own empty file")
			assert.Equal(t, lost.ID, result.Item.ID, "the content went into the file the lost create made")
			assert.Equal(t, map[string]string{"doc.txt": tc.content}, remote.tree(), "one remote file, holding the content")
			pending, found, err = store.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, lost.ID, pending.ItemID, "the claim is recorded, so observation knows the file as ours")
		})
	}
}

// Validates: R-5.6.15
//
// The create landed and its answer was lost; the retry that came next was
// refused before it created anything (a 429, or a request that never left),
// and only the retry after that met the empty file with a 409. The refused
// retry cleared the intent of the lost create, so the 409 found no intent,
// the empty file read as someone else's, and once the watch's remote poll
// showed it the user's file became a conflict copy with the empty file at its
// own name (watch model, seed 4). A refused retry keeps the earlier intent,
// and the next retry's 409 finishes the upload into the lost create's file.
func TestUploadNewFile_RefusedRetryKeepsTheIntentOfAnEarlierLostCreate(t *testing.T) {
	t.Parallel()

	intendedAt := time.Date(2026, 11, 23, 8, 16, 2, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		refusal error
	}{
		{
			name:    "a 429",
			refusal: &graph.GraphError{StatusCode: http.StatusTooManyRequests, Code: "activityLimitReached", Err: graph.ErrThrottled},
		},
		{name: "a request that never left", refusal: errors.New("graph: obtaining token: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clock := intendedAt
			now := func() time.Time { return clock }
			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			e, store, syncRoot := newScenarioCreateExecutorOn(t, remote, remote.client(t), now)
			local := syncfixture.WriteExecTestFile(t, syncRoot, "watch-4.txt", "the words the user wrote\n")
			action := &syncdomain.Action{Type: syncdomain.ActionUpload, Path: "watch-4.txt"}
			upload := func() (*driveops.UploadResult, error) {
				clock = clock.Add(time.Second)
				return e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "watch-4.txt", local)
			}

			loseCreateAnswers(remote, true)
			_, err := upload()
			require.ErrorIs(t, err, graph.ErrCreateOutcomeUnknown)
			loseCreateAnswers(remote, false)
			lost, ok := remote.itemAt("watch-4.txt")
			require.True(t, ok, "the lost create left its empty file")

			refuseCreates(remote, func(string, string) error { return tc.refusal })
			_, err = upload()
			require.ErrorIs(t, err, tc.refusal)
			refuseCreates(remote, nil)
			pending, found, err := store.PendingCreateAt(t.Context(), "watch-4.txt")
			require.NoError(t, err)
			require.True(t, found, "the refused retry keeps the intent of the earlier lost create")
			assert.Empty(t, pending.ItemID)

			result, err := upload()
			require.NoError(t, err, "the 409 is about the lost create's own empty file")
			assert.Equal(t, lost.ID, result.Item.ID, "the content went into the file the lost create made")
			assert.Equal(t, map[string]string{"watch-4.txt": "the words the user wrote\n"}, remote.tree(),
				"one remote file at its own name, holding the content; no conflict copy, no empty file")
			pending, found, err = store.PendingCreateAt(t.Context(), "watch-4.txt")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, lost.ID, pending.ItemID, "the claim is recorded, so observation knows the file as ours")
		})
	}
}

// refuseCreates sets remote's refuseCreate under its lock (nil lifts it).
func refuseCreates(remote *moveScenarioRemote, refuse func(parentID, name string) error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.refuseCreate = refuse
}

// Validates: R-5.6.15
//
// The create landed, but the listing that follows the retry's 409 does not
// show the file yet. Nothing can be claimed in this attempt, and the intent
// stays for the attempt or the observation that does see it.
func TestUploadNewFile_ConflictOnAFileNotListedYetKeepsThePriorIntent(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	e, store, syncRoot := newScenarioCreateExecutorOn(t, remote, client, nil)
	local := syncfixture.WriteExecTestFile(t, syncRoot, "doc.txt", "my words\n")
	action := &syncdomain.Action{Type: syncdomain.ActionUpload, Path: "doc.txt"}
	loseCreateAnswers(remote, true)
	_, lostErr := e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "doc.txt", local)
	require.Error(t, lostErr)
	loseCreateAnswers(remote, false)

	client.ListChildrenFn = func(context.Context, driveid.ID, string) ([]graph.Item, error) {
		return nil, nil // the folder's listing has not caught up with the create
	}
	_, err := e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "doc.txt", local)
	require.ErrorIs(t, err, graph.ErrConflict)

	assert.Equal(t, map[string]string{"doc.txt": ""}, remote.tree())
	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "the intent is kept for the file that is not visible yet")
	assert.Empty(t, pending.ItemID)
}

// Validates: R-5.6.15
//
// A claim is only made for the empty file of a create this path was known to
// have asked for, recently. Without an earlier intent, with an intent older
// than its TTL, with content in the file, with another spelling of the name,
// or with a folder there, the 409 stays a conflict: nothing is uploaded into
// what holds the name and no record names it.
func TestUploadNewFile_ConflictNeverClaimsAFileThatIsNotItsOwnEmptyCreate(t *testing.T) {
	t.Parallel()

	intendedAt := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		remoteName    string
		remoteContent string
		remoteFolder  bool
		priorIntent   bool
		retriedAfter  time.Duration
		nameTaken     bool
	}{
		{name: "an empty file and no earlier intent", remoteName: "doc.txt"},
		{
			name: "an empty file and an expired intent", remoteName: "doc.txt",
			priorIntent: true, retriedAfter: syncdomain.PendingCreateIntentTTL + time.Minute,
		},
		{
			name: "a file with content and a fresh intent", remoteName: "doc.txt", remoteContent: "their words\n",
			priorIntent: true, retriedAfter: time.Minute,
		},
		{
			name: "an empty file spelled differently and a fresh intent", remoteName: "DOC.txt",
			priorIntent: true, retriedAfter: time.Minute, nameTaken: true,
		},
		{
			name: "a folder and a fresh intent", remoteName: "doc.txt", remoteFolder: true,
			priorIntent: true, retriedAfter: time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			if tc.remoteFolder {
				remote.addFolder("stranger", syncexec.GraphRootID, tc.remoteName)
			} else {
				remote.addFile(t, "stranger", syncexec.GraphRootID, tc.remoteName, tc.remoteContent)
			}
			before := remote.tree()
			// The intent, when there is one, is recorded at intendedAt; the
			// retry runs tc.retriedAfter later.
			retriedAt := intendedAt.Add(tc.retriedAfter)
			clock := intendedAt
			now := func() time.Time { return clock }
			e, store, syncRoot := newScenarioCreateExecutorOn(t, remote, remote.client(t), now)
			local := syncfixture.WriteExecTestFile(t, syncRoot, "doc.txt", "my words\n")
			if tc.priorIntent {
				require.NoError(t, store.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{
					DriveID: remote.driveID, Path: "doc.txt", ParentID: syncexec.GraphRootID,
				}))
			}
			clock = retriedAt
			stranger, ok := remote.itemAt("doc.txt")
			require.True(t, ok)

			_, err := e.UploadNewFileForTest(t.Context(), &syncdomain.Action{Type: syncdomain.ActionUpload, Path: "doc.txt"},
				remote.driveID, syncexec.GraphRootID, "doc.txt", local)
			require.ErrorIs(t, err, graph.ErrConflict)
			var nameTaken *syncexec.RemoteNameTakenError
			assert.Equal(t, tc.nameTaken, errors.As(err, &nameTaken), "only another spelling is a name collision")

			assert.Equal(t, before, remote.tree(), "what holds the name is untouched")
			after, ok := remote.itemAt("doc.txt")
			require.True(t, ok)
			assert.Equal(t, stranger.ETag, after.ETag, "nothing was written to it")
			assert.Zero(t, remote.uploads)
			_, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			assert.False(t, found, "neither an intent nor a claim is left")
		})
	}
}

// Validates: R-5.6.15
//
// The claim rests on the file being empty, and that is checked up to the
// write itself: content another device puts into the file after the listing
// is seen by the read that follows, and content put there after that read
// changes the eTag the upload is conditional on. Either way their content
// stays.
func TestUploadNewFile_ClaimNeverOverwritesContentWrittenMeanwhile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		afterListing  bool
		wantSentinel  error
		wantNoAttempt bool
	}{
		{name: "written after the listing", afterListing: true, wantSentinel: graph.ErrConflict, wantNoAttempt: true},
		{name: "written after the read", wantSentinel: graph.ErrPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			client := remote.client(t)
			e, _, syncRoot := newScenarioCreateExecutorOn(t, remote, client, nil)
			local := syncfixture.WriteExecTestFile(t, syncRoot, "doc.txt", "my words\n")
			action := &syncdomain.Action{Type: syncdomain.ActionUpload, Path: "doc.txt"}
			loseCreateAnswers(remote, true)
			_, lostErr := e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "doc.txt", local)
			require.Error(t, lostErr)
			loseCreateAnswers(remote, false)

			if tc.afterListing {
				client.ListChildrenFn = func(_ context.Context, _ driveid.ID, parentID string) ([]graph.Item, error) {
					children := remote.listChildren(parentID)
					remote.putFileAt(t, "doc.txt", "their words\n")
					return children, nil
				}
			} else {
				client.GetItemFn = func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
					item, err := remote.getItem(itemID)
					remote.putFileAt(t, "doc.txt", "their words\n")
					return item, err
				}
			}
			var attempts []string
			uploadToItem := client.UploadToItemFn
			client.UploadToItemFn = func(
				ctx context.Context, driveID driveid.ID, itemID, ifMatch string,
				content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
			) (*graph.Item, error) {
				attempts = append(attempts, ifMatch)
				return uploadToItem(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)
			}

			_, err := e.UploadNewFileForTest(t.Context(), action, remote.driveID, syncexec.GraphRootID, "doc.txt", local)
			require.ErrorIs(t, err, tc.wantSentinel)

			assert.Equal(t, map[string]string{"doc.txt": "their words\n"}, remote.tree(), "their content stays")
			if tc.wantNoAttempt {
				assert.Empty(t, attempts, "nothing is sent to a file that is no longer empty")
				return
			}
			require.Len(t, attempts, 1)
			assert.NotEmpty(t, attempts[0], "the upload is conditional on the eTag that was read")
		})
	}
}

// Validates: R-5.6.15
//
// The same loss across two passes: the create lands, its answer is lost and
// the upload fails. The next pass observes the empty file before it retries.
// With the intent cleared that file was a stranger's: a create conflict, the
// user's file renamed to a conflict copy and the empty file downloaded in its
// place. With the intent kept, observation knows it as this client's own and
// the upload goes into it.
func TestRunOnce_CreateWhoseAnswerWasLostFinishesIntoItsOwnEmptyFile(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("my words\n"), 0o600))

	loseCreateAnswers(remote, true)
	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"doc.txt": ""}, remote.tree(), "the create landed: an empty file")
	lost, ok := remote.itemAt("doc.txt")
	require.True(t, ok)

	loseCreateAnswers(remote, false)
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{"doc.txt": "my words\n"}, remote.tree(), "one remote file, holding the content")
	finished, ok := remote.itemAt("doc.txt")
	require.True(t, ok)
	assert.Equal(t, lost.ID, finished.ID, "the content went into the file the lost create made")
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "doc.txt"))
	require.NoError(t, err)
	assert.Equal(t, "my words\n", string(data))
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), "conflict", "no conflict copy")
	}
	requireScenarioQuietPlan(t, eng)
	assertSyncInvariants(t, &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot})
}
