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
	mgr, err := s.openSyncStore(ctx)
	if err != nil {
		return err
	}
	defer mgr.Close(ctx)

	listing, err := s.loadIssuesListing(ctx, mgr, history)
	if err != nil {
		return err
	}

	if listing.empty() {
		return s.writeEmptyIssues(history)
	}

	if s.cc.Flags.JSON {
		return printGroupedIssuesJSON(s.cc.Output(), listing.conflicts, listing.groups, listing.heldDeletes)
	}

	return printGroupedIssuesText(
		s.cc.Output(),
		listing.conflicts,
		listing.groups,
		listing.heldDeletes,
		listing.pendingRetries,
		listing.shortcuts,
		history,
		s.cc.Flags.Verbose,
	)
}

type issuesListing struct {
	conflicts      []synctypes.ConflictRecord
	groups         []failureGroup
	heldDeletes    []synctypes.SyncFailureRow
	pendingRetries []synctypes.PendingRetryGroup
	shortcuts      []synctypes.Shortcut
}

func (l issuesListing) empty() bool {
	return len(l.conflicts) == 0 &&
		len(l.groups) == 0 &&
		len(l.heldDeletes) == 0 &&
		len(l.pendingRetries) == 0
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

func (s *issuesService) loadIssuesListing(
	ctx context.Context,
	mgr *syncstore.SyncStore,
	history bool,
) (issuesListing, error) {
	conflicts, err := s.listConflicts(ctx, mgr, history)
	if err != nil {
		return issuesListing{}, err
	}

	failures, err := s.listVisibleFailures(ctx, mgr)
	if err != nil {
		return issuesListing{}, err
	}

	scopeBlocks, err := mgr.ListScopeBlocks(ctx)
	if err != nil {
		return issuesListing{}, fmt.Errorf("list scope blocks: %w", err)
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	if err != nil {
		return issuesListing{}, fmt.Errorf("list shortcuts: %w", err)
	}

	groups, heldDeletes := groupFailures(failures, shortcuts)
	groups = appendScopeOnlyGroups(groups, scopeBlocks, shortcuts)

	pendingRetries, err := mgr.PendingRetrySummary(ctx)
	if err != nil {
		return issuesListing{}, fmt.Errorf("summarize pending retries: %w", err)
	}

	return issuesListing{
		conflicts:      conflicts,
		groups:         groups,
		heldDeletes:    heldDeletes,
		pendingRetries: pendingRetries,
		shortcuts:      shortcuts,
	}, nil
}

func (s *issuesService) listConflicts(
	ctx context.Context,
	mgr *syncstore.SyncStore,
	history bool,
) ([]synctypes.ConflictRecord, error) {
	if history {
		conflicts, err := mgr.ListAllConflicts(ctx)
		if err != nil {
			return nil, fmt.Errorf("list conflicts: %w", err)
		}
		return conflicts, nil
	}

	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	return conflicts, nil
}

func (s *issuesService) listVisibleFailures(
	ctx context.Context,
	mgr *syncstore.SyncStore,
) ([]synctypes.SyncFailureRow, error) {
	failures, err := mgr.ListActionableFailures(ctx)
	if err != nil {
		return nil, fmt.Errorf("list actionable failures: %w", err)
	}

	remoteBlocked, err := mgr.ListRemoteBlockedFailures(ctx)
	if err != nil {
		return nil, fmt.Errorf("list remote blocked failures: %w", err)
	}

	return append(failures, remoteBlocked...), nil
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
