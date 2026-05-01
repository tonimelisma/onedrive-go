package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
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

	rollbackSnapshot, err := captureLoginRollbackSnapshot(canonicalID, finalTokenPath)
	if err != nil {
		cleanupPendingToken(cc, tempPath, "rollback snapshot failure")
		return fmt.Errorf("snapshot login rollback state: %w", err)
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
	if err := materializeDriveSyncDir(syncDir); err != nil {
		baseErr := fmt.Errorf("creating sync directory: %w", err)
		if rollbackErr := rollbackLoginSideEffects(cc.CfgPath, canonicalID, &rollbackSnapshot, added); rollbackErr != nil {
			logger.Warn("login rollback failed",
				"canonical_id", canonicalID.String(),
				"error", rollbackErr,
			)
			return errors.Join(baseErr, fmt.Errorf("rollback login side effects: %w", rollbackErr))
		}

		return baseErr
	}

	clearLoginAuthRequirement(ctx, email, logger)

	if !added {
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		if err := writef(cc.Output(), "Token refreshed for %s.\n", email); err != nil {
			return err
		}
		notifyDaemonIfRunning(ctx, cc)
		return nil
	}

	if err := printLoginSuccess(cc.Output(), canonicalID.DriveType(), email, orgName, canonicalID.String(), syncDir); err != nil {
		return err
	}
	notifyDaemonIfRunning(ctx, cc)
	return nil
}

func persistLoginMetadata(
	canonicalID driveid.CanonicalID,
	user *graph.User,
	orgName string,
	primaryDriveID driveid.ID,
	logger *slog.Logger,
) {
	if catalogErr := config.RecordLogin(
		config.DefaultDataDir(),
		canonicalID,
		user.ID,
		user.DisplayName,
		orgName,
		primaryDriveID,
	); catalogErr != nil {
		logger.Warn("failed to update catalog after login", "error", catalogErr)
	}
}

type loginRollbackSnapshot struct {
	tokenPath string
	tokenData []byte
	hadToken  bool

	catalogAccount    config.CatalogAccount
	hadCatalogAccount bool
	catalogDrive      config.CatalogDrive
	hadCatalogDrive   bool
}

func captureLoginRollbackSnapshot(
	canonicalID driveid.CanonicalID,
	tokenPath string,
) (loginRollbackSnapshot, error) {
	snapshot := loginRollbackSnapshot{tokenPath: tokenPath}

	if tokenPath != "" {
		tokenData, found, err := readManagedFileIfExists(tokenPath)
		if err != nil {
			return loginRollbackSnapshot{}, fmt.Errorf("read token rollback snapshot: %w", err)
		}
		snapshot.tokenData = tokenData
		snapshot.hadToken = found
	}

	catalog, err := config.LoadCatalog()
	if err != nil {
		return loginRollbackSnapshot{}, fmt.Errorf("load catalog rollback snapshot: %w", err)
	}
	if account, found := catalog.AccountByCanonicalID(canonicalID); found {
		snapshot.catalogAccount = account
		snapshot.hadCatalogAccount = true
	}
	if drive, found := catalog.DriveByCanonicalID(canonicalID); found {
		snapshot.catalogDrive = drive
		snapshot.hadCatalogDrive = true
	}

	return snapshot, nil
}

func rollbackLoginSideEffects(
	cfgPath string,
	canonicalID driveid.CanonicalID,
	snapshot *loginRollbackSnapshot,
	removeDriveConfig bool,
) error {
	var errs []error

	if removeDriveConfig {
		if err := config.DeleteDriveSection(cfgPath, canonicalID); err != nil {
			errs = append(errs, fmt.Errorf("remove drive config: %w", err))
		}
	}

	if err := restoreLoginCatalogSnapshot(canonicalID, snapshot); err != nil {
		errs = append(errs, fmt.Errorf("restore catalog: %w", err))
	}
	if err := restoreLoginTokenSnapshot(snapshot); err != nil {
		errs = append(errs, fmt.Errorf("restore token: %w", err))
	}

	return errors.Join(errs...)
}

func restoreLoginCatalogSnapshot(canonicalID driveid.CanonicalID, snapshot *loginRollbackSnapshot) error {
	if err := config.UpdateCatalog(func(catalog *config.Catalog) error {
		if snapshot.hadCatalogAccount {
			catalog.UpsertAccount(&snapshot.catalogAccount)
		} else {
			catalog.DeleteAccount(canonicalID)
		}

		if snapshot.hadCatalogDrive {
			catalog.UpsertDrive(&snapshot.catalogDrive)
		} else {
			catalog.DeleteDrive(canonicalID)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("update catalog: %w", err)
	}

	return nil
}

func restoreLoginTokenSnapshot(snapshot *loginRollbackSnapshot) error {
	if snapshot.tokenPath == "" {
		return nil
	}
	if !snapshot.hadToken {
		return removePathIfExists(snapshot.tokenPath)
	}

	return writeManagedFile(snapshot.tokenPath, snapshot.tokenData, tokenfile.FilePerms, tokenfile.DirPerms, ".token-*.tmp")
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
		ts, err := graph.LoginWithBrowser(ctx, tempPath, func(openCtx context.Context, authURL string) error {
			openErr := openBrowser(openCtx, authURL)
			if openErr != nil {
				writeWarningf(cc.Status(), "Open this URL in your browser:\n%s\n", authURL)
			}

			return openErr
		}, cc.Logger)
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
	if err == nil {
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

	cfg, catalog := loadLogoutRecoveryState(cc.CfgPath, logger, warnings, err)
	account, autoErr := resolveLogoutAccountWithCatalog(
		cfg,
		catalog,
		cc.Flags.Account,
		purge,
		logger,
	)
	if autoErr != nil {
		return autoErr
	}

	logger.Info("logout started with degraded state recovery", "account", account, "purge", purge)
	return executeLogout(cfg, catalog, cc.CfgPath, cc.Output(), account, purge, logger)
}

func loadLogoutRecoveryState(
	cfgPath string,
	logger *slog.Logger,
	validatedWarnings []config.ConfigWarning,
	validatedErr error,
) (*config.Config, *config.Catalog) {
	if len(validatedWarnings) > 0 {
		config.LogWarnings(validatedWarnings, logger)
	}
	if validatedErr != nil {
		logger.Warn("validated logout state unavailable, falling back to best-effort recovery",
			"error", validatedErr,
		)
	}

	cfg, fallbackWarnings, cfgErr := config.LoadOrDefaultLenient(cfgPath, logger)
	if len(fallbackWarnings) > 0 {
		config.LogWarnings(fallbackWarnings, logger)
	}
	if cfgErr != nil {
		logger.Warn("logout recovery could not load config, using defaults",
			"error", cfgErr,
		)
		cfg = config.DefaultConfig()
	}

	catalog, catalogErr := config.LoadCatalog()
	if catalogErr != nil {
		logger.Warn("logout recovery could not load catalog, using empty catalog",
			"error", catalogErr,
		)
		catalog = config.DefaultCatalog()
	}

	return cfg, catalog
}
