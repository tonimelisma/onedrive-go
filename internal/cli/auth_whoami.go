package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
)

func runWhoamiWithContext(ctx context.Context, cc *CLIContext) error {
	logger := cc.Logger
	snapshot, err := loadAccountCatalogSnapshot(ctx, cc)
	if err != nil {
		return err
	}

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	recorder := newAuthProofRecorder(logger)

	// Try the authenticated path: match drive → fetch from Graph API.
	authResult, authErr := fetchAuthenticatedAccount(
		ctx,
		cc,
		snapshot.Config,
		snapshot.Catalog,
		driveSelector,
		logger,
		recorder,
		cc.GraphBaseURL,
		cc.httpProvider(),
	)
	if authErr != nil {
		return authErr
	}

	if authResult.reconciled {
		snapshot, err = loadAccountCatalogSnapshot(ctx, cc)
		if err != nil {
			return err
		}
	}

	// Discover offline auth-required accounts from orphaned account profiles.
	authRequired := whoamiAuthRequired(snapshot, authResult.authenticatedEmail)
	if authResult.authRequired != nil {
		authRequired = mergeAuthRequirements(authRequired, []accountAuthRequirement{*authResult.authRequired})
	}
	degraded := mergeDegradedNotices(authResult.degraded)

	// If no authenticated account and no offline auth-required accounts, give a
	// clean auth-required error instead of a whoami-specific special case.
	if !authResult.hasAuthenticatedAccount && len(authRequired) == 0 && len(degraded) == 0 {
		return graph.ErrNotLoggedIn
	}

	if cc.Flags.JSON {
		return printWhoamiJSON(cc.Output(), authResult.user, authResult.drives, authRequired, degraded)
	}

	return printWhoamiText(cc.Output(), authResult.user, authResult.drives, authRequired, degraded)
}

type authenticatedAccountResult struct {
	user                    *graph.User
	drives                  []graph.Drive
	authenticatedEmail      string
	authRequired            *accountAuthRequirement
	degraded                []accountDegradedNotice
	hasAuthenticatedAccount bool
	reconciled              bool
}

// fetchAuthenticatedAccount attempts to resolve a drive from config, load its
// token, and fetch user/drive info from the Graph API. Returns found=false
// when no authenticated account is available. Returns a non-nil error only
// for hard failures after a token is located.
func fetchAuthenticatedAccount(
	ctx context.Context,
	cc *CLIContext,
	cfg *config.Config,
	catalog []accountCatalogEntry,
	driveSelector string,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	httpProvider *graphhttp.Provider,
) (authenticatedAccountResult, error) {
	cid, found, matchErr := matchAuthenticatedDrive(cfg, driveSelector, logger)
	if matchErr != nil {
		return authenticatedAccountResult{}, matchErr
	}
	if !found {
		return authenticatedAccountResult{}, nil
	}

	accountEmail := cid.Email()
	accountDriveIDs := drivesForAccount(cfg, accountEmail)
	if len(accountDriveIDs) == 0 {
		accountDriveIDs = []driveid.CanonicalID{cid}
	}
	catalogEntry, authRequired := whoamiCatalogContext(ctx, catalog, accountEmail, accountDriveIDs, logger)

	accountAuth := catalogEntry.AuthHealth
	if accountAuth.State == authStateAuthenticationNeeded && accountAuth.Reason != authReasonSyncAuthRejected {
		authRequired.Reason = accountAuth.Reason
		authRequired.Action = accountAuth.Action
		return authenticatedAccountResult{authRequired: &authRequired}, nil
	}

	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return authenticatedAccountResult{authRequired: &authRequired}, nil
	}

	logger.Debug("whoami", "drive", cid.String(), "token_path", tokenPath)

	ts, authResult := whoamiTokenSource(ctx, tokenPath, logger, authRequired)
	if authResult != nil {
		return *authResult, nil
	}
	client, err := newGraphClientWithHTTP(baseURL, httpProvider.BootstrapMeta(), ts, logger)
	if err != nil {
		return authenticatedAccountResult{}, err
	}
	attachAccountAuthProof(client, recorder, accountEmail, "whoami")

	user, authResult, err := fetchWhoamiUser(ctx, client, authRequired)
	if authResult != nil {
		return *authResult, nil
	}
	if err != nil {
		return authenticatedAccountResult{}, err
	}

	reconcileResult, err := cc.reconcileGraphUser(cid, user)
	if err != nil {
		return authenticatedAccountResult{}, err
	}

	driveResult := whoamiDrives(ctx, client, authRequired, user, logger)
	if driveResult.authResult != nil {
		return *driveResult.authResult, nil
	}

	return authenticatedAccountResult{
		user:                    user,
		drives:                  driveResult.drives,
		authenticatedEmail:      user.Email,
		degraded:                driveResult.degraded,
		hasAuthenticatedAccount: true,
		reconciled:              reconcileResult.Changed(),
	}, nil
}

func whoamiTokenSource(
	ctx context.Context,
	tokenPath string,
	logger *slog.Logger,
	authRequired accountAuthRequirement,
) (graph.TokenSource, *authenticatedAccountResult) {
	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err == nil {
		return ts, nil
	}

	switch {
	case errors.Is(err, graph.ErrNotLoggedIn):
		return nil, authRequiredResult(authRequired, authReasonMissingLogin)
	default:
		return nil, authRequiredResult(authRequired, authReasonInvalidSavedLogin)
	}
}

func fetchWhoamiUser(
	ctx context.Context,
	client *graph.Client,
	authRequired accountAuthRequirement,
) (*graph.User, *authenticatedAccountResult, error) {
	user, err := client.Me(ctx)
	if err == nil {
		return user, nil, nil
	}

	if errors.Is(err, graph.ErrUnauthorized) {
		return nil, authRequiredResult(authRequired, authReasonSyncAuthRejected), nil
	}

	return nil, nil, fmt.Errorf("fetching user profile: %w", err)
}

type whoamiDriveCatalogResult struct {
	drives     []graph.Drive
	degraded   []accountDegradedNotice
	authResult *authenticatedAccountResult
}

type whoamiDriveCatalogClient interface {
	Drives(context.Context) ([]graph.Drive, error)
	PrimaryDrive(context.Context) (*graph.Drive, error)
}

func whoamiDrives(
	ctx context.Context,
	client whoamiDriveCatalogClient,
	authRequired accountAuthRequirement,
	user *graph.User,
	logger *slog.Logger,
) whoamiDriveCatalogResult {
	drives, err := client.Drives(ctx)
	if err == nil {
		return whoamiDriveCatalogResult{drives: drives}
	}

	if errors.Is(err, graph.ErrUnauthorized) {
		return whoamiDriveCatalogResult{authResult: authRequiredResult(authRequired, authReasonSyncAuthRejected)}
	}

	notice := driveCatalogDegradedNotice(user.Email, user.DisplayName, authRequired.DriveType)
	logger.Warn("degrading whoami drive discovery after /me/drives failure",
		degradedDiscoveryLogAttrs(user.Email, graphMeDrivesEndpoint, err)...,
	)

	primary, primaryErr := client.PrimaryDrive(ctx)
	if primaryErr == nil {
		notice.DriveType = primary.DriveType
		return whoamiDriveCatalogResult{
			drives:     []graph.Drive{*primary},
			degraded:   []accountDegradedNotice{notice},
			authResult: nil,
		}
	}

	logger.Warn("primary drive fallback unavailable after /me/drives failure",
		"account", user.Email,
		"error", primaryErr,
	)

	return whoamiDriveCatalogResult{
		drives:   nil,
		degraded: []accountDegradedNotice{notice},
	}
}

func authRequiredResult(
	authRequired accountAuthRequirement,
	reason string,
) *authenticatedAccountResult {
	authRequired.Reason = reason
	authRequired.Action = authAction(reason)

	return &authenticatedAccountResult{authRequired: &authRequired}
}

func whoamiCatalogContext(
	ctx context.Context,
	catalog []accountCatalogEntry,
	accountEmail string,
	accountDriveIDs []driveid.CanonicalID,
	logger *slog.Logger,
) (accountCatalogEntry, accountAuthRequirement) {
	catalogEntry, found := catalogEntryByEmail(catalog, accountEmail)
	if !found {
		catalogEntry = accountCatalogEntry{
			DriveType:    accountDriveType(accountDriveIDs),
			DisplayName:  "",
			StateDBCount: len(config.DiscoverStateDBsForEmail(accountEmail, logger)),
			AuthHealth:   inspectAccountAuth(ctx, accountEmail, accountDriveIDs, logger),
		}
		catalogEntry.DisplayName = readAccountDisplayName(accountEmail, accountDriveIDs, logger)
	}

	return catalogEntry, authRequirement(
		accountEmail,
		catalogEntry.DisplayName,
		catalogEntry.DriveType,
		catalogEntry.StateDBCount,
		accountAuthHealth{},
	)
}

func accountDriveType(driveIDs []driveid.CanonicalID) string {
	for _, cid := range driveIDs {
		if cid.DriveType() != driveid.DriveTypeSharePoint {
			return cid.DriveType()
		}
	}

	if len(driveIDs) == 0 {
		return ""
	}

	return driveIDs[0].DriveType()
}

func matchAuthenticatedDrive(
	cfg *config.Config,
	driveSelector string,
	logger *slog.Logger,
) (driveid.CanonicalID, bool, error) {
	if len(cfg.Drives) == 0 {
		logger.Debug("whoami: skipping authenticated account lookup",
			slog.String("selector", driveSelector),
			slog.String("reason", "no configured drives"),
		)

		return driveid.CanonicalID{}, false, nil
	}

	cid, _, matchErr := config.MatchDrive(cfg, driveSelector, logger)
	if matchErr != nil {
		return driveid.CanonicalID{}, false, fmt.Errorf("%w", matchErr)
	}

	return cid, true, nil
}

// findWhoamiAuthRequiredAccounts discovers orphaned account profiles whose
// local saved-login state currently requires user attention. Accounts still in
// config or matching the authenticated email are excluded so the command keeps
// one live account plus a separate auth-required list.
func findWhoamiAuthRequiredAccounts(
	ctx context.Context,
	cfg *config.Config,
	authenticatedEmail string,
	logger *slog.Logger,
) []accountAuthRequirement {
	return whoamiAuthRequiredAccounts(buildAccountCatalog(ctx, cfg, logger), authenticatedEmail)
}

func whoamiAuthRequiredAccounts(catalog []accountCatalogEntry, authenticatedEmail string) []accountAuthRequirement {
	return catalogAuthRequirements(catalog, func(entry accountCatalogEntry) bool {
		if entry.Configured {
			return false
		}
		return entry.Email != authenticatedEmail
	})
}
