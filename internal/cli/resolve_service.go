package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type resolveService struct {
	cc *CLIContext
}

type resolveDeleteStore interface {
	ApproveHeldDeletes(context.Context) error
	Close(context.Context) error
}

type resolveConflictStore interface {
	ListConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	RequestConflictResolution(context.Context, string, string) (syncstore.ConflictRequestResult, error)
	Close(context.Context) error
}

func newResolveService(cc *CLIContext) *resolveService {
	return &resolveService{cc: cc}
}

func (s *resolveService) runApproveDeletes(ctx context.Context) error {
	_, err := routeDurableIntent(
		ctx,
		func(ctx context.Context) (struct{}, error) {
			return struct{}{}, s.approveDeletesDirect(ctx)
		},
		func(ctx context.Context, client *controlSocketClient) (struct{}, error) {
			if err := client.approveHeldDeletes(ctx, s.cc.Cfg.CanonicalID); err != nil {
				return struct{}{}, fmt.Errorf("approve held deletes via daemon: %w", err)
			}
			return struct{}{}, writeln(s.cc.Output(), resolveApproveDeletesSuccess)
		},
	)
	return err
}

func (s *resolveService) approveDeletesDirect(ctx context.Context) error {
	store, err := s.openWritableStore(ctx)
	if err != nil {
		return err
	}

	return s.runApproveDeletesWithStore(ctx, store)
}

func (s *resolveService) runApproveDeletesWithStore(ctx context.Context, store resolveDeleteStore) (err error) {
	storeClosed := false
	defer func() {
		if storeClosed {
			return
		}

		if closeErr := store.Close(ctx); closeErr != nil {
			closeErr = fmt.Errorf("close sync store: %w", closeErr)
			if err == nil {
				err = closeErr
				return
			}

			err = errors.Join(err, closeErr)
		}
	}()

	if err := store.ApproveHeldDeletes(ctx); err != nil {
		return fmt.Errorf("approve held deletes: %w", err)
	}

	storeClosed = true
	if err := store.Close(ctx); err != nil {
		return fmt.Errorf("close sync store: %w", err)
	}

	return writeln(s.cc.Output(), resolveApproveDeletesSuccess)
}

const resolveApproveDeletesSuccess = "Approved held deletes for this drive. The next sync pass will execute matching approved deletes."

func (s *resolveService) runResolve(
	ctx context.Context,
	args []string,
	resolution string,
	resolveAll bool,
	dryRun bool,
) error {
	store, err := s.openWritableStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := store.Close(ctx); closeErr != nil {
			s.cc.Logger.Debug("close resolve store", "error", closeErr.Error())
		}
	}()

	if resolveAll {
		conflicts, err := store.ListConflicts(ctx)
		if err != nil {
			return fmt.Errorf("list conflicts: %w", err)
		}

		return s.queueEachConflictResolution(ctx, store, conflicts, resolution, dryRun)
	}

	return s.queueSingleConflictResolution(ctx, store, args[0], resolution, dryRun)
}

func (s *resolveService) requestConflictResolution(
	ctx context.Context,
	store resolveConflictStore,
	id string,
	resolution string,
) (syncstore.ConflictRequestResult, error) {
	return routeDurableIntent(
		ctx,
		func(ctx context.Context) (syncstore.ConflictRequestResult, error) {
			result, err := store.RequestConflictResolution(ctx, id, resolution)
			if err != nil {
				return syncstore.ConflictRequestResult{}, fmt.Errorf("queue conflict resolution: %w", err)
			}

			return result, nil
		},
		func(ctx context.Context, client *controlSocketClient) (syncstore.ConflictRequestResult, error) {
			status, err := client.requestConflictResolution(ctx, s.cc.Cfg.CanonicalID, id, resolution)
			if err != nil {
				return syncstore.ConflictRequestResult{}, err
			}

			return syncstore.ConflictRequestResult{Status: syncstore.ConflictRequestStatus(status)}, nil
		},
	)
}

func (s *resolveService) queueEachConflictResolution(
	ctx context.Context,
	store resolveConflictStore,
	conflicts []synctypes.ConflictRecord,
	resolution string,
	dryRun bool,
) error {
	if len(conflicts) == 0 {
		s.cc.Statusf("No unresolved conflicts.\n")
		return nil
	}

	for i := range conflicts {
		conflict := &conflicts[i]
		if dryRun {
			s.cc.Statusf("Would resolve %s as %s\n", conflict.Path, resolution)
			continue
		}

		result, err := s.requestConflictResolution(ctx, store, conflict.ID, resolution)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", conflict.Path, err)
		}

		writeQueuedConflictStatus(s.cc, conflict.Path, resolution, result.Status)
	}

	return nil
}

func (s *resolveService) queueSingleConflictResolution(
	ctx context.Context,
	store resolveConflictStore,
	idOrPath string,
	resolution string,
	dryRun bool,
) error {
	conflicts, err := store.ListConflicts(ctx)
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}

	target, found, err := findSelectedConflict(conflicts, idOrPath)
	if err != nil {
		return err
	}

	if !found {
		allConflicts, listErr := store.ListAllConflicts(ctx)
		if listErr != nil {
			return fmt.Errorf("list conflict history: %w", listErr)
		}

		resolvedConflict, resolved, findResolvedErr := findSelectedConflict(allConflicts, idOrPath)
		if findResolvedErr != nil {
			return findResolvedErr
		}
		if resolved && resolvedConflict.Resolution != synctypes.ResolutionUnresolved {
			s.cc.Statusf("Conflict %s already resolved as %s\n", resolvedConflict.Path, resolvedConflict.Resolution)
			return nil
		}

		return fmt.Errorf("conflict not found: %s", idOrPath)
	}

	if dryRun {
		s.cc.Statusf("Would resolve %s as %s\n", target.Path, resolution)
		return nil
	}

	result, err := s.requestConflictResolution(ctx, store, target.ID, resolution)
	if err != nil {
		return err
	}

	writeQueuedConflictStatus(s.cc, target.Path, resolution, result.Status)
	return nil
}

func findSelectedConflict(conflicts []synctypes.ConflictRecord, idOrPath string) (*synctypes.ConflictRecord, bool, error) {
	if idOrPath == "" {
		return nil, false, nil
	}

	for i := range conflicts {
		conflict := &conflicts[i]
		if conflict.ID == idOrPath || conflict.Path == idOrPath {
			return conflict, true, nil
		}
	}

	var match *synctypes.ConflictRecord
	for i := range conflicts {
		conflict := &conflicts[i]
		if len(conflict.ID) >= len(idOrPath) && conflict.ID[:len(idOrPath)] == idOrPath {
			if match != nil {
				return nil, false, fmt.Errorf("ambiguous conflict ID prefix %q — provide more characters", idOrPath)
			}
			match = conflict
		}
	}

	return match, match != nil, nil
}

func writeQueuedConflictStatus(
	cc *CLIContext,
	conflictPath string,
	resolution string,
	status syncstore.ConflictRequestStatus,
) {
	switch status {
	case syncstore.ConflictRequestQueued:
		cc.Statusf("Queued %s as %s (engine will resolve on the next sync pass)\n", conflictPath, resolution)
	case syncstore.ConflictRequestAlreadyQueued:
		cc.Statusf("Resolution already queued for %s as %s\n", conflictPath, resolution)
	case syncstore.ConflictRequestAlreadyApplying:
		cc.Statusf("Resolution already applying for %s\n", conflictPath)
	case syncstore.ConflictRequestAlreadyResolved:
		cc.Statusf("Conflict %s is already resolved\n", conflictPath)
	default:
		cc.Statusf("Resolution request for %s returned status %s\n", conflictPath, status)
	}
}

func (s *resolveService) openWritableStore(ctx context.Context) (*syncstore.SyncStore, error) {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	store, err := syncstore.NewSyncStore(ctx, dbPath, s.cc.Logger)
	if err != nil {
		return nil, recoverAwareStoreOpenError(
			s.cc.Cfg.CanonicalID.String(),
			fmt.Errorf("open sync store: %w", err),
		)
	}

	return store, nil
}
