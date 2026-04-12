package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
)

type stubResolveDeleteStore struct {
	approveErr error
	closeErr   error
	closeCalls int
}

func (s *stubResolveDeleteStore) ApproveHeldDeletes(context.Context) error {
	return s.approveErr
}

func (s *stubResolveDeleteStore) Close(context.Context) error {
	s.closeCalls++
	return s.closeErr
}

type stubResolveConflictQueueStore struct {
	conflicts        []syncstore.ConflictRecord
	allConflicts     []syncstore.ConflictRecord
	requestResult    syncstore.ConflictRequestResult
	requestErr       error
	requestCalls     int
	requestedIDs     []string
	requestedActions []string
}

func (s *stubResolveConflictQueueStore) ListConflicts(context.Context) ([]syncstore.ConflictRecord, error) {
	return s.conflicts, nil
}

func (s *stubResolveConflictQueueStore) ListAllConflicts(context.Context) ([]syncstore.ConflictRecord, error) {
	return s.allConflicts, nil
}

func (s *stubResolveConflictQueueStore) RequestConflictResolution(
	_ context.Context,
	id string,
	resolution string,
) (syncstore.ConflictRequestResult, error) {
	s.requestCalls++
	s.requestedIDs = append(s.requestedIDs, id)
	s.requestedActions = append(s.requestedActions, resolution)
	return s.requestResult, s.requestErr
}

func (s *stubResolveConflictQueueStore) Close(context.Context) error {
	return nil
}

func TestResolveService_RunApproveDeletesWithStore_CloseFailureSuppressesSuccessOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	closeErr := errors.New("db close failed")
	store := &stubResolveDeleteStore{closeErr: closeErr}

	err := runApproveDeletesWithStore(t.Context(), &CLIContext{OutputWriter: &out}, store)
	require.Error(t, err)
	require.ErrorIs(t, err, closeErr)
	assert.Contains(t, err.Error(), "close sync store")
	assert.Empty(t, out.String())
	assert.Equal(t, 1, store.closeCalls)
}

func TestResolveService_RunApproveDeletesWithStore_JoinsApproveAndCloseErrors(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	approveErr := errors.New("approve failed")
	closeErr := errors.New("db close failed")
	store := &stubResolveDeleteStore{
		approveErr: approveErr,
		closeErr:   closeErr,
	}

	err := runApproveDeletesWithStore(t.Context(), &CLIContext{OutputWriter: &out}, store)
	require.Error(t, err)
	require.ErrorIs(t, err, approveErr)
	require.ErrorIs(t, err, closeErr)
	assert.Contains(t, err.Error(), "approve held deletes")
	assert.Contains(t, err.Error(), "close sync store")
	assert.Empty(t, out.String())
	assert.Equal(t, 1, store.closeCalls)
}

func TestResolveService_QueueEachConflictResolution_DryRun(t *testing.T) {
	t.Parallel()

	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status, Logger: slog.New(slog.DiscardHandler)}
	store := &stubResolveConflictQueueStore{
		conflicts: []syncstore.ConflictRecord{{ID: "id-1", Path: "/foo.txt"}},
	}

	require.NoError(t, queueEachConflictResolution(t.Context(), cc, store, store.conflicts, syncstore.ResolutionKeepLocal, true))
	assert.Contains(t, status.String(), "Would resolve /foo.txt as keep_local")
	assert.Zero(t, store.requestCalls)
}

func TestResolveService_QueueSingleConflictResolution_AlreadyResolvedIsReplaySafe(t *testing.T) {
	t.Parallel()

	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status, Logger: slog.New(slog.DiscardHandler)}
	store := &stubResolveConflictQueueStore{
		allConflicts: []syncstore.ConflictRecord{
			{ID: "id-1", Path: "/foo.txt", Resolution: syncstore.ResolutionKeepBoth},
		},
	}

	require.NoError(t, queueSingleConflictResolution(t.Context(), cc, store, "/foo.txt", syncstore.ResolutionKeepLocal, false))
	assert.Contains(t, status.String(), "already resolved as keep_both")
	assert.Zero(t, store.requestCalls)
}

func TestFindSelectedConflict_AmbiguousPrefix(t *testing.T) {
	t.Parallel()

	conflicts := []syncstore.ConflictRecord{
		{ID: "aabb1122-dead-beef-cafe-000000000001", Path: "/foo/bar.txt"},
		{ID: "aabb1122-dead-beef-cafe-000000000002", Path: "/baz/qux.txt"},
	}

	_, _, err := findSelectedConflict(conflicts, "aabb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"aabb"`)
}

// Validates: R-2.3.12
func TestResolveService_RequestConflictResolutionConcurrentCLIsLastWriteWinsWhileQueued(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	canonicalID := driveid.MustCanonicalID("personal:concurrent-conflicts@example.com")
	store, err := syncstore.NewSyncStore(t.Context(), config.DriveStatePath(canonicalID), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	_, err = store.DB().ExecContext(t.Context(), `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
			('conflict-concurrent-cli', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`,
		canonicalID.String(),
	)
	require.NoError(t, err)

	cc := &CLIContext{
		Cfg: &config.ResolvedDrive{CanonicalID: canonicalID},
	}

	const requestCount = 16
	statuses := make(chan syncstore.ConflictRequestStatus, requestCount)
	errorsCh := make(chan error, requestCount)
	start := make(chan struct{})
	ctx := t.Context()
	var wg sync.WaitGroup

	for i := range requestCount {
		strategy := syncstore.ResolutionKeepLocal
		if i%2 == 1 {
			strategy = syncstore.ResolutionKeepRemote
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, requestErr := requestConflictResolution(
				ctx,
				cc,
				store,
				"conflict-concurrent-cli",
				strategy,
			)
			if requestErr != nil {
				errorsCh <- requestErr
				return
			}
			statuses <- result.Status
		}()
	}

	close(start)
	wg.Wait()
	close(statuses)
	close(errorsCh)

	require.Empty(t, errorsCh)

	queued := 0
	for status := range statuses {
		switch status {
		case syncstore.ConflictRequestQueued:
			queued++
		case syncstore.ConflictRequestAlreadyQueued:
		case syncstore.ConflictRequestAlreadyApplying, syncstore.ConflictRequestAlreadyResolved:
			require.Failf(t, "unexpected terminal conflict request status", "status=%s", status)
		default:
			require.Failf(t, "unexpected conflict request status", "status=%s", status)
		}
	}
	assert.GreaterOrEqual(t, queued, 1)

	conflict, err := store.GetConflictRequest(t.Context(), "conflict-concurrent-cli")
	require.NoError(t, err)
	assert.Equal(t, syncstore.ConflictStateQueued, conflict.State)
	assert.Contains(t, []string{syncstore.ResolutionKeepLocal, syncstore.ResolutionKeepRemote}, conflict.RequestedResolution)
}
