package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/failures"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// pendingTokenFile is the filename for the temporary token saved during login
// before the canonical ID is known. This solves the chicken-and-egg problem:
// we need a token to call /me, but the token filename depends on /me results.
const pendingTokenFile = ".token-pending.json"

// tokenDirPerms is the permission mode for token directories (owner only).
const tokenDirPerms = 0o700

// graphDriveTypeDocumentLibrary is the Graph API drive type for SharePoint libraries.
const graphDriveTypeDocumentLibrary = "documentLibrary"

const (
	httpScheme  = "http"
	httpsScheme = "https"
)

func newLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with OneDrive",
		Long: `Authenticate with OneDrive using the device code flow (default) or browser-based
authorization code flow (--browser).

Discovers your account type (personal/business) and organization automatically.
Creates or updates the config file with the new drive section.

The --browser flag opens your default browser for authentication, which can be
useful when the device code flow is blocked by organizational policies.`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runLogin,
	}

	cmd.Flags().Bool("browser", false, "use browser-based auth (authorization code + PKCE) instead of device code")

	return cmd
}

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove saved authentication token and drive config",
		Long: `Remove the saved authentication token and drive config sections for an account.
State databases are kept so the drive can be re-added without a full re-sync.

With --purge, state databases are also deleted.

If only one account is configured, it is selected automatically.
Otherwise, use --account to specify which account to log out.`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runLogout,
	}

	cmd.Flags().Bool("purge", false, "also delete state databases")

	return cmd
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "whoami",
		Short:       "Display the authenticated user and drive info",
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runWhoami,
	}
}

// findTokenFallback tries personal and business canonical ID prefixes
// and returns whichever one has a token file on disk. Falls back to
// "personal:" if neither exists, since personal is the most common case.
// Logs the probe results so --debug reveals which token path was selected.
func findTokenFallback(account string, logger *slog.Logger) driveid.CanonicalID {
	personalID := driveid.MustCanonicalID("personal:" + account)

	personalPath := config.DriveTokenPath(personalID)
	if personalPath != "" {
		if managedPathExists(personalPath) {
			logger.Debug("token fallback: found personal token", "path", personalPath)

			return personalID
		}
	}

	businessID := driveid.MustCanonicalID("business:" + account)

	businessPath := config.DriveTokenPath(businessID)
	if businessPath != "" {
		if managedPathExists(businessPath) {
			logger.Debug("token fallback: found business token", "path", businessPath)

			return businessID
		}
	}

	// Default to personal if neither exists (best guess for most users).
	logger.Debug("token fallback: no token found, defaulting to personal", "account", account)

	return personalID
}

// pendingTokenPath returns the path for the temporary token file used during
// login before the canonical ID is discovered.
func pendingTokenPath() string {
	return filepath.Join(config.DefaultDataDir(), pendingTokenFile)
}

// openBrowser attempts to open a URL in the user's default browser.
// Uses "open" on macOS and "xdg-open" on Linux. Returns an error if the
// browser command fails or the platform is unsupported.
func openBrowser(ctx context.Context, rawURL string) error {
	validatedURL, err := validateBrowserAuthURL(rawURL)
	if err != nil {
		return err
	}

	command, err := browserOpenCommand(runtime.GOOS)
	if err != nil {
		return err
	}

	// Command name is selected from a fixed allowlist and the URL has already
	// been validated against the Microsoft auth hosts.
	cmd := exec.CommandContext(ctx, command, validatedURL) //nolint:gosec // Fixed browser command with validated auth URL.

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}

	return nil
}

const (
	goosDarwin = "darwin"
	goosLinux  = "linux"
)

func browserOpenCommand(goos string) (string, error) {
	switch goos {
	case goosDarwin:
		return "open", nil
	case goosLinux:
		return "xdg-open", nil
	default:
		return "", fmt.Errorf("unsupported platform %s: open the URL manually", goos)
	}
}

func validateBrowserAuthURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing browser auth URL: %w", err)
	}

	if parsed.User != nil {
		return "", fmt.Errorf("browser auth URL must not contain userinfo")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("browser auth URL host is empty")
	}

	if isLoopbackBrowserHost(host) {
		if parsed.Scheme != httpScheme && parsed.Scheme != httpsScheme {
			return "", fmt.Errorf("browser auth URL loopback host must use http or https")
		}

		return parsed.String(), nil
	}

	if parsed.Scheme != httpsScheme {
		return "", fmt.Errorf("browser auth URL must use https")
	}

	if !browserHostAllowed(host) {
		return "", fmt.Errorf("browser auth URL host %q is not allowed", host)
	}

	return parsed.String(), nil
}

func browserHostAllowed(host string) bool {
	for _, allowedHost := range []string{
		"login.microsoftonline.com",
		"login.microsoftonline.us",
		"login.partner.microsoftonline.cn",
		"login.live.com",
	} {
		if host == allowedHost || strings.HasSuffix(host, "."+allowedHost) {
			return true
		}
	}

	return false
}

func isLoopbackBrowserHost(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// runLogin implements the discovery-based login flow per accounts.md section 9:
// device code auth -> /me -> /me/drive -> /me/organization -> construct canonical ID -> config.
func runLogin(cmd *cobra.Command, _ []string) error {
	useBrowser, err := cmd.Flags().GetBool("browser")
	if err != nil {
		return fmt.Errorf("reading --browser flag: %w", err)
	}

	return newAuthService(mustCLIContext(cmd.Context())).runLogin(cmd.Context(), useBrowser)
}

// discoverAccount calls /me, /me/drive, and /me/organization to build the
// canonical drive ID and extract the organization name. Returns the canonical
// ID, user profile, org display name, and the primary drive's Graph API ID.
func discoverAccount(
	ctx context.Context, ts graph.TokenSource, logger *slog.Logger,
) (driveid.CanonicalID, *graph.User, string, driveid.ID, error) {
	client, err := newGraphClient(ts, logger)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, err
	}

	// GET /me -> email, user GUID
	user, err := client.Me(ctx)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, fmt.Errorf("fetching user profile: %w", err)
	}

	logger.Info("discovered user", "email", user.Email, "display_name", user.DisplayName)

	// GET /me/drive (singular) -> primary drive ID and type.
	// Must use /me/drive, NOT /me/drives. The /me/drives endpoint returns all
	// drives including phantom system drives (Photos face crops, album metadata)
	// that Microsoft creates on personal accounts. These appear in non-deterministic
	// order and return HTTP 400 "ObjectHandle is Invalid" when accessed.
	primary, err := client.PrimaryDrive(ctx)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, fmt.Errorf("fetching primary drive: %w", err)
	}

	driveType := primary.DriveType
	logger.Info("discovered drive type", "drive_type", driveType)

	// Warn on unknown drive types — don't block login, but flag it for debugging.
	// Known types: "personal", "business", "documentLibrary" (SharePoint).
	switch driveType {
	case driveid.DriveTypePersonal, driveid.DriveTypeBusiness, graphDriveTypeDocumentLibrary:
		// expected
	default:
		logger.Warn("unknown drive type from Graph API, proceeding anyway",
			"drive_type", driveType)
	}

	primaryDriveID := primary.ID
	logger.Info("discovered primary drive", "drive_id", primaryDriveID.String())

	// GET /me/organization -> org display name (business only)
	var orgName string

	org, err := client.Organization(ctx)
	if err != nil {
		logger.Warn("failed to fetch organization, continuing without org name", "error", err)
	} else if org.DisplayName != "" {
		orgName = org.DisplayName
		logger.Info("discovered organization", "org_name", orgName)
	}

	cid, err := driveid.Construct(driveType, user.Email)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, fmt.Errorf("constructing canonical ID: %w", err)
	}

	logger.Info("constructed canonical ID", "canonical_id", cid.String())

	return cid, user, orgName, primaryDriveID, nil
}

// moveToken renames the pending token file to its final canonical path.
// Creates the destination directory if needed.
func moveToken(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := localpath.MkdirAll(dir, tokenDirPerms); err != nil {
		baseErr := fmt.Errorf("creating token directory: %w", err)
		if cleanupErr := removePathIfExists(src); cleanupErr != nil {
			return errors.Join(baseErr, cleanupErr)
		}

		return baseErr
	}

	if err := localpath.Rename(src, dst); err != nil {
		baseErr := fmt.Errorf("moving token to final path: %w", err)
		if cleanupErr := removePathIfExists(src); cleanupErr != nil {
			return errors.Join(baseErr, cleanupErr)
		}

		return baseErr
	}

	return nil
}

// printLoginSuccess prints the user-facing login output. Format differs
// for personal vs. business accounts per accounts.md section 9.
func printLoginSuccess(w io.Writer, driveType, email, orgName, canonicalID, syncDir string) error {
	switch driveType {
	case "personal":
		if err := writef(w, "Signed in as %s (personal account).\n", email); err != nil {
			return err
		}

		return writef(w, "Drive added: %s -> %s\n", canonicalID, syncDir)
	case "business":
		orgLabel := orgName
		if orgLabel == "" {
			orgLabel = "business account"
		}

		if err := writef(w, "Signed in as %s (%s).\n", email, orgLabel); err != nil {
			return err
		}
		if err := writef(w, "Drive added: %s -> %s\n", canonicalID, syncDir); err != nil {
			return err
		}
		if err := writeln(w); err != nil {
			return err
		}
		if err := writeln(w, "You also have access to SharePoint libraries."); err != nil {
			return err
		}

		return writeln(w, "Run 'onedrive-go drive search <term>' to find and add them.")
	default:
		if err := writef(w, "Signed in as %s.\n", email); err != nil {
			return err
		}

		return writef(w, "Drive added: %s -> %s\n", canonicalID, syncDir)
	}
}

// runLogout removes the authentication token for an account. Identifies the
// account via --account flag or auto-selects if only one account exists.
func runLogout(cmd *cobra.Command, _ []string) error {
	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	return newAuthService(mustCLIContext(cmd.Context())).runLogout(purge)
}

// resolveLogoutAccount determines the account email for logout. Uses the
// account flag if provided, otherwise auto-selects when there is exactly one
// account. When config is empty (prior logout removed config sections), falls
// back to discovering orphaned account profiles on disk.
func resolveLogoutAccount(cfg *config.Config, accountFlag string, purge bool, logger *slog.Logger) (string, error) {
	if accountFlag != "" {
		return accountFlag, nil
	}

	// Collect unique account emails from configured drives.
	accounts := uniqueAccounts(cfg)

	if len(accounts) == 1 {
		return accounts[0], nil
	}

	if len(accounts) > 1 {
		return "", fmt.Errorf(
			"multiple accounts configured — specify with --account:\n  %s",
			strings.Join(accounts, "\n  "),
		)
	}

	// No accounts in config — check for orphaned account profiles (logged out
	// but not purged). This enables `logout --purge` after a prior `logout`.
	orphanEmails := discoverOrphanedEmails(logger)

	if len(orphanEmails) == 0 {
		return "", fmt.Errorf("no accounts configured — nothing to log out")
	}

	if !purge {
		return "", fmt.Errorf(
			"no accounts configured, but orphaned data remains for:\n  %s\n"+
				"run 'onedrive-go logout --purge --account <email>' to remove",
			strings.Join(orphanEmails, "\n  "),
		)
	}

	if len(orphanEmails) == 1 {
		return orphanEmails[0], nil
	}

	return "", fmt.Errorf(
		"multiple orphaned accounts — specify with --account:\n  %s",
		strings.Join(orphanEmails, "\n  "),
	)
}

// discoverOrphanedEmails returns unique emails from account profiles on disk
// that lack a token file. Used by resolveLogoutAccount to find accounts that
// were logged out but not purged.
func discoverOrphanedEmails(logger *slog.Logger) []string {
	profiles := config.DiscoverAccountProfiles(logger)

	seen := make(map[string]bool)
	var emails []string

	for _, cid := range profiles {
		email := cid.Email()
		if seen[email] {
			continue
		}

		// Check if token still exists — if so, not orphaned.
		tokenPath := config.DriveTokenPath(cid)
		if tokenPath != "" && managedPathExists(tokenPath) {
			continue
		}

		seen[email] = true
		emails = append(emails, email)
	}

	return emails
}

// uniqueAccounts extracts unique account emails from all configured drives.
func uniqueAccounts(cfg *config.Config) []string {
	seen := make(map[string]bool)
	var accounts []string

	for id := range cfg.Drives {
		email := id.Email()
		if !seen[email] {
			seen[email] = true
			accounts = append(accounts, email)
		}
	}

	return accounts
}

// executeLogout performs the actual logout: finds affected drives, deletes
// token, and optionally purges config sections and state databases.
func executeLogout(cfg *config.Config, cfgPath string, w io.Writer, account string, purge bool, logger *slog.Logger) error {
	// Find all drives belonging to this account.
	affected := drivesForAccount(cfg, account)

	// Determine canonical ID for the token path. We need any drive ID with this
	// account email to derive the token path (all drives for one account share a token).
	tokenCanonicalID := canonicalIDForToken(account, affected)
	if tokenCanonicalID.IsZero() {
		// No drives in config — probe the filesystem for an existing token.
		tokenCanonicalID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID)

	// Delete the token file. graph.Logout handles "not found" gracefully
	// (returns nil), so this works even after a prior logout.
	if tokenPath != "" {
		if err := graph.Logout(tokenPath, logger); err != nil {
			return fmt.Errorf("remove token file: %w", err)
		}

		if err := writef(w, "Token removed for %s.\n", account); err != nil {
			return err
		}
	} else if !purge {
		return fmt.Errorf("cannot determine token path for account %q", account)
	} else {
		// Purge after prior logout — token already gone, that's fine.
		if err := writef(w, "Token already removed for %s.\n", account); err != nil {
			return err
		}
	}

	if err := printAffectedDrives(w, cfg, affected); err != nil {
		return err
	}

	if purge {
		if err := purgeAccountDrives(w, cfgPath, affected, logger); err != nil {
			return fmt.Errorf("purging account drives: %w", err)
		}

		// Also purge orphaned files (state DBs, drive metadata, account profiles)
		// that may remain from a prior non-purge logout or from drives that were
		// removed from config but left data on disk.
		if err := purgeOrphanedFiles(w, account, logger); err != nil {
			return fmt.Errorf("purging orphaned files: %w", err)
		}

		return writeln(w, "Sync directories untouched — your files remain on disk.")
	} else {
		if clearErr := clearAccountAuthScopes(context.Background(), account, logger); clearErr != nil {
			logger.Warn("clearing stale auth scopes during logout", "account", account, "error", clearErr)
		}

		if err := removeAccountDriveConfigs(cfgPath, affected, logger); err != nil {
			return fmt.Errorf("removing drive configs: %w", err)
		}

		if err := writeln(w, "\nState databases kept. Run 'onedrive-go login' to re-authenticate."); err != nil {
			return err
		}

		return writeln(w, "Sync directories untouched — your files remain on disk.")
	}
}

// purgeOrphanedFiles removes state databases, drive metadata files, and
// account profile files for the given email. Idempotent — ignores files
// that don't exist. This handles cleanup after a prior non-purge logout
// where config sections and tokens were removed but data files remained.
func purgeOrphanedFiles(w io.Writer, email string, logger *slog.Logger) error {
	var errs []error

	// Remove orphaned state databases.
	for _, path := range config.DiscoverStateDBsForEmail(email, logger) {
		if err := removeOrphanedPath(
			path,
			logger,
			"failed to remove orphaned state DB",
			"removed orphaned state DB",
			func(removedPath string) error {
				return writef(w, "Purged orphaned state DB: %s\n", filepath.Base(removedPath))
			},
		); err != nil {
			errs = append(errs, err)
		}
	}

	// Remove orphaned drive metadata files.
	for _, path := range config.DiscoverDriveMetadataForEmail(email, logger) {
		if err := removeOrphanedPath(
			path,
			logger,
			"failed to remove orphaned drive metadata",
			"removed orphaned drive metadata",
			nil,
		); err != nil {
			errs = append(errs, err)
		}
	}

	// Remove account profile files. Discover actual profiles on disk rather
	// than guessing drive types, so we handle any type that exists.
	for _, profileCID := range config.DiscoverAccountProfiles(logger) {
		if profileCID.Email() != email {
			continue
		}

		profilePath := config.AccountFilePath(profileCID)
		if profilePath == "" {
			continue
		}

		if err := removeOrphanedPath(
			profilePath,
			logger,
			"failed to remove account profile",
			"removed account profile",
			nil,
		); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func removeOrphanedPath(
	path string,
	logger *slog.Logger,
	warnMsg string,
	infoMsg string,
	onRemoved func(path string) error,
) error {
	removed, err := removeManagedPath(path)
	if err != nil {
		logger.Warn(warnMsg, "path", path, "error", err)
		return fmt.Errorf("removing %s: %w", path, err)
	}
	if !removed {
		return nil
	}

	logger.Info(infoMsg, "path", path)
	if onRemoved != nil {
		if err := onRemoved(path); err != nil {
			return err
		}
	}

	return nil
}

// drivesForAccount returns all canonical IDs whose email matches the given account.
func drivesForAccount(cfg *config.Config, account string) []driveid.CanonicalID {
	var ids []driveid.CanonicalID

	for id := range cfg.Drives {
		if id.Email() == account {
			ids = append(ids, id)
		}
	}

	return ids
}

// canonicalIDForToken picks a canonical ID to use for token path derivation.
// SharePoint drives share the business token, so we prefer a non-sharepoint ID.
// DriveTokenPath handles the SharePoint→business mapping internally.
func canonicalIDForToken(account string, driveIDs []driveid.CanonicalID) driveid.CanonicalID {
	for _, cid := range driveIDs {
		if !cid.IsSharePoint() {
			return cid
		}
	}

	// All drives are SharePoint — derive the business token ID.
	if len(driveIDs) > 0 {
		cid, err := driveid.Construct("business", account)
		if err != nil {
			return driveid.CanonicalID{}
		}

		return cid
	}

	return driveid.CanonicalID{}
}

// printAffectedDrives lists drives that can no longer sync after logout.
func printAffectedDrives(w io.Writer, cfg *config.Config, affected []driveid.CanonicalID) error {
	if len(affected) == 0 {
		return nil
	}

	if err := writeln(w, "Affected drives (can no longer sync):"); err != nil {
		return err
	}

	for _, id := range affected {
		syncDir := cfg.Drives[id].SyncDir
		if err := writef(w, "  %s (%s)\n", id.String(), syncDir); err != nil {
			return err
		}
	}

	return nil
}

// purgeSingleDrive removes the state database, drive metadata, account profile,
// and config section for one drive. Token deletion is handled separately since
// tokens may be shared (SharePoint).
func purgeSingleDrive(cfgPath string, canonicalID driveid.CanonicalID, logger *slog.Logger) error {
	// Remove state DB and drive metadata (best-effort, errors logged).
	if _, err := removeDriveDataFiles(canonicalID, logger); err != nil {
		logger.Warn("errors removing drive data files", "drive", canonicalID.String(), "error", err)
	}

	// Remove account profile file (only for personal/business — shared/SP
	// don't have their own profile files).
	profilePath := config.AccountFilePath(canonicalID)
	if profilePath != "" {
		removed, err := removeManagedPath(profilePath)
		if err != nil {
			logger.Warn("failed to remove account profile", "path", profilePath, "error", err)
		} else if removed {
			logger.Info("removed account profile", "path", profilePath)
		}
	}

	if err := config.DeleteDriveSection(cfgPath, canonicalID); err != nil {
		return fmt.Errorf("deleting drive section: %w", err)
	}

	return nil
}

// purgeAccountDrives removes drive config sections and state databases for
// all affected drives. Token deletion is already handled before this call.
func purgeAccountDrives(w io.Writer, cfgPath string, affected []driveid.CanonicalID, logger *slog.Logger) error {
	if err := writeln(w); err != nil {
		return err
	}

	var errs []error

	for _, cid := range affected {
		if err := purgeSingleDrive(cfgPath, cid, logger); err != nil {
			logger.Warn("failed to purge drive", "drive", cid.String(), "error", err)
			errs = append(errs, fmt.Errorf("purging drive %s: %w", cid.String(), err))
		} else {
			if writeErr := writef(w, "Purged config and state for %s.\n", cid.String()); writeErr != nil {
				errs = append(errs, writeErr)
			}
		}
	}

	return errors.Join(errs...)
}

// removeAccountDriveConfigs deletes config sections for all affected drives
// without removing state databases. Used by regular logout (without --purge).
func removeAccountDriveConfigs(cfgPath string, affected []driveid.CanonicalID, logger *slog.Logger) error {
	var errs []error

	for _, cid := range affected {
		if err := config.DeleteDriveSection(cfgPath, cid); err != nil {
			logger.Warn("failed to remove drive config section", "drive", cid.String(), "error", err)
			errs = append(errs, fmt.Errorf("removing drive %s: %w", cid.String(), err))
		} else {
			logger.Info("removed drive config section", "drive", cid.String())
		}
	}

	return errors.Join(errs...)
}

// whoamiOutput is the JSON schema for `whoami --json`.
type whoamiOutput struct {
	User                  *whoamiUser              `json:"user,omitempty"`
	Drives                []whoamiDrive            `json:"drives,omitempty"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth,omitempty"`
}

type whoamiUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type whoamiDrive struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DriveType  string `json:"drive_type"`
	QuotaUsed  int64  `json:"quota_used"`
	QuotaTotal int64  `json:"quota_total"`
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	return newAuthService(mustCLIContext(cmd.Context())).runWhoami(cmd.Context())
}

func runWhoamiWithContext(ctx context.Context, cc *CLIContext) error {
	logger := cc.Logger
	cfg, warnings, err := config.LoadOrDefaultLenient(cc.CfgPath, logger)
	outcome := config.ClassifyLoadOutcome(err, warnings)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if outcome.Class == failures.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	recorder := newAuthProofRecorder(logger)

	// Try the authenticated path: match drive → fetch from Graph API.
	authResult, authErr := fetchAuthenticatedAccount(ctx, cfg, driveSelector, logger, recorder)
	if authErr != nil {
		return authErr
	}

	// Discover offline auth-required accounts from orphaned account profiles.
	authRequired := findWhoamiAuthRequiredAccounts(ctx, cfg, authResult.authenticatedEmail, logger)
	if authResult.authRequired != nil {
		authRequired = mergeAuthRequirements(authRequired, []accountAuthRequirement{*authResult.authRequired})
	}

	// If no authenticated account and no offline auth-required accounts, give a
	// clean auth-required error instead of a whoami-specific special case.
	if !authResult.hasAuthenticatedAccount && len(authRequired) == 0 {
		return graph.ErrNotLoggedIn
	}

	if cc.Flags.JSON {
		return printWhoamiJSON(cc.Output(), authResult.user, authResult.drives, authRequired)
	}

	return printWhoamiText(cc.Output(), authResult.user, authResult.drives, authRequired)
}

type authenticatedAccountResult struct {
	user                    *graph.User
	drives                  []graph.Drive
	authenticatedEmail      string
	authRequired            *accountAuthRequirement
	hasAuthenticatedAccount bool
}

// fetchAuthenticatedAccount attempts to resolve a drive from config, load its
// token, and fetch user/drive info from the Graph API. Returns found=false
// when no authenticated account is available. Returns a non-nil error only
// for hard failures after a token is located.
func fetchAuthenticatedAccount(
	ctx context.Context, cfg *config.Config, driveSelector string, logger *slog.Logger, recorder *authProofRecorder,
) (authenticatedAccountResult, error) {
	cid, found := matchAuthenticatedDrive(cfg, driveSelector, logger)
	if !found {
		return authenticatedAccountResult{}, nil
	}

	accountEmail := cid.Email()
	accountDriveIDs := drivesForAccount(cfg, accountEmail)
	if len(accountDriveIDs) == 0 {
		accountDriveIDs = []driveid.CanonicalID{cid}
	}
	displayName, _ := readAccountMeta(accountEmail, accountDriveIDs, logger)
	stateDBCount := len(config.DiscoverStateDBsForEmail(accountEmail, logger))
	authRequired := authRequirement(
		accountEmail,
		displayName,
		accountDriveType(accountDriveIDs),
		stateDBCount,
		accountAuthHealth{},
	)

	accountAuth := inspectAccountAuth(ctx, accountEmail, accountDriveIDs, logger)
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

	ts, tsErr := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if tsErr != nil {
		if errors.Is(tsErr, graph.ErrNotLoggedIn) {
			authRequired.Reason = authReasonMissingLogin
			authRequired.Action = authAction(authReasonMissingLogin)
			return authenticatedAccountResult{authRequired: &authRequired}, nil
		}

		authRequired.Reason = authReasonInvalidSavedLogin
		authRequired.Action = authAction(authReasonInvalidSavedLogin)
		return authenticatedAccountResult{authRequired: &authRequired}, nil
	}

	client, err := newGraphClient(ts, logger)
	if err != nil {
		return authenticatedAccountResult{}, err
	}
	attachAccountAuthProof(client, recorder, accountEmail)

	user, err := client.Me(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrUnauthorized) {
			authRequired.Reason = authReasonSyncAuthRejected
			authRequired.Action = authAction(authReasonSyncAuthRejected)
			return authenticatedAccountResult{authRequired: &authRequired}, nil
		}

		return authenticatedAccountResult{}, fmt.Errorf("fetching user profile: %w", err)
	}

	drives, err := client.Drives(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrUnauthorized) {
			authRequired.Reason = authReasonSyncAuthRejected
			authRequired.Action = authAction(authReasonSyncAuthRejected)
			return authenticatedAccountResult{authRequired: &authRequired}, nil
		}

		return authenticatedAccountResult{}, fmt.Errorf("listing drives: %w", err)
	}

	return authenticatedAccountResult{
		user:                    user,
		drives:                  drives,
		authenticatedEmail:      user.Email,
		hasAuthenticatedAccount: true,
	}, nil
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
) (driveid.CanonicalID, bool) {
	cid, _, matchErr := config.MatchDrive(cfg, driveSelector, logger)
	if matchErr != nil {
		logger.Debug("whoami: skipping authenticated account lookup",
			slog.String("selector", driveSelector),
			slog.String("reason", matchErr.Error()),
		)

		return driveid.CanonicalID{}, false
	}

	return cid, true
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
	profiles := config.DiscoverAccountProfiles(logger)
	if len(profiles) == 0 {
		return nil
	}

	// Build set of emails that are still authenticated (in config).
	configEmails := make(map[string]bool)
	for id := range cfg.Drives {
		configEmails[id.Email()] = true
	}

	if authenticatedEmail != "" {
		configEmails[authenticatedEmail] = true
	}

	var result []accountAuthRequirement

	for _, profileCID := range profiles {
		email := profileCID.Email()

		// Skip if this account is still in config (authenticated).
		if configEmails[email] {
			continue
		}

		health := inspectAccountAuth(ctx, email, []driveid.CanonicalID{profileCID}, logger)
		if health.State != authStateAuthenticationNeeded {
			continue
		}

		// Load profile for display name.
		profile, found, profileErr := config.LookupAccountProfile(profileCID)
		if profileErr != nil {
			logger.Debug("could not load account profile for logged-out display",
				"canonical_id", profileCID.String(), "error", profileErr)
		}

		var displayName string
		if found {
			displayName = profile.DisplayName
		}

		// Count state DBs for this email.
		stateDBs := config.DiscoverStateDBsForEmail(email, logger)

		result = append(result, authRequirement(
			email,
			displayName,
			profileCID.DriveType(),
			len(stateDBs),
			health,
		))
	}

	sortAccountAuthRequirements(result)

	return result
}

func printWhoamiJSON(w io.Writer, user *graph.User, drives []graph.Drive, authRequired []accountAuthRequirement) error {
	out := whoamiOutput{
		AccountsRequiringAuth: authRequired,
	}

	if user != nil {
		out.User = &whoamiUser{
			ID:          user.ID,
			DisplayName: user.DisplayName,
			Email:       user.Email,
		}
		out.Drives = make([]whoamiDrive, 0, len(drives))

		for i := range drives {
			out.Drives = append(out.Drives, whoamiDrive{
				ID:         drives[i].ID.String(),
				Name:       drives[i].Name,
				DriveType:  drives[i].DriveType,
				QuotaUsed:  drives[i].QuotaUsed,
				QuotaTotal: drives[i].QuotaTotal,
			})
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printWhoamiText(w io.Writer, user *graph.User, drives []graph.Drive, authRequired []accountAuthRequirement) error {
	if user != nil {
		if err := writef(w, "User:  %s (%s)\n", user.DisplayName, user.Email); err != nil {
			return err
		}
		if err := writef(w, "ID:    %s\n", user.ID); err != nil {
			return err
		}

		for i := range drives {
			if err := writef(w, "\nDrive: %s (%s)\n", drives[i].Name, drives[i].DriveType); err != nil {
				return err
			}
			if err := writef(w, "  ID:    %s\n", drives[i].ID); err != nil {
				return err
			}
			if err := writef(w, "  Quota: %s / %s\n", formatSize(drives[i].QuotaUsed), formatSize(drives[i].QuotaTotal)); err != nil {
				return err
			}
		}
	}

	return printWhoamiAuthRequiredText(w, authRequired)
}

// printWhoamiAuthRequiredText prints accounts that need user re-authentication
// in a consistent, account-scoped format.
func printWhoamiAuthRequiredText(w io.Writer, authRequired []accountAuthRequirement) error {
	return printAccountAuthRequirementsText(w, "\nAccounts requiring authentication:", authRequired)
}
