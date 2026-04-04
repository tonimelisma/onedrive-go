package cli

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

type statusService struct {
	cc *CLIContext
}

func newStatusService(cc *CLIContext) *statusService {
	return &statusService{cc: cc}
}

func (s *statusService) run() error {
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

	accounts := readModel.statusAccounts(snapshot)
	if s.cc.Flags.JSON {
		return printStatusJSON(s.cc.Output(), accounts)
	}

	return printStatusText(s.cc.Output(), accounts)
}
