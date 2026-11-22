package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// localSource is the local side of the drive as a run observes it: the sync
// tree and the folder identity it must match, how local observers are built,
// and the one commit of a full local scan. The identity guards itself; nothing
// else on it changes after construction.
type localSource struct {
	syncTree *synctree.Root
	store    *syncstore.SyncStore
	// identity is the one owner of the identity the sync folder must match;
	// the executor and every observer read it, never a copy.
	identity     *syncdomain.SyncRootIdentity
	checkWorkers int
	logger       *slog.Logger
	// clock is the engine's real clock, which every observer the engine
	// builds runs on.
	clock syncdomain.RealClock
	hooks syncobserve.Hooks
}

// localSource is the engine's local side as it is now. A run takes it once
// (flowInputs).
func (e *Engine) localSource() *localSource {
	return &localSource{
		syncTree:     e.syncTree,
		store:        e.baseline,
		identity:     e.syncRootIdentity,
		checkWorkers: e.checkWorkers,
		logger:       e.logger,
		clock:        e.wallClock,
		hooks:        e.hooks.observe,
	}
}

// observer builds a local observer on the engine's real clock, with the
// engine's observer test seams. logger is the run's.
func (s *localSource) observer(bl syncdomain.BaselineView, logger *slog.Logger) *syncobserve.LocalObserver {
	obs := syncobserve.NewLocalObserver(bl, logger, s.checkWorkers)
	obs.SetClock(s.clock)
	obs.SetHooks(s.hooks)

	return obs
}

// commitSnapshot commits one full local scan in one transaction (its
// findings, its rows and the mounts it crossed) and, while a primary engine
// still has to bind its sync folder, records the folder's identity in the
// same transaction. Binding first writes a fresh token on the folder (an
// extended attribute) so later starts recognize it by name rather than by
// inode; a filesystem without extended attributes binds on inode and fsid
// alone. Mark-and-bind is serialized: two commits racing would otherwise
// write two tokens, and the folder would keep the one the store did not
// record. It reports what the commit changed and marks nothing itself.
func (s *localSource) commitSnapshot(
	ctx context.Context, commit syncstore.LocalSnapshotCommit,
) (syncstore.LocalSnapshotCommitResult, error) {
	var result syncstore.LocalSnapshotCommitResult
	ran, err := s.identity.CompleteBinding(func(expected *synctree.RootIdentity) (*synctree.RootIdentity, error) {
		var bindErr error
		var bound *synctree.RootIdentity
		result, bound, bindErr = s.commitSnapshotBinding(ctx, &commit, expected)
		return bound, bindErr
	})
	if !ran {
		return s.commitSnapshotUnbound(ctx, &commit)
	}
	return result, err
}

// commitSnapshotBinding is commitSnapshot while a binding is pending: it
// marks the folder, commits with the binding, and returns the identity the
// store recorded.
func (s *localSource) commitSnapshotBinding(
	ctx context.Context, commit *syncstore.LocalSnapshotCommit, expected *synctree.RootIdentity,
) (syncstore.LocalSnapshotCommitResult, *synctree.RootIdentity, error) {
	if expected == nil {
		return syncstore.LocalSnapshotCommitResult{}, nil, fmt.Errorf("sync: binding sync folder: no identity was taken at startup")
	}
	bind := *expected
	bind.Token = ""
	token, err := s.syncTree.MarkRoot()
	switch {
	case err == nil:
		bind.Token = token
	case errors.Is(err, synctree.ErrRootMarkUnsupported):
		s.logger.Debug("sync folder cannot carry a bind token; binding on inode and filesystem id",
			slog.Any("error", err))
	default:
		return syncstore.LocalSnapshotCommitResult{}, nil, fmt.Errorf("sync: binding sync folder: %w", err)
	}

	commit.Bind = &bind
	result, err := s.store.CommitLocalSnapshot(ctx, commit)
	if err != nil {
		return result, nil, fmt.Errorf("sync: replacing local_state snapshot: %w", err)
	}
	bound := result.Bound
	if bound != nil && bound.Token != bind.Token && bound.Token != "" {
		// Another binding won in the store; the folder must carry the token
		// the store recorded, not the one just written.
		if markErr := s.syncTree.SetRootMark(bound.Token); markErr != nil {
			return result, nil, fmt.Errorf("sync: restoring the recorded sync folder marker: %w", markErr)
		}
	}
	if bound == nil {
		bound = &bind
	}
	return result, bound, nil
}

func (s *localSource) commitSnapshotUnbound(
	ctx context.Context, commit *syncstore.LocalSnapshotCommit,
) (syncstore.LocalSnapshotCommitResult, error) {
	result, err := s.store.CommitLocalSnapshot(ctx, commit)
	if err != nil {
		return result, fmt.Errorf("sync: replacing local_state snapshot: %w", err)
	}
	return result, nil
}
