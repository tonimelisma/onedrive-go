package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type conflictsService struct {
	cc *CLIContext
}

type conflictsInspector interface {
	ListConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	Close() error
}

type conflictsResolver interface {
	ListConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	RequestConflictResolution(context.Context, string, string) (syncstore.ConflictRequestResult, error)
	Close(context.Context) error
}

type emptyConflictsInspector struct{}

func (emptyConflictsInspector) ListConflicts(context.Context) ([]synctypes.ConflictRecord, error) {
	return nil, nil
}

func (emptyConflictsInspector) ListAllConflicts(context.Context) ([]synctypes.ConflictRecord, error) {
	return nil, nil
}

func (emptyConflictsInspector) Close() error {
	return nil
}

func newConflictsService(cc *CLIContext) *conflictsService {
	return &conflictsService{cc: cc}
}

func (s *conflictsService) runList(ctx context.Context, history bool) error {
	inspector, err := s.openInspector()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := inspector.Close(); closeErr != nil {
			s.cc.Logger.Debug("close conflicts inspector", "error", closeErr.Error())
		}
	}()

	var conflicts []synctypes.ConflictRecord
	if history {
		conflicts, err = inspector.ListAllConflicts(ctx)
	} else {
		conflicts, err = inspector.ListConflicts(ctx)
	}
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

func (s *conflictsService) openInspector() (conflictsInspector, error) {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	if !managedPathExists(dbPath) {
		return emptyConflictsInspector{}, nil
	}

	inspector, err := syncstore.OpenInspector(dbPath, s.cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("open sync store inspector: %w", err)
	}

	return inspector, nil
}

func (s *conflictsService) openResolver(ctx context.Context) (conflictsResolver, error) {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	store, err := syncstore.NewSyncStore(ctx, dbPath, s.cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("open sync store: %w", err)
	}

	return store, nil
}

func (s *conflictsService) requestConflictResolution(
	ctx context.Context,
	resolver conflictsResolver,
	id string,
	resolution string,
) (syncstore.ConflictRequestResult, error) {
	if client, ok := openControlSocketClient(ctx); ok {
		if client.ownerMode() == synccontrol.OwnerModeWatch {
			status, err := client.requestConflictResolution(ctx, s.cc.Cfg.CanonicalID, id, resolution)
			if err == nil {
				return syncstore.ConflictRequestResult{Status: syncstore.ConflictRequestStatus(status)}, nil
			}
			if isControlDaemonError(err) {
				return syncstore.ConflictRequestResult{}, err
			}
		}
	}

	result, err := resolver.RequestConflictResolution(ctx, id, resolution)
	if err != nil {
		return syncstore.ConflictRequestResult{}, fmt.Errorf("queue conflict resolution: %w", err)
	}

	return result, nil
}
