package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// accountViewSnapshot is the shared validated account/auth snapshot used by
// status and drive-management commands. Keeping config loading, warning
// handling, and account-view construction in one place prevents each command
// family from rebuilding its own auth/account semantics.
type accountViewSnapshot struct {
	Config        *config.Config
	Stored        *config.Catalog
	ShortcutRoots map[driveid.CanonicalID][]syncshortcut.ShortcutRootStatusView
	Accounts      []accountView
}

type driveListSnapshot struct {
	Configured            []driveListEntry
	Available             []driveListEntry
	AccountsRequiringAuth []accountAuthRequirement
	AccountsDegraded      []accountDegradedNotice
	SharePointGaps        sharePointGaps
}

func loadAccountViewSnapshot(ctx context.Context, cc *CLIContext) (accountViewSnapshot, error) {
	logger := cc.Logger

	validated, warnings, err := config.LoadValidatedState(cc.CfgPath, logger)
	outcome := config.ClassifyLoadOutcome(err, warnings)
	if err != nil {
		return accountViewSnapshot{}, fmt.Errorf("loading config and catalog: %w", err)
	}

	if outcome.Class == errclass.ClassActionable {
		config.LogWarnings(cc.CfgPath, warnings, logger)
	}

	accounts, err := buildAccountViews(ctx, validated.Config, validated.Catalog, logger)
	if err != nil {
		return accountViewSnapshot{}, fmt.Errorf("loading accounts: %w", err)
	}

	return accountViewSnapshot{
		Config:        validated.Config,
		Stored:        validated.Catalog,
		ShortcutRoots: loadShortcutRootStatusSnapshots(ctx, validated.Config, logger),
		Accounts:      accounts,
	}, nil
}

// loadAccountViewSnapshotWithBestEffortIdentityRefresh refreshes token-backed
// account identity, for every account at the same time and under one budget
// (probeAccountIdentities), before building the offline account view. The
// snapshot owns this best-effort freshness step so command handlers and
// downstream discovery helpers do not duplicate `/me` probe logic.
func loadAccountViewSnapshotWithBestEffortIdentityRefresh(
	ctx context.Context,
	cc *CLIContext,
) (accountViewSnapshot, error) {
	logger := cc.Logger

	// Config warnings are reported by the loadAccountViewSnapshot call below;
	// reporting them here too printed every warning twice.
	validated, _, err := config.LoadValidatedState(cc.CfgPath, logger)
	if err != nil {
		return accountViewSnapshot{}, fmt.Errorf("loading config and catalog: %w", err)
	}

	var tokenCIDs []driveid.CanonicalID
	for _, key := range validated.Catalog.SortedAccountKeys() {
		account := validated.Catalog.Accounts[key]
		if tokenCID, hasID := account.ID(); hasID {
			tokenCIDs = append(tokenCIDs, tokenCID)
		}
	}
	_, errs := cc.probeAccountIdentities(ctx, tokenCIDs, "account-view")
	for i, err := range errs {
		if err != nil {
			logger.Debug("skip email reconciliation during account view refresh",
				"account", tokenCIDs[i].String(),
				"error", err,
			)
		}
	}

	return loadAccountViewSnapshot(ctx, cc)
}

func statusAccounts(ctx context.Context, cc *CLIContext, snapshot accountViewSnapshot) []statusAccount {
	return buildStatusAccountsFromViews(ctx, snapshot.Config, snapshot.ShortcutRoots, snapshot.Accounts, &liveSyncStateQuerier{
		logger:        cc.Logger,
		verbose:       cc.Flags.Verbose,
		examplesLimit: defaultVisiblePaths,
	})
}

func authRequirements(
	snapshot accountViewSnapshot,
	include func(accountView) bool,
) []accountAuthRequirement {
	return accountViewAuthRequirements(snapshot.Accounts, include)
}

func loadDriveListSnapshot(
	ctx context.Context,
	cc *CLIContext,
	showAll bool,
) (driveListSnapshot, error) {
	viewSnapshot, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return driveListSnapshot{}, err
	}

	logger := cc.Logger
	configured := buildConfiguredDriveEntries(ctx, viewSnapshot.Config, &liveSyncStateQuerier{
		logger:        logger,
		examplesLimit: defaultVisiblePaths,
	}, logger)
	configuredAuth := accountViewAuthByDrive(viewSnapshot.Accounts)
	annotateConfiguredDriveAuth(configured, configuredAuth)

	siteLimit := sharePointSiteLimit
	if showAll {
		siteLimit = sharePointSiteUnlimited
	}

	// The drive discovery and the shared search run side by side. Each
	// guards its own accounts' panics; a panic outside them is the whole
	// discovery's failure, reported for every account as a failed one is.
	// The runtime is built here, on the command goroutine, and handed to the
	// discoveries, which run on goroutines of their own (CLIContext.runtime).
	runtime := cc.runtime()
	recorder := newAuthProofRecorder(logger)
	var (
		available              []driveListEntry
		discoveredAuthRequired []accountAuthRequirement
		discoveredDegraded     []accountDegradedNotice
		gaps                   sharePointGaps
		sharedDiscovery        sharedDiscoveryResult
	)
	discoveries := []func(){
		func() {
			available, discoveredAuthRequired, discoveredDegraded, gaps = discoverAvailableDrives(
				ctx,
				viewSnapshot.Config,
				viewSnapshot.Accounts,
				siteLimit,
				logger,
				recorder,
				runtime,
			)
		},
		func() { sharedDiscovery = discoverSharedTargets(ctx, cc, runtime, viewSnapshot.Accounts) },
	}
	discoveryFailed := []func(){
		func() {
			for _, tokenCID := range accountViewTokenIDs(viewSnapshot.Accounts) {
				discoveredDegraded = append(discoveredDegraded,
					tokenDriveCatalogDegradedNotice(viewSnapshot.Accounts, tokenCID, logger))
			}
		},
		func() {
			for i := range viewSnapshot.Accounts {
				entry := &viewSnapshot.Accounts[i]
				sharedDiscovery.AccountsDegraded = append(sharedDiscovery.AccountsDegraded,
					sharedDiscoveryDegradedNotice(entry.Email, entry.DisplayName, entry.DriveType))
			}
		},
	}
	runBounded(ctx, logger, len(discoveries),
		func(int) string { return "while discovering drives" },
		func(i int) { discoveries[i]() },
		func(i int) { discoveryFailed[i]() },
	)
	available = append(available, sharedFoldersToEntries(projectSharedFolders(viewSnapshot.Config, sharedDiscovery.Targets))...)
	slices.SortFunc(available, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})
	if err := annotateStateDB(available); err != nil {
		return driveListSnapshot{}, err
	}

	authRequired := mergeAuthRequirements(
		authRequirements(viewSnapshot, func(accountView) bool {
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
		SharePointGaps:        gaps,
	}, nil
}
