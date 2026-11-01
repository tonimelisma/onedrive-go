package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// sharePointSiteLimit caps the number of SharePoint sites fetched during
// drive list. Use "drive search" for targeted queries, or --all to lift this cap.
const sharePointSiteLimit = 10

// sharePointSiteUnlimited is used when --all is passed to drive list,
// removing the SharePoint site discovery cap.
const sharePointSiteUnlimited = 999

const fallbackSharedItemName = "shared item"

const driveStateAvailable = "available"

// driveListEntry represents one drive in the list output.
type driveListEntry struct {
	CanonicalID         string                    `json:"canonical_id"`
	DisplayName         string                    `json:"display_name,omitempty"`
	SyncDir             string                    `json:"sync_dir,omitempty"`
	State               string                    `json:"state"`
	AuthState           string                    `json:"auth_state,omitempty"`
	AuthReason          string                    `json:"auth_reason,omitempty"`
	Source              string                    `json:"source"` // "configured" or "available"
	SiteName            string                    `json:"site_name,omitempty"`
	LibraryName         string                    `json:"library_name,omitempty"`
	OwnerName           string                    `json:"owner_name,omitempty"`
	OwnerEmail          string                    `json:"owner_email,omitempty"`
	OwnerIdentityStatus sharedOwnerIdentityStatus `json:"owner_identity_status,omitempty"`
	HasStateDB          bool                      `json:"has_state_db,omitempty"`

	// parsedCID holds the pre-parsed canonical ID, avoiding string→CanonicalID
	// re-parsing in annotateStateDB. Not serialized to JSON — internal only.
	parsedCID driveid.CanonicalID
}

// buildConfiguredDriveEntries creates list entries from the config. Each
// entry's state comes from configuredDriveState, the derivation status uses,
// so drive list and status never disagree about a drive.
func buildConfiguredDriveEntries(
	ctx context.Context,
	cfg *config.Config,
	syncQ syncStateQuerier,
	logger *slog.Logger,
) []driveListEntry {
	if len(cfg.Drives) == 0 {
		return nil
	}

	entries := make([]driveListEntry, 0, len(cfg.Drives))

	for id := range cfg.Drives {
		d := cfg.Drives[id]
		state, _ := configuredDriveState(ctx, id, &d, syncQ)

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
		result = append(result, authRequirement(
			email,
			readAccountDisplayName(email, driveIDs, logger),
			accountDriveType(driveIDs),
			stateDBCountForEmail(email, logger),
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
		entries[i].AuthReason = string(health.Reason)
	}
}

// discoverAvailableDrives queries the network for all drives accessible via
// existing tokens. Filters out drives already in config. spSiteLimit controls
// how many SharePoint sites are fetched (sharePointSiteLimit for default,
// sharePointSiteUnlimited when --all is passed).
func discoverAvailableDrives(
	ctx context.Context,
	cfg *config.Config,
	accounts []accountView,
	spSiteLimit int,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	runtime *driveops.SessionRuntime,
) ([]driveListEntry, []accountAuthRequirement, []accountDegradedNotice) {
	tokens := accountViewTokenIDs(accounts)
	if len(tokens) == 0 {
		return nil, nil, nil
	}

	var (
		mu           sync.Mutex // guards entries/authRequired/degraded while token workers append results
		entries      []driveListEntry
		authRequired []accountAuthRequirement
		degraded     []accountDegradedNotice
	)

	var wg sync.WaitGroup

	for _, tokenCID := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()

			tokenEntries, tokenAuthRequired, tokenDegraded := discoverDrivesForToken(
				ctx,
				tokenCID,
				cfg,
				accounts,
				spSiteLimit,
				logger,
				recorder,
				baseURL,
				runtime,
			)
			mu.Lock()
			entries = append(entries, tokenEntries...)
			authRequired = append(authRequired, tokenAuthRequired...)
			degraded = append(degraded, tokenDegraded...)
			mu.Unlock()
		}()
	}

	wg.Wait()

	slices.SortFunc(entries, func(a, b driveListEntry) int {
		return cmp.Compare(a.CanonicalID, b.CanonicalID)
	})

	return entries, mergeAuthRequirements(authRequired), mergeDegradedNotices(degraded)
}

// discoverDrivesForToken discovers all available drives for a single token.
func discoverDrivesForToken(
	ctx context.Context,
	tokenCID driveid.CanonicalID,
	cfg *config.Config,
	accounts []accountView,
	spSiteLimit int,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	runtime *driveops.SessionRuntime,
) ([]driveListEntry, []accountAuthRequirement, []accountDegradedNotice) {
	tokenPath := config.DriveTokenPath(tokenCID)
	if tokenPath == "" {
		return nil, nil, nil
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		logger.Debug("skipping token for drive discovery", "token", tokenCID.String(), "error", err)

		return nil, []accountAuthRequirement{tokenDiscoveryAuthRequirement(tokenCID, err, logger)}, nil
	}

	client, clientErr := newGraphClientWithHTTP(
		baseURL,
		runtime.BootstrapMeta(),
		ts,
		logger,
	)
	if clientErr != nil {
		logger.Debug("skipping token for drive discovery", "token", tokenCID.String(), "error", clientErr)

		return nil, nil, nil
	}
	attachAccountAuthProof(client, recorder, tokenCID.Email(), "drive-list")

	entries, authRequired, degraded := discoverAccessibleDrives(
		ctx,
		client,
		cfg,
		accounts,
		tokenCID,
		logger,
	)
	if len(authRequired) > 0 {
		return nil, authRequired, nil
	}

	// For business accounts, discover SharePoint sites.
	if tokenCID.DriveType() == driveid.DriveTypeBusiness {
		spEntries := discoverSharePointDrives(ctx, client, cfg, tokenCID.Email(), spSiteLimit, logger)
		entries = append(entries, spEntries...)
	}

	return entries, nil, degraded
}

type accessibleDriveCatalogClient interface {
	Drives(context.Context) ([]graph.Drive, error)
	PrimaryDrive(context.Context) (*graph.Drive, error)
}

func discoverAccessibleDrives(
	ctx context.Context,
	client accessibleDriveCatalogClient,
	cfg *config.Config,
	accounts []accountView,
	tokenCID driveid.CanonicalID,
	logger *slog.Logger,
) ([]driveListEntry, []accountAuthRequirement, []accountDegradedNotice) {
	drives, err := client.Drives(ctx)
	if err != nil {
		logger.Debug("failed to list drives for token", "token", tokenCID.String(), "error", err)

		if errors.Is(err, graph.ErrUnauthorized) {
			return nil, []accountAuthRequirement{tokenAuthRequirement(tokenCID, authReasonSyncAuthRejected, logger)}, nil
		}
		logger.Warn("degrading drive-list live discovery after /me/drives failure",
			degradedDiscoveryLogAttrs(tokenCID.Email(), graphMeDrivesEndpoint, err)...,
		)

		notice := tokenDriveCatalogDegradedNotice(accounts, tokenCID, logger)
		entries := appendPrimaryDriveFallbackEntry(ctx, nil, client, cfg, tokenCID.Email(), logger)
		return entries, nil, []accountDegradedNotice{notice}
	}

	var entries []driveListEntry
	for _, d := range drives {
		entries = appendAvailableCatalogDrive(entries, cfg, tokenCID.Email(), d)
	}

	return entries, nil, nil
}

func tokenDriveCatalogDegradedNotice(
	accounts []accountView,
	tokenCID driveid.CanonicalID,
	logger *slog.Logger,
) accountDegradedNotice {
	entry, found := accountViewByEmail(accounts, tokenCID.Email())
	if found {
		return driveCatalogDegradedNotice(entry.Email, entry.DisplayName, entry.DriveType)
	}

	return driveCatalogDegradedNotice(
		tokenCID.Email(),
		readAccountDisplayName(tokenCID.Email(), []driveid.CanonicalID{tokenCID}, logger),
		tokenCID.DriveType(),
	)
}

func appendAvailableCatalogDrive(
	entries []driveListEntry,
	cfg *config.Config,
	email string,
	drive graph.Drive,
) []driveListEntry {
	cid, cidErr := driveid.Construct(drive.DriveType, email)
	if cidErr != nil {
		return entries
	}

	if _, exists := cfg.Drives[cid]; exists {
		return entries
	}

	return append(entries, driveListEntry{
		CanonicalID: cid.String(),
		State:       driveStateAvailable,
		Source:      "available",
		parsedCID:   cid,
	})
}

func appendPrimaryDriveFallbackEntry(
	ctx context.Context,
	entries []driveListEntry,
	client accessibleDriveCatalogClient,
	cfg *config.Config,
	email string,
	logger *slog.Logger,
) []driveListEntry {
	primary, err := client.PrimaryDrive(ctx)
	if err != nil {
		logger.Warn("primary drive fallback unavailable during drive-list degradation",
			"account", email,
			"error", err,
		)
		return entries
	}

	return appendAvailableCatalogDrive(entries, cfg, email, *primary)
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
	reason authstate.Reason,
	logger *slog.Logger,
) accountAuthRequirement {
	return authRequirement(
		tokenCID.Email(),
		readAccountDisplayName(tokenCID.Email(), []driveid.CanonicalID{tokenCID}, logger),
		tokenCID.DriveType(),
		stateDBCountForEmail(tokenCID.Email(), logger),
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
	ctx context.Context,
	client *graph.Client,
	cfg *config.Config,
	email string,
	spSiteLimit int,
	logger *slog.Logger,
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
				State:       driveStateAvailable,
				Source:      "available",
				SiteName:    site.DisplayName,
				LibraryName: d.Name,
				parsedCID:   cid,
			})
		}
	}

	return entries
}

// sharedFoldersToEntries converts validated shared folders to drive list entries.
func sharedFoldersToEntries(folders []sharedFolderInfo) []driveListEntry {
	entries := make([]driveListEntry, 0, len(folders))

	for i := range folders {
		f := &folders[i]
		entries = append(entries, driveListEntry{
			CanonicalID:         f.cid.String(),
			DisplayName:         f.displayName,
			State:               driveStateAvailable,
			Source:              "available",
			OwnerName:           f.target.SharedByName,
			OwnerEmail:          f.target.SharedByEmail,
			OwnerIdentityStatus: f.target.OwnerIdentityStatus,
			parsedCID:           f.cid,
		})
	}

	return entries
}

// configuredDriveListState is what drive list can say without contacting
// OneDrive. A drive with no state database has never completed a sync here,
// so calling it up to date would be false.
// It returns an error when the state database cannot be checked, which is
// neither "not synced" nor "ready".
func configuredDriveListState(cid driveid.CanonicalID, paused bool) (string, error) {
	if paused {
		return driveStatePaused, nil
	}

	path := config.DriveStatePath(cid)
	if path == "" {
		return driveStateNotSynced, nil
	}
	exists, err := managedPathExists(path)
	if err != nil {
		return "", err
	}
	if !exists {
		return driveStateNotSynced, nil
	}

	return driveStateReady, nil
}

// annotateStateDB sets HasStateDB on available drive entries that have a state
// database on disk from a previous configuration. This is a post-processing
// step separate from network discovery — keeping filesystem I/O out of the
// discovery functions preserves their single responsibility. Uses the
// pre-parsed parsedCID field to avoid string→CanonicalID re-parsing. A state
// database that cannot be checked is an error: "no state database" would
// present a drive with retained state as a clean add.
func annotateStateDB(entries []driveListEntry) error {
	for i := range entries {
		if entries[i].parsedCID.IsZero() {
			continue
		}

		path := config.DriveStatePath(entries[i].parsedCID)
		if path == "" {
			continue
		}

		exists, err := managedPathExists(path)
		if err != nil {
			return fmt.Errorf("checking for the state database of %s: %w", entries[i].CanonicalID, err)
		}
		if exists {
			entries[i].HasStateDB = true
		}
	}

	return nil
}

// stateDBCountForEmail is the advisory "N state databases" count in a sign-in
// notice. The discovery itself warns when the data directory cannot be read,
// and the account snapshot built before this notice already failed on that
// same directory, so a failure here is logged at Debug and counted as none
// rather than failing the notice.
func stateDBCountForEmail(email string, logger *slog.Logger) int {
	paths, err := config.DiscoverStateDBsForEmail(email, logger)
	if err != nil {
		logger.Debug("state database count unavailable for sign-in notice", "account", email, "error", err)
		return 0
	}

	return len(paths)
}

// resolveExactDriveSelector resolves --drive for commands that delete state
// (drive remove, drive reset-sync-state). They accept an exact canonical ID,
// which may name a drive no longer in config.toml so its leftovers can be
// purged, or a configured drive's display name. Unlike sync and pause they
// never take a partial match, so a substring can't select the wrong drive.
func resolveExactDriveSelector(cfg *config.Config, selector string) (driveid.CanonicalID, bool, error) {
	if cid, err := driveid.NewCanonicalID(selector); err == nil {
		_, inConfig := cfg.Drives[cid]
		return cid, inConfig, nil
	}

	for cid := range cfg.Drives {
		if strings.EqualFold(cfg.Drives[cid].DisplayName, selector) {
			return cid, true, nil
		}
	}

	configured := make([]string, 0, len(cfg.Drives))
	for cid := range cfg.Drives {
		configured = append(configured, cid.String())
	}
	slices.Sort(configured)

	if len(configured) == 0 {
		return driveid.CanonicalID{}, false, fmt.Errorf("no drive matching %q, and no drives are configured", selector)
	}

	return driveid.CanonicalID{}, false, fmt.Errorf("no drive matching %q; use its full ID or display name. Configured drives: %s",
		selector, strings.Join(configured, ", "))
}
