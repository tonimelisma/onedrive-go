package cli

import (
	"context"
	"errors"
	"fmt"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type resolveDeleteStore interface {
	ApproveHeldDeletes(context.Context) error
	Close(context.Context) error
}

type resolveConflictStore interface {
	ListConflicts(context.Context) ([]syncengine.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]syncengine.ConflictRecord, error)
	RequestConflictResolution(context.Context, string, string) (syncengine.ConflictRequestResult, error)
	Close(context.Context) error
}

func runApproveDeletes(ctx context.Context, cc *CLIContext) error {
	probe, err := probeControlOwner(ctx)
	if err != nil && probe.state == controlOwnerStateProbeFailed {
		return fmt.Errorf("probe control owner: %w", err)
	}

	switch probe.state {
	case controlOwnerStateOneShotOwner, controlOwnerStateNoSocket, controlOwnerStatePathUnavailable:
		return approveDeletesDirect(ctx, cc)
	case controlOwnerStateWatchOwner:
		watchErr := probe.client.approveHeldDeletes(ctx, cc.Cfg.CanonicalID)
		if watchErr != nil {
			watchErr = fmt.Errorf("approve held deletes via daemon: %w", watchErr)
		}
		if watchErr == nil {
			return writeln(cc.Output(), resolveApproveDeletesSuccess)
		}
		if isControlDaemonError(watchErr) {
			return watchErr
		}
		if isControlSocketGone(watchErr) {
			return approveDeletesDirect(ctx, cc)
		}

		return watchErr
	case controlOwnerStateProbeFailed:
		return fmt.Errorf("probe control owner: %w", err)
	default:
		return fmt.Errorf("probe control owner: unhandled probe state %q", probe.state)
	}
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
) (syncengine.ConflictRequestResult, error) {
	probe, err := probeControlOwner(ctx)
	if err != nil && probe.state == controlOwnerStateProbeFailed {
		return syncengine.ConflictRequestResult{}, fmt.Errorf("probe control owner: %w", err)
	}

	switch probe.state {
	case controlOwnerStateOneShotOwner, controlOwnerStateNoSocket, controlOwnerStatePathUnavailable:
		result, requestErr := store.RequestConflictResolution(ctx, id, resolution)
		if requestErr != nil {
			return syncengine.ConflictRequestResult{}, fmt.Errorf("queue conflict resolution: %w", requestErr)
		}

		return result, nil
	case controlOwnerStateWatchOwner:
		status, requestErr := probe.client.requestConflictResolution(ctx, cc.Cfg.CanonicalID, id, resolution)
		if requestErr == nil {
			return syncengine.ConflictRequestResult{Status: syncengine.ConflictRequestStatus(status)}, nil
		}
		if isControlDaemonError(requestErr) {
			return syncengine.ConflictRequestResult{}, requestErr
		}
		if isControlSocketGone(requestErr) {
			result, directErr := store.RequestConflictResolution(ctx, id, resolution)
			if directErr != nil {
				return syncengine.ConflictRequestResult{}, fmt.Errorf("queue conflict resolution: %w", directErr)
			}

			return result, nil
		}

		return syncengine.ConflictRequestResult{}, requestErr
	case controlOwnerStateProbeFailed:
		return syncengine.ConflictRequestResult{}, fmt.Errorf("probe control owner: %w", err)
	default:
		return syncengine.ConflictRequestResult{}, fmt.Errorf("probe control owner: unhandled probe state %q", probe.state)
	}
}

func queueEachConflictResolution(
	ctx context.Context,
	cc *CLIContext,
	store resolveConflictStore,
	conflicts []syncengine.ConflictRecord,
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
		if resolved && resolvedConflict.Resolution != syncengine.ResolutionUnresolved {
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

func findSelectedConflict(conflicts []syncengine.ConflictRecord, idOrPath string) (*syncengine.ConflictRecord, bool, error) {
	if idOrPath == "" {
		return nil, false, nil
	}

	for i := range conflicts {
		conflict := &conflicts[i]
		if conflict.ID == idOrPath || conflict.Path == idOrPath {
			return conflict, true, nil
		}
	}

	var match *syncengine.ConflictRecord
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
	status syncengine.ConflictRequestStatus,
) {
	switch status {
	case syncengine.ConflictRequestQueued:
		cc.Statusf("Queued %s as %s (engine will resolve on the next sync pass)\n", conflictPath, resolution)
	case syncengine.ConflictRequestAlreadyQueued:
		cc.Statusf("Resolution already queued for %s as %s\n", conflictPath, resolution)
	case syncengine.ConflictRequestAlreadyApplying:
		cc.Statusf("Resolution already applying for %s\n", conflictPath)
	case syncengine.ConflictRequestAlreadyResolved:
		cc.Statusf("Conflict %s is already resolved\n", conflictPath)
	default:
		cc.Statusf("Resolution request for %s returned status %s\n", conflictPath, status)
	}
}

func openWritableStoreForContext(ctx context.Context, cc *CLIContext) (*syncengine.SyncStore, error) {
	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	store, err := syncengine.NewSyncStore(ctx, dbPath, cc.Logger)
	if err != nil {
		return nil, recoverAwareStoreOpenError(
			cc.Cfg.CanonicalID.String(),
			fmt.Errorf("open sync store: %w", err),
		)
	}

	return store, nil
}
