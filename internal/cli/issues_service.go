package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type issuesService struct {
	cc *CLIContext
}

type issuesMutationStore interface {
	ClearActionableSyncFailures(context.Context) error
	ClearAllRemoteBlockedFailures(context.Context) error
	FindRemoteBlockedTarget(context.Context, string) (syncstore.RemoteBlockedTarget, bool, error)
	ClearRemoteBlockedTarget(context.Context, syncstore.RemoteBlockedTarget) error
	ClearSyncFailureByPath(context.Context, string) error
	ResetAllFailures(context.Context) error
	RequestRemoteBlockedTrial(context.Context, syncstore.RemoteBlockedTarget) error
	RequestScopeRecheck(context.Context, synctypes.ScopeKey) error
	ResetFailure(context.Context, string) error
	Close(context.Context) error
}

type issuesConflictResolver interface {
	ListConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	ListAllConflicts(context.Context) ([]synctypes.ConflictRecord, error)
	ResolveConflict(context.Context, string, string) error
	Close(context.Context) error
}

func newIssuesService(cc *CLIContext) *issuesService {
	return &issuesService{cc: cc}
}

func (s *issuesService) runList(ctx context.Context, history bool) error {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	if !managedPathExists(dbPath) {
		return s.writeEmptyIssues(history)
	}

	inspector, err := syncstore.OpenInspector(dbPath, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("open sync store inspector: %w", err)
	}
	defer func() {
		if closeErr := inspector.Close(); closeErr != nil {
			s.cc.Logger.Debug("close sync store inspector", "error", closeErr.Error())
		}
	}()

	snapshot, err := inspector.ReadIssuesSnapshot(ctx, history)
	if err != nil {
		return fmt.Errorf("read issues snapshot: %w", err)
	}

	if snapshot.Empty() {
		return s.writeEmptyIssues(history)
	}

	if s.cc.Flags.JSON {
		return printGroupedIssuesJSON(s.cc.Output(), snapshot)
	}

	return printGroupedIssuesText(
		s.cc.Output(),
		snapshot,
		history,
		s.cc.Flags.Verbose,
	)
}

func (s *issuesService) writeEmptyIssues(history bool) error {
	if history {
		return writeln(s.cc.Output(), "No issues in history.")
	}

	return writeln(s.cc.Output(), "No issues.")
}

func (s *issuesService) runResolve(ctx context.Context, args []string, resolution string, resolveAll, dryRun bool) error {
	resolver, err := s.openConflictResolver(ctx)
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

		return resolveEachConflict(s.cc, conflicts, resolution, dryRun, func(id, res string) error {
			return resolver.ResolveConflict(ctx, id, res)
		})
	}

	return resolveSingleConflict(
		s.cc,
		args[0],
		resolution,
		dryRun,
		func() ([]synctypes.ConflictRecord, error) { return resolver.ListConflicts(ctx) },
		func() ([]synctypes.ConflictRecord, error) { return resolver.ListAllConflicts(ctx) },
		func(id, res string) error { return resolver.ResolveConflict(ctx, id, res) },
	)
}

func (s *issuesService) runScopeRecheck(ctx context.Context, input string) error {
	store, err := s.openMutationStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close(ctx)

	target, found, err := store.FindRemoteBlockedTarget(ctx, input)
	if err != nil {
		return fmt.Errorf("find blocked shared-folder boundary for %s: %w", input, err)
	}
	if !found {
		return fmt.Errorf("shared-folder write block not found: %s", input)
	}
	humanScope := target.ScopeKey.Humanize(nil)

	if err := store.RequestScopeRecheck(ctx, target.ScopeKey); err != nil {
		return fmt.Errorf("request shared-folder boundary recheck for %s: %w", humanScope, err)
	}

	return writef(s.cc.Output(), "Queued permission recheck for %s.\n", humanScope)
}

func (s *issuesService) runClear(ctx context.Context, args []string, clearAll bool) error {
	store, err := s.openMutationStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close(ctx)

	if clearAll {
		if err := store.ClearActionableSyncFailures(ctx); err != nil {
			return fmt.Errorf("clear actionable failures: %w", err)
		}
		if err := store.ClearAllRemoteBlockedFailures(ctx); err != nil {
			return fmt.Errorf("clear blocked shared-folder writes: %w", err)
		}

		return writeln(s.cc.Output(), "Cleared actionable failures and blocked shared-folder writes.")
	}

	if len(args) == 0 {
		return fmt.Errorf("provide a path to clear, or use --all to clear all actionable failures")
	}

	path := args[0]
	if target, found, err := store.FindRemoteBlockedTarget(ctx, path); err != nil {
		return fmt.Errorf("find blocked shared-folder write for %s: %w", path, err)
	} else if found {
		if err := store.ClearRemoteBlockedTarget(ctx, target); err != nil {
			return fmt.Errorf("clear blocked shared-folder write for %s: %w", path, err)
		}

		return writef(s.cc.Output(), "Cleared failure for %s.\n", path)
	}

	if err := store.ClearSyncFailureByPath(ctx, path); err != nil {
		return fmt.Errorf("clear actionable failure for %s: %w", path, err)
	}

	return writef(s.cc.Output(), "Cleared failure for %s.\n", path)
}

func (s *issuesService) runRetry(ctx context.Context, args []string, retryAll bool) error {
	store, err := s.openMutationStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close(ctx)

	if retryAll {
		if err := store.ResetAllFailures(ctx); err != nil {
			return fmt.Errorf("reset all failures: %w", err)
		}

		return writeln(s.cc.Output(), "Reset all failures for retry.")
	}

	if len(args) == 0 {
		return fmt.Errorf("provide a path to retry, or use --all to retry all failures")
	}

	path := args[0]
	if target, found, err := store.FindRemoteBlockedTarget(ctx, path); err != nil {
		return fmt.Errorf("find blocked shared-folder write for %s: %w", path, err)
	} else if found {
		if target.Kind == syncstore.RemoteBlockedTargetBoundary {
			return fmt.Errorf("shared-folder write blocks must be retried by blocked path, not boundary")
		}
		if err := store.RequestRemoteBlockedTrial(ctx, target); err != nil {
			return fmt.Errorf("request blocked shared-folder retry for %s: %w", path, err)
		}

		return writef(s.cc.Output(), "Requested retry for %s.\n", path)
	}

	if err := store.ResetFailure(ctx, path); err != nil {
		return fmt.Errorf("reset failure for %s: %w", path, err)
	}

	return writef(s.cc.Output(), "Requested retry for %s.\n", path)
}

func (s *issuesService) openMutationStore(ctx context.Context) (issuesMutationStore, error) {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	mgr, err := syncstore.NewSyncStore(ctx, dbPath, s.cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("open sync store: %w", err)
	}

	return mgr, nil
}

func (s *issuesService) openConflictResolver(ctx context.Context) (issuesConflictResolver, error) {
	session, err := s.cc.Session(ctx)
	if err != nil {
		return nil, err
	}

	engine, err := newSyncEngine(ctx, session, s.cc.Cfg, false, s.cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("create sync engine: %w", err)
	}

	return engine, nil
}
