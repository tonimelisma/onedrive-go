package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type driveService struct {
	cc *CLIContext
}

func newDriveService(cc *CLIContext) *driveService {
	return &driveService{cc: cc}
}

func (s *driveService) runList(ctx context.Context, showAll bool) error {
	logger := s.cc.Logger

	cfg, warnings, err := config.LoadOrDefaultLenient(s.cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	config.LogWarnings(warnings, logger)

	configured := buildConfiguredDriveEntries(cfg, logger)

	siteLimit := sharePointSiteLimit
	if showAll {
		siteLimit = sharePointSiteUnlimited
	}

	available := discoverAvailableDrives(ctx, cfg, siteLimit, logger)
	annotateStateDB(available)

	if s.cc.Flags.JSON {
		return printDriveListJSON(s.cc.Output(), configured, available)
	}

	return printDriveListText(s.cc.Output(), configured, available)
}

func (s *driveService) runAdd(ctx context.Context, args []string) error {
	logger := s.cc.Logger

	selector := ""
	if len(args) > 0 {
		selector = args[0]
	}

	if selector == "" {
		var driveErr error
		selector, driveErr = s.cc.Flags.SingleDrive()
		if driveErr != nil {
			return driveErr
		}
	}

	if selector == "" {
		return listAvailableDrives(s.cc.Output())
	}

	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		if strings.Contains(selector, ":") {
			return fmt.Errorf("invalid canonical ID %q: %w\n"+
				"Run 'onedrive-go drive list' to see valid canonical IDs", selector, err)
		}

		return addSharedDriveByName(ctx, selector, s.cc.CfgPath, s.cc.Output(), logger)
	}

	if cid.IsShared() {
		return addSharedDrive(ctx, s.cc.CfgPath, s.cc.Output(), cid, "", logger)
	}

	return addNewDrive(s.cc.Output(), s.cc.CfgPath, cid, logger)
}

func (s *driveService) runRemove(purge bool) error {
	logger := s.cc.Logger

	driveSelector, driveErr := s.cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive to remove)")
	}

	cfg, err := config.LoadOrDefault(s.cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, cidErr := driveid.NewCanonicalID(driveSelector)
	if cidErr != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveSelector, cidErr)
	}

	_, inConfig := cfg.Drives[cid]
	if !inConfig && !purge {
		return fmt.Errorf("drive %q not found in config — use --purge to clean up leftover state", driveSelector)
	}

	if !inConfig && purge {
		logger.Info("purging orphaned drive state", "drive", cid.String())
		return purgeOrphanedDriveState(s.cc.Output(), cid, logger)
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)
	if purge {
		return purgeDrive(s.cc.Output(), s.cc.CfgPath, cid, logger)
	}

	return removeDrive(s.cc.Output(), s.cc.CfgPath, cid, cfg.Drives[cid].SyncDir, logger)
}

func (s *driveService) runSearch(ctx context.Context, query string) error {
	logger := s.cc.Logger

	businessTokens := findBusinessTokens(s.cc.Flags.Account, logger)
	if len(businessTokens) == 0 {
		if s.cc.Flags.Account != "" {
			return fmt.Errorf("no business account found for %s — run 'onedrive-go login' first", s.cc.Flags.Account)
		}

		return fmt.Errorf("no business accounts found — SharePoint search requires a business account")
	}

	var results []driveSearchResult
	for _, tokenCID := range businessTokens {
		accountResults := searchAccountSharePoint(ctx, tokenCID, query, logger)
		results = append(results, accountResults...)
	}

	if s.cc.Flags.JSON {
		return printDriveSearchJSON(s.cc.Output(), results)
	}

	return printDriveSearchText(s.cc.Output(), results, query)
}
