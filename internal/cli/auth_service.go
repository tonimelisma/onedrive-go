package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type authService struct {
	cc *CLIContext
}

func newAuthService(cc *CLIContext) *authService {
	return &authService{cc: cc}
}

func (s *authService) runLogin(ctx context.Context, useBrowser bool) error {
	logger := s.cc.Logger
	logger.Info("login started", slog.Bool("browser", useBrowser))

	tempPath := pendingTokenPath()

	var (
		ts  graph.TokenSource
		err error
	)

	if useBrowser {
		ts, err = graph.LoginWithBrowser(ctx, tempPath, openBrowser, logger)
	} else {
		ts, err = graph.Login(ctx, tempPath, func(da graph.DeviceAuth) {
			writeWarningf(s.cc.Status(), "To sign in, visit: %s\n", da.VerificationURI)
			writeWarningf(s.cc.Status(), "Enter code: %s\n", da.UserCode)
		}, logger)
	}

	if err != nil {
		if cleanupErr := removePathIfExists(tempPath); cleanupErr != nil {
			logger.Warn("failed to remove pending token after login failure", "path", tempPath, "error", cleanupErr)
		}

		return fmt.Errorf("authenticate account: %w", err)
	}

	canonicalID, user, orgName, primaryDriveID, err := discoverAccount(ctx, ts, logger)
	if err != nil {
		if cleanupErr := removePathIfExists(tempPath); cleanupErr != nil {
			logger.Warn("failed to remove pending token after discovery failure", "path", tempPath, "error", cleanupErr)
		}

		return fmt.Errorf("discovering account: %w", err)
	}

	finalTokenPath := config.DriveTokenPath(canonicalID)
	if finalTokenPath == "" {
		if cleanupErr := removePathIfExists(tempPath); cleanupErr != nil {
			logger.Warn("failed to remove pending token after path resolution failure", "path", tempPath, "error", cleanupErr)
		}

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
	syncDir, added, err := config.EnsureDriveInConfig(s.cc.CfgPath, canonicalID, logger)
	if err != nil {
		return fmt.Errorf("configuring drive: %w", err)
	}

	if !added {
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		return writef(s.cc.Output(), "Token refreshed for %s.\n", email)
	}

	return printLoginSuccess(s.cc.Output(), canonicalID.DriveType(), email, orgName, canonicalID.String(), syncDir)
}

func (s *authService) runLogout(purge bool) error {
	logger := s.cc.Logger

	cfg, loadErr := config.LoadOrDefault(s.cc.CfgPath, logger)
	if loadErr != nil {
		logger.Warn("failed to load config, proceeding with --account only", "error", loadErr)
		cfg = config.DefaultConfig()
	}

	account, autoErr := resolveLogoutAccount(cfg, s.cc.Flags.Account, purge, logger)
	if autoErr != nil {
		return autoErr
	}

	logger.Info("logout started", "account", account, "purge", purge)
	return executeLogout(cfg, s.cc.CfgPath, s.cc.Output(), account, purge, logger)
}

func (s *authService) runWhoami(ctx context.Context) error {
	return runWhoamiWithContext(ctx, s.cc)
}
