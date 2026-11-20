package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// newSilentTLSServer listens on loopback, accepts every connection and never
// writes to one, as a server that took the connection and then stopped
// answering does. It returns the listener's https URL, so a client's TLS
// handshake waits on it.
func newSilentTLSServer(t *testing.T) string {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var held []net.Conn
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	t.Cleanup(func() {
		assert.NoError(t, listener.Close())
		<-accepting
		for _, conn := range held {
			assert.NoError(t, conn.Close())
		}
	})

	return "https://" + listener.Addr().String()
}

// tlsHandshakeTimedOutError is the error graph returns for one item request
// whose TLS handshake the server never answers.
func tlsHandshakeTimedOutError(t *testing.T) error {
	t.Helper()

	transport := &http.Transport{TLSHandshakeTimeout: stalledRequestTimeout}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := graph.NewClient(newSilentTLSServer(t), &http.Client{Transport: transport}, stalledServerToken{},
		slog.New(slog.DiscardHandler), "silent-tls-server")
	require.NoError(t, err)

	_, err = client.GetItem(t.Context(), driveid.New(engineTestDriveID), "silent-item")
	require.Error(t, err)

	return fmt.Errorf("get the silent item: %w", err)
}

// unreachedGraphURL is the base URL of a graph client whose requests never
// leave: its transport or its token source fails each one first.
const unreachedGraphURL = "http://127.0.0.1:1"

// failingTokenSource hands graph err in place of a token.
type failingTokenSource struct {
	err error
}

func (s failingTokenSource) Token(context.Context) (string, error) {
	return "", s.err
}

// signInTimedOutError is the error graph returns for one item request whose
// token Microsoft sign-in never answered: the sign-in client's whole-request
// timeout against a token endpoint that took the connection and stopped
// answering, marked as the sign-in service's failure and wrapped by the token
// refresh as the token source returns it, then by the request, which was
// never sent.
func signInTimedOutError(t *testing.T) error {
	t.Helper()

	server, _ := newStalledServer(t)
	refresh, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, http.NoBody)
	require.NoError(t, err)
	resp, refreshErr := (&http.Client{Timeout: stalledRequestTimeout}).Do(refresh)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Error(t, refreshErr)

	signIn := failingTokenSource{err: fmt.Errorf("refreshing access token: %w", &graph.SignInRequestError{Err: refreshErr})}
	client, err := graph.NewClient(unreachedGraphURL, &http.Client{}, signIn, slog.New(slog.DiscardHandler), "silent-sign-in")
	require.NoError(t, err)

	_, err = client.GetItem(t.Context(), driveid.New(engineTestDriveID), "item-without-a-token")
	var signInErr *graph.SignInRequestError
	require.ErrorAs(t, err, &signInErr, "the failure is the sign-in service's")

	return fmt.Errorf("get the item: %w", err)
}

// readOp is the operation a failed read names.
const readOp = "read"

// keepaliveTimedOutReadError is a body read whose connection's keepalive
// found the server gone: the read fails with ETIMEDOUT.
func keepaliveTimedOutReadError() error {
	return fmt.Errorf("read the response body: %w",
		&net.OpError{Op: readOp, Net: "tcp", Err: os.NewSyscallError(readOp, syscall.ETIMEDOUT)})
}

// localReadTimedOutError is an upload whose read of the local file it sends
// timed out (a network filesystem answers ETIMEDOUT), read by a reader that
// returns the file's error unwrapped: net/http returns the body's read error
// as the request's, and the *url.Error the client wraps it in would then say
// it timed out. The upload reader of driveops wraps that error, so a sync
// upload does not build this shape; the classifier refuses it in case a
// reader ever does.
func localReadTimedOutError() error {
	read := &fs.PathError{Op: readOp, Path: "a.bin", Err: syscall.ETIMEDOUT}

	return fmt.Errorf("upload a chunk of a.bin: %w",
		&url.Error{Op: http.MethodPut, URL: unreachedGraphURL + "/upload-session", Err: read})
}

// timedOutRequest is one way a request ends without OneDrive's answer, and
// the error sync receives for it.
type timedOutRequest struct {
	name string
	err  func(t *testing.T) error
}

// timedOutRequests are the ways a request ends because OneDrive stopped
// answering: the transport's response-header, dial and whole-request
// timeouts and its TLS handshake timeout against a real silent server, a
// transfer the stall watchdog abandoned, an answer whose body brought nothing
// for the stall window, a body read whose connection's keepalive found the
// server gone, a request and a body read on an HTTP/2 connection whose ping
// went unanswered, and a token Microsoft sign-in never answered for, without
// which no request to OneDrive can be sent. The graph package's tests make
// the stalled body and the lost connection against real servers; here they
// are built from the types graph gives them.
func timedOutRequests() []timedOutRequest {
	kinds := stalledRequestKinds()
	requests := make([]timedOutRequest, 0, len(kinds)+7)
	for _, kind := range kinds {
		requests = append(requests, timedOutRequest{name: kind.name, err: func(t *testing.T) error {
			t.Helper()

			return stalledGraphRequestError(t, kind)
		}})
	}

	return append(requests,
		timedOutRequest{name: "TLS handshake timeout", err: tlsHandshakeTimedOutError},
		timedOutRequest{name: "stalled transfer", err: func(t *testing.T) error {
			t.Helper()

			// The shape the stall watchdog of graph returns: the cancel it
			// made is named in the text, not wrapped.
			return fmt.Errorf("download a.bin: %w: no data moved for 2m0s (context canceled)", graph.ErrTransferStalled)
		}},
		timedOutRequest{name: "answer whose body brought nothing for the stall window", err: func(t *testing.T) error {
			t.Helper()

			// The shape graph's response body returns: the closed body's read
			// error is named in the text, not wrapped.
			return fmt.Errorf("get the item: %w", &graph.ResponseReadError{
				Err: fmt.Errorf("%w: no data moved for 2m0s (http: read on closed response body)", graph.ErrResponseStalled),
			})
		}},
		timedOutRequest{name: "body read the keepalive gave up on", err: func(t *testing.T) error {
			t.Helper()

			return keepaliveTimedOutReadError()
		}},
		timedOutRequest{name: "request on an HTTP/2 connection whose ping went unanswered", err: func(t *testing.T) error {
			t.Helper()

			return fmt.Errorf("get the item: %w", graph.ErrConnectionLost)
		}},
		timedOutRequest{name: "body read on an HTTP/2 connection whose ping went unanswered", err: func(t *testing.T) error {
			t.Helper()

			return fmt.Errorf("list the folder: %w", &graph.ResponseReadError{Err: graph.ErrConnectionLost})
		}},
		timedOutRequest{name: "token Microsoft sign-in never answered for", err: signInTimedOutError},
	)
}

// completionsToBlockTheService feeds scope detection one failure with err per
// path and returns how many paths it takes to block the service scope, or 0
// when twice the window's threshold does not.
func completionsToBlockTheService(t *testing.T, err error) int {
	t.Helper()

	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	detection := NewScopeState(func() time.Time { return now }, slog.New(slog.DiscardHandler))
	for paths := 1; paths <= 2*ServiceWindowThreshold; paths++ {
		result := detection.UpdateScope(&ActionCompletion{
			Path:       fmt.Sprintf("file-%d", paths),
			Err:        err,
			HTTPStatus: ExtractHTTPStatus(err),
		})
		if result.Block {
			assert.Equal(t, SKService(), result.ScopeKey)
			return paths
		}
	}

	return 0
}

// Every way a request ends without OneDrive's answer, while its run goes on,
// is the evidence of an outage that an answered 504 is: the same scope,
// counted in the same window, kept by a trial that fails the same way. The
// file's own issue names the timeout.
//
// Validates: R-2.10.3, R-6.8.29
func TestClassifyResult_EveryTimedOutRequestIsOutageEvidenceLikeA504(t *testing.T) {
	t.Parallel()

	answered := &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError}
	gatewayTimeout := classifyResult(&ActionCompletion{HTTPStatus: http.StatusGatewayTimeout, Err: answered})
	require.Equal(t, SKService(), gatewayTimeout.ScopeEvidence)
	require.Equal(t, ServiceWindowThreshold, completionsToBlockTheService(t, answered))

	for _, request := range timedOutRequests() {
		t.Run(request.name, func(t *testing.T) {
			t.Parallel()

			err := request.err(t)
			got := classifyResult(&ActionCompletion{Err: err, HTTPStatus: ExtractHTTPStatus(err)})

			assert.Equal(t, resultRequeue, got.Class, "%v", err)
			assert.Equal(t, PersistRetryWork, got.Persistence)
			assert.Equal(t, IssueRequestTimeout, got.ConditionType, "the file's issue names the timeout")
			assert.Equal(t, gatewayTimeout.ScopeEvidence, got.ScopeEvidence, "the scope a 504 is evidence of")
			assert.Equal(t, gatewayTimeout.RunScopeDetection, got.RunScopeDetection)
			assert.Equal(t, gatewayTimeout.TrialHint, got.TrialHint, "a trial that times out is the scope's evidence")
			assert.Equal(t, ServiceWindowThreshold, completionsToBlockTheService(t, err), "the window a 504 crosses")
		})
	}
}

// stalledOutageEngine is a one-pass engine, so the run does not wait out a
// pause for its trial, whose every folder create goes to one server that
// never answers, with folders new local folders to create, and the count of
// the creates sent to that server.
func stalledOutageEngine(t *testing.T, folders int, logger *slog.Logger) (*testEngine, *atomic.Int64) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	server, _ := newStalledServer(t)
	stalled, requests := stalledGraphClient(t, server, &http.Client{Timeout: stalledRequestTimeout})
	client.createFolderFn = func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
		return stalled.CreateFolder(ctx, driveID, parentID, name)
	}
	eng, syncRoot := newTestEngineWithLogger(t, client, logger)
	eng.oneShotPassLimit = 1
	for i := range folders {
		require.NoError(t, os.Mkdir(filepath.Join(syncRoot, fmt.Sprintf("folder-%d", i)), 0o750))
	}

	return eng, requests
}

// Creates that time out against one silent server, as many as the service
// window holds, open one service scope, as many answered 5xx would: the one
// that crossed the window is its blocked work, and each of the others is
// timed retry work that records the scope as its evidence.
//
// Validates: R-2.10.3, R-6.8.29
func TestRunOnce_TimeoutsAgainstOneSilentServiceOpenOneOutageScope(t *testing.T) {
	t.Parallel()

	eng, requests := stalledOutageEngine(t, ServiceWindowThreshold, slog.New(slog.DiscardHandler))

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(ServiceWindowThreshold), requests.Load(), "errors: %v", report.Errors)

	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, scopes, 1, "the timeouts crossed the service window")
	assert.Equal(t, SKService(), scopes[0].Key)

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, ServiceWindowThreshold)
	blocked := 0
	for i := range rows {
		assert.Equal(t, SKService(), rows[i].ScopeKey, "each timeout is the outage's evidence: %s", rows[i].Path)
		if rows[i].Blocked {
			blocked++
		}
	}
	assert.Equal(t, 1, blocked, "the timeout that crossed the window is the scope's blocked work")
}

// One request that times out is below the service window: it is timed retry
// work and opens no scope.
//
// Validates: R-2.10.3, R-6.8.29
func TestRunOnce_OneTimedOutRequestOpensNoOutageScope(t *testing.T) {
	t.Parallel()

	eng, requests := stalledOutageEngine(t, 1, slog.New(slog.DiscardHandler))

	_, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), requests.Load())

	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scopes, "one timeout is below the service window")
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Blocked, "there is no scope to block under")
	assert.Positive(t, rows[0].NextRetryAt, "timed retry work has a due time")
}

// An outage of timeouts is said the way an outage of 5xx is: one line when
// the scope starts, one condition in the issues status lists, and one line
// in the watch condition log, not one per file.
//
// Validates: R-6.6.30, R-2.10.3
func TestRunOnce_AnOutageOfTimeoutsIsOneConditionNotOnePerFile(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := stalledOutageEngine(t, ServiceWindowThreshold+2, logger)

	_, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(logs.String(), "OneDrive is temporarily unavailable"), "the scope starts once")

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	groups := ProjectStoredConditionGroups(&snapshot)
	require.Len(t, groups, 1, "the issues name the outage once: %+v", groups)
	assert.Equal(t, ConditionServiceOutage, groups[0].ConditionKey)
	assert.Equal(t, SKService(), groups[0].ScopeKey)

	setupWatchEngine(t, eng)
	testWatchRuntime(t, eng).logWatchSummary(t.Context())
	assert.Equal(t, 1, strings.Count(logs.String(), "waiting for OneDrive to become available"),
		"the watch condition log names the outage in one line")
}

// A trial of the service scope whose request times out says the outage goes
// on, as a trial answered by a 504 does: the scope stays, the next trial is
// due a trial interval later (a minute: no Retry-After, ComputeTrialInterval),
// and the trial's work stays blocked under it for that trial.
//
// Validates: R-2.10.5, R-6.8.29
func TestEngineFlow_ProcessTrialDecision_ATimedOutTrialKeepsTheOutageScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	setTestBlockScope(t, eng, &BlockScope{Key: SKService(), NextTrialAt: eng.nowFunc()})
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "dir",
		ActionType:   ActionFolderCreate,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 1,
	}))
	current := rt.sched.graph.Add(&Action{Type: ActionFolderCreate, Path: "dir", DriveID: eng.driveID}, 1, nil)
	require.NotNil(t, current)

	timedOut := stalledGraphRequestError(t, clientTimeoutKind())
	completion := &ActionCompletion{
		ActionID:      current.ID,
		Path:          "dir",
		ActionType:    ActionFolderCreate,
		DriveID:       eng.driveID,
		Err:           timedOut,
		ErrMsg:        timedOut.Error(),
		IsTrial:       true,
		TrialScopeKey: SKService(),
	}
	decision := classifyResult(completion)
	_, err := flow.applyTrialCompletionDecision(t.Context(), SKService(), &decision, current, completion, nil)
	require.NoError(t, err)

	scope, active := rt.scopes.lookupActiveScope(SKService())
	require.True(t, active, "the outage goes on")
	assert.Equal(t, eng.nowFunc().Add(time.Minute), scope.NextTrialAt, "the next trial is a trial interval away")
	rows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, rows, 1)
	assert.Equal(t, SKService(), rows[0].ScopeKey)
	assert.True(t, rows[0].Blocked, "the trial's work waits for the next trial")
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// servicePathsInWindow is the count of paths the service window of flow's
// scope detection holds now.
func servicePathsInWindow(flow *engineFlow) int {
	count, _ := flow.scopes.state.WindowPathCountForTest(SKService(), flow.deps.now())

	return count
}

// Only the service's silence is outage evidence: a timeout the end of the
// run landed on, a timeout of any shape that lands once the run has ended,
// the run's own deadline, a cancel, of the run or of something narrower, and
// a read of the local file an upload sends that timed out add nothing to the
// service window, which one timeout of the live run then starts.
//
// Validates: R-6.8.29, R-2.10.3
func TestEngineFlow_OnlyTheServicesSilenceIsOutageEvidence(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	timedOut := stalledGraphRequestError(t, clientTimeoutKind())
	notEvidence := []*ActionCompletion{
		{Err: timedOut, RunEnded: true},
		{Err: keepaliveTimedOutReadError(), RunEnded: true},
		{Err: fmt.Errorf("create folder: %w", context.DeadlineExceeded), RunEnded: true},
		{Err: fmt.Errorf("create folder: %w", context.Canceled), RunEnded: true},
		{Err: fmt.Errorf("create folder: %w", context.Canceled)},
		{Err: localReadTimedOutError()},
	}
	apply := func(i int, completion *ActionCompletion) {
		completion.Path = fmt.Sprintf("dir-%d", i)
		completion.ActionType = ActionFolderCreate
		decision := classifyResult(completion)
		_, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, completion, nil)
		require.NoError(t, err)
	}

	for i, completion := range notEvidence {
		apply(i, completion)
	}
	assert.Zero(t, servicePathsInWindow(flow), "none of them is the service's silence")
	localRead := classifyResult(&ActionCompletion{Err: localReadTimedOutError()})
	assert.NotEqual(t, IssueRequestTimeout, localRead.ConditionType, "a local read is no request")

	apply(len(notEvidence), &ActionCompletion{Err: timedOut})
	assert.Equal(t, 1, servicePathsInWindow(flow), "the live run's timeout is")
}

// workerTestCommitBound is the commit bound of a pool whose store stops
// answering: short, so the test does not wait out OutcomeCommitTimeout.
const workerTestCommitBound = 20 * time.Millisecond

// wedgedCommitCompletion runs a folder create whose request succeeds and
// whose outcome commit finds the store's one connection taken until the
// commit's bound has expired, and returns the action's completion.
func wedgedCommitCompletion(t *testing.T) ActionCompletion {
	t.Helper()

	cfg, store, _ := newWorkerTestSetup(t)
	var wedge atomic.Pointer[sql.Conn]
	cfg.SetItemsForTest(&workerMockItemClient{
		createFolderFn: func(ctx context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			conn, err := store.RawDBForTest().Conn(ctx)
			if err != nil {
				return nil, fmt.Errorf("take the store's connection: %w", err)
			}
			wedge.Store(conn)

			return &graph.Item{ID: "created-" + name, Name: name, ParentID: "root-id"}, nil
		},
	})
	pool := NewWorkerPool(cfg, nil, store, synctest.TestLogger(t), 1)
	pool.SetOutcomeCommitTimeoutForTest(workerTestCommitBound)

	pool.ExecuteActionForTest(t.Context(), &TrackedAction{ID: 1, Action: Action{
		Type:       ActionFolderCreate,
		Path:       "Projects",
		DriveID:    driveid.New(engineTestDriveID),
		CreateSide: CreateRemote,
		View:       &PathView{Path: "Projects"},
	}})
	conn := wedge.Load()
	require.NotNil(t, conn, "the create ran and took the connection")
	require.NoError(t, conn.Close())

	return receiveWithin(t, pool.Completions(), "the completion of the action whose commit failed")
}

// lastJoinedCause follows err as the sync console does to print a failure's
// reason: through each wrap and, at an error that joins several, through the
// last one it joins, to the error that wraps nothing.
func lastJoinedCause(err error) error {
	for {
		next := errors.Unwrap(err)
		var joined interface{ Unwrap() []error }
		if next == nil && errors.As(err, &joined) {
			if all := joined.Unwrap(); len(all) > 0 {
				next = all[len(all)-1]
			}
		}
		if next == nil {
			return err
		}
		err = next
	}
}

// A store that does not answer within the bound of a completed action's
// commit is a store failure, the class any other failed commit has, also
// once the run has ended: not a request that timed out, not the echo of a
// stop, and no evidence of an outage of OneDrive however many commits it
// fails. The reason a user reads names the database.
//
// Validates: R-2.10.3, R-6.3.9, R-6.8.29
func TestWorkerPool_ACommitThatOutlastsItsBoundIsAStoreFailure(t *testing.T) {
	t.Parallel()

	wedged := wedgedCommitCompletion(t)
	require.False(t, wedged.Success)
	require.ErrorIs(t, wedged.Err, context.DeadlineExceeded, "the commit gave up at its bound")
	require.ErrorIs(t, wedged.Err, ErrStoreAccessTimedOut)
	assert.Equal(t, ErrStoreAccessTimedOut.Error(), lastJoinedCause(wedged.Err).Error(),
		"the console prints the sentence of the database that did not answer")

	decision := classifyResult(&wedged)
	assert.NotEqual(t, IssueRequestTimeout, decision.ConditionType, "a store is not a request: %v", wedged.Err)
	failedCommit := classifyResult(&ActionCompletion{Err: errors.New("sync: commit outcome: disk I/O error")})
	assert.Equal(t, failedCommit, decision, "the class of a failed commit")
	whileStopping := wedged
	whileStopping.RunEnded = true
	assert.Equal(t, failedCommit, classifyResult(&whileStopping), "a store's own bound is no echo of the stop")

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	for i := range ServiceWindowThreshold {
		completion := wedged
		completion.Path = fmt.Sprintf("folder-%d", i)
		completion.ActionID = int64(i + 1)
		perFile := classifyResult(&completion)
		_, err := flow.applyNormalCompletionDecision(t.Context(), &perFile, nil, &completion, nil)
		require.NoError(t, err)
	}
	assert.False(t, rt.scopes.hasActiveScope(SKService()), "a store that stopped answering is no outage of OneDrive")
}

// unansweringRecordStore is a pending-create store whose record of a created
// item never answers: RecordPendingCreate waits until its context ends and
// returns that context's error, as database/sql does. It waits only on a
// context that ends within bound and fails at once on any other, so a record
// that does not take the executor's store bound fails the test quickly
// instead of waiting out OutcomeCommitTimeout.
type unansweringRecordStore struct {
	PendingCreateStore
	bound time.Duration
}

func (s unansweringRecordStore) RecordPendingCreate(ctx context.Context, _ *PendingCreate) error {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return errors.New("the record's context has no deadline")
	}
	if left := time.Until(deadline); left > s.bound {
		return fmt.Errorf("the record's context ends in %s, past the executor's store bound of %s", left, s.bound)
	}
	<-ctx.Done()

	return fmt.Errorf("sync: record pending create: %w", ctx.Err())
}

// A store that does not answer within the bound of the record of a created
// item is a store failure, as a commit that outlasted its bound is: the
// record runs on the executor's store bound and gives up there, the upload's
// completion is not a request that timed out, and it adds nothing to the
// service window.
//
// Validates: R-2.10.3, R-6.3.9, R-6.8.29
func TestUploadNewFile_ARecordThatOutlastsItsBoundIsAStoreFailure(t *testing.T) {
	t.Parallel()

	created := &graph.Item{ID: "new-item", ETag: `"c,1"`}
	ul := &creatingTestUploader{create: func(_ context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
		if hookErr := onCreated(created); hookErr != nil {
			return nil, &graph.CreatedItemError{Item: created, Err: hookErr}
		}

		return created, nil
	}}
	e, store, syncRoot := newPendingCreateTestExecutor(t, ul, synctest.TestLogger(t))
	e.SetPendingCreates(unansweringRecordStore{PendingCreateStore: store, bound: workerTestCommitBound})
	e.SetStoreAccessTimeoutForTest(workerTestCommitBound)
	local := writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

	_, uploadErr := e.UploadNewFileForTest(t.Context(), &Action{Type: ActionUpload, Path: "doc.txt"},
		driveid.New(testDriveID), GraphRootID, "doc.txt", local)
	require.ErrorIs(t, uploadErr, context.DeadlineExceeded, "the record gave up at its bound")
	require.ErrorIs(t, uploadErr, ErrStoreAccessTimedOut)

	completion := ActionCompletion{Err: uploadErr, Path: "doc.txt", ActionType: ActionUpload}
	decision := classifyResult(&completion)
	assert.NotEqual(t, IssueRequestTimeout, decision.ConditionType, "a store is not a request: %v", uploadErr)

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	_, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, &completion, nil)
	require.NoError(t, err)
	assert.Zero(t, servicePathsInWindow(flow), "a store that stopped answering is no outage of OneDrive")
}

// A stopping run that receives an action whose outcome commit outlasted its
// bound records that failure as retry work on the stop's own bound, so the
// next sync tries the action again and there is no loss to say. Only when the
// store does not answer the stop's record either is the loss said, once, and
// the stop ends with the store failure.
//
// Validates: R-2.5.3, R-2.8.3, R-6.6.21
func TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid(t *testing.T) {
	t.Parallel()

	wedged := wedgedCommitCompletion(t)
	wedged.RunEnded = true

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)
	require.NoError(t, flow.processShutdownCompletion(stoppedContext(t), &wedged, nil))
	require.NoError(t, flow.endStopDrain())
	assert.Equal(t, []string{wedged.Path}, retryWorkPaths(t, eng.baseline), "the stop records the failure")
	assert.Zero(t, countLevel(logs.String(), "WARN"), "nothing is lost: %s", logs.String())

	logger, logs = newCapturedLogger()
	eng, _ = newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow = testEngineFlow(t, eng)
	flow.stopBound = workerTestCommitBound
	stopped := stoppedContext(t)
	flow.beginStopDrain()
	release := holdStoreConnection(t, eng.baseline)
	require.NoError(t, flow.processShutdownCompletion(stopped, &wedged, nil))
	release()
	require.ErrorIs(t, flow.endStopDrain(), ErrStoreAccessTimedOut, "the stop ends with the store failure")
	assert.Empty(t, retryWorkPaths(t, eng.baseline))
	assert.Equal(t, 1, countLevel(logs.String(), "WARN"), "the lost record is said once: %s", logs.String())
	assert.Contains(t, logs.String(), "The stop's time ran out before 1 result was recorded")
}
