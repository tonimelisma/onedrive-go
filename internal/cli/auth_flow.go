package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
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

	now := time.Now().UTC().Format(time.RFC3339)
	if profileErr := config.SaveAccountProfile(canonicalID, &config.AccountProfile{
		UserID:         user.ID,
		DisplayName:    user.DisplayName,
		OrgName:        orgName,
		PrimaryDriveID: primaryDriveID.String(),
	}); profileErr != nil {
		logger.Warn("failed to save account profile", "error", profileErr)
	}

	if driveMetaErr := config.SaveDriveMetadata(canonicalID, &config.DriveMetadata{
		DriveID:  primaryDriveID.String(),
		CachedAt: now,
	}); driveMetaErr != nil {
		logger.Warn("failed to save drive metadata", "error", driveMetaErr)
	}

	email := canonicalID.Email()
	syncDir, added, err := config.EnsureDriveInConfig(cc.CfgPath, canonicalID, logger)
	if err != nil {
		return fmt.Errorf("configuring drive: %w", err)
	}

	if clearErr := clearAccountAuthScopes(ctx, email, logger); clearErr != nil {
		logger.Warn("clearing stale auth scopes after login", "account", email, "error", clearErr)
	}

	if !added {
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		return writef(cc.Output(), "Token refreshed for %s.\n", email)
	}

	return printLoginSuccess(cc.Output(), canonicalID.DriveType(), email, orgName, canonicalID.String(), syncDir)
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

	cfg, loadErr := config.LoadOrDefault(cc.CfgPath, logger)
	if loadErr != nil {
		logger.Warn("failed to load config, proceeding with --account only", "error", loadErr)
		cfg = config.DefaultConfig()
	}

	account, autoErr := resolveLogoutAccount(cfg, cc.Flags.Account, purge, logger)
	if autoErr != nil {
		return autoErr
	}

	logger.Info("logout started", "account", account, "purge", purge)
	return executeLogout(cfg, cc.CfgPath, cc.Output(), account, purge, logger)
}
