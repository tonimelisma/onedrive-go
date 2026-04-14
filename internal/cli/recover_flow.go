package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func runRecoverCommand(cmd *cobra.Command, cc *CLIContext, yes bool) error {
	_ = yes

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	if err := ensureNoLiveRecoverOwner(cmd.Context(), cc); err != nil {
		return err
	}

	preflight := recoverPreflight{HasStateDB: managedPathExists(dbPath)}
	if !preflight.HasStateDB {
		return writeln(cc.Output(), recoverResultMessage(syncengine.StateDBRepairResult{Action: syncengine.StateDBRepairNoState}))
	}

	if err := confirmRecoverIntent(cmd, cc, preflight); err != nil {
		return err
	}

	result, err := syncengine.RepairStateDB(cmd.Context(), dbPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("recover sync database: %w", err)
	}

	return writeln(cc.Output(), recoverResultMessage(result))
}

func ensureNoLiveRecoverOwner(ctx context.Context, cc *CLIContext) error {
	probe, err := probeControlOwner(ctx)
	if err != nil && probe.state == controlOwnerStateProbeFailed {
		return fmt.Errorf("probe control owner: %w", err)
	}
	if probe.state != controlOwnerStateWatchOwner && probe.state != controlOwnerStateOneShotOwner {
		return nil
	}
	if probe.client == nil {
		return nil
	}

	for _, drive := range probe.client.status.Drives {
		if strings.EqualFold(drive, cc.Cfg.CanonicalID.String()) {
			return fmt.Errorf(
				"cannot recover while a sync owner is active for %s (owner mode: %s); stop sync first",
				cc.Cfg.CanonicalID,
				probe.client.ownerMode(),
			)
		}
	}

	return nil
}

func recoverHintForDrive(canonicalID string) string {
	return fmt.Sprintf("Run 'onedrive-go --drive %s recover' to repair, rebuild, or reset the sync database.", canonicalID)
}

func recoverAwareStateStoreHint(canonicalID string) string {
	return recoverHintForDrive(canonicalID)
}
