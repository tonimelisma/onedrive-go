package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
)

type issuesService struct {
	cc *CLIContext
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

func (s *issuesService) openSyncStore(ctx context.Context) (*syncstore.SyncStore, error) {
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

func (s *issuesService) writeEmptyIssues(history bool) error {
	if history {
		return writeln(s.cc.Output(), "No issues in history.")
	}

	return writeln(s.cc.Output(), "No issues.")
}

func (s *issuesService) runResolve(ctx context.Context, args []string, resolution string, resolveAll, dryRun bool) error {
	return resolveWithTransfers(ctx, s.cc, args, resolution, resolveAll, dryRun)
}

func (s *issuesService) runScopeRecheck(ctx context.Context, input string) error {
	mgr, err := s.openSyncStore(ctx)
	if err != nil {
		return err
	}
	defer mgr.Close(ctx)

	target, found, err := mgr.FindRemoteBlockedTarget(ctx, input)
	if err != nil {
		return fmt.Errorf("find blocked shared-folder boundary for %s: %w", input, err)
	}
	if !found {
		return fmt.Errorf("shared-folder write block not found: %s", input)
	}
	humanScope := target.ScopeKey.Humanize(nil)

	if err := mgr.RequestScopeRecheck(ctx, target.ScopeKey); err != nil {
		return fmt.Errorf("request shared-folder boundary recheck for %s: %w", humanScope, err)
	}

	return writef(s.cc.Output(), "Queued permission recheck for %s.\n", humanScope)
}

func (s *issuesService) runFailureAction(ctx context.Context, args []string, doAll bool, action failureAction) error {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	mgr, err := syncstore.NewSyncStore(ctx, dbPath, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("open sync store: %w", err)
	}
	defer mgr.Close(ctx)

	if doAll {
		if err := action.allFn(ctx, mgr); err != nil {
			return err
		}

		return writeln(s.cc.Output(), action.allMsg)
	}

	if len(args) == 0 {
		return fmt.Errorf("%s", action.noArgMsg)
	}

	if err := action.singleFn(ctx, mgr, args[0]); err != nil {
		return err
	}

	return writef(s.cc.Output(), action.singleFmt+"\n", args[0])
}
