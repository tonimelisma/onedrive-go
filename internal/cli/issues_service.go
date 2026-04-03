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

func newIssuesService(cc *CLIContext) *issuesService {
	return &issuesService{cc: cc}
}

func (s *issuesService) runList(ctx context.Context, history bool) error {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	mgr, err := syncstore.NewSyncStore(ctx, dbPath, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("open sync store: %w", err)
	}
	defer mgr.Close(ctx)

	var conflicts []synctypes.ConflictRecord
	if history {
		conflicts, err = mgr.ListAllConflicts(ctx)
	} else {
		conflicts, err = mgr.ListConflicts(ctx)
	}
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}

	failures, err := mgr.ListActionableFailures(ctx)
	if err != nil {
		return fmt.Errorf("list actionable failures: %w", err)
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	if err != nil {
		return fmt.Errorf("list shortcuts: %w", err)
	}

	groups, heldDeletes := groupFailures(failures, shortcuts)
	pendingRetries, err := mgr.PendingRetrySummary(ctx)
	if err != nil {
		return fmt.Errorf("summarize pending retries: %w", err)
	}

	if len(conflicts) == 0 && len(groups) == 0 && len(heldDeletes) == 0 && len(pendingRetries) == 0 {
		if history {
			return writeln(s.cc.Output(), "No issues in history.")
		}

		return writeln(s.cc.Output(), "No issues.")
	}

	if s.cc.Flags.JSON {
		return printGroupedIssuesJSON(s.cc.Output(), conflicts, groups, heldDeletes)
	}

	return printGroupedIssuesText(
		s.cc.Output(),
		conflicts,
		groups,
		heldDeletes,
		pendingRetries,
		shortcuts,
		history,
		s.cc.Flags.Verbose,
	)
}

func (s *issuesService) runResolve(ctx context.Context, args []string, resolution string, resolveAll, dryRun bool) error {
	return resolveWithTransfers(ctx, s.cc, args, resolution, resolveAll, dryRun)
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
