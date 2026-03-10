package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// sharePointSiteLimit caps the number of SharePoint sites fetched during
// drive list. Use "drive search" for targeted queries, or --all to lift this cap.
const sharePointSiteLimit = 10

// sharePointSiteUnlimited is used when --all is passed to drive list,
// removing the SharePoint site discovery cap.
const sharePointSiteUnlimited = 999

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
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show configured and available drives",
		Long: `Display all configured drives with their sync status, plus all available
drives discovered from your accounts (personal, business, SharePoint).

SharePoint discovery is limited to the first 10 sites by default.
Use --all to show all discoverable drives, or 'drive search' for
targeted SharePoint queries.`,
		RunE: runDriveList,
	}

	cmd.Flags().Bool("all", false, "show all discoverable drives (remove SharePoint site cap)")

	return cmd
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
	OwnerName   string `json:"owner_name,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	HasStateDB  bool   `json:"has_state_db,omitempty"`
}

func runDriveList(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	ctx := cmd.Context()
	cfgPath := cc.CfgPath

	cfg, warnings, err := config.LoadOrDefaultLenient(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	for _, w := range warnings {
		logger.Warn("config issue", "message", w.Message)
	}

	// Section 1: configured drives.
	configured := buildConfiguredDriveEntries(cfg, logger)

	showAll, err := cmd.Flags().GetBool("all")
	if err != nil {
		return fmt.Errorf("reading --all flag: %w", err)
	}

	// Section 2: available drives from network (best-effort).
	available := discoverAvailableDrives(ctx, cfg, showAll, logger)

	// Annotate available drives with local state DB presence. This is a
	// separate pass from discovery — discovery is network I/O, state DB
	// detection is local filesystem I/O. Keeping them separate makes
	// discovery functions pure and this annotation independently testable.
	annotateStateDB(available)

	if cc.Flags.JSON {
		return printDriveListJSON(os.Stdout, configured, available)
	}

	printDriveListText(os.Stdout, configured, available)

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
			orgName, displayName := config.ResolveAccountNames(id, logger)
			otherDirs := config.CollectOtherSyncDirs(cfg, id, logger)
			syncDir = config.DefaultSyncDir(id, orgName, displayName, otherDirs)
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
// existing tokens. Filters out drives already in config. When showAll is true,
// the SharePoint site discovery cap is removed.
func discoverAvailableDrives(ctx context.Context, cfg *config.Config, showAll bool, logger *slog.Logger) []driveListEntry {
	tokens := config.DiscoverTokens(logger)
	if len(tokens) == 0 {
		return nil
	}

	var (
		mu      sync.Mutex
		entries []driveListEntry
	)

	g, gctx := errgroup.WithContext(ctx)

	for _, tokenCID := range tokens {
		g.Go(func() error {
			tokenEntries := discoverDrivesForToken(gctx, tokenCID, cfg, showAll, logger)

			mu.Lock()
			entries = append(entries, tokenEntries...)
			mu.Unlock()

			return nil
		})
	}

	//nolint:errcheck // errors are logged per-token goroutines, never returned
	g.Wait()

	slices.SortFunc(entries, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})

	return entries
}

// discoverDrivesForToken discovers all available drives for a single token.
func discoverDrivesForToken(
	ctx context.Context, tokenCID driveid.CanonicalID,
	cfg *config.Config, showAll bool, logger *slog.Logger,
) []driveListEntry {
	tokenPath := config.DriveTokenPath(tokenCID)
	if tokenPath == "" {
		return nil
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		logger.Debug("skipping token for drive discovery", "token", tokenCID.String(), "error", err)

		return nil
	}

	client := newGraphClient(ts, logger)

	var entries []driveListEntry

	// Discover personal/business drives.
	drives, err := client.Drives(ctx)
	if err != nil {
		logger.Debug("failed to list drives for token", "token", tokenCID.String(), "error", err)

		return nil
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
		spEntries := discoverSharePointDrives(ctx, client, cfg, email, showAll, logger)
		entries = append(entries, spEntries...)
	}

	// Discover shared folders (all account types).
	sharedEntries := discoverSharedDrives(ctx, client, cfg, email, logger)
	entries = append(entries, sharedEntries...)

	return entries
}

// discoverSharePointDrives queries SharePoint sites and their document libraries.
// When showAll is true, the site limit is raised to discover all sites.
func discoverSharePointDrives(
	ctx context.Context, client *graph.Client, cfg *config.Config, email string, showAll bool, logger *slog.Logger,
) []driveListEntry {
	limit := sharePointSiteLimit
	if showAll {
		limit = sharePointSiteUnlimited
	}

	sites, err := client.SearchSites(ctx, "*", limit)
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

// discoverSharedDrives discovers shared folders using search (non-deprecated)
// with SharedWithMe as fallback.
func discoverSharedDrives(
	ctx context.Context, client *graph.Client, cfg *config.Config, email string, logger *slog.Logger,
) []driveListEntry {
	items := searchSharedItemsWithFallback(ctx, client, email, logger)
	if items == nil {
		return nil
	}

	folders := filterSharedFolders(ctx, client, items, cfg, email, logger)

	return sharedFoldersToEntries(folders)
}

// sharedFolderInfo holds a validated shared folder ready for use.
type sharedFolderInfo struct {
	cid         driveid.CanonicalID
	item        *graph.Item // may be enriched with identity
	displayName string
}

// sharedFoldersToEntries converts validated shared folders to drive list entries.
func sharedFoldersToEntries(folders []sharedFolderInfo) []driveListEntry {
	entries := make([]driveListEntry, 0, len(folders))

	for _, f := range folders {
		entries = append(entries, driveListEntry{
			CanonicalID: f.cid.String(),
			DisplayName: f.displayName,
			State:       "available",
			Source:      "available",
			OwnerName:   f.item.SharedOwnerName,
			OwnerEmail:  f.item.SharedOwnerEmail,
		})
	}

	return entries
}

// candidateSharedFolder holds a filtered shared item pending enrichment.
type candidateSharedFolder struct {
	cid  driveid.CanonicalID
	item *graph.Item
}

// filterSharedFolders applies common filtering to shared items: folders only,
// valid remote references, not already configured, identity enrichment
// (parallelized), and display name derivation with collision tracking.
func filterSharedFolders(
	ctx context.Context, client *graph.Client, items []graph.Item,
	cfg *config.Config, email string, logger *slog.Logger,
) []sharedFolderInfo {
	// Phase 1: Filter — identify valid shared folders.
	var candidates []candidateSharedFolder

	for i := range items {
		item := &items[i]

		if !item.IsFolder || item.RemoteDriveID == "" || item.RemoteItemID == "" {
			continue
		}

		cid, cidErr := driveid.ConstructShared(email, item.RemoteDriveID, item.RemoteItemID)
		if cidErr != nil {
			continue
		}

		if _, exists := cfg.Drives[cid]; exists {
			continue
		}

		candidates = append(candidates, candidateSharedFolder{cid: cid, item: item})
	}

	// Phase 2: Enrich — parallel identity resolution (up to 5 concurrent).
	const enrichConcurrency = 5

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(enrichConcurrency)

	for i := range candidates {
		item := candidates[i].item

		g.Go(func() error {
			enrichSharedItem(gctx, client, item, logger)

			return nil
		})
	}

	//nolint:errcheck // enrichment is best-effort; errors logged inside enrichSharedItem
	g.Wait()

	// Phase 3: Name — sequential display name derivation with collision tracking.
	var folders []sharedFolderInfo

	existingNames := make(map[string]bool)

	for _, c := range candidates {
		displayName, nameErr := deriveSharedDisplayName(c.item, existingNames)
		if nameErr != nil {
			logger.Warn("skipping shared folder with no owner identity",
				"name", c.item.Name, "error", nameErr)

			continue
		}

		existingNames[displayName] = true

		folders = append(folders, sharedFolderInfo{
			cid:         c.cid,
			item:        c.item,
			displayName: displayName,
		})
	}

	return folders
}

// enrichSharedItem fills in missing identity fields by fetching the item
// directly via GET /drives/{driveId}/items/{itemId}. Search results lack
// owner email; direct access provides it. Falls back to the original item
// if the enrichment call fails.
func enrichSharedItem(
	ctx context.Context, client *graph.Client, item *graph.Item, logger *slog.Logger,
) {
	if item.SharedOwnerEmail != "" {
		return // already has full identity
	}

	if item.RemoteDriveID == "" || item.RemoteItemID == "" {
		return
	}

	enriched, err := client.GetItem(ctx, driveid.New(item.RemoteDriveID), item.RemoteItemID)
	if err != nil {
		logger.Debug("could not enrich shared item identity",
			"name", item.Name, "error", err)

		return
	}

	// Merge enriched identity into original item (keep original metadata).
	if enriched.SharedOwnerName != "" {
		item.SharedOwnerName = enriched.SharedOwnerName
	}

	if enriched.SharedOwnerEmail != "" {
		item.SharedOwnerEmail = enriched.SharedOwnerEmail
	}
}

// deriveSharedDisplayName builds a human-friendly name for a shared folder.
// Three-step uniqueness escalation per MULTIDRIVE.md §2.1:
//  1. "{FirstName}'s {FolderName}" — if unique
//  2. "{FullName}'s {FolderName}" — if step 1 collides
//  3. "{FullName}'s {FolderName} ({email})" — if step 2 collides
//
// Returns an error when both SharedOwnerName and SharedOwnerEmail are empty —
// this is a data integrity issue (after the remoteItem parsing fix, identity
// should always be populated). Pass nil for existingNames when listing.
func deriveSharedDisplayName(item *graph.Item, existingNames map[string]bool) (string, error) {
	folderName := item.Name
	ownerName := item.SharedOwnerName

	if ownerName == "" && item.SharedOwnerEmail == "" {
		return "", fmt.Errorf("shared item %q has no owner identity (name and email both empty)", item.Name)
	}

	// When owner name is unknown but email is available, use email as identity.
	if ownerName == "" {
		return fmt.Sprintf("%s (shared by %s)", folderName, item.SharedOwnerEmail), nil
	}

	firstName := extractFirstName(ownerName)

	// Step 1: "John's Documents"
	name := fmt.Sprintf("%s's %s", firstName, folderName)
	if existingNames == nil || !existingNames[name] {
		return name, nil
	}

	// Step 2: "John Doe's Documents"
	name = fmt.Sprintf("%s's %s", ownerName, folderName)
	if !existingNames[name] {
		return name, nil
	}

	// Step 3: "John Doe's Documents (john@example.com)"
	return fmt.Sprintf("%s's %s (%s)", ownerName, folderName, item.SharedOwnerEmail), nil
}

// extractFirstName returns the first space-separated token from a full name.
func extractFirstName(fullName string) string {
	if i := strings.Index(fullName, " "); i > 0 {
		return fullName[:i]
	}

	return fullName
}

// driveListJSONOutput is the structured JSON schema for drive list output.
// Separates configured and available drives into distinct top-level keys,
// replacing the flat array that required callers to filter by "source" field.
type driveListJSONOutput struct {
	Configured []driveListEntry `json:"configured"`
	Available  []driveListEntry `json:"available"`
}

func printDriveListJSON(w io.Writer, configured, available []driveListEntry) error {
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

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

// driveLabel returns a human-readable label for a drive list entry.
// Shows "DisplayName (CanonicalID)" when a display name differs from the
// canonical ID, otherwise just the canonical ID.
func driveLabel(e *driveListEntry) string {
	if e.DisplayName != "" && e.DisplayName != e.CanonicalID {
		return fmt.Sprintf("%s (%s)", e.DisplayName, e.CanonicalID)
	}

	return e.CanonicalID
}

func printDriveListText(w io.Writer, configured, available []driveListEntry) {
	if len(configured) == 0 && len(available) == 0 {
		fmt.Fprintln(w, "No drives configured. Run 'onedrive-go login' to get started.")

		return
	}

	if len(configured) > 0 {
		printConfiguredDrives(w, configured)
	}

	if len(available) > 0 {
		if len(configured) > 0 {
			fmt.Fprintln(w)
		}

		printAvailableDrives(w, available)
	}
}

func printConfiguredDrives(w io.Writer, entries []driveListEntry) {
	fmt.Fprintln(w, "Configured drives:")

	maxName, maxDir := 0, 0
	for i := range entries {
		label := driveLabel(&entries[i])
		if len(label) > maxName {
			maxName = len(label)
		}

		sd := entries[i].SyncDir
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

	for i := range entries {
		syncDir := entries[i].SyncDir
		if syncDir == "" {
			syncDir = syncDirNotSet
		}

		fmt.Fprintf(w, fmtStr, driveLabel(&entries[i]), syncDir, entries[i].State)
	}
}

// annotateStateDB sets HasStateDB on available drive entries that have a state
// database on disk from a previous configuration. This is a post-processing
// step separate from network discovery — keeping filesystem I/O out of the
// discovery functions preserves their single responsibility.
func annotateStateDB(entries []driveListEntry) {
	for i := range entries {
		cid, err := driveid.NewCanonicalID(entries[i].CanonicalID)
		if err != nil {
			continue
		}

		path := config.DriveStatePath(cid)
		if path == "" {
			continue
		}

		if _, statErr := os.Stat(path); statErr == nil {
			entries[i].HasStateDB = true
		}
	}
}

func printAvailableDrives(w io.Writer, entries []driveListEntry) {
	fmt.Fprintln(w, "Available drives (not configured):")

	for i := range entries {
		var parts []string
		if entries[i].SiteName != "" {
			parts = append(parts, entries[i].SiteName)
		}

		if entries[i].OwnerEmail != "" {
			parts = append(parts, "shared by "+entries[i].OwnerEmail)
		}

		label := ""
		if len(parts) > 0 {
			label = fmt.Sprintf(" (%s)", strings.Join(parts, ", "))
		}

		stateDBMarker := ""
		if entries[i].HasStateDB {
			stateDBMarker = " [has sync data]"
		}

		fmt.Fprintf(w, "  %s%s%s\n", driveLabel(&entries[i]), label, stateDBMarker)
	}

	fmt.Fprintln(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a drive.")
}

// --- drive add ---

func newDriveAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [canonical-id]",
		Short: "Add a new drive to the configuration",
		Long: `Add a drive to the configuration by canonical ID or shared folder name.

If the drive already exists in config, reports it as already configured.
If the drive is new, it is added with a default sync directory.

For shared drives, you can use a search term instead of a canonical ID.
The term is matched against shared folder names (case-insensitive substring).

Without arguments, lists available drives that can be added.

Examples:
  onedrive-go drive add personal:user@example.com
  onedrive-go drive add sharepoint:user@contoso.com:marketing:Documents
  onedrive-go drive add "Shared Folder"`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runDriveAdd,
		Args:        cobra.MaximumNArgs(1),
	}
}

func runDriveAdd(cmd *cobra.Command, args []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	cfgPath := cc.CfgPath

	// If a positional arg is provided, use it as the canonical ID.
	selector := ""
	if len(args) > 0 {
		selector = args[0]
	}

	// Also accept --drive.
	if selector == "" {
		var driveErr error
		selector, driveErr = cc.Flags.SingleDrive()
		if driveErr != nil {
			return driveErr
		}
	}

	if selector == "" {
		return listAvailableDrives()
	}

	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		// If selector looks like a partial canonical ID (contains ':'), reject
		// with a clear error instead of falling through to substring search.
		if strings.Contains(selector, ":") {
			return fmt.Errorf("invalid canonical ID %q: %w\n"+
				"Run 'onedrive-go drive list' to see valid canonical IDs", selector, err)
		}

		// Not a canonical ID — try substring matching against shared drives.
		return addSharedDriveByName(cmd.Context(), selector, cfgPath, logger)
	}

	if cid.IsShared() {
		return addSharedDrive(cmd.Context(), cfgPath, cid, "", logger)
	}

	return addNewDrive(cfgPath, cid, logger)
}

// addNewDrive adds a new drive to the config with a computed default sync_dir.
// If the drive already exists, reports it as already configured. Token
// existence is verified as a precondition before writing config.
func addNewDrive(cfgPath string, cid driveid.CanonicalID, logger *slog.Logger) error {
	// Verify a token exists for this drive's account.
	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if _, statErr := os.Stat(tokenPath); errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	syncDir, added, err := config.EnsureDriveInConfig(cfgPath, cid, logger)
	if err != nil {
		return fmt.Errorf("adding drive to config: %w", err)
	}

	if !added {
		fmt.Printf("Drive %s is already configured.\n", cid.String())

		return nil
	}

	driveDisplayName := config.DefaultDisplayName(cid)
	fmt.Printf("Added drive %s (%s) -> %s\n", driveDisplayName, cid.String(), syncDir)

	return nil
}

// listAvailableDrives lists drives that can be added. Shows usage guidance
// when no canonical ID argument is provided.
func listAvailableDrives() error {
	fmt.Println("Run 'onedrive-go drive add <canonical-id>' to add a drive.")
	fmt.Println("Run 'onedrive-go drive list' to see available drives.")

	return nil
}

// addSharedDrive adds a shared drive to config by canonical ID.
// If preResolvedName is non-empty, it is used directly (avoids re-querying
// the API when the caller already has the display name from search results).
// If empty, the display name is resolved via the API.
func addSharedDrive(
	ctx context.Context, cfgPath string, cid driveid.CanonicalID,
	preResolvedName string, logger *slog.Logger,
) error {
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if _, exists := cfg.Drives[cid]; exists {
		fmt.Printf("Drive %s is already configured.\n", cid.String())

		return nil
	}

	// Shared drives don't have their own token — find the parent account.
	// DriveTokenPath(sharedCID) reads drive metadata which doesn't exist yet
	// for new drives. Probe the filesystem for existing personal/business tokens.
	parentCID := findTokenFallback(cid.Email(), logger)

	tokenPath := config.DriveTokenPath(parentCID)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if _, statErr := os.Stat(tokenPath); errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	// Register drive metadata so DriveTokenPath works for this shared drive
	// in subsequent operations.
	if saveErr := config.SaveDriveMetadata(cid, &config.DriveMetadata{
		AccountCanonicalID: parentCID.String(),
	}); saveErr != nil {
		return fmt.Errorf("registering shared drive metadata: %w", saveErr)
	}

	displayName := preResolvedName
	if displayName == "" {
		var resolveErr error

		displayName, resolveErr = resolveSharedDisplayName(ctx, cid, tokenPath, cfg, logger)
		if resolveErr != nil {
			displayName = config.DefaultDisplayName(cid)
			logger.Warn("could not derive shared drive display name, using fallback",
				"error", resolveErr, "fallback", displayName)
		}
	}

	syncDir := config.BaseSyncDir(cid, "", displayName)

	if err := config.AppendDriveSection(cfgPath, cid, syncDir); err != nil {
		return fmt.Errorf("writing drive config: %w", err)
	}

	if err := config.SetDriveKey(cfgPath, cid, "display_name", displayName); err != nil {
		return fmt.Errorf("writing display_name to config: %w", err)
	}

	fmt.Printf("Added drive %s (%s) -> %s\n", displayName, cid.String(), syncDir)

	return nil
}

// resolveSharedDisplayName finds the matching shared item and derives a
// collision-free display name. Tries search (non-deprecated) then SharedWithMe.
func resolveSharedDisplayName(
	ctx context.Context, cid driveid.CanonicalID,
	tokenPath string, cfg *config.Config, logger *slog.Logger,
) (string, error) {
	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		return "", err
	}

	client := newGraphClient(ts, logger)
	existingNames := collectExistingDisplayNames(cfg)

	items := searchSharedItemsWithFallback(ctx, client, cid.Email(), logger)

	for i := range items {
		item := &items[i]

		if !item.IsFolder || item.RemoteDriveID == "" || item.RemoteItemID == "" {
			continue
		}

		itemCID, cidErr := driveid.ConstructShared(cid.Email(), item.RemoteDriveID, item.RemoteItemID)
		if cidErr != nil {
			continue
		}

		if itemCID == cid {
			enrichSharedItem(ctx, client, item, logger)

			return deriveSharedDisplayName(item, existingNames)
		}
	}

	return "", fmt.Errorf("shared item not found in API response")
}

// collectExistingDisplayNames gathers all configured drive display names
// for collision detection during shared drive naming.
func collectExistingDisplayNames(cfg *config.Config) map[string]bool {
	names := make(map[string]bool, len(cfg.Drives))

	for id := range cfg.Drives {
		name := cfg.Drives[id].DisplayName
		if name == "" {
			name = config.DefaultDisplayName(id)
		}

		names[name] = true
	}

	return names
}

// sharedMatch holds a shared folder that matched a search term.
type sharedMatch struct {
	cid         driveid.CanonicalID
	displayName string
	ownerEmail  string
	tokenEmail  string // account that discovered this match
}

// addSharedDriveByName searches SharedWithMe for a folder matching the
// given search term (case-insensitive substring match against folder name
// and derived display name). Single match → add. Multiple → show list.
func addSharedDriveByName(
	ctx context.Context, selector, cfgPath string, logger *slog.Logger,
) error {
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	matches := searchSharedDrives(ctx, cfg, selector, logger)

	switch len(matches) {
	case 0:
		return fmt.Errorf(
			"no shared folders matching %q found — run 'onedrive-go drive list' to see available drives",
			selector)

	case 1:
		return addSharedDrive(ctx, cfgPath, matches[0].cid, matches[0].displayName, logger)

	default:
		fmt.Printf("Multiple shared folders match %q — be more specific:\n\n", selector)

		for i := range matches {
			ownerInfo := ""
			if matches[i].ownerEmail != "" {
				ownerInfo = fmt.Sprintf(" (shared by %s)", matches[i].ownerEmail)
			}

			viaInfo := ""
			if matches[i].tokenEmail != "" {
				viaInfo = fmt.Sprintf(" [via %s]", matches[i].tokenEmail)
			}

			fmt.Printf("  %d. %s%s%s\n     %s\n",
				i+1, matches[i].displayName, ownerInfo, viaInfo, matches[i].cid.String())
		}

		fmt.Println("\nRun 'onedrive-go drive add <canonical-id>' to add a specific drive.")

		return nil
	}
}

// searchSharedDrives queries all tokens for shared folders matching selector.
// Uses search-based discovery (non-deprecated) with SharedWithMe fallback.
func searchSharedDrives(
	ctx context.Context, cfg *config.Config, selector string, logger *slog.Logger,
) []sharedMatch {
	tokens := config.DiscoverTokens(logger)
	lowerSelector := strings.ToLower(selector)

	var matches []sharedMatch

	for _, tokenCID := range tokens {
		tokenPath := config.DriveTokenPath(tokenCID)
		if tokenPath == "" {
			continue
		}

		ts, tsErr := graph.TokenSourceFromPath(ctx, tokenPath, logger)
		if tsErr != nil {
			continue
		}

		client := newGraphClient(ts, logger)
		email := tokenCID.Email()

		items := searchSharedItemsWithFallback(ctx, client, email, logger)
		folders := filterSharedFolders(ctx, client, items, cfg, email, logger)

		for _, f := range folders {
			if !strings.Contains(strings.ToLower(f.item.Name), lowerSelector) &&
				!strings.Contains(strings.ToLower(f.displayName), lowerSelector) {
				continue
			}

			matches = append(matches, sharedMatch{
				cid:         f.cid,
				displayName: f.displayName,
				ownerEmail:  f.item.SharedOwnerEmail,
				tokenEmail:  email,
			})
		}
	}

	return matches
}

// searchSharedItemsWithFallback returns shared items from the search endpoint
// (non-deprecated), falling back to SharedWithMe on error.
func searchSharedItemsWithFallback(
	ctx context.Context, client *graph.Client, email string, logger *slog.Logger,
) []graph.Item {
	items, err := client.SearchDriveItems(ctx, "*")
	if err == nil {
		return items
	}

	logger.Debug("search-based discovery failed, trying SharedWithMe",
		"email", email, "error", err)

	items, err = client.SharedWithMe(ctx)
	if err != nil {
		logger.Debug("SharedWithMe fallback also failed",
			"email", email, "error", err)

		return nil
	}

	return items
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

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector == "" {
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

	cid, cidErr := driveid.NewCanonicalID(driveSelector)
	if cidErr != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveSelector, cidErr)
	}

	_, inConfig := cfg.Drives[cid]

	if !inConfig && !purge {
		return fmt.Errorf("drive %q not found in config — use --purge to clean up leftover state", driveSelector)
	}

	// Purge orphaned state: drive not in config but state DB may exist on disk
	// from a previous configuration. Only removes state DB + drive metadata.
	if !inConfig && purge {
		logger.Info("purging orphaned drive state", "drive", cid.String())

		return purgeOrphanedDriveState(cid, logger)
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

// purgeOrphanedDriveState removes state DB and drive metadata files for a
// drive that is no longer in config. Unlike purgeSingleDrive (which also
// removes config sections and account profiles), this only deletes data files
// left behind from a previous `drive remove` without --purge.
func purgeOrphanedDriveState(cid driveid.CanonicalID, logger *slog.Logger) error {
	removed := 0

	// Remove state database.
	statePath := config.DriveStatePath(cid)
	if statePath != "" {
		if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing state database: %w", err)
		} else if err == nil {
			logger.Info("removed state database", "path", statePath)
			removed++
		}
	}

	// Remove drive metadata file.
	metaPath := config.DriveMetadataPath(cid)
	if metaPath != "" {
		if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing drive metadata: %w", err)
		} else if err == nil {
			logger.Info("removed drive metadata", "path", metaPath)
			removed++
		}
	}

	if removed == 0 {
		fmt.Printf("No orphaned state found for %s.\n", cid.String())
	} else {
		fmt.Printf("Purged %d orphaned data file(s) for %s.\n", removed, cid.String())
	}

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
		return printDriveSearchJSON(os.Stdout, results)
	}

	printDriveSearchText(os.Stdout, results, query)

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
	tokenPath := config.DriveTokenPath(tokenCID)
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

func printDriveSearchJSON(w io.Writer, results []driveSearchResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(results); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printDriveSearchText(w io.Writer, results []driveSearchResult, query string) {
	if len(results) == 0 {
		fmt.Fprintf(w, "No SharePoint sites found matching %q.\n", query)

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
	fmt.Fprintf(w, "SharePoint sites matching %q:\n", query)

	currentSite := ""

	for _, r := range sorted {
		if r.SiteName != currentSite {
			if currentSite != "" {
				fmt.Fprintln(w)
			}

			currentSite = r.SiteName
			label := r.SiteName
			if r.WebURL != "" {
				label = fmt.Sprintf("%s (%s)", r.SiteName, r.WebURL)
			}

			fmt.Fprintf(w, "\n  %s\n", label)
		}

		fmt.Fprintf(w, "    %s\n", r.CanonicalID)
	}

	fmt.Fprintf(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a drive.\n")
}
