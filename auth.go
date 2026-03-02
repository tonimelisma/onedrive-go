package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// pendingTokenFile is the filename for the temporary token saved during login
// before the canonical ID is known. This solves the chicken-and-egg problem:
// we need a token to call /me, but the token filename depends on /me results.
const pendingTokenFile = ".token-pending.json"

// tokenDirPerms is the permission mode for token directories (owner only).
const tokenDirPerms = 0o700

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

	personalPath := config.DriveTokenPath(personalID, nil)
	if personalPath != "" {
		if _, err := os.Stat(personalPath); err == nil {
			logger.Debug("token fallback: found personal token", "path", personalPath)

			return personalID
		}
	}

	businessID := driveid.MustCanonicalID("business:" + account)

	businessPath := config.DriveTokenPath(businessID, nil)
	if businessPath != "" {
		if _, err := os.Stat(businessPath); err == nil {
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
func openBrowser(rawURL string) error {
	ctx := context.Background()

	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL)
	default:
		return fmt.Errorf("unsupported platform %s: open the URL manually", runtime.GOOS)
	}

	return cmd.Start()
}

// runLogin implements the discovery-based login flow per accounts.md section 9:
// device code auth -> /me -> /me/drive -> /me/organization -> construct canonical ID -> config.
func runLogin(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	ctx := cmd.Context()

	useBrowser, err := cmd.Flags().GetBool("browser")
	if err != nil {
		return fmt.Errorf("reading --browser flag: %w", err)
	}

	logger.Info("login started", slog.Bool("browser", useBrowser))

	// Step 1: Authenticate with a temporary token path. The real token path
	// depends on the canonical ID, which we discover after authentication.
	tempPath := pendingTokenPath()

	var ts graph.TokenSource

	if useBrowser {
		ts, err = graph.LoginWithBrowser(ctx, tempPath, openBrowser, logger)
	} else {
		ts, err = graph.Login(ctx, tempPath, func(da graph.DeviceAuth) {
			// Device code prompts must always be visible -- not suppressed by --quiet.
			fmt.Fprintf(os.Stderr, "To sign in, visit: %s\n", da.VerificationURI)
			fmt.Fprintf(os.Stderr, "Enter code: %s\n", da.UserCode)
		}, logger)
	}

	if err != nil {
		// Clean up the pending token on auth failure.
		os.Remove(tempPath)

		return err
	}

	// Step 2-4: Discover account details from the Graph API.
	canonicalID, user, orgName, primaryDriveID, err := discoverAccount(ctx, ts, logger)
	if err != nil {
		os.Remove(tempPath)

		return fmt.Errorf("discovering account: %w", err)
	}

	// Step 5: Move token from temp path to its canonical location.
	finalTokenPath := config.DriveTokenPath(canonicalID, nil)
	if finalTokenPath == "" {
		os.Remove(tempPath)

		return fmt.Errorf("cannot determine token path for drive %q", canonicalID.String())
	}

	if moveErr := moveToken(tempPath, finalTokenPath); moveErr != nil {
		return moveErr
	}

	// Step 5b: Save metadata to the token file. Every login (including re-login)
	// refreshes cached metadata so org renames and display name changes propagate.
	// drive_id is cached here to avoid runtime re-discovery (DRY: the Graph API
	// was already called in discoverAccount).
	if saveErr := tokenfile.LoadAndMergeMeta(finalTokenPath, map[string]string{
		"user_id":      user.ID,
		"display_name": user.DisplayName,
		"org_name":     orgName,
		"drive_id":     primaryDriveID.String(),
		"cached_at":    time.Now().UTC().Format(time.RFC3339),
	}); saveErr != nil {
		logger.Warn("failed to save cached metadata", "error", saveErr)
	}

	// Step 6: Ensure drive is in config (idempotent — handles both new login and re-login).
	email := canonicalID.Email()
	cfgPath := cc.CfgPath

	syncDir, added, err := config.EnsureDriveInConfig(cfgPath, canonicalID, logger)
	if err != nil {
		return fmt.Errorf("configuring drive: %w", err)
	}

	if !added {
		logger.Info("re-login detected, token and metadata refreshed", "canonical_id", canonicalID.String())
		fmt.Printf("Token refreshed for %s.\n", email)

		return nil
	}

	printLoginSuccess(canonicalID.DriveType(), email, orgName, canonicalID.String(), syncDir)

	return nil
}

// discoverAccount calls /me, /me/drive, and /me/organization to build the
// canonical drive ID and extract the organization name. Returns the canonical
// ID, user profile, org display name, and the primary drive's Graph API ID.
func discoverAccount(
	ctx context.Context, ts graph.TokenSource, logger *slog.Logger,
) (driveid.CanonicalID, *graph.User, string, driveid.ID, error) {
	client := newGraphClient(ts, logger)

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
	case "personal", "business", "documentLibrary": //nolint:goconst // case labels are self-documenting
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
	if err := os.MkdirAll(dir, tokenDirPerms); err != nil {
		os.Remove(src)

		return fmt.Errorf("creating token directory: %w", err)
	}

	if err := os.Rename(src, dst); err != nil {
		os.Remove(src)

		return fmt.Errorf("moving token to final path: %w", err)
	}

	return nil
}

// printLoginSuccess prints the user-facing login output. Format differs
// for personal vs. business accounts per accounts.md section 9.
func printLoginSuccess(driveType, email, orgName, canonicalID, syncDir string) {
	switch driveType {
	case "personal":
		fmt.Printf("Signed in as %s (personal account).\n", email)
		fmt.Printf("Drive added: %s -> %s\n", canonicalID, syncDir)
	case "business":
		orgLabel := orgName
		if orgLabel == "" {
			orgLabel = "business account"
		}

		fmt.Printf("Signed in as %s (%s).\n", email, orgLabel)
		fmt.Printf("Drive added: %s -> %s\n", canonicalID, syncDir)
		fmt.Println()
		fmt.Println("You also have access to SharePoint libraries.")
		fmt.Println("Run 'onedrive-go drive search <term>' to find and add them.")
	default:
		fmt.Printf("Signed in as %s.\n", email)
		fmt.Printf("Drive added: %s -> %s\n", canonicalID, syncDir)
	}
}

// runLogout removes the authentication token for an account. Identifies the
// account via --account flag or auto-selects if only one account exists.
func runLogout(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger

	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	cfgPath := cc.CfgPath

	// Load config to find drives associated with the account.
	cfg, loadErr := config.LoadOrDefault(cfgPath, logger)
	if loadErr != nil {
		logger.Warn("failed to load config, proceeding with --account only", "error", loadErr)
		cfg = config.DefaultConfig()
	}

	// Determine which account to log out.
	account, autoErr := resolveLogoutAccount(cfg, cc.Flags.Account)
	if autoErr != nil {
		return autoErr
	}

	logger.Info("logout started", "account", account, "purge", purge)

	return executeLogout(cfg, cfgPath, account, purge, logger)
}

// resolveLogoutAccount determines the account email for logout. Uses the
// account flag if provided, otherwise auto-selects when there is exactly one account.
func resolveLogoutAccount(cfg *config.Config, accountFlag string) (string, error) {
	if accountFlag != "" {
		return accountFlag, nil
	}

	// Collect unique account emails from configured drives.
	accounts := uniqueAccounts(cfg)

	if len(accounts) == 0 {
		return "", fmt.Errorf("no accounts configured — nothing to log out")
	}

	if len(accounts) == 1 {
		return accounts[0], nil
	}

	return "", fmt.Errorf(
		"multiple accounts configured — specify with --account:\n  %s",
		strings.Join(accounts, "\n  "),
	)
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
func executeLogout(cfg *config.Config, cfgPath, account string, purge bool, logger *slog.Logger) error {
	// Find all drives belonging to this account.
	affected := drivesForAccount(cfg, account)

	// Determine canonical ID for the token path. We need any drive ID with this
	// account email to derive the token path (all drives for one account share a token).
	tokenCanonicalID := canonicalIDForToken(account, affected)
	if tokenCanonicalID.IsZero() {
		// No drives in config — probe the filesystem for an existing token.
		tokenCanonicalID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID, nil)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine token path for account %q", account)
	}

	// Delete the token file.
	if err := graph.Logout(tokenPath, logger); err != nil {
		return err
	}

	logger.Info("logout successful", "account", account, "token_path", tokenPath)
	fmt.Printf("Token removed for %s.\n", account)

	printAffectedDrives(cfg, affected)

	if purge {
		if err := purgeAccountDrives(cfgPath, affected, logger); err != nil {
			return fmt.Errorf("purging account drives: %w", err)
		}

		fmt.Println("Sync directories untouched — your files remain on disk.")
	} else {
		if err := removeAccountDriveConfigs(cfgPath, affected, logger); err != nil {
			return fmt.Errorf("removing drive configs: %w", err)
		}

		fmt.Println("\nState databases kept. Run 'onedrive-go login' to re-authenticate.")
		fmt.Println("Sync directories untouched — your files remain on disk.")
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
// Uses driveid.TokenCanonicalID() to handle the SharePoint→business mapping.
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
func printAffectedDrives(cfg *config.Config, affected []driveid.CanonicalID) {
	if len(affected) == 0 {
		return
	}

	fmt.Println("Affected drives (can no longer sync):")

	for _, id := range affected {
		syncDir := cfg.Drives[id].SyncDir
		fmt.Printf("  %s (%s)\n", id.String(), syncDir)
	}
}

// purgeSingleDrive removes the state database and config section for one drive.
// Token deletion is handled separately since tokens may be shared (SharePoint).
func purgeSingleDrive(cfgPath string, canonicalID driveid.CanonicalID, logger *slog.Logger) error {
	statePath := config.DriveStatePath(canonicalID)
	if statePath != "" {
		if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove state database", "path", statePath, "error", err)
		} else if err == nil {
			logger.Info("removed state database", "path", statePath)
		}
	}

	if err := config.DeleteDriveSection(cfgPath, canonicalID); err != nil {
		return fmt.Errorf("deleting drive section: %w", err)
	}

	return nil
}

// purgeAccountDrives removes drive config sections and state databases for
// all affected drives. Token deletion is already handled before this call.
func purgeAccountDrives(cfgPath string, affected []driveid.CanonicalID, logger *slog.Logger) error {
	fmt.Println()

	var errs []error

	for _, cid := range affected {
		if err := purgeSingleDrive(cfgPath, cid, logger); err != nil {
			logger.Warn("failed to purge drive", "drive", cid.String(), "error", err)
			errs = append(errs, fmt.Errorf("purging drive %s: %w", cid.String(), err))
		} else {
			fmt.Printf("Purged config and state for %s.\n", cid.String())
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
	User   whoamiUser    `json:"user"`
	Drives []whoamiDrive `json:"drives"`
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
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	ctx := cmd.Context()

	// Delegate drive resolution to config.MatchDrive for consistent behavior.
	cfgPath := cc.CfgPath

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, _, err := config.MatchDrive(cfg, cc.Flags.SingleDrive(), logger)
	if err != nil {
		return err
	}

	tokenPath := config.DriveTokenPath(cid, nil)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine token path for drive %q", cid.String())
	}

	logger.Debug("whoami", "drive", cid.String(), "token_path", tokenPath)

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return fmt.Errorf("not logged in — run 'onedrive-go login' first")
		}

		return err
	}

	client := newGraphClient(ts, logger)

	user, err := client.Me(ctx)
	if err != nil {
		return fmt.Errorf("fetching user profile: %w", err)
	}

	drives, err := client.Drives(ctx)
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}

	if cc.Flags.JSON {
		return printWhoamiJSON(user, drives)
	}

	printWhoamiText(user, drives)

	return nil
}

func printWhoamiJSON(user *graph.User, drives []graph.Drive) error {
	out := whoamiOutput{
		User: whoamiUser{
			ID:          user.ID,
			DisplayName: user.DisplayName,
			Email:       user.Email,
		},
		Drives: make([]whoamiDrive, 0, len(drives)),
	}

	for i := range drives {
		out.Drives = append(out.Drives, whoamiDrive{
			ID:         drives[i].ID.String(),
			Name:       drives[i].Name,
			DriveType:  drives[i].DriveType,
			QuotaUsed:  drives[i].QuotaUsed,
			QuotaTotal: drives[i].QuotaTotal,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printWhoamiText(user *graph.User, drives []graph.Drive) {
	fmt.Printf("User:  %s (%s)\n", user.DisplayName, user.Email)
	fmt.Printf("ID:    %s\n", user.ID)

	for i := range drives {
		fmt.Printf("\nDrive: %s (%s)\n", drives[i].Name, drives[i].DriveType)
		fmt.Printf("  ID:    %s\n", drives[i].ID)
		fmt.Printf("  Quota: %s / %s\n", formatSize(drives[i].QuotaUsed), formatSize(drives[i].QuotaTotal))
	}
}
