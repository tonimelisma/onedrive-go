package sync

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// unreachedRequest is one way a request does not reach OneDrive, built as
// net/http returns it.
type unreachedRequest struct {
	name string
	err  error
}

// connectionRefusedError is a create whose dial the network refused.
func connectionRefusedError() error {
	return dialFailedError(syscall.ECONNREFUSED)
}

// dialFailedError is a request whose dial failed with errno: a *url.Error
// around a *net.OpError around the connect's *os.SyscallError.
func dialFailedError(errno syscall.Errno) error {
	return fmt.Errorf("create folder: %w", &url.Error{Op: http.MethodPost, URL: unreachedGraphURL, Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno),
	}})
}

// unreachedRequests are the ways a request does not reach OneDrive without
// timing out: a refused, unroutable or reset connection, and a name the
// resolver does not find.
func unreachedRequests() []unreachedRequest {
	return []unreachedRequest{
		{name: "connection refused", err: connectionRefusedError()},
		{name: "no route to host", err: dialFailedError(syscall.EHOSTUNREACH)},
		{name: "network unreachable", err: dialFailedError(syscall.ENETUNREACH)},
		{name: "connection reset", err: fmt.Errorf("list children: %w", &url.Error{
			Op: http.MethodGet, URL: unreachedGraphURL,
			Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
		})},
		{name: "name not found", err: fmt.Errorf("list children: %w", &url.Error{
			Op: http.MethodGet, URL: unreachedGraphURL,
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
				Err: "no such host", Name: "graph.microsoft.com", IsNotFound: true,
			}},
		})},
	}
}

// Every request that did not reach OneDrive, while its run goes on, is the
// outage evidence an answered 504 is: the same scope, counted in the same
// window, and never an issue that names the file. Once the run has ended it
// is retry work but no evidence.
//
// Validates: R-2.10.3
func TestClassifyResult_EveryUnreachedRequestIsOutageEvidenceLikeA504(t *testing.T) {
	t.Parallel()

	answered := &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError}
	gatewayTimeout := classifyResult(&syncdomain.ActionCompletion{HTTPStatus: http.StatusGatewayTimeout, Err: answered})
	require.Equal(t, syncdomain.SKService(), gatewayTimeout.ScopeEvidence)

	for _, request := range unreachedRequests() {
		t.Run(request.name, func(t *testing.T) {
			t.Parallel()

			require.True(t, syncdomain.IsServiceUnreached(request.err))
			got := classifyResult(&syncdomain.ActionCompletion{Err: request.err, ActionType: syncdomain.ActionFolderCreate, Path: "docs"})

			assert.Equal(t, resultRequeue, got.Class, "%v", request.err)
			assert.Equal(t, syncdomain.PersistRetryWork, got.Persistence)
			assert.Equal(t, syncdomain.IssueServiceOutage, got.ConditionType, "the outage, not the file")
			assert.Equal(t, syncdomain.ConditionServiceOutage, got.ConditionKey)
			assert.Equal(t, gatewayTimeout.ScopeEvidence, got.ScopeEvidence, "the scope a 504 is evidence of")
			assert.Equal(t, gatewayTimeout.RunScopeDetection, got.RunScopeDetection)
			assert.Equal(t, gatewayTimeout.TrialHint, got.TrialHint)
			assert.Equal(t, syncsched.ServiceWindowThreshold, completionsToBlockTheService(t, request.err), "the window a 504 crosses")

			stopped := classifyResult(&syncdomain.ActionCompletion{Err: request.err, RunEnded: true})
			assert.Equal(t, resultRequeue, stopped.Class)
			assert.Equal(t, syncdomain.ScopeKey{}, stopped.ScopeEvidence, "a stop is never the service's silence")
			assert.False(t, stopped.RunScopeDetection)
		})
	}
}

// refusedOutageEngine is a one-pass engine whose every folder create is
// refused by the network, with folders new local folders to create, and the
// count of the creates it tried.
func refusedOutageEngine(t *testing.T, folders int) (*testEngine, *atomic.Int64) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	var creates atomic.Int64
	client.CreateFolderFn = func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
		creates.Add(1)
		return nil, connectionRefusedError()
	}
	eng, syncRoot := newTestEngineWithLogger(t, client, slog.New(slog.DiscardHandler))
	eng.hooks.oneShotPassLimit = 1
	for i := range folders {
		require.NoError(t, os.Mkdir(filepath.Join(syncRoot, fmt.Sprintf("folder-%d", i)), 0o750))
	}

	return eng, &creates
}

// Creates the network refuses, as many as the service window holds, open one
// service scope and list one condition, the outage, and no issue per file.
//
// Validates: R-2.10.3
func TestRunOnce_RefusedConnectionsOpenOneOutageScopeAndNoFileIssue(t *testing.T) {
	t.Parallel()

	eng, creates := refusedOutageEngine(t, syncsched.ServiceWindowThreshold)

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(syncsched.ServiceWindowThreshold), creates.Load())

	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, scopes, 1, "the refusals crossed the service window")
	assert.Equal(t, syncdomain.SKService(), scopes[0].Key)

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	groups := syncstore.ProjectStoredConditionGroups(&snapshot)
	require.Len(t, groups, 1, "the issues name the outage once: %+v", groups)
	assert.Equal(t, syncdomain.ConditionServiceOutage, groups[0].ConditionKey)
}

// One refused create is below the service window: it is timed retry work and
// opens no scope.
//
// Validates: R-2.10.3
func TestRunOnce_OneRefusedConnectionOpensNoOutageScope(t *testing.T) {
	t.Parallel()

	eng, creates := refusedOutageEngine(t, 1)

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), creates.Load())
	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scopes, "one refusal is below the service window")
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Blocked, "there is no scope to block under")
	assert.Positive(t, rows[0].NextRetryAt, "timed retry work has a due time")
}
