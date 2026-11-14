package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/graphtransport"
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
// discover the account, install its token, enroll its primary drive in one
// config.AddDrive transaction (putting the previous token back if that
// fails), and report the result. It is split from the sign-in exchange so it
// can be tested with a fake Graph server and a token that is already in hand.
func completeLogin(ctx context.Context, cc *CLIContext, ts graph.TokenSource, tempPath string) error {
	logger := cc.Logger

	canonicalID, user, orgName, primaryDriveID, err := discoverAccount(ctx, ts, logger, cc.runtime())
	if err != nil {
		cleanupPendingToken(cc, tempPath, "discovery failure")
		return fmt.Errorf("discovering account: %w", err)
	}

	if _, reconcileErr := cc.reconcileGraphUser(ctx, canonicalID, user); reconcileErr != nil {
		cleanupPendingToken(cc, tempPath, "reconciliation failure")
		return reconcileErr
	}

	finalTokenPath := config.DriveTokenPath(canonicalID)
	if finalTokenPath == "" {
		cleanupPendingToken(cc, tempPath, "path resolution failure")
		return fmt.Errorf("cannot determine token path for drive %q", canonicalID.String())
	}

	restoreToken, err := installLoginToken(tempPath, finalTokenPath)
	if err != nil {
		cleanupPendingToken(cc, tempPath, "token install failure")
		return err
	}

	enrolled, err := config.AddDrive(&config.AddDriveRequest{
		ConfigPath: cc.CfgPath,
		DataDir:    config.DefaultDataDir(),
		Drive:      canonicalID,
		Login: &config.AccountLogin{
			UserID:         user.ID,
			DisplayName:    user.DisplayName,
			OrgName:        orgName,
			PrimaryDriveID: primaryDriveID,
		},
		MakeSyncDir: materializeDriveSyncDir,
	}, logger)
	if err != nil {
		enrollErr := fmt.Errorf("adding drive %s: %w", canonicalID, err)
		if restoreErr := restoreToken(); restoreErr != nil {
			return errors.Join(enrollErr, fmt.Errorf("putting the previous token back: %w", restoreErr))
		}

		return enrollErr
	}

	email := canonicalID.Email()
	clearLoginAuthRequirement(email, logger)

	if err := printLoginResult(cc.resultWriter(), canonicalID, orgName, &enrolled, logger); err != nil {
		return err
	}
	notifyDaemonIfRunning(ctx, cc)

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), loginJSONOutput{
			Account:      email,
			Drive:        canonicalID.String(),
			DriveType:    canonicalID.DriveType(),
			Organization: orgName,
			SyncDir:      enrolled.SyncDir,
			Added:        enrolled.Added,
			Completed:    enrolled.Completed,
		})
	}

	return nil
}

// loginJSONOutput is `login --json`. The sign-in prompt (device code or
// browser URL) still goes to stderr, so a script can show it to the person
// and read this document from stdout once sign-in finishes. added is false
// when the account was already configured and only its token was refreshed;
// completed is true when its drive was configured but not fully set up and
// this login finished it.
type loginJSONOutput struct {
	Account      string `json:"account"`
	Drive        string `json:"drive"`
	DriveType    string `json:"drive_type"`
	Organization string `json:"organization,omitempty"`
	SyncDir      string `json:"sync_dir"`
	Added        bool   `json:"added"`
	Completed    bool   `json:"completed"`
}

// installLoginToken moves the pending token to its final path and returns
// what undoes that for a login whose enrollment then fails: the token that
// was there before is written back, or the new one removed when there was
// none. Config and catalog need no undoing; config.AddDrive leaves them as
// they were when it fails.
func installLoginToken(pendingPath, tokenPath string) (func() error, error) {
	previous, hadPrevious, err := readManagedFileIfExists(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading the previous token: %w", err)
	}

	if moveErr := moveToken(pendingPath, tokenPath); moveErr != nil {
		return nil, moveErr
	}

	return func() error {
		if !hadPrevious {
			return removePathIfExists(tokenPath)
		}

		return writeManagedFile(tokenPath, previous, tokenfile.FilePerms, tokenfile.DirPerms, ".token-*.tmp")
	}, nil
}

// printLoginResult prints what the login did: the welcome for a new account,
// the completed setup of a drive that was configured but not fully set up,
// or a refreshed token.
func printLoginResult(
	w io.Writer,
	canonicalID driveid.CanonicalID,
	orgName string,
	enrolled *config.AddDriveResult,
	logger *slog.Logger,
) error {
	email := canonicalID.Email()

	switch {
	case enrolled.Added:
		return printLoginSuccess(w, canonicalID.DriveType(), email, orgName, enrolled.SyncDir)
	case enrolled.Completed:
		return writef(w, "Token refreshed for %s.\nCompleted drive %s -> %s: it was configured but not fully set up.\n",
			email, canonicalID.String(), enrolled.SyncDir)
	default:
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		return writef(w, "Token refreshed for %s.\n", email)
	}
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
	// The sign-in requests get the bounded auth client (R-6.8.27); so does
	// every later refresh of the token source the login returns.
	authHTTP := graphtransport.AuthClient()

	if useBrowser {
		ts, err := graph.LoginWithBrowser(ctx, tempPath, authHTTP, func(openCtx context.Context, authURL string) error {
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

	ts, err := graph.Login(ctx, tempPath, authHTTP, func(da graph.DeviceAuth) {
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
		return logoutAndReport(ctx, cc, validated.Config, validated.Catalog, account, purge)
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
	return logoutAndReport(ctx, cc, cfg, catalog, account, purge)
}

// logoutAndReport runs the logout and prints its result: prose by default,
// one JSON document under --json.
func logoutAndReport(
	ctx context.Context,
	cc *CLIContext,
	cfg *config.Config,
	catalog *config.Catalog,
	account string,
	purge bool,
) error {
	result, err := executeLogout(ctx, cc, cfg, catalog, account, purge)
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
