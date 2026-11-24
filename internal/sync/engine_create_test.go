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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// followUpReadFailure is a read made after OneDrive answered a create or an
// upload (a listing to resolve a 409, a lookup of an upload's outcome) that
// fails on its own, with the decision that failure calls for.
type followUpReadFailure struct {
	name     string
	err      error
	class    errclass.Class
	evidence func(driveid.ID) syncdomain.ScopeKey
}

func followUpReadFailures() []followUpReadFailure {
	return []followUpReadFailure{
		{
			name:     "outage",
			err:      &graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			class:    resultRequeue,
			evidence: func(driveid.ID) syncdomain.ScopeKey { return syncdomain.SKService() },
		},
		{
			name:     "throttle",
			err:      &graph.GraphError{StatusCode: http.StatusTooManyRequests, Err: graph.ErrThrottled},
			class:    resultBlockScope,
			evidence: syncdomain.SKThrottleDrive,
		},
	}
}

// requireClassifiedAsTheRead checks that err, an action's failure whose
// follow-up read failed with read.err, is that read's failure: not a stale
// precondition, not the 409 or 412 the read followed, and classified by the
// read's own answer.
func requireClassifiedAsTheRead(
	t *testing.T, read followUpReadFailure, actionType syncdomain.ActionType, driveID driveid.ID, err error,
) {
	t.Helper()

	require.ErrorIs(t, err, read.err)
	require.NotErrorIs(t, err, syncdomain.ErrActionPreconditionChanged, "a failed read proves nothing about the plan")
	require.NotErrorIs(t, err, graph.ErrConflict)
	require.NotErrorIs(t, err, graph.ErrPreconditionFailed)
	decision := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path: "read-failed", ActionType: actionType, DriveID: driveID,
		Err: err, HTTPStatus: syncexec.ExtractHTTPStatus(err),
	}))
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

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFile(t, "theirs", "root", "doc.txt", "theirs\n")
			client := remote.client(t)
			client.ListChildrenFn = func(context.Context, driveid.ID, string) ([]graph.Item, error) {
				return nil, read.err
			}
			e, _, syncRoot := newScenarioCreateExecutorOn(t, remote, client, nil)
			local := syncfixture.WriteExecTestFile(t, syncRoot, "doc.txt", "mine\n")

			_, err := e.UploadNewFileForTest(t.Context(), &syncdomain.Action{Type: syncdomain.ActionUpload, Path: "doc.txt"},
				remote.driveID, syncexec.GraphRootID, "doc.txt", local)

			requireClassifiedAsTheRead(t, read, syncdomain.ActionUpload, remote.driveID, syncexec.UploadTransferError(err))
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

			items := &syncfixture.ExecutorMockItemClient{
				CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
					return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
				},
				ListChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
					return nil, read.err
				},
			}
			cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
			e := syncexec.NewExecution(cfg, syncfixture.EmptyBaseline())
			driveID := driveid.New(synctest.TestDriveID)

			o := e.ExecuteFolderCreate(t.Context(), &syncdomain.Action{
				Type: syncdomain.ActionFolderCreate, Path: "photos", DriveID: driveID, CreateSide: syncdomain.CreateRemote,
				View: &syncdomain.PathView{Path: "photos"},
			})

			syncfixture.RequireOutcomeFailure(t, &o)
			requireClassifiedAsTheRead(t, read, syncdomain.ActionFolderCreate, driveID, o.Error)
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
		{StatusCode: http.StatusConflict, Code: syncexec.GraphCodeResourceModified, Err: graph.ErrConflict},
		{StatusCode: http.StatusPreconditionFailed, Code: syncexec.GraphCodeResourceModified, Err: graph.ErrPreconditionFailed},
	} {
		for _, read := range followUpReadFailures() {
			t.Run(fmt.Sprintf("%d then %s", answer.StatusCode, read.name), func(t *testing.T) {
				t.Parallel()

				client := &syncfixture.EngineMockClient{
					UploadToItemFn: func(
						context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc,
					) (*graph.Item, error) {
						return nil, &graph.FinalFragmentError{Err: answer}
					},
					GetItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
						return nil, read.err
					},
				}
				_, logger := syncfixture.NewDebugLogBuffer()
				transfers := driveops.NewTransferManager(client, client, nil, logger)
				localPath := syncfixture.WriteExecTestFile(t, t.TempDir(), "doc.txt", "mine\n")
				driveID := driveid.New(syncfixture.EngineTestDriveID)

				_, err := transfers.UploadFileToItem(t.Context(), driveID, "item-doc", localPath,
					driveops.UploadOpts{IfMatch: "etag-planned"})

				requireClassifiedAsTheRead(t, read, syncdomain.ActionUpload, driveID, syncexec.UploadTransferError(err))
			})
		}
	}
}
