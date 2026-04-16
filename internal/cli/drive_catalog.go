package cli

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

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
		result = append(result, authRequirement(
			email,
			readAccountDisplayName(email, driveIDs, logger),
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
	catalog []accountCatalogEntry,
	spSiteLimit int,
	logger *slog.Logger,
	recorder *authProofRecorder,
	baseURL string,
	runtime *driveops.SessionRuntime,
) ([]driveListEntry, []accountAuthRequirement, []accountDegradedNotice) {
	tokens := catalogTokenIDs(catalog)
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
				catalog,
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
	catalog []accountCatalogEntry,
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
		catalog,
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
	catalog []accountCatalogEntry,
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

		notice := tokenDriveCatalogDegradedNotice(catalog, tokenCID, logger)
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
	catalog []accountCatalogEntry,
	tokenCID driveid.CanonicalID,
	logger *slog.Logger,
) accountDegradedNotice {
	entry, found := catalogEntryByEmail(catalog, tokenCID.Email())
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
	reason string,
	logger *slog.Logger,
) accountAuthRequirement {
	return authRequirement(
		tokenCID.Email(),
		readAccountDisplayName(tokenCID.Email(), []driveid.CanonicalID{tokenCID}, logger),
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
