package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

func runDriveListWithContext(ctx context.Context, cc *CLIContext, showAll bool) error {
	snapshot, err := loadDriveListSnapshot(ctx, cc, showAll)
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		return printDriveListJSON(
			cc.Output(),
			snapshot.Configured,
			snapshot.Available,
			snapshot.AccountsRequiringAuth,
			snapshot.AccountsDegraded,
		)
	}

	return printDriveListText(
		cc.Output(),
		snapshot.Configured,
		snapshot.Available,
		snapshot.AccountsRequiringAuth,
		snapshot.AccountsDegraded,
	)
}

func runDriveAddWithContext(ctx context.Context, cc *CLIContext, args []string) error {
	logger := cc.Logger

	if cc.SharedTarget != nil {
		item, _, err := cc.resolveSharedItem(ctx)
		if err != nil {
			return err
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		cid, err := driveid.NewCanonicalID(cc.SharedTarget.Selector())
		if err != nil {
			return fmt.Errorf("parse shared drive identity: %w", err)
		}

		return addSharedDrive(ctx, cc.CfgPath, cc.Output(), cid, "", logger, cc.httpProvider())
	}

	selector := ""
	if len(args) > 0 {
		selector = args[0]
	}

	if selector == "" {
		var driveErr error
		selector, driveErr = cc.Flags.SingleDrive()
		if driveErr != nil {
			return driveErr
		}
	}

	if selector == "" {
		return listAvailableDrives(cc.Output())
	}

	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		if strings.Contains(selector, ":") {
			return fmt.Errorf("invalid canonical ID %q: %w\n"+
				"Run 'onedrive-go drive list' to see valid canonical IDs", selector, err)
		}

		return addSharedDriveByName(ctx, cc, selector)
	}

	if cid.IsShared() {
		clients, err := cc.sharedTargetClients(ctx, sharedref.Ref{
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

		return addSharedDrive(ctx, cc.CfgPath, cc.Output(), cid, "", logger, cc.httpProvider())
	}

	return addNewDrive(cc.Output(), cc.CfgPath, cid, logger)
}

func runDriveRemoveWithContext(cc *CLIContext, purge bool) error {
	logger := cc.Logger

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive to remove)")
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, logger)
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
		return purgeOrphanedDriveState(cc.Output(), cid, logger)
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)
	if purge {
		return purgeDrive(cc.Output(), cc.CfgPath, cid, logger)
	}

	return removeDrive(cc.Output(), cc.CfgPath, cid, cfg.Drives[cid].SyncDir, logger)
}

func runDriveSearchWithContext(ctx context.Context, cc *CLIContext, query string) error {
	logger := cc.Logger
	recorder := newAuthProofRecorder(logger)

	snapshot, err := loadLenientCatalogWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return err
	}

	businessTokens := searchableBusinessTokenIDs(snapshot.Catalog, cc.Flags.Account)
	businessAuthRequired := authRequirements(snapshot, func(entry accountCatalogEntry) bool {
		if cc.Flags.Account != "" && entry.Email != cc.Flags.Account {
			return false
		}
		return entry.DriveType == driveid.DriveTypeBusiness
	})
	if len(businessTokens) == 0 && len(businessAuthRequired) == 0 {
		if cc.Flags.Account != "" {
			return fmt.Errorf("no business account found for %s — run 'onedrive-go login' first", cc.Flags.Account)
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
			cc.GraphBaseURL,
			cc.httpProvider(),
		)
		results = append(results, accountResults...)
		discoveredAuthRequired = append(discoveredAuthRequired, accountAuthRequired...)
	}
	authRequired := mergeAuthRequirements(businessAuthRequired, discoveredAuthRequired)

	if cc.Flags.JSON {
		return printDriveSearchJSON(cc.Output(), results, authRequired)
	}

	return printDriveSearchText(cc.Output(), results, query, authRequired)
}
