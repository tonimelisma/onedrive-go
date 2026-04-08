package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

type driveService struct {
	cc *CLIContext
}

func newDriveService(cc *CLIContext) *driveService {
	return &driveService{cc: cc}
}

func (s *driveService) runList(ctx context.Context, showAll bool) error {
	logger := s.cc.Logger
	recorder := newAuthProofRecorder(logger)

	for _, tokenCID := range config.DiscoverTokens(logger) {
		if _, err := s.cc.probeAccountIdentity(ctx, tokenCID, "drive-list-bootstrap"); err != nil {
			logger.Debug("skip email reconciliation during drive list bootstrap",
				"account", tokenCID.String(),
				"error", err,
			)
		}
	}

	readModel := newAccountReadModelService(s.cc)
	snapshot, err := readModel.loadLenientCatalog(ctx)
	if err != nil {
		return err
	}

	configured := buildConfiguredDriveEntries(snapshot.Config, logger)
	configuredAuth := catalogAuthByEmail(snapshot.Catalog)
	annotateConfiguredDriveAuth(configured, configuredAuth)

	siteLimit := sharePointSiteLimit
	if showAll {
		siteLimit = sharePointSiteUnlimited
	}

	available, discoveredAuthRequired, discoveredDegraded := discoverAvailableDrives(
		ctx,
		snapshot.Config,
		snapshot.Catalog,
		siteLimit,
		logger,
		recorder,
		s.cc.GraphBaseURL,
		s.cc.httpProvider(),
	)
	sharedDiscovery := newSharedDiscoveryService(s.cc).discoverTargets(ctx, snapshot.Catalog)
	available = append(available, sharedFoldersToEntries(projectSharedFolders(snapshot.Config, sharedDiscovery.Targets))...)
	slices.SortFunc(available, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})
	annotateStateDB(available)
	authRequired := mergeAuthRequirements(readModel.authRequirements(snapshot, func(entry accountCatalogEntry) bool {
		return true
	}), discoveredAuthRequired, sharedDiscovery.AccountsRequiringAuth)
	degraded := mergeDegradedNotices(discoveredDegraded, sharedDiscovery.AccountsDegraded)

	if s.cc.Flags.JSON {
		return printDriveListJSON(s.cc.Output(), configured, available, authRequired, degraded)
	}

	return printDriveListText(s.cc.Output(), configured, available, authRequired, degraded)
}

func (s *driveService) runAdd(ctx context.Context, args []string) error {
	logger := s.cc.Logger

	if s.cc.SharedTarget != nil {
		item, _, err := s.cc.resolveSharedItem(ctx)
		if err != nil {
			return err
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		cid, err := driveid.NewCanonicalID(s.cc.SharedTarget.Selector())
		if err != nil {
			return fmt.Errorf("parse shared drive identity: %w", err)
		}

		return addSharedDrive(ctx, s.cc.CfgPath, s.cc.Output(), cid, "", logger, s.cc.httpProvider())
	}

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

		return addSharedDriveByName(ctx, s.cc, selector)
	}

	if cid.IsShared() {
		clients, err := s.cc.sharedTargetClients(ctx, sharedref.Ref{
			AccountEmail:  cid.Email(),
			RemoteDriveID: cid.SourceDriveID(),
			RemoteItemID:  cid.SourceItemID(),
		})
		if err != nil {
			return err
		}

		item, err := clients.Meta.GetItem(ctx, driveid.New(cid.SourceDriveID()), cid.SourceItemID())
		if err != nil {
			return fmt.Errorf("loading shared item: %w", err)
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		return addSharedDrive(ctx, s.cc.CfgPath, s.cc.Output(), cid, "", logger, s.cc.httpProvider())
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
	recorder := newAuthProofRecorder(logger)

	for _, tokenCID := range config.DiscoverTokens(logger) {
		if _, err := s.cc.probeAccountIdentity(ctx, tokenCID, "drive-search-bootstrap"); err != nil {
			logger.Debug("skip email reconciliation during drive search bootstrap",
				"account", tokenCID.String(),
				"error", err,
			)
		}
	}

	readModel := newAccountReadModelService(s.cc)
	snapshot, err := readModel.loadLenientCatalog(ctx)
	if err != nil {
		return err
	}

	businessTokens := searchableBusinessTokenIDs(snapshot.Catalog, s.cc.Flags.Account)
	businessAuthRequired := readModel.authRequirements(snapshot, func(entry accountCatalogEntry) bool {
		if s.cc.Flags.Account != "" && entry.Email != s.cc.Flags.Account {
			return false
		}
		return entry.DriveType == driveid.DriveTypeBusiness
	})
	if len(businessTokens) == 0 && len(businessAuthRequired) == 0 {
		if s.cc.Flags.Account != "" {
			return fmt.Errorf("no business account found for %s — run 'onedrive-go login' first", s.cc.Flags.Account)
		}

		return fmt.Errorf("no business accounts found — SharePoint search requires a business account")
	}

	var (
		results                []driveSearchResult
		discoveredAuthRequired []accountAuthRequirement
	)
	for _, tokenCID := range businessTokens {
		accountResults, accountAuthRequired := searchAccountSharePoint(
			ctx,
			tokenCID,
			query,
			logger,
			recorder,
			s.cc.GraphBaseURL,
			s.cc.httpProvider(),
		)
		results = append(results, accountResults...)
		discoveredAuthRequired = append(discoveredAuthRequired, accountAuthRequired...)
	}
	authRequired := mergeAuthRequirements(businessAuthRequired, discoveredAuthRequired)

	if s.cc.Flags.JSON {
		return printDriveSearchJSON(s.cc.Output(), results, authRequired)
	}

	return printDriveSearchText(s.cc.Output(), results, query, authRequired)
}
