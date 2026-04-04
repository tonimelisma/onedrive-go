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
