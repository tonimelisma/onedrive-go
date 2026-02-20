package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// pendingTokenFile is the filename for the temporary token saved during login
// before the canonical ID is known. This solves the chicken-and-egg problem:
// we need a token to call /me, but the token filename depends on /me results.
const pendingTokenFile = ".token-pending.json"

// tokenDirPerms is the permission mode for token directories (owner only).
const tokenDirPerms = 0o700

func newLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with OneDrive using device code flow",
		Long: `Authenticate with OneDrive using the device code flow.

Discovers your account type (personal/business) and organization automatically.
Creates or updates the config file with the new drive section.`,
		RunE: runLogin,
	}
}

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove saved authentication token",
		Long: `Remove the saved authentication token for an account.

If only one account is configured, it is selected automatically.
Otherwise, use --account to specify which account to log out.`,
		RunE: runLogout,
	}

	cmd.Flags().Bool("purge", false, "also remove drive config sections and state databases")

	return cmd
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Display the authenticated user and drive info",
		RunE:  runWhoami,
	}
}

// constructCanonicalID builds a canonical drive identifier from the drive type
// and user email. The format is "type:email" (e.g. "personal:user@example.com").
func constructCanonicalID(driveType, email string) string {
	return driveType + ":" + email
}

// canonicalIDParts is the maximum split limit for parsing canonical IDs.
// Using SplitN with limit 3 splits "type:email:rest" into at most 3 parts,
// ensuring the email (parts[1]) is extracted cleanly even for SharePoint IDs
// like "sharepoint:alice@contoso.com:marketing:Docs".
const canonicalIDParts = 3

// accountEmailFromCanonicalID extracts the email portion from a canonical ID.
// For "personal:user@example.com" returns "user@example.com".
// For "sharepoint:alice@contoso.com:marketing:Docs" returns "alice@contoso.com".
// Returns the input unchanged if no colon separator is found.
func accountEmailFromCanonicalID(id string) string {
	parts := strings.SplitN(id, ":", canonicalIDParts)
	if len(parts) < 2 {
		return id
	}

	return parts[1]
}

// driveTypeFromCanonicalID extracts the type portion from a canonical ID.
// For "personal:user@example.com" returns "personal".
// Returns the input unchanged if no colon separator is found.
func driveTypeFromCanonicalID(id string) string {
	parts := strings.SplitN(id, ":", canonicalIDParts)
	if len(parts) < 2 {
		return id
	}

	return parts[0]
}

// findTokenFallback tries personal and business canonical ID prefixes
// and returns whichever one has a token file on disk. Falls back to
// "personal:" if neither exists, since personal is the most common case.
// Logs the probe results so --verbose reveals which token path was selected.
func findTokenFallback(account string, logger *slog.Logger) string {
	personalID := "personal:" + account

	personalPath := config.DriveTokenPath(personalID)
	if personalPath != "" {
		if _, err := os.Stat(personalPath); err == nil {
			logger.Debug("token fallback: found personal token", "path", personalPath)

			return personalID
		}
	}

	businessID := "business:" + account

	businessPath := config.DriveTokenPath(businessID)
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

// runLogin implements the discovery-based login flow per accounts.md section 9:
// device code auth -> /me -> /me/drive -> /me/organization -> construct canonical ID -> config.
func runLogin(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	ctx := context.Background()

	logger.Info("login started")

	// Step 1: Device code auth with a temporary token path. The real token path
	// depends on the canonical ID, which we discover after authentication.
	tempPath := pendingTokenPath()

	ts, err := graph.Login(ctx, tempPath, func(da graph.DeviceAuth) {
		// Device code prompts must always be visible -- not suppressed by --quiet.
		fmt.Fprintf(os.Stderr, "To sign in, visit: %s\n", da.VerificationURI)
		fmt.Fprintf(os.Stderr, "Enter code: %s\n", da.UserCode)
	}, logger)
	if err != nil {
		// Clean up the pending token on auth failure.
		os.Remove(tempPath)

		return err
	}

	// Step 2-4: Discover account details from the Graph API.
	canonicalID, orgName, err := discoverAccount(ctx, ts, logger)
	if err != nil {
		os.Remove(tempPath)

		return fmt.Errorf("discovering account: %w", err)
	}

	// Step 5: Move token from temp path to its canonical location.
	finalTokenPath := config.DriveTokenPath(canonicalID)
	if finalTokenPath == "" {
		os.Remove(tempPath)

		return fmt.Errorf("cannot determine token path for drive %q", canonicalID)
	}

	if moveErr := moveToken(tempPath, finalTokenPath); moveErr != nil {
		return moveErr
	}

	// Step 6: Check if this is a re-login (drive already exists in config).
	email := accountEmailFromCanonicalID(canonicalID)
	cfgPath := resolveLoginConfigPath()

	isRelogin, err := driveExistsInConfig(cfgPath, canonicalID)
	if err != nil {
		logger.Debug("config check failed, treating as new login", "error", err)
	}

	if isRelogin {
		logger.Info("re-login detected, token refreshed", "canonical_id", canonicalID)
		fmt.Printf("Token refreshed for %s.\n", email)

		return nil
	}

	// Step 7: Create or update config with the new drive section.
	driveType := driveTypeFromCanonicalID(canonicalID)

	return writeLoginConfig(cfgPath, canonicalID, driveType, email, orgName, logger)
}

// discoverAccount calls /me, /me/drive, and /me/organization to build the
// canonical drive ID and extract the organization name. Returns the canonical
// ID and org display name.
func discoverAccount(ctx context.Context, ts graph.TokenSource, logger *slog.Logger) (string, string, error) {
	client := graph.NewClient(graph.DefaultBaseURL, defaultHTTPClient(), ts, logger)

	// GET /me -> email, user GUID
	user, err := client.Me(ctx)
	if err != nil {
		return "", "", fmt.Errorf("fetching user profile: %w", err)
	}

	logger.Info("discovered user", "email", user.Email, "display_name", user.DisplayName)

	// GET /me/drives -> driveType (personal, business)
	drives, err := client.Drives(ctx)
	if err != nil {
		return "", "", fmt.Errorf("listing drives: %w", err)
	}

	if len(drives) == 0 {
		return "", "", fmt.Errorf("no drives found for this account")
	}

	driveType := drives[0].DriveType
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

	// GET /me/organization -> org display name (business only)
	var orgName string

	org, err := client.Organization(ctx)
	if err != nil {
		logger.Warn("failed to fetch organization, continuing without org name", "error", err)
	} else if org.DisplayName != "" {
		orgName = org.DisplayName
		logger.Info("discovered organization", "org_name", orgName)
	}

	canonicalID := constructCanonicalID(driveType, user.Email)
	logger.Info("constructed canonical ID", "canonical_id", canonicalID)

	return canonicalID, orgName, nil
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

// resolveLoginConfigPath determines which config file path to use for login.
// Uses --config if set, otherwise the platform default.
func resolveLoginConfigPath() string {
	if flagConfigPath != "" {
		return flagConfigPath
	}

	return config.DefaultConfigPath()
}

// driveExistsInConfig checks whether a canonical ID already exists in the config file.
func driveExistsInConfig(cfgPath, canonicalID string) (bool, error) {
	cfg, err := config.LoadOrDefault(cfgPath)
	if err != nil {
		return false, err
	}

	_, exists := cfg.Drives[canonicalID]

	return exists, nil
}

// collectExistingSyncDirs reads the config file and returns all configured sync_dir values.
// Used for collision detection when picking a default sync directory.
func collectExistingSyncDirs(cfgPath string, logger *slog.Logger) []string {
	cfg, err := config.LoadOrDefault(cfgPath)
	if err != nil {
		logger.Warn("failed to load config for sync dir collision check",
			"config_path", cfgPath, "error", err)

		return nil
	}

	dirs := make([]string, 0, len(cfg.Drives))
	for id := range cfg.Drives {
		if cfg.Drives[id].SyncDir != "" {
			dirs = append(dirs, cfg.Drives[id].SyncDir)
		}
	}

	return dirs
}

// writeLoginConfig creates or appends to the config file with a new drive section,
// and prints the appropriate login success message.
func writeLoginConfig(cfgPath, canonicalID, driveType, email, orgName string, logger *slog.Logger) error {
	existingDirs := collectExistingSyncDirs(cfgPath, logger)
	syncDir := config.DefaultSyncDir(driveType, orgName, existingDirs)

	logger.Info("writing config", "config_path", cfgPath, "canonical_id", canonicalID, "sync_dir", syncDir)

	// Check if config file exists to decide create vs. append.
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if createErr := config.CreateConfigWithDrive(cfgPath, canonicalID, syncDir); createErr != nil {
			return fmt.Errorf("creating config: %w", createErr)
		}
	} else {
		if appendErr := config.AppendDriveSection(cfgPath, canonicalID, syncDir); appendErr != nil {
			return fmt.Errorf("updating config: %w", appendErr)
		}
	}

	printLoginSuccess(driveType, email, orgName, canonicalID, syncDir)

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
		fmt.Println("Run 'onedrive-go drive add' to add them.")
	default:
		fmt.Printf("Signed in as %s.\n", email)
		fmt.Printf("Drive added: %s -> %s\n", canonicalID, syncDir)
	}
}

// runLogout removes the authentication token for an account. Identifies the
// account via --account flag or auto-selects if only one account exists.
func runLogout(cmd *cobra.Command, _ []string) error {
	logger := buildLogger()

	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	cfgPath := resolveLoginConfigPath()

	// Load config to find drives associated with the account.
	cfg, loadErr := config.LoadOrDefault(cfgPath)
	if loadErr != nil {
		logger.Warn("failed to load config, proceeding with --account only", "error", loadErr)
		cfg = config.DefaultConfig()
	}

	// Determine which account to log out.
	account, autoErr := resolveLogoutAccount(cfg)
	if autoErr != nil {
		return autoErr
	}

	logger.Info("logout started", "account", account, "purge", purge)

	return executeLogout(cfg, cfgPath, account, purge, logger)
}

// resolveLogoutAccount determines the account email for logout. Uses --account
// if provided, otherwise auto-selects when there is exactly one account.
func resolveLogoutAccount(cfg *config.Config) (string, error) {
	if flagAccount != "" {
		return flagAccount, nil
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
		email := accountEmailFromCanonicalID(id)
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
	if tokenCanonicalID == "" {
		// No drives in config — probe the filesystem for an existing token.
		tokenCanonicalID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID)
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
		purgeAccountDrives(cfgPath, affected, logger)
	} else {
		fmt.Println("\nState databases and config kept. Run 'onedrive-go login' to re-authenticate.")
	}

	fmt.Println("Sync directories untouched — your files remain on disk.")

	return nil
}

// drivesForAccount returns all canonical IDs whose email matches the given account.
func drivesForAccount(cfg *config.Config, account string) []string {
	var ids []string

	for id := range cfg.Drives {
		if accountEmailFromCanonicalID(id) == account {
			ids = append(ids, id)
		}
	}

	return ids
}

// canonicalIDForToken picks a canonical ID to use for token path derivation.
// SharePoint drives share the business token, so we prefer a non-sharepoint ID.
func canonicalIDForToken(account string, driveIDs []string) string {
	for _, id := range driveIDs {
		if !strings.HasPrefix(id, "sharepoint:") {
			return id
		}
	}

	// All drives are SharePoint — derive the business token ID.
	if len(driveIDs) > 0 {
		return "business:" + account
	}

	return ""
}

// printAffectedDrives lists drives that can no longer sync after logout.
func printAffectedDrives(cfg *config.Config, affected []string) {
	if len(affected) == 0 {
		return
	}

	fmt.Println("Affected drives (can no longer sync):")

	for _, id := range affected {
		syncDir := cfg.Drives[id].SyncDir
		fmt.Printf("  %s (%s)\n", id, syncDir)
	}
}

// purgeSingleDrive removes the state database and config section for one drive.
// Token deletion is handled separately since tokens may be shared (SharePoint).
func purgeSingleDrive(cfgPath, driveID string, logger *slog.Logger) error {
	statePath := config.DriveStatePath(driveID)
	if statePath != "" {
		if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove state database", "path", statePath, "error", err)
		} else if err == nil {
			logger.Info("removed state database", "path", statePath)
		}
	}

	if err := config.DeleteDriveSection(cfgPath, driveID); err != nil {
		return fmt.Errorf("deleting drive section: %w", err)
	}

	return nil
}

// purgeAccountDrives removes drive config sections and state databases for
// all affected drives. Token deletion is already handled before this call.
func purgeAccountDrives(cfgPath string, affected []string, logger *slog.Logger) {
	fmt.Println()

	for _, id := range affected {
		if err := purgeSingleDrive(cfgPath, id, logger); err != nil {
			logger.Warn("failed to purge drive", "drive", id, "error", err)
		} else {
			fmt.Printf("Purged config and state for %s.\n", id)
		}
	}
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

func runWhoami(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	ctx := context.Background()

	// Determine which drive to query. If --drive is set, use it directly.
	// Otherwise try to auto-select from config.
	canonicalID, err := resolveWhoamiDrive()
	if err != nil {
		return err
	}

	tokenPath := config.DriveTokenPath(canonicalID)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine token path for drive %q", canonicalID)
	}

	logger.Debug("whoami", "drive", canonicalID, "token_path", tokenPath)

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return fmt.Errorf("not logged in — run 'onedrive-go login' first")
		}

		return err
	}

	client := graph.NewClient(graph.DefaultBaseURL, defaultHTTPClient(), ts, logger)

	user, err := client.Me(ctx)
	if err != nil {
		return fmt.Errorf("fetching user profile: %w", err)
	}

	drives, err := client.Drives(ctx)
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}

	if flagJSON {
		return printWhoamiJSON(user, drives)
	}

	printWhoamiText(user, drives)

	return nil
}

// resolveWhoamiDrive determines the canonical ID for whoami. Uses --drive if
// set, otherwise loads config and auto-selects when exactly one drive exists.
func resolveWhoamiDrive() (string, error) {
	if flagDrive != "" {
		return flagDrive, nil
	}

	cfgPath := resolveLoginConfigPath()

	cfg, err := config.LoadOrDefault(cfgPath)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Drives) == 0 {
		return "", fmt.Errorf("no accounts configured — run 'onedrive-go login' to get started")
	}

	if len(cfg.Drives) == 1 {
		for id := range cfg.Drives {
			return id, nil
		}
	}

	// Multiple drives — need explicit selection.
	var ids []string
	for id := range cfg.Drives {
		ids = append(ids, id)
	}

	return "", fmt.Errorf(
		"multiple drives configured — specify with --drive:\n  %s",
		strings.Join(ids, "\n  "),
	)
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
			ID:         drives[i].ID,
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
