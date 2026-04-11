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

type accountReadModelService struct {
	cc *CLIContext
}

func newAccountReadModelService(cc *CLIContext) *accountReadModelService {
	return &accountReadModelService{cc: cc}
}

func (s *accountReadModelService) loadLenientCatalog(ctx context.Context) (accountReadModelSnapshot, error) {
	logger := s.cc.Logger

	cfg, warnings, err := config.LoadOrDefaultLenient(s.cc.CfgPath, logger)
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
func (s *accountReadModelService) loadLenientCatalogWithBestEffortIdentityRefresh(
	ctx context.Context,
) (accountReadModelSnapshot, error) {
	logger := s.cc.Logger

	for _, tokenCID := range config.DiscoverTokens(logger) {
		if _, err := s.cc.probeAccountIdentity(ctx, tokenCID, "account-read-model"); err != nil {
			logger.Debug("skip email reconciliation during account read model refresh",
				"account", tokenCID.String(),
				"error", err,
			)
		}
	}

	return s.loadLenientCatalog(ctx)
}

func (s *accountReadModelService) statusAccounts(snapshot accountReadModelSnapshot, history bool) []statusAccount {
	return buildStatusAccountsFromCatalog(snapshot.Config, snapshot.Catalog, &liveSyncStateQuerier{
		logger:        s.cc.Logger,
		history:       history,
		verbose:       s.cc.Flags.Verbose,
		examplesLimit: defaultVisiblePaths,
	})
}

func (s *accountReadModelService) authRequirements(
	snapshot accountReadModelSnapshot,
	include func(accountCatalogEntry) bool,
) []accountAuthRequirement {
	return catalogAuthRequirements(snapshot.Catalog, include)
}

func (s *accountReadModelService) whoamiAuthRequired(
	snapshot accountReadModelSnapshot,
	authenticatedEmail string,
) []accountAuthRequirement {
	return whoamiAuthRequiredAccounts(snapshot.Catalog, authenticatedEmail)
}

func (s *accountReadModelService) loadDriveListSnapshot(
	ctx context.Context,
	showAll bool,
) (driveListReadModelSnapshot, error) {
	catalogSnapshot, err := s.loadLenientCatalogWithBestEffortIdentityRefresh(ctx)
	if err != nil {
		return driveListReadModelSnapshot{}, err
	}

	logger := s.cc.Logger
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
		s.cc.GraphBaseURL,
		s.cc.httpProvider(),
	)
	sharedDiscovery := newSharedDiscoveryService(s.cc).discoverTargets(ctx, catalogSnapshot.Catalog)
	available = append(available, sharedFoldersToEntries(projectSharedFolders(catalogSnapshot.Config, sharedDiscovery.Targets))...)
	slices.SortFunc(available, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})
	annotateStateDB(available)

	authRequired := mergeAuthRequirements(
		s.authRequirements(catalogSnapshot, func(accountCatalogEntry) bool {
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
