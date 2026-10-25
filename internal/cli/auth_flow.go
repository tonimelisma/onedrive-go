package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

	return completeLogin(ctx, cc, ts, tempPath)
}

// completeLogin is everything login does once the person has signed in:
// discover the account, save the token and config, and report the result.
// It is split from the sign-in exchange so it can be tested with a fake
// Graph server and a token that is already in hand.
func completeLogin(ctx context.Context, cc *CLIContext, ts graph.TokenSource, tempPath string) error {
	logger := cc.Logger

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

	rollbackSnapshot, err := captureLoginRollbackSnapshot(canonicalID, finalTokenPath, cc.CfgPath)
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
		if rollbackErr := rollbackLoginSideEffects(cc.CfgPath, canonicalID, &rollbackSnapshot); rollbackErr != nil {
			logger.Warn("login rollback failed",
				"canonical_id", canonicalID.String(),
				"error", rollbackErr,
			)
			return errors.Join(baseErr, fmt.Errorf("rollback login side effects: %w", rollbackErr))
		}

		return baseErr
	}

	clearLoginAuthRequirement(email, logger)

	if !added {
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		if err := writef(cc.resultWriter(), "Token refreshed for %s.\n", email); err != nil {
			return err
		}
	} else if err := printLoginSuccess(cc.resultWriter(), canonicalID.DriveType(), email, orgName, syncDir); err != nil {
		return err
	}
	notifyDaemonIfRunning(ctx, cc)

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), loginJSONOutput{
			Account:      email,
			Drive:        canonicalID.String(),
			DriveType:    canonicalID.DriveType(),
			Organization: orgName,
			SyncDir:      syncDir,
			Added:        added,
		})
	}

	return nil
}

// loginJSONOutput is `login --json`. The sign-in prompt (device code or
// browser URL) still goes to stderr, so a script can show it to the person
// and read this document from stdout once sign-in finishes. added is false
// when the account was already configured and only its token was refreshed.
type loginJSONOutput struct {
	Account      string `json:"account"`
	Drive        string `json:"drive"`
	DriveType    string `json:"drive_type"`
	Organization string `json:"organization,omitempty"`
	SyncDir      string `json:"sync_dir"`
	Added        bool   `json:"added"`
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

	hadConfigDrive   bool
	hadConfigSyncDir bool

	catalogAccount    config.CatalogAccount
	hadCatalogAccount bool
	catalogDrive      config.CatalogDrive
	hadCatalogDrive   bool
}

func captureLoginRollbackSnapshot(
	canonicalID driveid.CanonicalID,
	tokenPath string,
	cfgPath string,
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

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	if err != nil {
		return loginRollbackSnapshot{}, fmt.Errorf("load config rollback snapshot: %w", err)
	}
	if drive, found := cfg.Drives[canonicalID]; found {
		snapshot.hadConfigDrive = true
		snapshot.hadConfigSyncDir = drive.SyncDir != ""
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
) error {
	var errs []error

	if err := restoreLoginConfigSnapshot(cfgPath, canonicalID, snapshot); err != nil {
		errs = append(errs, fmt.Errorf("restore config: %w", err))
	}
	if err := restoreLoginCatalogSnapshot(canonicalID, snapshot); err != nil {
		errs = append(errs, fmt.Errorf("restore catalog: %w", err))
	}
	if err := restoreLoginTokenSnapshot(snapshot); err != nil {
		errs = append(errs, fmt.Errorf("restore token: %w", err))
	}

	return errors.Join(errs...)
}

func restoreLoginConfigSnapshot(
	cfgPath string,
	canonicalID driveid.CanonicalID,
	snapshot *loginRollbackSnapshot,
) error {
	switch {
	case !snapshot.hadConfigDrive:
		if err := config.DeleteDriveSection(cfgPath, canonicalID); err != nil {
			return fmt.Errorf("delete added drive section: %w", err)
		}
	case !snapshot.hadConfigSyncDir:
		if err := config.DeleteDriveKey(cfgPath, canonicalID, "sync_dir"); err != nil {
			return fmt.Errorf("delete backfilled sync_dir: %w", err)
		}
	}

	return nil
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

func clearLoginAuthRequirement(email string, logger *slog.Logger) {
	if clearErr := clearAccountAuthRequirementForSource(email, config.AuthClearSourceLogin, logger); clearErr != nil {
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
		writeWarningf(cc.Status(), "%s", deviceLoginInstructions(da, cc.Flags.Account, time.Now()))
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

func runLogoutWithContext(ctx context.Context, cc *CLIContext, purge bool) error {
	logger := cc.Logger

	validated, warnings, err := config.LoadValidatedState(cc.CfgPath, true, logger)
	if err == nil {
		if outcome := config.ClassifyLoadOutcome(err, warnings); outcome.Class == errclass.ClassActionable {
			config.LogWarnings(cc.CfgPath, warnings, logger)
		}

		account, autoErr := resolveLogoutAccountWithCatalog(
			ctx,
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
		return logoutAndReport(cc, validated.Config, validated.Catalog, account, purge)
	}

	cfg, catalog := loadLogoutRecoveryState(cc.CfgPath, logger, warnings, err)
	account, autoErr := resolveLogoutAccountWithCatalog(
		ctx,
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
	return logoutAndReport(cc, cfg, catalog, account, purge)
}

// logoutAndReport runs the logout and prints its result: prose by default,
// one JSON document under --json.
func logoutAndReport(cc *CLIContext, cfg *config.Config, catalog *config.Catalog, account string, purge bool) error {
	result, err := executeLogout(cfg, catalog, cc.CfgPath, cc.resultWriter(), account, purge, cc.Logger)
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), result)
	}

	return nil
}

func loadLogoutRecoveryState(
	cfgPath string,
	logger *slog.Logger,
	validatedWarnings []config.ConfigWarning,
	validatedErr error,
) (*config.Config, *config.Catalog) {
	if len(validatedWarnings) > 0 {
		config.LogWarnings(cfgPath, validatedWarnings, logger)
	}
	if validatedErr != nil {
		logger.Warn("validated logout state unavailable, falling back to best-effort recovery",
			"error", validatedErr,
		)
	}

	cfg, fallbackWarnings, cfgErr := config.LoadOrDefaultLenient(cfgPath, logger)
	if len(fallbackWarnings) > 0 {
		config.LogWarnings(cfgPath, fallbackWarnings, logger)
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

// deviceLoginInstructions tells the user exactly what to do in the browser
// and what happens meanwhile. The passkey tip is there because a passkey or
// Windows Hello sign-in in this flow can end on a "localhost" error page,
// which otherwise gives no hint that a password sign-in works.
func deviceLoginInstructions(da graph.DeviceAuth, account string, now time.Time) string {
	var b strings.Builder

	fmt.Fprintf(&b, "To sign in, open %s in any browser and enter this code:\n\n    %s\n\n", da.VerificationURI, da.UserCode)
	if account != "" {
		fmt.Fprintf(&b, "Sign in as %s. ", account)
	}
	if !da.Expiry.IsZero() {
		minutes := int(da.Expiry.Sub(now).Round(time.Minute).Minutes())
		fmt.Fprintf(&b, "The code expires at %s (in %d minutes).", da.Expiry.Local().Format("15:04"), minutes)
	}
	b.WriteString("\n")
	b.WriteString("If a passkey or Windows Hello sign-in ends on an error page, choose \"Other ways to sign in\" and use your password.\n")
	b.WriteString("Waiting for you to finish signing in...\n")

	return b.String()
}
