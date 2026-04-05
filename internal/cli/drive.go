package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
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
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
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
		// skipConfig: drive list loads config leniently itself (R-4.8.4) —
		// Phase 2 strict loading must not run.
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveList,
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
	AuthState   string `json:"auth_state,omitempty"`
	AuthReason  string `json:"auth_reason,omitempty"`
	Source      string `json:"source"` // "configured" or "available"
	SiteName    string `json:"site_name,omitempty"`
	LibraryName string `json:"library_name,omitempty"`
	OwnerName   string `json:"owner_name,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	HasStateDB  bool   `json:"has_state_db,omitempty"`

	// parsedCID holds the pre-parsed canonical ID, avoiding string→CanonicalID
	// re-parsing in annotateStateDB. Not serialized to JSON — internal only.
	parsedCID driveid.CanonicalID
}

func runDriveList(cmd *cobra.Command, _ []string) error {
	showAll, err := cmd.Flags().GetBool("all")
	if err != nil {
		return fmt.Errorf("reading --all flag: %w", err)
	}

	return newDriveService(mustCLIContext(cmd.Context())).runList(cmd.Context(), showAll)
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
		if d.IsPaused(time.Now()) {
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
			parsedCID:   id,
		})
	}

	slices.SortFunc(entries, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})

	return entries
}

func buildConfiguredAuthRequirements(
	cfg *config.Config,
	authByEmail map[string]accountAuthHealth,
	logger *slog.Logger,
) []accountAuthRequirement {
	grouped, order := groupDrivesByAccount(cfg)
	var result []accountAuthRequirement

	for _, email := range order {
		health := authByEmail[email]
		if health.State != authStateAuthenticationNeeded {
			continue
		}

		driveIDs := grouped[email]
		displayName, _ := readAccountMeta(email, driveIDs, logger)
		result = append(result, authRequirement(
			email,
			displayName,
			accountDriveType(driveIDs),
			len(config.DiscoverStateDBsForEmail(email, logger)),
			health,
		))
	}

	return result
}

func annotateConfiguredDriveAuth(entries []driveListEntry, authByEmail map[string]accountAuthHealth) {
	for i := range entries {
		email := entries[i].parsedCID.Email()
		health, ok := authByEmail[email]
		if !ok {
			continue
		}

		entries[i].AuthState = health.State
		entries[i].AuthReason = health.Reason
	}
}

// discoverAvailableDrives queries the network for all drives accessible via
// existing tokens. Filters out drives already in config. spSiteLimit controls
// how many SharePoint sites are fetched (sharePointSiteLimit for default,
// sharePointSiteUnlimited when --all is passed).
func discoverAvailableDrives(
	ctx context.Context,
	cfg *config.Config,
	spSiteLimit int,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	httpProvider *graphhttp.Provider,
) ([]driveListEntry, []accountAuthRequirement) {
	tokens := config.DiscoverTokens(logger)
	if len(tokens) == 0 {
		return nil, nil
	}

	var (
		mu           sync.Mutex // guards entries and authRequired while token workers append results
		entries      []driveListEntry
		authRequired []accountAuthRequirement
	)

	var wg sync.WaitGroup

	for _, tokenCID := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()

			tokenEntries, tokenAuthRequired := discoverDrivesForToken(
				ctx,
				tokenCID,
				cfg,
				spSiteLimit,
				logger,
				recorder,
				baseURL,
				httpProvider,
			)
			mu.Lock()
			entries = append(entries, tokenEntries...)
			authRequired = append(authRequired, tokenAuthRequired...)
			mu.Unlock()
		}()
	}

	wg.Wait()

	slices.SortFunc(entries, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})

	return entries, mergeAuthRequirements(authRequired)
}

// discoverDrivesForToken discovers all available drives for a single token.
func discoverDrivesForToken(
	ctx context.Context, tokenCID driveid.CanonicalID,
	cfg *config.Config,
	spSiteLimit int,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	httpProvider *graphhttp.Provider,
) ([]driveListEntry, []accountAuthRequirement) {
	tokenPath := config.DriveTokenPath(tokenCID)
	if tokenPath == "" {
		return nil, nil
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		logger.Debug("skipping token for drive discovery", "token", tokenCID.String(), "error", err)

		return nil, []accountAuthRequirement{tokenDiscoveryAuthRequirement(tokenCID, err, logger)}
	}

	client, clientErr := newGraphClientWithHTTP(
		baseURL,
		httpProvider.BootstrapMeta(),
		ts,
		logger,
	)
	if clientErr != nil {
		logger.Debug("skipping token for drive discovery", "token", tokenCID.String(), "error", clientErr)

		return nil, nil
	}
	attachAccountAuthProof(client, recorder, tokenCID.Email(), "drive-list")

	var entries []driveListEntry

	// Discover personal/business drives.
	drives, err := client.Drives(ctx)
	if err != nil {
		logger.Debug("failed to list drives for token", "token", tokenCID.String(), "error", err)

		if errors.Is(err, graph.ErrUnauthorized) {
			return nil, []accountAuthRequirement{tokenAuthRequirement(tokenCID, authReasonSyncAuthRejected, logger)}
		}

		return nil, nil
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
			parsedCID:   cid,
		})
	}

	// For business accounts, discover SharePoint sites.
	if tokenCID.DriveType() == driveid.DriveTypeBusiness {
		spEntries := discoverSharePointDrives(ctx, client, cfg, email, spSiteLimit, logger)
		entries = append(entries, spEntries...)
	}

	// Discover shared folders (all account types).
	sharedEntries := discoverSharedDrives(ctx, client, cfg, email, logger)
	entries = append(entries, sharedEntries...)

	return entries, nil
}

func tokenDiscoveryAuthRequirement(
	tokenCID driveid.CanonicalID,
	err error,
	logger *slog.Logger,
) accountAuthRequirement {
	switch {
	case errors.Is(err, graph.ErrNotLoggedIn):
		return tokenAuthRequirement(tokenCID, authReasonMissingLogin, logger)
	default:
		return tokenAuthRequirement(tokenCID, authReasonInvalidSavedLogin, logger)
	}
}

func tokenAuthRequirement(
	tokenCID driveid.CanonicalID,
	reason string,
	logger *slog.Logger,
) accountAuthRequirement {
	displayName, _ := readAccountMeta(tokenCID.Email(), []driveid.CanonicalID{tokenCID}, logger)
	return authRequirement(
		tokenCID.Email(),
		displayName,
		tokenCID.DriveType(),
		len(config.DiscoverStateDBsForEmail(tokenCID.Email(), logger)),
		accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: reason,
			Action: authAction(reason),
		},
	)
}

// discoverSharePointDrives queries SharePoint sites and their document libraries.
// spSiteLimit controls how many sites are fetched from the API.
func discoverSharePointDrives(
	ctx context.Context, client *graph.Client, cfg *config.Config, email string, spSiteLimit int, logger *slog.Logger,
) []driveListEntry {
	sites, err := client.SearchSites(ctx, "*", spSiteLimit)
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
				parsedCID:   cid,
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
			parsedCID:   f.cid,
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

	var wg sync.WaitGroup
	sema := make(chan struct{}, enrichConcurrency)

launchEnrichment:
	for i := range candidates {
		item := candidates[i].item

		select {
		case sema <- struct{}{}:
		case <-ctx.Done():
			break launchEnrichment
		}

		wg.Add(1)
		go func(item *graph.Item) {
			defer wg.Done()
			defer func() {
				<-sema
			}()

			enrichSharedItem(ctx, client, item, logger)
		}(item)
	}

	wg.Wait()

	// Search is still the primary discovery surface, but some valid search hits
	// only become nameable after a single SharedWithMe identity backfill.
	// Use the deprecated endpoint narrowly as a repair source for missing owner
	// identity instead of dropping usable shared folders from drive list/add.
	backfillSharedIdentityFromSharedWithMe(ctx, client, items, email, logger)

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

// backfillSharedIdentityFromSharedWithMe repairs missing shared-owner identity
// only when the search-first path plus direct item enrichment still left some
// usable remote references without owner email. This preserves the search-first
// contract while avoiding silent shared-folder loss when Search/GetItem omit
// the identity details needed for display naming or JSON output.
func backfillSharedIdentityFromSharedWithMe(
	ctx context.Context, client *graph.Client, items []graph.Item, email string, logger *slog.Logger,
) {
	if !needsSharedIdentityBackfill(items) {
		return
	}

	fallbackItems, err := client.SharedWithMe(ctx)
	if err != nil {
		logger.Debug("SharedWithMe identity backfill failed",
			"email", email,
			"error", err,
		)

		return
	}

	identityByRemoteKey := make(map[string]graph.Item, len(fallbackItems))

	for i := range fallbackItems {
		key, ok := sharedIdentityKey(&fallbackItems[i])
		if !ok {
			continue
		}

		identityByRemoteKey[key] = fallbackItems[i]
	}

	for i := range items {
		key, ok := sharedIdentityKey(&items[i])
		if !ok || items[i].SharedOwnerEmail != "" {
			continue
		}

		fallbackItem, ok := identityByRemoteKey[key]
		if !ok {
			continue
		}

		if items[i].SharedOwnerName == "" && fallbackItem.SharedOwnerName != "" {
			items[i].SharedOwnerName = fallbackItem.SharedOwnerName
		}

		if items[i].SharedOwnerEmail == "" && fallbackItem.SharedOwnerEmail != "" {
			items[i].SharedOwnerEmail = fallbackItem.SharedOwnerEmail
		}
	}
}

func needsSharedIdentityBackfill(items []graph.Item) bool {
	for i := range items {
		if items[i].SharedOwnerEmail != "" {
			continue
		}

		if _, ok := sharedIdentityKey(&items[i]); ok {
			return true
		}
	}

	return false
}

func sharedIdentityKey(item *graph.Item) (string, bool) {
	if item.RemoteDriveID == "" || item.RemoteItemID == "" {
		return "", false
	}

	return item.RemoteDriveID + "\x00" + item.RemoteItemID, true
}

// deriveSharedDisplayName builds a human-friendly name for a shared folder.
// Use escalating owner identity detail so shared-folder names stay readable
// without sacrificing uniqueness:
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
	Configured            []driveListEntry         `json:"configured"`
	Available             []driveListEntry         `json:"available"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth,omitempty"`
}

func printDriveListJSON(
	w io.Writer,
	configured, available []driveListEntry,
	authRequiredOpt ...[]accountAuthRequirement,
) error {
	var authRequired []accountAuthRequirement
	if len(authRequiredOpt) > 0 {
		authRequired = authRequiredOpt[0]
	}

	// Initialize nil slices to empty so JSON renders [] not null.
	if configured == nil {
		configured = []driveListEntry{}
	}

	if available == nil {
		available = []driveListEntry{}
	}

	out := driveListJSONOutput{
		Configured:            configured,
		Available:             available,
		AccountsRequiringAuth: authRequired,
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

func printDriveListText(
	w io.Writer,
	configured, available []driveListEntry,
	authRequiredOpt ...[]accountAuthRequirement,
) error {
	authRequired := optionalAuthRequirements(authRequiredOpt)

	if len(configured) == 0 && len(available) == 0 && len(authRequired) == 0 {
		return writeln(w, "No drives configured. Run 'onedrive-go login' to get started.")
	}

	return printDriveListSections(w, configured, available, authRequired)
}

func printConfiguredDrives(w io.Writer, entries []driveListEntry) error {
	if err := writeln(w, "Configured drives:"); err != nil {
		return err
	}

	maxName, maxDir, maxAuth := 0, 0, 0
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

		authLabel := driveAuthLabel(&entries[i])
		if len(authLabel) > maxAuth {
			maxAuth = len(authLabel)
		}
	}

	maxName = max(maxName, minColumnWidth)
	maxDir = max(maxDir, minColumnWidth)
	maxAuth = max(maxAuth, len("AUTH"))

	fmtStr := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%s\n", maxName, maxDir, maxAuth)
	if err := writef(w, fmtStr, "DRIVE", "SYNC DIR", "AUTH", "STATE"); err != nil {
		return err
	}

	for i := range entries {
		syncDir := entries[i].SyncDir
		if syncDir == "" {
			syncDir = syncDirNotSet
		}

		if err := writef(w, fmtStr, driveLabel(&entries[i]), syncDir, driveAuthLabel(&entries[i]), entries[i].State); err != nil {
			return err
		}
	}

	return nil
}

func driveAuthLabel(entry *driveListEntry) string {
	if entry == nil {
		return authStateReady
	}

	if entry.AuthState == authStateAuthenticationNeeded {
		return "required"
	}

	return authStateReady
}

func optionalAuthRequirements(authRequiredOpt [][]accountAuthRequirement) []accountAuthRequirement {
	if len(authRequiredOpt) == 0 {
		return nil
	}

	return authRequiredOpt[0]
}

func printDriveListSections(
	w io.Writer,
	configured, available []driveListEntry,
	authRequired []accountAuthRequirement,
) error {
	if err := printConfiguredSection(w, configured); err != nil {
		return err
	}

	if err := printAvailableSection(w, configured, available); err != nil {
		return err
	}

	return printAuthRequiredSection(w, len(configured) > 0 || len(available) > 0, authRequired)
}

func printConfiguredSection(w io.Writer, configured []driveListEntry) error {
	if len(configured) == 0 {
		return nil
	}

	return printConfiguredDrives(w, configured)
}

func printAvailableSection(
	w io.Writer,
	configured, available []driveListEntry,
) error {
	if len(available) == 0 {
		return nil
	}

	if len(configured) > 0 {
		if err := writeln(w); err != nil {
			return err
		}
	}

	return printAvailableDrives(w, available)
}

func printAuthRequiredSection(
	w io.Writer,
	hasPriorSection bool,
	authRequired []accountAuthRequirement,
) error {
	if len(authRequired) == 0 {
		return nil
	}

	if hasPriorSection {
		if err := writeln(w); err != nil {
			return err
		}
	}

	return printAccountAuthRequirementsText(w, "Authentication required:", authRequired)
}

// annotateStateDB sets HasStateDB on available drive entries that have a state
// database on disk from a previous configuration. This is a post-processing
// step separate from network discovery — keeping filesystem I/O out of the
// discovery functions preserves their single responsibility. Uses the
// pre-parsed parsedCID field to avoid string→CanonicalID re-parsing.
func annotateStateDB(entries []driveListEntry) {
	for i := range entries {
		if entries[i].parsedCID.IsZero() {
			continue
		}

		path := config.DriveStatePath(entries[i].parsedCID)
		if path == "" {
			continue
		}

		if managedPathExists(path) {
			entries[i].HasStateDB = true
		}
	}
}

func printAvailableDrives(w io.Writer, entries []driveListEntry) error {
	if err := writeln(w, "Available drives (not configured):"); err != nil {
		return err
	}

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

		if err := writef(w, "  %s%s%s\n", driveLabel(&entries[i]), label, stateDBMarker); err != nil {
			return err
		}
	}

	return writeln(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a drive.")
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
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveAdd,
		Args:        cobra.MaximumNArgs(1),
	}
}

func runDriveAdd(cmd *cobra.Command, args []string) error {
	return newDriveService(mustCLIContext(cmd.Context())).runAdd(cmd.Context(), args)
}

// addNewDrive adds a new drive to the config with a computed default sync_dir.
// If the drive already exists, reports it as already configured. Token
// existence is verified as a precondition before writing config.
func addNewDrive(w io.Writer, cfgPath string, cid driveid.CanonicalID, logger *slog.Logger) error {
	// Verify a token exists for this drive's account.
	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if !managedPathExists(tokenPath) {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	syncDir, added, err := config.EnsureDriveInConfig(cfgPath, cid, logger)
	if err != nil {
		return fmt.Errorf("adding drive to config: %w", err)
	}

	if !added {
		return writef(w, "Drive %s is already configured.\n", cid.String())
	}

	driveDisplayName := config.DefaultDisplayName(cid)
	return writef(w, "Added drive %s (%s) -> %s\n", driveDisplayName, cid.String(), syncDir)
}

// listAvailableDrives lists drives that can be added. Shows usage guidance
// when no canonical ID argument is provided.
func listAvailableDrives(w io.Writer) error {
	if err := writeln(w, "Run 'onedrive-go drive add <canonical-id>' to add a drive."); err != nil {
		return err
	}

	return writeln(w, "Run 'onedrive-go drive list' to see available drives.")
}

// addSharedDrive adds a shared drive to config by canonical ID.
// If preResolvedName is non-empty, it is used directly (avoids re-querying
// the API when the caller already has the display name from search results).
// If empty, the display name is resolved via the API.
func addSharedDrive(
	ctx context.Context, cfgPath string, w io.Writer, cid driveid.CanonicalID,
	preResolvedName string, logger *slog.Logger, httpProvider *graphhttp.Provider,
) error {
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if _, exists := cfg.Drives[cid]; exists {
		return writef(w, "Drive %s is already configured.\n", cid.String())
	}

	// Shared drives don't have their own token — find the parent account.
	// DriveTokenPath(sharedCID) reads drive metadata which doesn't exist yet
	// for new drives. Probe the filesystem for existing personal/business tokens.
	parentCID := findTokenFallback(cid.Email(), logger)

	tokenPath := config.DriveTokenPath(parentCID)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if !managedPathExists(tokenPath) {
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

		displayName, resolveErr = resolveSharedDisplayName(ctx, cid, tokenPath, cfg, logger, httpProvider)
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

	return writef(w, "Added drive %s (%s) -> %s\n", displayName, cid.String(), syncDir)
}

// resolveSharedDisplayName finds the matching shared item and derives a
// collision-free display name. Tries search (non-deprecated) then SharedWithMe.
func resolveSharedDisplayName(
	ctx context.Context, cid driveid.CanonicalID,
	tokenPath string, cfg *config.Config, logger *slog.Logger, httpProvider *graphhttp.Provider,
) (string, error) {
	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		return "", fmt.Errorf("load token source: %w", err)
	}

	client, err := newGraphClientWithHTTP("", httpProvider.BootstrapMeta(), ts, logger)
	if err != nil {
		return "", err
	}
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
			if item.SharedOwnerEmail == "" {
				backfillSharedIdentityFromSharedWithMe(ctx, client, items, cid.Email(), logger)
			}

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
	ctx context.Context, selector, cfgPath string, w io.Writer, logger *slog.Logger, httpProvider *graphhttp.Provider,
) error {
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	matches := searchSharedDrives(ctx, cfg, selector, logger, httpProvider)

	switch len(matches) {
	case 0:
		return fmt.Errorf(
			"no shared folders matching %q found — run 'onedrive-go drive list' to see available drives",
			selector)

	case 1:
		return addSharedDrive(ctx, cfgPath, w, matches[0].cid, matches[0].displayName, logger, httpProvider)

	default:
		if err := writef(w, "Multiple shared folders match %q — be more specific:\n\n", selector); err != nil {
			return err
		}

		for i := range matches {
			ownerInfo := ""
			if matches[i].ownerEmail != "" {
				ownerInfo = fmt.Sprintf(" (shared by %s)", matches[i].ownerEmail)
			}

			viaInfo := ""
			if matches[i].tokenEmail != "" {
				viaInfo = fmt.Sprintf(" [via %s]", matches[i].tokenEmail)
			}

			if err := writef(
				w,
				"  %d. %s%s%s\n     %s\n",
				i+1,
				matches[i].displayName,
				ownerInfo,
				viaInfo,
				matches[i].cid.String(),
			); err != nil {
				return err
			}
		}

		return writeln(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a specific drive.")
	}
}

// searchSharedDrives queries all tokens for shared folders matching selector.
// Uses search-based discovery (non-deprecated) with SharedWithMe fallback.
func searchSharedDrives(
	ctx context.Context, cfg *config.Config, selector string, logger *slog.Logger, httpProvider *graphhttp.Provider,
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

		client, clientErr := newGraphClientWithHTTP(
			"",
			httpProvider.BootstrapMeta(),
			ts,
			logger,
		)
		if clientErr != nil {
			continue
		}
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
	if err == nil && hasUsableSharedItemIdentity(items) {
		return items
	}

	if err != nil {
		logger.Debug("search-based discovery failed, trying SharedWithMe",
			"email", email, "error", err)
	} else {
		logger.Debug("search-based discovery returned no usable shared items, trying SharedWithMe",
			"email", email,
			"search_count", len(items),
		)
	}

	items, err = client.SharedWithMe(ctx)
	if err != nil {
		logger.Debug("SharedWithMe fallback also failed",
			"email", email, "error", err)

		return nil
	}

	return items
}

func hasUsableSharedItemIdentity(items []graph.Item) bool {
	for i := range items {
		if items[i].RemoteDriveID != "" && items[i].RemoteItemID != "" {
			return true
		}
	}

	return false
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
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveRemove,
	}

	cmd.Flags().Bool("purge", false, "also delete the state database")

	return cmd
}

func runDriveRemove(cmd *cobra.Command, _ []string) error {
	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	return newDriveService(mustCLIContext(cmd.Context())).runRemove(purge)
}

// removeDrive deletes the config section for the drive, preserving token,
// state database, and sync directory.
func removeDrive(w io.Writer, cfgPath string, driveID driveid.CanonicalID, syncDir string, logger *slog.Logger) error {
	if err := config.DeleteDriveSection(cfgPath, driveID); err != nil {
		return fmt.Errorf("removing drive: %w", err)
	}

	logger.Info("removed drive config section", "drive", driveID.String())

	idStr := driveID.String()
	if err := writef(w, "Removed drive %s from config.\n", idStr); err != nil {
		return err
	}
	if err := writef(w, "Token and state database kept for %s.\n", idStr); err != nil {
		return err
	}
	if err := writef(w, "Sync directory untouched: %s\n", syncDir); err != nil {
		return err
	}

	return writeln(w, "Run 'onedrive-go drive add "+idStr+"' to re-add.")
}

// purgeDrive deletes the config section and state database for a drive.
// The token is NOT deleted here — it may be shared with other drives (SharePoint).
func purgeDrive(w io.Writer, cfgPath string, driveID driveid.CanonicalID, logger *slog.Logger) error {
	if err := purgeSingleDrive(cfgPath, driveID, logger); err != nil {
		return err
	}

	if err := writef(w, "Purged config and state for %s.\n", driveID.String()); err != nil {
		return err
	}

	return writeln(w, "Sync directory untouched — delete manually if desired.")
}

// purgeOrphanedDriveState removes state DB and drive metadata files for a
// drive that is no longer in config. Unlike purgeSingleDrive (which also
// removes the drive's config section), this only deletes drive-owned data files
// left behind from a previous `drive remove` without --purge.
func purgeOrphanedDriveState(w io.Writer, cid driveid.CanonicalID, logger *slog.Logger) error {
	removed, err := removeDriveDataFiles(cid, logger)
	if err != nil {
		return err
	}

	if removed == 0 {
		return writef(w, "No orphaned state found for %s.\n", cid.String())
	}

	return writef(w, "Purged %d orphaned data file(s) for %s.\n", removed, cid.String())
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
		// skipConfig: drive search loads config leniently itself —
		// Phase 2 strict loading must not run.
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveSearch,
		Args:        cobra.ExactArgs(1),
	}
}

// driveSearchResult represents a search result for JSON output.
type driveSearchResult struct {
	CanonicalID string `json:"canonical_id"`
	SiteName    string `json:"site_name"`
	LibraryName string `json:"library_name"`
	WebURL      string `json:"web_url,omitempty"`
}

type driveSearchJSONOutput struct {
	Results               []driveSearchResult      `json:"results"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth,omitempty"`
}

// sharePointSearchLimit caps the number of SharePoint sites returned per search query.
const sharePointSearchLimit = 50

func runDriveSearch(cmd *cobra.Command, args []string) error {
	return newDriveService(mustCLIContext(cmd.Context())).runSearch(cmd.Context(), args[0])
}

// searchAccountSharePoint searches SharePoint sites for a single business account.
func searchAccountSharePoint(
	ctx context.Context,
	tokenCID driveid.CanonicalID,
	query string,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	httpProvider *graphhttp.Provider,
) ([]driveSearchResult, []accountAuthRequirement) {
	tokenPath := config.DriveTokenPath(tokenCID)
	if tokenPath == "" {
		return nil, nil
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		logger.Debug("skipping token for search", "token", tokenCID.String(), "error", err)

		return nil, []accountAuthRequirement{tokenDiscoveryAuthRequirement(tokenCID, err, logger)}
	}

	client, err := newGraphClientWithHTTP(
		baseURL,
		httpProvider.BootstrapMeta(),
		ts,
		logger,
	)
	if err != nil {
		logger.Debug("skipping token for search", "token", tokenCID.String(), "error", err)

		return nil, nil
	}
	attachAccountAuthProof(client, recorder, tokenCID.Email(), "drive-search")

	sites, err := client.SearchSites(ctx, query, sharePointSearchLimit)
	if err != nil {
		logger.Warn("SharePoint search failed", "account", tokenCID.Email(), "error", err)

		if errors.Is(err, graph.ErrUnauthorized) {
			return nil, []accountAuthRequirement{tokenAuthRequirement(tokenCID, authReasonSyncAuthRejected, logger)}
		}

		return nil, nil
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

	return results, nil
}

func printDriveSearchJSON(
	w io.Writer,
	results []driveSearchResult,
	authRequiredOpt ...[]accountAuthRequirement,
) error {
	var authRequired []accountAuthRequirement
	if len(authRequiredOpt) > 0 {
		authRequired = authRequiredOpt[0]
	}

	if results == nil {
		results = []driveSearchResult{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(driveSearchJSONOutput{
		Results:               results,
		AccountsRequiringAuth: authRequired,
	}); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printDriveSearchText(
	w io.Writer,
	results []driveSearchResult,
	query string,
	authRequiredOpt ...[]accountAuthRequirement,
) error {
	authRequired := optionalAuthRequirements(authRequiredOpt)

	if len(results) == 0 {
		if len(authRequired) == 0 {
			return writef(w, "No SharePoint sites found matching %q.\n", query)
		}

		if err := printDriveSearchAuthSection(w, authRequired, false); err != nil {
			return err
		}
		return writef(w, "No SharePoint sites found matching %q in searchable business accounts.\n", query)
	}

	if err := printDriveSearchAuthSection(w, authRequired, len(results) > 0); err != nil {
		return err
	}

	return printDriveSearchResults(w, sortedDriveSearchResults(results), query)
}

func printDriveSearchAuthSection(
	w io.Writer,
	authRequired []accountAuthRequirement,
	hasResults bool,
) error {
	if len(authRequired) == 0 {
		return nil
	}

	if err := printAccountAuthRequirementsText(w, "Authentication required:", authRequired); err != nil {
		return err
	}

	if !hasResults {
		return nil
	}

	return writeln(w)
}

func sortedDriveSearchResults(results []driveSearchResult) []driveSearchResult {
	sorted := slices.Clone(results)
	slices.SortFunc(sorted, func(a, b driveSearchResult) int {
		if c := cmp.Compare(a.SiteName, b.SiteName); c != 0 {
			return c
		}

		return cmp.Compare(a.LibraryName, b.LibraryName)
	})

	return sorted
}

func printDriveSearchResults(
	w io.Writer,
	results []driveSearchResult,
	query string,
) error {
	if err := writef(w, "SharePoint sites matching %q:\n", query); err != nil {
		return err
	}

	currentSite := ""

	for i := range results {
		if err := printDriveSearchSiteEntry(w, &results[i], &currentSite); err != nil {
			return err
		}
	}

	return writef(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a drive.\n")
}

func printDriveSearchSiteEntry(
	w io.Writer,
	result *driveSearchResult,
	currentSite *string,
) error {
	if result == nil || currentSite == nil {
		return nil
	}

	if result.SiteName != *currentSite {
		if *currentSite != "" {
			if err := writeln(w); err != nil {
				return err
			}
		}

		*currentSite = result.SiteName
		label := result.SiteName
		if result.WebURL != "" {
			label = fmt.Sprintf("%s (%s)", result.SiteName, result.WebURL)
		}

		if err := writef(w, "\n  %s\n", label); err != nil {
			return err
		}
	}

	return writef(w, "    %s\n", result.CanonicalID)
}
