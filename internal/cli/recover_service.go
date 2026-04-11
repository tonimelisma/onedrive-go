package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
)

type recoverService struct {
	cc *CLIContext
}

func newRecoverService(cc *CLIContext) *recoverService {
	return &recoverService{cc: cc}
}

func (s *recoverService) run(cmd *cobra.Command, yes bool) error {
	_ = yes

	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	if err := s.ensureNoLiveOwner(cmd.Context()); err != nil {
		return err
	}

	preflight := recoverPreflight{HasStateDB: managedPathExists(dbPath)}
	if !preflight.HasStateDB {
		return writeln(s.cc.Output(), recoverResultMessage(syncstore.RecoverResult{Action: syncstore.RecoverActionNoState}))
	}

	if err := confirmRecoverIntent(cmd, s.cc, preflight); err != nil {
		return err
	}

	result, err := syncstore.RecoverSyncStore(cmd.Context(), dbPath, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("recover sync database: %w", err)
	}

	return writeln(s.cc.Output(), recoverResultMessage(result))
}

func (s *recoverService) ensureNoLiveOwner(ctx context.Context) error {
	client, ok := openControlSocketClient(ctx)
	if !ok {
		return nil
	}

	for _, drive := range client.status.Drives {
		if strings.EqualFold(drive, s.cc.Cfg.CanonicalID.String()) {
			return fmt.Errorf(
				"cannot recover while a sync owner is active for %s (owner mode: %s); stop sync first",
				s.cc.Cfg.CanonicalID,
				client.ownerMode(),
			)
		}
	}

	if client.ownerMode() == synccontrol.OwnerModeOneShot || client.ownerMode() == synccontrol.OwnerModeWatch {
		return nil
	}

	return nil
}

func recoverHintForDrive(canonicalID string) string {
	return fmt.Sprintf("Run 'onedrive-go --drive %s recover' to repair, rebuild, or reset the sync database.", canonicalID)
}

func formatRecoverableStoreError(canonicalID string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%w\n%s", err, recoverHintForDrive(canonicalID))
}

func recoverAwareStoreOpenError(canonicalID string, err error) error {
	return formatRecoverableStoreError(canonicalID, err)
}

func recoverAwareStateStoreHint(canonicalID string) string {
	return recoverHintForDrive(canonicalID)
}
