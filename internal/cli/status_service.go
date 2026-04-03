package cli

import (
	"fmt"

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
	cfgPath := s.cc.CfgPath

	cfg, warnings, err := config.LoadOrDefaultLenient(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	config.LogWarnings(warnings, logger)

	if len(cfg.Drives) == 0 {
		tokens := config.DiscoverTokens(logger)
		if len(tokens) > 0 {
			return writeln(s.cc.Output(), "No drives configured. Run 'onedrive-go drive add' to add a drive.")
		}

		return writeln(s.cc.Output(), "No accounts configured. Run 'onedrive-go login' to get started.")
	}

	accounts := buildStatusAccounts(cfg, logger)
	if s.cc.Flags.JSON {
		return printStatusJSON(s.cc.Output(), accounts)
	}

	return printStatusText(s.cc.Output(), accounts)
}
