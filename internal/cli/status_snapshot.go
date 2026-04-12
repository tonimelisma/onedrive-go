package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func runStatusCommand(cc *CLIContext, history bool, showPerf ...bool) error {
	logger := cc.Logger
	perfEnabled := len(showPerf) > 0 && showPerf[0]
	snapshot, err := loadAccountCatalogSnapshot(context.Background(), cc)
	if err != nil {
		return err
	}

	if len(snapshot.Config.Drives) == 0 {
		tokens := config.DiscoverTokens(logger)
		if len(tokens) > 0 {
			return writeln(cc.Output(), "No drives configured. Run 'onedrive-go drive add' to add a drive.")
		}

		return writeln(cc.Output(), "No accounts configured. Run 'onedrive-go login' to get started.")
	}

	filteredSnapshot, err := filterStatusSnapshot(snapshot, cc.Flags.Drive, logger)
	if err != nil {
		return err
	}
	if len(filteredSnapshot.Config.Drives) == 0 {
		return writeln(cc.Output(), "No matching drives selected.")
	}

	accounts := statusAccounts(cc, filteredSnapshot, history)
	applyStatusPerfOverlay(accounts, loadStatusPerfOverlay(context.Background(), perfEnabled))
	if cc.Flags.JSON {
		return printStatusJSON(cc.Output(), accounts)
	}

	return printStatusText(cc.Output(), accounts, history)
}

func filterStatusSnapshot(
	snapshot accountCatalogSnapshot,
	selectors []string,
	logger *slog.Logger,
) (accountCatalogSnapshot, error) {
	if len(selectors) == 0 {
		return snapshot, nil
	}

	selectedDrives, err := config.ResolveDrives(snapshot.Config, selectors, true, logger)
	if err != nil {
		return accountCatalogSnapshot{}, fmt.Errorf("resolving status drive selectors: %w", err)
	}

	filtered := *snapshot.Config
	filtered.Drives = make(map[driveid.CanonicalID]config.Drive, len(selectedDrives))
	for i := range selectedDrives {
		rd := selectedDrives[i]
		filtered.Drives[rd.CanonicalID] = snapshot.Config.Drives[rd.CanonicalID]
	}

	return accountCatalogSnapshot{
		Config:  &filtered,
		Catalog: snapshot.Catalog,
	}, nil
}
