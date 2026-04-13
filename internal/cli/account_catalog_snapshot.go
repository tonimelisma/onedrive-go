package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/failures"
)

// accountCatalogSnapshot is the shared offline account/auth snapshot used by
// status, whoami, and drive-management commands. Keeping config loading,
// warning handling, and catalog construction in one place prevents each
// command family from rebuilding its own auth/account semantics.
type accountCatalogSnapshot struct {
	Config  *config.Config
	Catalog []accountCatalogEntry
}

type driveListSnapshot struct {
	Configured            []driveListEntry
	Available             []driveListEntry
	AccountsRequiringAuth []accountAuthRequirement
	AccountsDegraded      []accountDegradedNotice
}

func loadAccountCatalogSnapshot(ctx context.Context, cc *CLIContext) (accountCatalogSnapshot, error) {
	logger := cc.Logger

	cfg, warnings, err := config.LoadOrDefaultLenient(cc.CfgPath, logger)
	outcome := config.ClassifyLoadOutcome(err, warnings)
	if err != nil {
		return accountCatalogSnapshot{}, fmt.Errorf("loading config: %w", err)
	}

	if outcome.Class == failures.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	return accountCatalogSnapshot{
		Config:  cfg,
		Catalog: buildAccountCatalog(ctx, cfg, logger),
	}, nil
}

// loadAccountCatalogSnapshotWithBestEffortIdentityRefresh refreshes token-backed
// account identity before building the offline account catalog. The account
// catalog snapshot owns this best-effort freshness step so command handlers and
// downstream discovery helpers do not duplicate `/me` probe logic.
func loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(
	ctx context.Context,
	cc *CLIContext,
) (accountCatalogSnapshot, error) {
	logger := cc.Logger

	for _, tokenCID := range config.DiscoverTokens(logger) {
		if _, err := cc.probeAccountIdentity(ctx, tokenCID, "account-catalog"); err != nil {
			logger.Debug("skip email reconciliation during account catalog refresh",
				"account", tokenCID.String(),
				"error", err,
			)
		}
	}

	return loadAccountCatalogSnapshot(ctx, cc)
}

func statusAccounts(cc *CLIContext, snapshot accountCatalogSnapshot, history bool) []statusAccount {
	return buildStatusAccountsFromCatalog(snapshot.Config, snapshot.Catalog, &liveSyncStateQuerier{
		logger:        cc.Logger,
		history:       history,
		verbose:       cc.Flags.Verbose,
		examplesLimit: defaultVisiblePaths,
	})
}

func authRequirements(
	snapshot accountCatalogSnapshot,
	include func(accountCatalogEntry) bool,
) []accountAuthRequirement {
	return catalogAuthRequirements(snapshot.Catalog, include)
}

func whoamiAuthRequired(
	snapshot accountCatalogSnapshot,
	authenticatedEmail string,
) []accountAuthRequirement {
	return whoamiAuthRequiredAccounts(snapshot.Catalog, authenticatedEmail)
}

func loadDriveListSnapshot(
	ctx context.Context,
	cc *CLIContext,
	showAll bool,
) (driveListSnapshot, error) {
	catalogSnapshot, err := loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return driveListSnapshot{}, err
	}

	logger := cc.Logger
	configured := buildConfiguredDriveEntries(catalogSnapshot.Config, logger)
	configuredAuth := catalogAuthByEmail(catalogSnapshot.Catalog)
	annotateConfiguredDriveAuth(configured, configuredAuth)

	siteLimit := sharePointSiteLimit
	if showAll {
		siteLimit = sharePointSiteUnlimited
	}

	recorder := newAuthProofRecorder(logger)
	available, discoveredAuthRequired, discoveredDegraded := discoverAvailableDrives(
		ctx,
		catalogSnapshot.Config,
		catalogSnapshot.Catalog,
		siteLimit,
		logger,
		recorder,
		cc.GraphBaseURL,
		cc.runtime(),
	)
	sharedDiscovery := discoverSharedTargets(ctx, cc, catalogSnapshot.Catalog)
	available = append(available, sharedFoldersToEntries(projectSharedFolders(catalogSnapshot.Config, sharedDiscovery.Targets))...)
	slices.SortFunc(available, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})
	annotateStateDB(available)

	authRequired := mergeAuthRequirements(
		authRequirements(catalogSnapshot, func(accountCatalogEntry) bool {
			return true
		}),
		discoveredAuthRequired,
		sharedDiscovery.AccountsRequiringAuth,
	)
	degraded := mergeDegradedNotices(discoveredDegraded, sharedDiscovery.AccountsDegraded)

	return driveListSnapshot{
		Configured:            configured,
		Available:             available,
		AccountsRequiringAuth: authRequired,
		AccountsDegraded:      degraded,
	}, nil
}
