package syncrecovery

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type fakeStore struct {
	resetDownloadingCalled bool
	listDeletingCalled     bool
	finalizeDeleted        []synctypes.RecoveryCandidate
	finalizePending        []synctypes.RecoveryCandidate
	candidates             []synctypes.RecoveryCandidate
	resetDownloadingErr    error
	listDeletingErr        error
	finalizeErr            error
}

func (s *fakeStore) ResetDownloadingStates(_ context.Context, _ func(int) time.Duration) error {
	s.resetDownloadingCalled = true
	return s.resetDownloadingErr
}

func (s *fakeStore) ListDeletingCandidates(_ context.Context) ([]synctypes.RecoveryCandidate, error) {
	s.listDeletingCalled = true
	return s.candidates, s.listDeletingErr
}

func (s *fakeStore) FinalizeDeletingStates(
	_ context.Context,
	deleted []synctypes.RecoveryCandidate,
	pending []synctypes.RecoveryCandidate,
	_ func(int) time.Duration,
) error {
	s.finalizeDeleted = append([]synctypes.RecoveryCandidate(nil), deleted...)
	s.finalizePending = append([]synctypes.RecoveryCandidate(nil), pending...)
	return s.finalizeErr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// Validates: R-2.5.1
func TestResetInProgressStates_PartitionsDeletingCandidates(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "exists.txt"), []byte("x"), 0o600))

	tree, err := synctree.Open(rootDir)
	require.NoError(t, err)

	store := &fakeStore{
		candidates: []synctypes.RecoveryCandidate{
			{DriveID: "d1", ItemID: "gone", Path: "gone.txt"},
			{DriveID: "d1", ItemID: "exists", Path: "/exists.txt"},
		},
	}

	require.NoError(t, ResetInProgressStates(t.Context(), store, tree, func(int) time.Duration { return time.Second }, discardLogger()))

	assert.True(t, store.resetDownloadingCalled)
	assert.True(t, store.listDeletingCalled)
	require.Len(t, store.finalizeDeleted, 1)
	require.Len(t, store.finalizePending, 1)
	assert.Equal(t, "gone", store.finalizeDeleted[0].ItemID)
	assert.Equal(t, "exists", store.finalizePending[0].ItemID)
}

// Validates: R-2.5.1
func TestResetInProgressStates_StatErrorRetriesDelete(t *testing.T) {
	t.Parallel()

	tree, err := synctree.Open(t.TempDir())
	require.NoError(t, err)

	store := &fakeStore{
		candidates: []synctypes.RecoveryCandidate{
			{DriveID: "d1", ItemID: "bad", Path: "/../bad.txt"},
		},
	}

	require.NoError(t, ResetInProgressStates(t.Context(), store, tree, func(int) time.Duration { return time.Second }, discardLogger()))

	assert.Empty(t, store.finalizeDeleted)
	require.Len(t, store.finalizePending, 1)
	assert.Equal(t, "bad", store.finalizePending[0].ItemID)
}

// Validates: R-2.5.1
func TestResetInProgressStates_ResetDownloadingError(t *testing.T) {
	t.Parallel()

	tree, err := synctree.Open(t.TempDir())
	require.NoError(t, err)

	store := &fakeStore{resetDownloadingErr: errors.New("boom")}

	err = ResetInProgressStates(t.Context(), store, tree, func(int) time.Duration { return time.Second }, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reset downloading states")
}

// Validates: R-2.5.1
func TestResetInProgressStates_ListDeletingCandidatesError(t *testing.T) {
	t.Parallel()

	tree, err := synctree.Open(t.TempDir())
	require.NoError(t, err)

	store := &fakeStore{listDeletingErr: errors.New("boom")}

	err = ResetInProgressStates(t.Context(), store, tree, func(int) time.Duration { return time.Second }, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list deleting candidates")
}

// Validates: R-2.5.1
func TestResetInProgressStates_FinalizeDeletingStatesError(t *testing.T) {
	t.Parallel()

	tree, err := synctree.Open(t.TempDir())
	require.NoError(t, err)

	store := &fakeStore{
		finalizeErr: errors.New("boom"),
		candidates: []synctypes.RecoveryCandidate{
			{DriveID: "d1", ItemID: "gone", Path: "gone.txt"},
		},
	}

	err = ResetInProgressStates(t.Context(), store, tree, func(int) time.Duration { return time.Second }, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finalize deleting states")
}
