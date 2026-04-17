package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func runLoginWithContext(ctx context.Context, cc *CLIContext, useBrowser bool) error {
	logger := cc.Logger
	logger.Info("login started", slog.Bool("browser", useBrowser))

	tempPath := pendingTokenPath()
	ts, err := authenticateLogin(ctx, cc, useBrowser, tempPath)
	if err != nil {
		cleanupPendingToken(cc, tempPath, "login failure")
		return fmt.Errorf("authenticate account: %w", err)
	}

	canonicalID, user, orgName, primaryDriveID, err := discoverAccount(ctx, ts, logger, cc.runtime())
	if err != nil {
		cleanupPendingToken(cc, tempPath, "discovery failure")
		return fmt.Errorf("discovering account: %w", err)
	}

	if _, reconcileErr := cc.reconcileGraphUser(canonicalID, user); reconcileErr != nil {
		cleanupPendingToken(cc, tempPath, "reconciliation failure")
		return reconcileErr
	}

	finalTokenPath := config.DriveTokenPath(canonicalID)
	if finalTokenPath == "" {
		cleanupPendingToken(cc, tempPath, "path resolution failure")
		return fmt.Errorf("cannot determine token path for drive %q", canonicalID.String())
	}

	if moveErr := moveToken(tempPath, finalTokenPath); moveErr != nil {
		return moveErr
	}

	persistLoginMetadata(canonicalID, user, orgName, primaryDriveID, logger)

	email := canonicalID.Email()
	syncDir, added, err := config.EnsureDriveInConfig(cc.CfgPath, canonicalID, logger)
	if err != nil {
		return fmt.Errorf("configuring drive: %w", err)
	}

	clearLoginAuthRequirement(ctx, email, logger)

	if !added {
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		return writef(cc.Output(), "Token refreshed for %s.\n", email)
	}

	return printLoginSuccess(cc.Output(), canonicalID.DriveType(), email, orgName, canonicalID.String(), syncDir)
}

func persistLoginMetadata(
	canonicalID driveid.CanonicalID,
	user *graph.User,
	orgName string,
	primaryDriveID driveid.ID,
	logger *slog.Logger,
) {
	if catalogErr := config.UpdateCatalog(func(catalog *config.Catalog) error {
		account := buildLoginCatalogAccount(canonicalID, user, orgName, primaryDriveID, catalog)
		drive := buildLoginCatalogDrive(canonicalID, primaryDriveID, catalog)
		drive.CachedAt = time.Now().UTC().Format(time.RFC3339)
		catalog.UpsertAccount(&account)
		catalog.UpsertDrive(&drive)
		return nil
	}); catalogErr != nil {
		logger.Warn("failed to update catalog after login", "error", catalogErr)
	}
}

func buildLoginCatalogAccount(
	canonicalID driveid.CanonicalID,
	user *graph.User,
	orgName string,
	primaryDriveID driveid.ID,
	catalog *config.Catalog,
) config.CatalogAccount {
	account := config.CatalogAccount{
		CanonicalID:           canonicalID.String(),
		Email:                 canonicalID.Email(),
		DriveType:             canonicalID.DriveType(),
		UserID:                user.ID,
		DisplayName:           user.DisplayName,
		OrgName:               orgName,
		PrimaryDriveID:        primaryDriveID.String(),
		PrimaryDriveCanonical: canonicalID.String(),
	}
	if existing, found := catalog.AccountByCanonicalID(canonicalID); found {
		account.AuthRequirementReason = existing.AuthRequirementReason
	}
	return account
}

func buildLoginCatalogDrive(
	canonicalID driveid.CanonicalID,
	primaryDriveID driveid.ID,
	catalog *config.Catalog,
) config.CatalogDrive {
	drive := config.CatalogDrive{
		CanonicalID:           canonicalID.String(),
		OwnerAccountCanonical: canonicalID.String(),
		DriveType:             canonicalID.DriveType(),
		DisplayName:           config.DefaultDisplayName(canonicalID),
		PrimaryForAccount:     true,
		RemoteDriveID:         primaryDriveID.String(),
	}
	if existing, found := catalog.DriveByCanonicalID(canonicalID); found {
		drive = existing
		drive.OwnerAccountCanonical = canonicalID.String()
		drive.DriveType = canonicalID.DriveType()
		drive.PrimaryForAccount = true
		drive.RemoteDriveID = primaryDriveID.String()
	}
	return drive
}

func clearLoginAuthRequirement(ctx context.Context, email string, logger *slog.Logger) {
	if clearErr := clearAccountAuthRequirementForSource(ctx, email, config.AuthClearSourceLogin, logger); clearErr != nil {
		logger.Warn("clearing stale account auth requirement after login", "account", email, "error", clearErr)
	}
}

func authenticateLogin(
	ctx context.Context,
	cc *CLIContext,
	useBrowser bool,
	tempPath string,
) (graph.TokenSource, error) {
	if useBrowser {
		ts, err := graph.LoginWithBrowser(ctx, tempPath, openBrowser, cc.Logger)
		if err != nil {
			return nil, fmt.Errorf("browser login: %w", err)
		}

		return ts, nil
	}

	ts, err := graph.Login(ctx, tempPath, func(da graph.DeviceAuth) {
		writeWarningf(cc.Status(), "To sign in, visit: %s\n", da.VerificationURI)
		writeWarningf(cc.Status(), "Enter code: %s\n", da.UserCode)
	}, cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("device-code login: %w", err)
	}

	return ts, nil
}

func cleanupPendingToken(cc *CLIContext, path string, reason string) {
	if cleanupErr := removePathIfExists(path); cleanupErr != nil {
		cc.Logger.Warn("failed to remove pending token", "reason", reason, "path", path, "error", cleanupErr)
	}
}

func runLogoutWithContext(cc *CLIContext, purge bool) error {
	logger := cc.Logger

	validated, warnings, err := config.LoadValidatedState(cc.CfgPath, true, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if outcome := config.ClassifyLoadOutcome(err, warnings); outcome.Class == errclass.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	account, autoErr := resolveLogoutAccountWithCatalog(
		validated.Config,
		validated.Catalog,
		cc.Flags.Account,
		purge,
		logger,
	)
	if autoErr != nil {
		return autoErr
	}

	logger.Info("logout started", "account", account, "purge", purge)
	return executeLogout(validated.Config, validated.Catalog, cc.CfgPath, cc.Output(), account, purge, logger)
}
