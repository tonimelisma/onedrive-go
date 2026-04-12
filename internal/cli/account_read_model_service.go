package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/failures"
)

// accountReadModelSnapshot is the shared offline account/auth projection used
// by status, whoami, and drive-management commands. Keeping config loading,
// warning handling, and catalog construction in one place prevents each
// command family from rebuilding its own auth/account semantics.
type accountReadModelSnapshot struct {
	Config  *config.Config
	Catalog []accountCatalogEntry
}

type driveListReadModelSnapshot struct {
	Configured            []driveListEntry
	Available             []driveListEntry
	AccountsRequiringAuth []accountAuthRequirement
	AccountsDegraded      []accountDegradedNotice
}

func loadLenientCatalog(ctx context.Context, cc *CLIContext) (accountReadModelSnapshot, error) {
	logger := cc.Logger

	cfg, warnings, err := config.LoadOrDefaultLenient(cc.CfgPath, logger)
	outcome := config.ClassifyLoadOutcome(err, warnings)
	if err != nil {
		return accountReadModelSnapshot{}, fmt.Errorf("loading config: %w", err)
	}

	if outcome.Class == failures.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	return accountReadModelSnapshot{
		Config:  cfg,
		Catalog: buildAccountCatalog(ctx, cfg, logger),
	}, nil
}

// loadLenientCatalogWithBestEffortIdentityRefresh refreshes token-backed
// account identity before building the offline account catalog. The account
// read model owns this best-effort freshness step so command handlers and
// downstream discovery helpers do not duplicate `/me` probe logic.
func loadLenientCatalogWithBestEffortIdentityRefresh(
	ctx context.Context,
	cc *CLIContext,
) (accountReadModelSnapshot, error) {
	logger := cc.Logger

	for _, tokenCID := range config.DiscoverTokens(logger) {
		if _, err := cc.probeAccountIdentity(ctx, tokenCID, "account-read-model"); err != nil {
			logger.Debug("skip email reconciliation during account read model refresh",
				"account", tokenCID.String(),
				"error", err,
			)
		}
	}

	return loadLenientCatalog(ctx, cc)
}

func statusAccounts(cc *CLIContext, snapshot accountReadModelSnapshot, history bool) []statusAccount {
	return buildStatusAccountsFromCatalog(snapshot.Config, snapshot.Catalog, &liveSyncStateQuerier{
		logger:        cc.Logger,
		history:       history,
		verbose:       cc.Flags.Verbose,
		examplesLimit: defaultVisiblePaths,
	})
}

func authRequirements(
	snapshot accountReadModelSnapshot,
	include func(accountCatalogEntry) bool,
) []accountAuthRequirement {
	return catalogAuthRequirements(snapshot.Catalog, include)
}

func whoamiAuthRequired(
	snapshot accountReadModelSnapshot,
	authenticatedEmail string,
) []accountAuthRequirement {
	return whoamiAuthRequiredAccounts(snapshot.Catalog, authenticatedEmail)
}

func loadDriveListSnapshot(
	ctx context.Context,
	cc *CLIContext,
	showAll bool,
) (driveListReadModelSnapshot, error) {
	catalogSnapshot, err := loadLenientCatalogWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return driveListReadModelSnapshot{}, err
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
		cc.httpProvider(),
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

	return driveListReadModelSnapshot{
		Configured:            configured,
		Available:             available,
		AccountsRequiringAuth: authRequired,
		AccountsDegraded:      degraded,
	}, nil
}
