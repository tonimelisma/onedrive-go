package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type conflictsService struct {
	cc *CLIContext
}

type conflictsResolver interface {
	ListConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	RequestConflictResolution(context.Context, string, string) (syncstore.ConflictRequestResult, error)
	Close(context.Context) error
}

func newConflictsService(cc *CLIContext) *conflictsService {
	return &conflictsService{cc: cc}
}

func (s *conflictsService) runList(ctx context.Context, history bool) error {
	conflicts, err := s.listConflicts(ctx, history)
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}

	if len(conflicts) == 0 {
		if history {
			return writeln(s.cc.Output(), "No conflicts in history.")
		}
		return writeln(s.cc.Output(), "No conflicts.")
	}

	if s.cc.Flags.JSON {
		return printConflictsJSON(s.cc.Output(), conflicts)
	}

	return printConflictsTable(s.cc.Output(), conflicts, history)
}

func (s *conflictsService) runResolve(
	ctx context.Context,
	args []string,
	resolution string,
	resolveAll bool,
	dryRun bool,
) error {
	resolver, err := s.openResolver(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resolver.Close(ctx); closeErr != nil {
			s.cc.Logger.Debug("close conflict resolver", "error", closeErr.Error())
		}
	}()

	if resolveAll {
		conflicts, err := resolver.ListConflicts(ctx)
		if err != nil {
			return fmt.Errorf("list conflicts: %w", err)
		}

		return resolveEachConflict(s.cc, conflicts, resolution, dryRun, func(id, res string) (string, error) {
			result, err := s.requestConflictResolution(ctx, resolver, id, res)
			return string(result.Status), err
		})
	}

	return resolveSingleConflict(
		s.cc,
		args[0],
		resolution,
		dryRun,
		func() ([]synctypes.ConflictRecord, error) { return resolver.ListConflicts(ctx) },
		func() ([]synctypes.ConflictRecord, error) { return resolver.ListAllConflicts(ctx) },
		func(id, res string) (string, error) {
			result, err := s.requestConflictResolution(ctx, resolver, id, res)
			return string(result.Status), err
		},
	)
}

func (s *conflictsService) listConflicts(ctx context.Context, history bool) ([]synctypes.ConflictRecord, error) {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	if !managedPathExists(dbPath) {
		return nil, nil
	}

	conflicts, err := syncstore.ListConflictsAtPath(ctx, dbPath, history, s.cc.Logger)
	if err != nil {
		return nil, recoverAwareStoreOpenError(
			s.cc.Cfg.CanonicalID.String(),
			fmt.Errorf("read conflicts snapshot: %w", err),
		)
	}

	return conflicts, nil
}

func (s *conflictsService) openResolver(ctx context.Context) (conflictsResolver, error) {
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

func (s *conflictsService) requestConflictResolution(
	ctx context.Context,
	resolver conflictsResolver,
	id string,
	resolution string,
) (syncstore.ConflictRequestResult, error) {
	return routeDurableIntent(
		ctx,
		func(ctx context.Context) (syncstore.ConflictRequestResult, error) {
			result, err := resolver.RequestConflictResolution(ctx, id, resolution)
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
