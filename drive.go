package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// sharePointSiteLimit caps the number of SharePoint sites fetched during
// drive list. Use "drive search" for targeted queries.
const sharePointSiteLimit = 10

// minColumnWidth is the minimum column width for formatted text output,
// preventing narrow columns when all entries happen to be short.
const minColumnWidth = 20

func newDriveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "drive",
		Short:       "Manage drives (list, add, remove, search)",
		Long:        "List, add, remove, or search drives in the configuration.",
		Annotations: map[string]string{skipConfigAnnotation: "true"},
	}

	cmd.AddCommand(newDriveListCmd())
	cmd.AddCommand(newDriveAddCmd())
	cmd.AddCommand(newDriveRemoveCmd())
	cmd.AddCommand(newDriveSearchCmd())

	return cmd
}

// --- drive list ---

func newDriveListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show configured and available drives",
		Long: `Display all configured drives with their sync status, plus all available
drives discovered from your accounts (personal, business, SharePoint).

SharePoint discovery is limited to the first 10 sites. Use 'drive search'
for targeted SharePoint queries.`,
		RunE: runDriveList,
	}
}

// driveListEntry represents one drive in the list output.
type driveListEntry struct {
	CanonicalID string `json:"canonical_id"`
	DisplayName string `json:"display_name,omitempty"`
	SyncDir     string `json:"sync_dir,omitempty"`
	State       string `json:"state"`
	Source      string `json:"source"` // "configured" or "available"
	SiteName    string `json:"site_name,omitempty"`
	LibraryName string `json:"library_name,omitempty"`
}

func runDriveList(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	ctx := cmd.Context()
	cfgPath := cc.CfgPath

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Section 1: configured drives.
	configured := buildConfiguredDriveEntries(cfg, logger)

	// Section 2: available drives from network (best-effort).
	available := discoverAvailableDrives(ctx, cfg, logger)

	if cc.Flags.JSON {
		return printDriveListJSON(configured, available)
	}

	printDriveListText(configured, available)

	return nil
}

// buildConfiguredDriveEntries creates list entries from the config.
func buildConfiguredDriveEntries(cfg *config.Config, logger *slog.Logger) []driveListEntry {
	if len(cfg.Drives) == 0 {
		return nil
	}

	entries := make([]driveListEntry, 0, len(cfg.Drives))

	for id := range cfg.Drives {
		d := cfg.Drives[id]
		state := driveStateReady
		if d.Paused != nil && *d.Paused {
			state = driveStatePaused
		}

		syncDir := d.SyncDir
		if syncDir == "" {
			// Compute default sync_dir for display.
			meta := config.ReadTokenMeta(id, logger)
			otherDirs := config.CollectOtherSyncDirs(cfg, id, logger)
			syncDir = config.DefaultSyncDir(id, meta["org_name"], meta["display_name"], otherDirs)

			if syncDir == "" {
				state = driveStateNeedsSetup
			}
		}

		// Use explicit display_name from config, falling back to auto-derived.
		displayName := d.DisplayName
		if displayName == "" {
			displayName = config.DefaultDisplayName(id)
		}

		entries = append(entries, driveListEntry{
			CanonicalID: id.String(),
			DisplayName: displayName,
			SyncDir:     syncDir,
			State:       state,
			Source:      "configured",
		})
	}

	slices.SortFunc(entries, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})

	return entries
}

// discoverAvailableDrives queries the network for all drives accessible via
// existing tokens. Filters out drives already in config.
func discoverAvailableDrives(ctx context.Context, cfg *config.Config, logger *slog.Logger) []driveListEntry {
	tokens := config.DiscoverTokens(logger)
	if len(tokens) == 0 {
		return nil
	}

	var entries []driveListEntry

	for _, tokenCID := range tokens {
		tokenPath := config.DriveTokenPath(tokenCID, nil)
		if tokenPath == "" {
			continue
		}

		ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
		if err != nil {
			logger.Debug("skipping token for drive discovery", "token", tokenCID.String(), "error", err)

			continue
		}

		client := newGraphClient(ts, logger)

		// Discover personal/business drives.
		drives, err := client.Drives(ctx)
		if err != nil {
			logger.Debug("failed to list drives for token", "token", tokenCID.String(), "error", err)

			continue
		}

		email := tokenCID.Email()

		for _, d := range drives {
			cid, cidErr := driveid.Construct(d.DriveType, email)
			if cidErr != nil {
				continue
			}

			if _, exists := cfg.Drives[cid]; exists {
				continue // already configured
			}

			entries = append(entries, driveListEntry{
				CanonicalID: cid.String(),
				State:       "available",
				Source:      "available",
			})
		}

		// For business accounts, discover SharePoint sites.
		if tokenCID.DriveType() == driveid.DriveTypeBusiness {
			spEntries := discoverSharePointDrives(ctx, client, cfg, email, logger)
			entries = append(entries, spEntries...)
		}
	}

	slices.SortFunc(entries, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})

	return entries
}

// discoverSharePointDrives queries SharePoint sites and their document libraries.
func discoverSharePointDrives(
	ctx context.Context, client *graph.Client, cfg *config.Config, email string, logger *slog.Logger,
) []driveListEntry {
	sites, err := client.SearchSites(ctx, "*", sharePointSiteLimit)
	if err != nil {
		logger.Warn("SharePoint site search failed", "error", err)

		return nil
	}

	var entries []driveListEntry

	for _, site := range sites {
		siteDrives, driveErr := client.SiteDrives(ctx, site.ID)
		if driveErr != nil {
			logger.Debug("failed to list drives for site", "site", site.Name, "error", driveErr)

			continue
		}

		for _, d := range siteDrives {
			cid, cidErr := driveid.ConstructSharePoint(email, site.Name, d.Name)
			if cidErr != nil {
				logger.Debug("skipping SharePoint drive with invalid ID",
					"site", site.Name, "library", d.Name, "error", cidErr)

				continue
			}

			if _, exists := cfg.Drives[cid]; exists {
				continue // already configured
			}

			entries = append(entries, driveListEntry{
				CanonicalID: cid.String(),
				State:       "available",
				Source:      "available",
				SiteName:    site.DisplayName,
				LibraryName: d.Name,
			})
		}
	}

	return entries
}

// driveListJSONOutput is the structured JSON schema for drive list output.
// Separates configured and available drives into distinct top-level keys,
// replacing the flat array that required callers to filter by "source" field.
type driveListJSONOutput struct {
	Configured []driveListEntry `json:"configured"`
	Available  []driveListEntry `json:"available"`
}

func printDriveListJSON(configured, available []driveListEntry) error {
	// Initialize nil slices to empty so JSON renders [] not null.
	if configured == nil {
		configured = []driveListEntry{}
	}

	if available == nil {
		available = []driveListEntry{}
	}

	out := driveListJSONOutput{
		Configured: configured,
		Available:  available,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

// driveLabel returns a human-readable label for a drive list entry.
// Shows "DisplayName (CanonicalID)" when a display name differs from the
// canonical ID, otherwise just the canonical ID.
func driveLabel(e driveListEntry) string {
	if e.DisplayName != "" && e.DisplayName != e.CanonicalID {
		return fmt.Sprintf("%s (%s)", e.DisplayName, e.CanonicalID)
	}

	return e.CanonicalID
}

func printDriveListText(configured, available []driveListEntry) {
	if len(configured) == 0 && len(available) == 0 {
		fmt.Println("No drives configured. Run 'onedrive-go login' to get started.")

		return
	}

	if len(configured) > 0 {
		fmt.Println("Configured drives:")

		// Compute dynamic column widths from content, with minimums for readability.
		maxName, maxDir := 0, 0
		for _, e := range configured {
			label := driveLabel(e)
			if len(label) > maxName {
				maxName = len(label)
			}

			sd := e.SyncDir
			if sd == "" {
				sd = syncDirNotSet
			}

			if len(sd) > maxDir {
				maxDir = len(sd)
			}
		}

		maxName = max(maxName, minColumnWidth)
		maxDir = max(maxDir, minColumnWidth)

		fmtStr := fmt.Sprintf("  %%-%ds  %%-%ds  %%s\n", maxName, maxDir)

		for _, e := range configured {
			syncDir := e.SyncDir
			if syncDir == "" {
				syncDir = syncDirNotSet
			}

			fmt.Printf(fmtStr, driveLabel(e), syncDir, e.State)
		}
	}

	if len(available) > 0 {
		if len(configured) > 0 {
			fmt.Println()
		}

		fmt.Println("Available drives (not configured):")

		for _, e := range available {
			label := ""
			if e.SiteName != "" {
				label = fmt.Sprintf(" (%s)", e.SiteName)
			}

			fmt.Printf("  %s%s\n", e.CanonicalID, label)
		}

		fmt.Println("\nRun 'onedrive-go drive add <canonical-id>' to add a drive.")
	}
}

// --- drive add ---

func newDriveAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [canonical-id]",
		Short: "Add a new drive to the configuration",
		Long: `Add a drive to the configuration by canonical ID.

If the drive already exists in config, reports it as already configured.
If the drive is new, it is added with a default sync directory.

Without arguments, lists available drives that can be added.

Examples:
  onedrive-go drive add personal:user@example.com
  onedrive-go drive add sharepoint:user@contoso.com:marketing:Documents`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runDriveAdd,
		Args:        cobra.MaximumNArgs(1),
	}
}

func runDriveAdd(cmd *cobra.Command, args []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	cfgPath := cc.CfgPath

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// If a positional arg is provided, use it as the canonical ID.
	selector := ""
	if len(args) > 0 {
		selector = args[0]
	}

	// Also accept --drive for backwards compatibility.
	if selector == "" && cc.Flags.Drive != "" {
		selector = cc.Flags.Drive
	}

	if selector == "" {
		return listAvailableDrives(cfg)
	}

	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		return fmt.Errorf("invalid canonical ID %q: %w", selector, err)
	}

	// Check if the drive already exists in config.
	if _, exists := cfg.Drives[cid]; exists {
		fmt.Printf("Drive %s is already configured.\n", cid.String())

		return nil
	}

	return addNewDrive(cfgPath, cfg, cid, logger)
}

// addNewDrive adds a new drive to the config with a computed default sync_dir.
func addNewDrive(cfgPath string, cfg *config.Config, cid driveid.CanonicalID, logger *slog.Logger) error {
	// Verify a token exists for this drive's account.
	tokenCID, err := config.TokenCanonicalID(cid, cfg)
	if err != nil {
		return fmt.Errorf("cannot resolve token for %s: %w", cid.String(), err)
	}

	tokenPath := config.DriveTokenPath(tokenCID, nil)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if _, err := os.Stat(tokenPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	// Compute default sync_dir using the shared config helpers.
	meta := config.ReadTokenMeta(cid, logger)
	existingDirs := config.CollectOtherSyncDirs(cfg, cid, logger)
	syncDir := config.DefaultSyncDir(cid, meta["org_name"], meta["display_name"], existingDirs)
	logger.Info("adding drive", "canonical_id", cid.String(), "sync_dir", syncDir)

	// Append to config.
	if err := config.AppendDriveSection(cfgPath, cid, syncDir); err != nil {
		return fmt.Errorf("adding drive to config: %w", err)
	}

	driveDisplayName := config.DefaultDisplayName(cid)
	fmt.Printf("Added drive %s (%s) -> %s\n", driveDisplayName, cid.String(), syncDir)

	return nil
}

// listAvailableDrives lists drives that can be added. Shows usage guidance
// when no canonical ID argument is provided.
func listAvailableDrives(_ *config.Config) error {
	fmt.Println("Run 'onedrive-go drive add <canonical-id>' to add a drive.")
	fmt.Println("Run 'onedrive-go drive list' to see available drives.")

	return nil
}

// --- drive remove ---

func newDriveRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a drive from the configuration",
		Long: `Remove a drive's config section. The token, state database, and sync directory
are preserved so the drive can be re-added later without data loss.

With --purge, the state database is also deleted.
The sync directory is never deleted automatically.`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runDriveRemove,
	}

	cmd.Flags().Bool("purge", false, "also delete the state database")

	return cmd
}

func runDriveRemove(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger

	if cc.Flags.Drive == "" {
		return fmt.Errorf("--drive is required (specify which drive to remove)")
	}

	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	cfgPath := cc.CfgPath

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, cidErr := driveid.NewCanonicalID(cc.Flags.Drive)
	if cidErr != nil {
		return fmt.Errorf("invalid drive ID %q: %w", cc.Flags.Drive, cidErr)
	}

	if _, exists := cfg.Drives[cid]; !exists {
		return fmt.Errorf("drive %q not found in config", cc.Flags.Drive)
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)

	if purge {
		return purgeDrive(cfgPath, cid, logger)
	}

	return removeDrive(cfgPath, cid, cfg.Drives[cid].SyncDir, logger)
}

// removeDrive deletes the config section for the drive, preserving token,
// state database, and sync directory.
func removeDrive(cfgPath string, driveID driveid.CanonicalID, syncDir string, logger *slog.Logger) error {
	if err := config.DeleteDriveSection(cfgPath, driveID); err != nil {
		return fmt.Errorf("removing drive: %w", err)
	}

	logger.Info("removed drive config section", "drive", driveID.String())

	idStr := driveID.String()
	fmt.Printf("Removed drive %s from config.\n", idStr)
	fmt.Printf("Token and state database kept for %s.\n", idStr)
	fmt.Printf("Sync directory untouched: %s\n", syncDir)
	fmt.Println("Run 'onedrive-go drive add " + idStr + "' to re-add.")

	return nil
}

// purgeDrive deletes the config section and state database for a drive.
// The token is NOT deleted here — it may be shared with other drives (SharePoint).
func purgeDrive(cfgPath string, driveID driveid.CanonicalID, logger *slog.Logger) error {
	if err := purgeSingleDrive(cfgPath, driveID, logger); err != nil {
		return err
	}

	fmt.Printf("Purged config and state for %s.\n", driveID.String())
	fmt.Println("Sync directory untouched — delete manually if desired.")

	return nil
}

// --- drive search ---

func newDriveSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <term>",
		Short: "Search SharePoint sites by name",
		Long: `Search for SharePoint sites matching the given term.

Lists matching sites and their document libraries with canonical IDs
that can be used with 'drive add'.

Use --account to restrict the search to a specific business account.

Examples:
  onedrive-go drive search marketing
  onedrive-go drive search "project docs" --account user@contoso.com`,
		RunE: runDriveSearch,
		Args: cobra.ExactArgs(1),
	}
}

// driveSearchResult represents a search result for JSON output.
type driveSearchResult struct {
	CanonicalID string `json:"canonical_id"`
	SiteName    string `json:"site_name"`
	LibraryName string `json:"library_name"`
	WebURL      string `json:"web_url,omitempty"`
}

// sharePointSearchLimit caps the number of SharePoint sites returned per search query.
const sharePointSearchLimit = 50

func runDriveSearch(cmd *cobra.Command, args []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	ctx := cmd.Context()
	query := args[0]

	businessTokens := findBusinessTokens(cc.Flags.Account, logger)

	if len(businessTokens) == 0 {
		if cc.Flags.Account != "" {
			return fmt.Errorf("no business account found for %s — run 'onedrive-go login' first", cc.Flags.Account)
		}

		return fmt.Errorf("no business accounts found — SharePoint search requires a business account")
	}

	var results []driveSearchResult

	for _, tokenCID := range businessTokens {
		accountResults := searchAccountSharePoint(ctx, tokenCID, query, logger)
		results = append(results, accountResults...)
	}

	if cc.Flags.JSON {
		return printDriveSearchJSON(results)
	}

	printDriveSearchText(results, query)

	return nil
}

// findBusinessTokens returns all business account tokens, optionally filtered
// by accountFilter. When non-empty, accountFilter is matched against the
// token's email exactly (case-sensitive, not a partial match or canonical ID).
func findBusinessTokens(accountFilter string, logger *slog.Logger) []driveid.CanonicalID {
	tokens := config.DiscoverTokens(logger)

	var businessTokens []driveid.CanonicalID
	for _, t := range tokens {
		if t.DriveType() == driveid.DriveTypeBusiness {
			if accountFilter == "" || t.Email() == accountFilter {
				businessTokens = append(businessTokens, t)
			}
		}
	}

	return businessTokens
}

// searchAccountSharePoint searches SharePoint sites for a single business account.
func searchAccountSharePoint(
	ctx context.Context, tokenCID driveid.CanonicalID, query string, logger *slog.Logger,
) []driveSearchResult {
	tokenPath := config.DriveTokenPath(tokenCID, nil)
	if tokenPath == "" {
		return nil
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		logger.Debug("skipping token for search", "token", tokenCID.String(), "error", err)

		return nil
	}

	client := newGraphClient(ts, logger)

	sites, err := client.SearchSites(ctx, query, sharePointSearchLimit)
	if err != nil {
		logger.Warn("SharePoint search failed", "account", tokenCID.Email(), "error", err)

		return nil
	}

	email := tokenCID.Email()
	var results []driveSearchResult

	for _, site := range sites {
		siteDrives, driveErr := client.SiteDrives(ctx, site.ID)
		if driveErr != nil {
			logger.Debug("failed to list drives for site", "site", site.Name, "error", driveErr)

			continue
		}

		for _, d := range siteDrives {
			cid, cidErr := driveid.ConstructSharePoint(email, site.Name, d.Name)
			if cidErr != nil {
				logger.Debug("skipping SharePoint drive with invalid ID",
					"site", site.Name, "library", d.Name, "error", cidErr)

				continue
			}

			results = append(results, driveSearchResult{
				CanonicalID: cid.String(),
				SiteName:    site.DisplayName,
				LibraryName: d.Name,
				WebURL:      site.WebURL,
			})
		}
	}

	return results
}

func printDriveSearchJSON(results []driveSearchResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(results); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printDriveSearchText(results []driveSearchResult, query string) {
	if len(results) == 0 {
		fmt.Printf("No SharePoint sites found matching %q.\n", query)

		return
	}

	// Sort a copy so the caller's slice is not mutated.
	sorted := slices.Clone(results)
	slices.SortFunc(sorted, func(a, b driveSearchResult) int {
		if c := cmp.Compare(a.SiteName, b.SiteName); c != 0 {
			return c
		}

		return cmp.Compare(a.LibraryName, b.LibraryName)
	})

	// Group by site for readable output.
	fmt.Printf("SharePoint sites matching %q:\n", query)

	currentSite := ""

	for _, r := range sorted {
		if r.SiteName != currentSite {
			if currentSite != "" {
				fmt.Println()
			}

			currentSite = r.SiteName
			label := r.SiteName
			if r.WebURL != "" {
				label = fmt.Sprintf("%s (%s)", r.SiteName, r.WebURL)
			}

			fmt.Printf("\n  %s\n", label)
		}

		fmt.Printf("    %s\n", r.CanonicalID)
	}

	fmt.Printf("\nRun 'onedrive-go drive add <canonical-id>' to add a drive.\n")
}
