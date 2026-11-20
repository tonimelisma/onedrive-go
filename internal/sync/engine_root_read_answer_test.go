package sync

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// rootReadSharedFolderID is the item of the shared folder whose root an
// engine on the shared-folder path reads.
const rootReadSharedFolderID = "shared-folder-root"

// rootReadPath is one of the three ways an engine reads the root it syncs:
// a drive's own root incrementally or as a full refresh, or a shared folder's
// root below a drive.
type rootReadPath struct {
	name             string
	sharedFolderRoot bool
	fullReconcile    bool
}

func rootReadPaths() []rootReadPath {
	return []rootReadPath{
		{name: "drive root delta"},
		{name: "drive root full refresh", fullReconcile: true},
		{name: "shared folder root", sharedFolderRoot: true},
	}
}

// rootReadAnswer is what OneDrive answers the read of the root with: an
// error, or nil for a read that succeeds and finds nothing new. A test sets
// it only while no observation runs.
type rootReadAnswer struct {
	err error
}

// newRootReadAnswerEngine builds an engine whose root read gets answer.
func newRootReadAnswerEngine(t *testing.T, path rootReadPath, answer *rootReadAnswer) *testEngine {
	t.Helper()

	mock := &engineMockClient{
		deltaFn: func(context.Context, driveid.ID, string) (*graph.DeltaPage, error) {
			if answer.err != nil {
				return nil, answer.err
			}
			return deltaPageWithItems(nil, "drive-token"), nil
		},
		folderDeltaFn: func(context.Context, driveid.ID, string, string) ([]graph.Item, string, error) {
			if answer.err != nil {
				return nil, "", answer.err
			}
			return nil, "folder-token", nil
		},
		// A folder delta answered "not found" is read as change tracking
		// the folder does not offer, so the observation lists the folder
		// instead; the answer that listing gets is the root's.
		listChildrenRecursiveFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
			return nil, answer.err
		},
	}
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		if path.sharedFolderRoot {
			inputs.RemoteRootItemID = rootReadSharedFolderID
			inputs.RemoteRootDeltaCapable = true
		}
	})

	return eng
}

// observeRootIssues runs one root observation and returns the observation
// issues the store then holds.
func observeRootIssues(t *testing.T, eng *testEngine, path rootReadPath) []ObservationIssueRow {
	t.Helper()

	ctx := t.Context()
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, err = testEngineFlow(t, eng).observeAndCommitRemoteCurrentState(ctx, bl, path.fullReconcile)
	require.NoError(t, err)

	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)

	return issues
}

// storeRootIssue puts the root issue of the given type into the store, as an
// earlier root observation left it.
func storeRootIssue(t *testing.T, eng *testEngine, issueType IssueType) {
	t.Helper()

	batch := ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "/",
			DriveID:   eng.driveID,
			IssueType: issueType,
			ScopeKey:  SKPermRemoteRead(""),
		}},
		ManagedIssueTypes: []IssueType{issueType},
	}
	require.NoError(t, eng.baseline.ReconcileObservationFindings(t.Context(), &batch))
}

// Validates: R-2.10.4, R-6.6.28
//
// OneDrive's answer to the read of the root decides the root's issue: "not
// found" is remote_root_not_found, "forbidden" is remote_read_denied, on each
// of the three root paths, at the same path and read scope, so blocking and
// release treat both alike. The next observation replaces whatever an earlier
// one left, including a remote_read_denied row for a root whose next read
// answers "not found", and a read that succeeds clears either.
func TestRootObservation_AnswerDecidesTheStoredRootIssue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		stored IssueType
		answer error
		want   IssueType
	}{
		{name: "not found", answer: graph.ErrNotFound, want: IssueRemoteRootNotFound},
		{name: "forbidden", answer: graph.ErrForbidden, want: IssueRemoteReadDenied},
		{
			name:   "not found replaces the read denial",
			stored: IssueRemoteReadDenied, answer: graph.ErrNotFound, want: IssueRemoteRootNotFound,
		},
		{
			name:   "forbidden replaces not found",
			stored: IssueRemoteRootNotFound, answer: graph.ErrForbidden, want: IssueRemoteReadDenied,
		},
		{name: "a successful read clears not found", stored: IssueRemoteRootNotFound},
		{name: "a successful read clears the read denial", stored: IssueRemoteReadDenied},
	}

	for _, path := range rootReadPaths() {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				var answer rootReadAnswer
				eng := newRootReadAnswerEngine(t, path, &answer)
				if tc.stored != "" {
					storeRootIssue(t, eng, tc.stored)
				}

				answer.err = tc.answer
				issues := observeRootIssues(t, eng, path)

				if tc.want == "" {
					assert.Empty(t, issues)
					return
				}
				require.Len(t, issues, 1)
				assert.Equal(t, "/", issues[0].Path)
				assert.Equal(t, tc.want, issues[0].IssueType)
				assert.Equal(t, SKPermRemoteRead(""), issues[0].ScopeKey)
			})
		}
	}
}

// Validates: R-2.10.4
//
// A shared folder's delta answered "not found" is change tracking the folder
// does not offer, not the root's answer: the listing that follows decides. A
// listing that succeeds leaves no issue, and one answered "forbidden" leaves
// remote_read_denied.
func TestRootReadOfASharedFolder_TheListingDecidesAfterTheFolderDeltaAnswersNotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		listing error
		want    []IssueType
	}{
		{name: "listing succeeds"},
		{name: "listing forbidden", listing: graph.ErrForbidden, want: []IssueType{IssueRemoteReadDenied}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mock := &engineMockClient{
				folderDeltaFn: func(context.Context, driveid.ID, string, string) ([]graph.Item, string, error) {
					return nil, "", graph.ErrNotFound
				},
				listChildrenRecursiveFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
					return nil, tc.listing
				},
			}
			eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
				inputs.RemoteRootItemID = rootReadSharedFolderID
				inputs.RemoteRootDeltaCapable = true
			})

			issues := observeRootIssues(t, eng, rootReadPath{name: "shared folder root", sharedFolderRoot: true})

			var got []IssueType
			for i := range issues {
				got = append(got, issues[i].IssueType)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// Validates: R-2.10.4, R-2.15.2
//
// A drive root's full refresh whose later page OneDrive answers "not found"
// twice holds a position OneDrive refuses, not a root that is gone: it
// records no root issue and returns the error.
func TestRootReadFullRefresh_ALaterPageAnsweredNotFoundIsNotTheRoot(t *testing.T) {
	t.Parallel()

	const laterPage = "later-page"
	var laterPageReads atomic.Int32
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			if token != laterPage {
				return &graph.DeltaPage{NextLink: laterPage}, nil
			}
			laterPageReads.Add(1)
			return nil, &graph.GraphError{StatusCode: http.StatusNotFound, Err: graph.ErrNotFound}
		},
	}
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(*engineInputs) {})

	ctx := t.Context()
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, err = testEngineFlow(t, eng).observeAndCommitRemoteCurrentState(ctx, bl, true)
	require.ErrorIs(t, err, ErrDeltaExpired)
	assert.Equal(t, int32(2), laterPageReads.Load(), "the later page is asked for twice")

	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues, "a refused position is not a root that cannot be read")
}

// Validates: R-2.10.4
//
// An answer that is neither "not found" nor "forbidden" is not a root read
// boundary: it names no issue type, so the observation returns it.
func TestRootReadFailureIssueType_NamesOnlyNotFoundAndForbidden(t *testing.T) {
	t.Parallel()

	issueType, ok := rootReadFailureIssueType(graph.ErrNotFound)
	assert.True(t, ok)
	assert.Equal(t, IssueRemoteRootNotFound, issueType)

	issueType, ok = rootReadFailureIssueType(graph.ErrForbidden)
	assert.True(t, ok)
	assert.Equal(t, IssueRemoteReadDenied, issueType)

	for _, err := range []error{nil, graph.ErrThrottled, errors.New("connection reset")} {
		issueType, ok = rootReadFailureIssueType(err)
		assert.False(t, ok, "%v", err)
		assert.Empty(t, issueType, "%v", err)
	}
}

// Validates: R-2.4.8
//
// A removed shared folder's release runs over the work that cannot leave when
// the folder can no longer be read, whichever answer the root got.
func TestSharedFolderUnreadable_EitherRootAnswerMakesTheFolderUnreadable(t *testing.T) {
	t.Parallel()

	rootIssue := func(issueType IssueType, scopeKey ScopeKey) []ObservationIssueRow {
		return []ObservationIssueRow{{Path: "/", IssueType: issueType, ScopeKey: scopeKey}}
	}

	assert.True(t, sharedFolderUnreadable(rootIssue(IssueRemoteReadDenied, SKPermRemoteRead(""))))
	assert.True(t, sharedFolderUnreadable(rootIssue(IssueRemoteRootNotFound, SKPermRemoteRead(""))))
	assert.False(t, sharedFolderUnreadable(rootIssue(IssueRemoteReadDenied, SKPermRemoteRead("Docs"))),
		"a folder below the root that cannot be read leaves the rest readable")
	assert.False(t, sharedFolderUnreadable(nil))
}
