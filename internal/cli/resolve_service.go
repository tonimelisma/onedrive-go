package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
)

type resolveDeleteStore interface {
	ApproveHeldDeletes(context.Context) error
	Close(context.Context) error
}

type resolveConflictStore interface {
	ListConflicts(context.Context) ([]syncstore.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]syncstore.ConflictRecord, error)
	RequestConflictResolution(context.Context, string, string) (syncstore.ConflictRequestResult, error)
	Close(context.Context) error
}

func runApproveDeletes(ctx context.Context, cc *CLIContext) error {
	_, err := routeDurableIntent(
		ctx,
		func(ctx context.Context) (struct{}, error) {
			return struct{}{}, approveDeletesDirect(ctx, cc)
		},
		func(ctx context.Context, client *controlSocketClient) (struct{}, error) {
			if err := client.approveHeldDeletes(ctx, cc.Cfg.CanonicalID); err != nil {
				return struct{}{}, fmt.Errorf("approve held deletes via daemon: %w", err)
			}
			return struct{}{}, writeln(cc.Output(), resolveApproveDeletesSuccess)
		},
	)
	return err
}

func approveDeletesDirect(ctx context.Context, cc *CLIContext) error {
	store, err := openWritableStoreForContext(ctx, cc)
	if err != nil {
		return err
	}

	return runApproveDeletesWithStore(ctx, cc, store)
}

func runApproveDeletesWithStore(ctx context.Context, cc *CLIContext, store resolveDeleteStore) (err error) {
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

	return writeln(cc.Output(), resolveApproveDeletesSuccess)
}

const resolveApproveDeletesSuccess = "Approved held deletes for this drive. The next sync pass will execute matching approved deletes."

func runResolve(
	ctx context.Context,
	cc *CLIContext,
	args []string,
	resolution string,
	resolveAll bool,
	dryRun bool,
) error {
	store, err := openWritableStoreForContext(ctx, cc)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := store.Close(ctx); closeErr != nil {
			cc.Logger.Debug("close resolve store", "error", closeErr.Error())
		}
	}()

	if resolveAll {
		conflicts, err := store.ListConflicts(ctx)
		if err != nil {
			return fmt.Errorf("list conflicts: %w", err)
		}

		return queueEachConflictResolution(ctx, cc, store, conflicts, resolution, dryRun)
	}

	return queueSingleConflictResolution(ctx, cc, store, args[0], resolution, dryRun)
}

func requestConflictResolution(
	ctx context.Context,
	cc *CLIContext,
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
			status, err := client.requestConflictResolution(ctx, cc.Cfg.CanonicalID, id, resolution)
			if err != nil {
				return syncstore.ConflictRequestResult{}, err
			}

			return syncstore.ConflictRequestResult{Status: syncstore.ConflictRequestStatus(status)}, nil
		},
	)
}

func queueEachConflictResolution(
	ctx context.Context,
	cc *CLIContext,
	store resolveConflictStore,
	conflicts []syncstore.ConflictRecord,
	resolution string,
	dryRun bool,
) error {
	if len(conflicts) == 0 {
		cc.Statusf("No unresolved conflicts.\n")
		return nil
	}

	for i := range conflicts {
		conflict := &conflicts[i]
		if dryRun {
			cc.Statusf("Would resolve %s as %s\n", conflict.Path, resolution)
			continue
		}

		result, err := requestConflictResolution(ctx, cc, store, conflict.ID, resolution)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", conflict.Path, err)
		}

		writeQueuedConflictStatus(cc, conflict.Path, resolution, result.Status)
	}

	return nil
}

func queueSingleConflictResolution(
	ctx context.Context,
	cc *CLIContext,
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
		if resolved && resolvedConflict.Resolution != syncstore.ResolutionUnresolved {
			cc.Statusf("Conflict %s already resolved as %s\n", resolvedConflict.Path, resolvedConflict.Resolution)
			return nil
		}

		return fmt.Errorf("conflict not found: %s", idOrPath)
	}

	if dryRun {
		cc.Statusf("Would resolve %s as %s\n", target.Path, resolution)
		return nil
	}

	result, err := requestConflictResolution(ctx, cc, store, target.ID, resolution)
	if err != nil {
		return err
	}

	writeQueuedConflictStatus(cc, target.Path, resolution, result.Status)
	return nil
}

func findSelectedConflict(conflicts []syncstore.ConflictRecord, idOrPath string) (*syncstore.ConflictRecord, bool, error) {
	if idOrPath == "" {
		return nil, false, nil
	}

	for i := range conflicts {
		conflict := &conflicts[i]
		if conflict.ID == idOrPath || conflict.Path == idOrPath {
			return conflict, true, nil
		}
	}

	var match *syncstore.ConflictRecord
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

func openWritableStoreForContext(ctx context.Context, cc *CLIContext) (*syncstore.SyncStore, error) {
	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	store, err := syncstore.NewSyncStore(ctx, dbPath, cc.Logger)
	if err != nil {
		return nil, recoverAwareStoreOpenError(
			cc.Cfg.CanonicalID.String(),
			fmt.Errorf("open sync store: %w", err),
		)
	}

	return store, nil
}
