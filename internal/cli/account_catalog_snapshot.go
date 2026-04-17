package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// accountCatalogSnapshot is the shared offline account/auth snapshot used by
// status, whoami, and drive-management commands. Keeping config loading,
// warning handling, and catalog construction in one place prevents each
// command family from rebuilding its own auth/account semantics.
type accountCatalogSnapshot struct {
	Config  *config.Config
	Stored  *config.Catalog
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

	if outcome.Class == errclass.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	stored, err := config.LoadCatalog()
	if err != nil {
		return accountCatalogSnapshot{}, fmt.Errorf("loading catalog: %w", err)
	}
	if err := validateConfiguredDrivesInCatalog(cfg, stored); err != nil {
		return accountCatalogSnapshot{}, err
	}
	if err := validateCatalogDriveOwners(stored); err != nil {
		return accountCatalogSnapshot{}, err
	}
	if err := validatePrimaryDriveOwnership(stored); err != nil {
		return accountCatalogSnapshot{}, err
	}

	return accountCatalogSnapshot{
		Config:  cfg,
		Stored:  stored,
		Catalog: buildAccountCatalogWithStored(ctx, cfg, stored, logger),
	}, nil
}

func validateConfiguredDrivesInCatalog(cfg *config.Config, stored *config.Catalog) error {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if stored == nil {
		stored = config.DefaultCatalog()
	}

	for cid := range cfg.Drives {
		_, found := stored.DriveByCanonicalID(cid)
		if !found {
			return fmt.Errorf("catalog invariant: configured drive %s has no catalog entry", cid)
		}
	}

	return validateCatalogDriveOwners(stored)
}

func validateCatalogDriveOwners(stored *config.Catalog) error {
	if stored == nil {
		stored = config.DefaultCatalog()
	}

	for _, key := range stored.SortedDriveKeys() {
		drive := stored.Drives[key]
		if drive.OwnerAccountCanonical == "" {
			return fmt.Errorf("catalog invariant: drive %s has no owning account", drive.CanonicalID)
		}
		ownerCID, err := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
		if err != nil {
			return fmt.Errorf("catalog invariant: drive %s has malformed owning account %q: %w", drive.CanonicalID, drive.OwnerAccountCanonical, err)
		}
		if _, found := stored.AccountByCanonicalID(ownerCID); !found {
			return fmt.Errorf("catalog invariant: drive %s owner %s is missing from the catalog", drive.CanonicalID, ownerCID)
		}
	}

	return nil
}

func validatePrimaryDriveOwnership(stored *config.Catalog) error {
	if stored == nil {
		stored = config.DefaultCatalog()
	}

	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		if account.PrimaryDriveCanonical == "" {
			continue
		}
		primaryCID, err := driveid.NewCanonicalID(account.PrimaryDriveCanonical)
		if err != nil {
			return fmt.Errorf(
				"catalog invariant: account %s has malformed primary drive %q: %w",
				account.CanonicalID,
				account.PrimaryDriveCanonical,
				err,
			)
		}
		drive, found := stored.DriveByCanonicalID(primaryCID)
		if !found {
			return fmt.Errorf("catalog invariant: account %s primary drive %s is missing from the catalog", account.CanonicalID, primaryCID)
		}
		if drive.OwnerAccountCanonical != account.CanonicalID {
			return fmt.Errorf(
				"catalog invariant: account %s primary drive %s is owned by %s",
				account.CanonicalID,
				primaryCID,
				drive.OwnerAccountCanonical,
			)
		}
	}

	return nil
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

	stored, err := config.LoadCatalog()
	if err != nil {
		return accountCatalogSnapshot{}, fmt.Errorf("loading catalog: %w", err)
	}

	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		tokenCID, err := driveid.NewCanonicalID(account.CanonicalID)
		if err != nil {
			logger.Debug("skip malformed catalog account during identity refresh",
				"canonical_id", account.CanonicalID,
				"error", err,
			)
			continue
		}
		if _, err := cc.probeAccountIdentity(ctx, tokenCID, "account-catalog"); err != nil {
			logger.Debug("skip email reconciliation during account catalog refresh",
				"account", account.CanonicalID,
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
