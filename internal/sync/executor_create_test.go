package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-5.6.14
func TestExecutor_Upload_CreateConflictIsAStalePrecondition(t *testing.T) {
	t.Parallel()

	// A create never replaces, so a 409 means OneDrive already holds the
	// name (here another device's file of the same spelling): replan from
	// fresh truth instead of retrying the same create.
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
	}
	items := &executorMockItemClient{
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "theirs", Name: "x.docx", Size: int64(len("theirs"))}}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "x.docx", "mine")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "x.docx", DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "x.docx"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability, "OneDrive's answer proved the remote side moved on")
}

// followUpReadFailure is a read made after OneDrive answered a create or an
// upload (a listing to resolve a 409, a lookup of an upload's outcome) that
// fails on its own, with the decision that failure calls for.
type followUpReadFailure struct {
	name     string
	err      error
	class    errclass.Class
	evidence func(driveid.ID) ScopeKey
}

func followUpReadFailures() []followUpReadFailure {
	return []followUpReadFailure{
		{
			name:     "outage",
			err:      &graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			class:    resultRequeue,
			evidence: func(driveid.ID) ScopeKey { return SKService() },
		},
		{
			name:     "throttle",
			err:      &graph.GraphError{StatusCode: http.StatusTooManyRequests, Err: graph.ErrThrottled},
			class:    resultBlockScope,
			evidence: SKThrottleDrive,
		},
	}
}

// requireClassifiedAsTheRead checks that err, an action's failure whose
// follow-up read failed with read.err, is that read's failure: not a stale
// precondition, not the 409 or 412 the read followed, and classified by the
// read's own answer.
func requireClassifiedAsTheRead(
	t *testing.T, read followUpReadFailure, actionType actionType, driveID driveid.ID, err error,
) {
	t.Helper()

	require.ErrorIs(t, err, read.err)
	require.NotErrorIs(t, err, errActionPreconditionChanged, "a failed read proves nothing about the plan")
	require.NotErrorIs(t, err, graph.ErrConflict)
	require.NotErrorIs(t, err, graph.ErrPreconditionFailed)
	decision := classifyResult(&actionCompletion{
		Path: "read-failed", ActionType: actionType, DriveID: driveID,
		Err: err, HTTPStatus: extractHTTPStatus(err),
	})
	assert.Equal(t, read.class, decision.Class)
	assert.Equal(t, read.evidence(driveID), decision.ScopeEvidence)
}

// Validates: R-2.8.7, R-5.6.14
//
// After a create's 409 the folder is listed to resolve it. When that listing
// fails, nothing is known about the plan: the failure is the listing's own,
// classified by its answer (an outage, a throttle), not a stale precondition
// replanned at once. Joined to the 409, it read as stale and was recorded
// under the 409's status, so neither its outage nor its throttle was seen.
func TestExecutor_Upload_ListingFailureAfterACreateConflictIsClassifiedByItsOwnAnswer(t *testing.T) {
	t.Parallel()

	for _, read := range followUpReadFailures() {
		t.Run(read.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFile(t, "theirs", "root", "doc.txt", "theirs\n")
			e, _, syncRoot := newScenarioCreateExecutor(t, remote)
			client := remote.client(t)
			client.listChildrenFn = func(context.Context, driveid.ID, string) ([]graph.Item, error) {
				return nil, read.err
			}
			e.items = client
			local := writeExecTestFile(t, syncRoot, "doc.txt", "mine\n")

			_, err := e.uploadNewFile(t.Context(), &Action{Type: ActionUpload, Path: "doc.txt"},
				remote.driveID, graphRootID, "doc.txt", local)

			requireClassifiedAsTheRead(t, read, ActionUpload, remote.driveID, uploadTransferError(err))
			assert.Equal(t, map[string]string{"doc.txt": "theirs\n"}, remote.tree(), "nothing was written")
		})
	}
}

// Validates: R-2.8.7, R-5.6.14
//
// A folder create's 409 is resolved by listing the parent the same way. A
// listing that fails is the listing's failure, classified by its answer.
// Joined to the 409, an outage or a throttle was recorded under the 409's
// status as a per-item refusal.
func TestExecutor_CreateRemoteFolder_ListingFailureAfterAConflictIsClassifiedByItsOwnAnswer(t *testing.T) {
	t.Parallel()

	for _, read := range followUpReadFailures() {
		t.Run(read.name, func(t *testing.T) {
			t.Parallel()

			items := &executorMockItemClient{
				createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
					return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
				},
				listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
					return nil, read.err
				},
			}
			cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
			e := newExecution(cfg, emptyBaseline())
			driveID := driveid.New(synctest.TestDriveID)

			o := e.ExecuteFolderCreate(t.Context(), &Action{
				Type: ActionFolderCreate, Path: "photos", DriveID: driveID, CreateSide: CreateRemote,
				View: &pathView{Path: "photos"},
			})

			requireOutcomeFailure(t, &o)
			requireClassifiedAsTheRead(t, read, ActionFolderCreate, driveID, o.Error)
		})
	}
}

// Validates: R-2.8.7, R-5.6.12
//
// An upload whose final fragment may have committed is looked up before it
// is retried. When that lookup fails, the outcome is unknown and the failure
// is the lookup's, classified by its answer. Joined to a final fragment's 409
// or 412, it read as a stale precondition, was replanned at once, and hid the
// lookup's outage or throttle.
func TestUploadTransferError_AFailedOutcomeLookupIsNotAStalePrecondition(t *testing.T) {
	t.Parallel()

	for _, answer := range []*graph.GraphError{
		{StatusCode: http.StatusConflict, Code: graphCodeResourceModified, Err: graph.ErrConflict},
		{StatusCode: http.StatusPreconditionFailed, Code: graphCodeResourceModified, Err: graph.ErrPreconditionFailed},
	} {
		for _, read := range followUpReadFailures() {
			t.Run(fmt.Sprintf("%d then %s", answer.StatusCode, read.name), func(t *testing.T) {
				t.Parallel()

				client := &engineMockClient{
					uploadToItemFn: func(
						context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc,
					) (*graph.Item, error) {
						return nil, &graph.FinalFragmentError{Err: answer}
					},
					getItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
						return nil, read.err
					},
				}
				_, logger := newDebugLogBuffer()
				transfers := driveops.NewTransferManager(client, client, nil, logger)
				localPath := writeExecTestFile(t, t.TempDir(), "doc.txt", "mine\n")
				driveID := driveid.New(engineTestDriveID)

				_, err := transfers.UploadFileToItem(t.Context(), driveID, "item-doc", localPath,
					driveops.UploadOpts{IfMatch: "etag-planned"})

				requireClassifiedAsTheRead(t, read, ActionUpload, driveID, uploadTransferError(err))
			})
		}
	}
}

// Validates: R-5.6.2
func TestExecutor_Upload_OverwriteIsConditionalOnThePreflightETag(t *testing.T) {
	t.Parallel()

	var sentIfMatch []string
	ul := &executorMockUploader{
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _, ifMatch string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			sentIfMatch = append(sentIfMatch, ifMatch)
			return &graph.Item{ID: "known-item-id", ETag: "after"}, nil
		},
	}
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			item := defaultMockGetItem(driveID, itemID)
			item.ETag = "live-etag"
			return item, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "known.txt"},
	})
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"live-etag"}, sentIfMatch)
}

// Validates: R-5.6.2
func TestExecutor_Upload_OverwriteRaceWithARemoteEditIsAStalePrecondition(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusPreconditionFailed, Err: graph.ErrPreconditionFailed}
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "known.txt"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability,
		"a post-preflight 412 is a remote-write precondition, as for a remote delete or move")
}

// Validates: R-5.6.14
func TestExecutor_CreateRemoteFolder_ConflictAdoptsTheSameNamedFolder(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{
				{ID: "file-photos", Name: "photo", IsFolder: false},
				{ID: "existing-folder", Name: "photos", IsFolder: true, ETag: "e1"},
			}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &Action{
		Type: ActionFolderCreate, Path: "photos", CreateSide: CreateRemote, View: &pathView{Path: "photos"},
	})
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, "existing-folder", o.ItemID)
}

// Validates: R-5.6.14, R-2.12.2
func TestExecutor_CreateRemoteFolder_ConflictWithAnotherSpellingIsADurableCollision(t *testing.T) {
	t.Parallel()

	// "Photos" is the same OneDrive name as "photos" but not this folder's
	// spelling: the executor does not merge them, and the user has to rename
	// one, as for an upload (remoteNameTakenError).
	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "other", Name: "Photos", IsFolder: true}}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &Action{
		Type: ActionFolderCreate, Path: "photos", CreateSide: CreateRemote, View: &pathView{Path: "photos"},
	})
	requireOutcomeFailure(t, &o)
	var taken *remoteNameTakenError
	require.ErrorAs(t, o.Error, &taken)
	assert.NotErrorIs(t, o.Error, errActionPreconditionChanged, "not a precondition the next plan can fix")
}
