package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type statusService struct {
	cc *CLIContext
}

func newStatusService(cc *CLIContext) *statusService {
	return &statusService{cc: cc}
}

func (s *statusService) run(history bool) error {
	logger := s.cc.Logger
	readModel := newAccountReadModelService(s.cc)
	snapshot, err := readModel.loadLenientCatalog(context.Background())
	if err != nil {
		return err
	}

	if len(snapshot.Config.Drives) == 0 {
		tokens := config.DiscoverTokens(logger)
		if len(tokens) > 0 {
			return writeln(s.cc.Output(), "No drives configured. Run 'onedrive-go drive add' to add a drive.")
		}

		return writeln(s.cc.Output(), "No accounts configured. Run 'onedrive-go login' to get started.")
	}

	filteredSnapshot, err := filterStatusSnapshot(snapshot, s.cc.Flags.Drive, logger)
	if err != nil {
		return err
	}
	if len(filteredSnapshot.Config.Drives) == 0 {
		return writeln(s.cc.Output(), "No matching drives selected.")
	}

	accounts := readModel.statusAccounts(filteredSnapshot, history)
	if s.cc.Flags.JSON {
		return printStatusJSON(s.cc.Output(), accounts)
	}

	return printStatusText(s.cc.Output(), accounts, history)
}

func filterStatusSnapshot(
	snapshot accountReadModelSnapshot,
	selectors []string,
	logger *slog.Logger,
) (accountReadModelSnapshot, error) {
	if len(selectors) == 0 {
		return snapshot, nil
	}

	selectedDrives, err := config.ResolveDrives(snapshot.Config, selectors, true, logger)
	if err != nil {
		return accountReadModelSnapshot{}, fmt.Errorf("resolving status drive selectors: %w", err)
	}

	filtered := *snapshot.Config
	filtered.Drives = make(map[driveid.CanonicalID]config.Drive, len(selectedDrives))
	for i := range selectedDrives {
		rd := selectedDrives[i]
		filtered.Drives[rd.CanonicalID] = snapshot.Config.Drives[rd.CanonicalID]
	}

	return accountReadModelSnapshot{
		Config:  &filtered,
		Catalog: snapshot.Catalog,
	}, nil
}
