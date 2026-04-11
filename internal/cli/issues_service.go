package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
)

type issuesService struct {
	cc *CLIContext
}

type issuesMutationStore interface {
	ApproveHeldDeletes(context.Context) error
	Close(context.Context) error
}

func newIssuesService(cc *CLIContext) *issuesService {
	return &issuesService{cc: cc}
}

func (s *issuesService) runList(ctx context.Context) error {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	if !managedPathExists(dbPath) {
		return s.writeEmptyIssues()
	}

	snapshot, err := syncstore.ReadIssuesSnapshot(ctx, dbPath, false, s.cc.Logger)
	if err != nil {
		return recoverAwareStoreOpenError(
			s.cc.Cfg.CanonicalID.String(),
			fmt.Errorf("read issues snapshot: %w", err),
		)
	}

	if snapshot.Empty() {
		return s.writeEmptyIssues()
	}

	if s.cc.Flags.JSON {
		return printGroupedIssuesJSON(s.cc.Output(), snapshot)
	}

	return printGroupedIssuesText(
		s.cc.Output(),
		snapshot,
		s.cc.Flags.Verbose,
	)
}

func (s *issuesService) writeEmptyIssues() error {
	return writeln(s.cc.Output(), "No issues.")
}

const approveDeletesSuccess = "Approved held deletes for this drive. The next sync pass will execute matching approved deletes."

func (s *issuesService) runApproveDeletes(ctx context.Context) error {
	_, err := routeDurableIntent(
		ctx,
		func(ctx context.Context) (struct{}, error) {
			return struct{}{}, s.approveDeletesDirect(ctx)
		},
		func(ctx context.Context, client *controlSocketClient) (struct{}, error) {
			if err := client.approveHeldDeletes(ctx, s.cc.Cfg.CanonicalID); err != nil {
				return struct{}{}, fmt.Errorf("approve held deletes via daemon: %w", err)
			}
			return struct{}{}, writeln(s.cc.Output(), approveDeletesSuccess)
		},
	)
	return err
}

func (s *issuesService) approveDeletesDirect(ctx context.Context) error {
	store, err := s.openMutationStore(ctx)
	if err != nil {
		return err
	}

	return s.runApproveDeletesWithStore(ctx, store)
}

func (s *issuesService) runApproveDeletesWithStore(ctx context.Context, store issuesMutationStore) (err error) {
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

	return writeln(s.cc.Output(), approveDeletesSuccess)
}

func (s *issuesService) openMutationStore(ctx context.Context) (issuesMutationStore, error) {
	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return nil, fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	mgr, err := syncstore.NewSyncStore(ctx, dbPath, s.cc.Logger)
	if err != nil {
		return nil, recoverAwareStoreOpenError(
			s.cc.Cfg.CanonicalID.String(),
			fmt.Errorf("open sync store: %w", err),
		)
	}

	return mgr, nil
}
