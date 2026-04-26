package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// accountViewSnapshot is the shared validated account/auth snapshot used by
// status and drive-management commands. Keeping config loading, warning
// handling, and account-view construction in one place prevents each command
// family from rebuilding its own auth/account semantics.
type accountViewSnapshot struct {
	Config        *config.Config
	Stored        *config.Catalog
	ShortcutRoots map[driveid.CanonicalID][]syncengine.ShortcutRootRecord
	Accounts      []accountView
}

type driveListSnapshot struct {
	Configured            []driveListEntry
	Available             []driveListEntry
	AccountsRequiringAuth []accountAuthRequirement
	AccountsDegraded      []accountDegradedNotice
}

func loadAccountViewSnapshot(ctx context.Context, cc *CLIContext) (accountViewSnapshot, error) {
	logger := cc.Logger

	validated, warnings, err := config.LoadValidatedState(cc.CfgPath, true, logger)
	outcome := config.ClassifyLoadOutcome(err, warnings)
	if err != nil {
		return accountViewSnapshot{}, fmt.Errorf("loading config: %w", err)
	}

	if outcome.Class == errclass.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	return accountViewSnapshot{
		Config:        validated.Config,
		Stored:        validated.Catalog,
		ShortcutRoots: loadShortcutRootStatusSnapshots(ctx, validated.Config, logger),
		Accounts:      buildAccountViews(ctx, validated.Config, validated.Catalog, logger),
	}, nil
}

// loadAccountViewSnapshotWithBestEffortIdentityRefresh refreshes token-backed
// account identity before building the offline account view. The snapshot owns
// this best-effort freshness step so command handlers and downstream discovery
// helpers do not duplicate `/me` probe logic.
func loadAccountViewSnapshotWithBestEffortIdentityRefresh(
	ctx context.Context,
	cc *CLIContext,
) (accountViewSnapshot, error) {
	logger := cc.Logger

	validated, warnings, err := config.LoadValidatedState(cc.CfgPath, true, logger)
	if err != nil {
		return accountViewSnapshot{}, fmt.Errorf("loading config: %w", err)
	}

	if outcome := config.ClassifyLoadOutcome(err, warnings); outcome.Class == errclass.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	for _, key := range validated.Catalog.SortedAccountKeys() {
		account := validated.Catalog.Accounts[key]
		tokenCID, err := driveid.NewCanonicalID(account.CanonicalID)
		if err != nil {
			logger.Debug("skip malformed catalog account during identity refresh",
				"canonical_id", account.CanonicalID,
				"error", err,
			)
			continue
		}
		if _, err := cc.probeAccountIdentity(ctx, tokenCID, "account-view"); err != nil {
			logger.Debug("skip email reconciliation during account view refresh",
				"account", account.CanonicalID,
				"error", err,
			)
		}
	}

	return loadAccountViewSnapshot(ctx, cc)
}

func statusAccounts(cc *CLIContext, snapshot accountViewSnapshot, history bool) []statusAccount {
	return buildStatusAccountsFromViews(snapshot.Config, snapshot.ShortcutRoots, snapshot.Accounts, &liveSyncStateQuerier{
		logger:        cc.Logger,
		history:       history,
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
	configured := buildConfiguredDriveEntries(viewSnapshot.Config, logger)
	configuredAuth := accountViewAuthByEmail(viewSnapshot.Accounts)
	annotateConfiguredDriveAuth(configured, configuredAuth)

	siteLimit := sharePointSiteLimit
	if showAll {
		siteLimit = sharePointSiteUnlimited
	}

	recorder := newAuthProofRecorder(logger)
	available, discoveredAuthRequired, discoveredDegraded := discoverAvailableDrives(
		ctx,
		viewSnapshot.Config,
		viewSnapshot.Accounts,
		siteLimit,
		logger,
		recorder,
		cc.GraphBaseURL,
		cc.runtime(),
	)
	sharedDiscovery := discoverSharedTargets(ctx, cc, viewSnapshot.Accounts)
	available = append(available, sharedFoldersToEntries(projectSharedFolders(viewSnapshot.Config, sharedDiscovery.Targets))...)
	slices.SortFunc(available, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})
	annotateStateDB(available)

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
	}, nil
}
