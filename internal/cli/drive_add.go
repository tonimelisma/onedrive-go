package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

func runDriveAddWithContext(ctx context.Context, cc *CLIContext, args []string) error {
	logger := cc.Logger

	if cc.SharedTarget != nil {
		item, _, err := cc.resolveSharedItem(ctx)
		if err != nil {
			return err
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		cid, err := driveid.NewCanonicalID(cc.SharedTarget.Selector())
		if err != nil {
			return fmt.Errorf("parse shared drive identity: %w", err)
		}

		return finishDriveAdd(ctx, cc, addSharedDrive(ctx, cc.CfgPath, cc.Output(), cid, "", logger, cc.runtime()))
	}

	selector := ""
	if len(args) > 0 {
		selector = args[0]
	}

	if selector == "" {
		var driveErr error
		selector, driveErr = cc.Flags.SingleDrive()
		if driveErr != nil {
			return driveErr
		}
	}

	if selector == "" {
		return listAvailableDrives(cc.Output())
	}

	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		if strings.Contains(selector, ":") {
			return fmt.Errorf("invalid canonical ID %q: %w\n"+
				"Run 'onedrive-go drive list' to see valid canonical IDs", selector, err)
		}

		return finishDriveAdd(ctx, cc, addSharedDriveByName(ctx, cc, selector))
	}

	if cid.IsShared() {
		clients, err := cc.sharedTargetClients(ctx, sharedref.Ref{
			AccountEmail:  cid.Email(),
			RemoteDriveID: cid.SourceDriveID(),
			RemoteItemID:  cid.SourceItemID(),
		})
		if err != nil {
			return err
		}

		item, err := clients.Meta.GetItem(ctx, driveid.New(cid.SourceDriveID()), cid.SourceItemID())
		if err != nil {
			return fmt.Errorf("loading shared item: %w", err)
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		return finishDriveAdd(ctx, cc, addSharedDrive(ctx, cc.CfgPath, cc.Output(), cid, "", logger, cc.runtime()))
	}

	return finishDriveAdd(ctx, cc, addNewDrive(cc.Output(), cc.CfgPath, cid, logger))
}

func finishDriveAdd(ctx context.Context, cc *CLIContext, err error) error {
	if err != nil {
		return err
	}
	notifyDaemonIfRunning(ctx, cc)
	return nil
}

type driveAddDurableOps struct {
	registerDrive       func(dataDir string, cid driveid.CanonicalID, displayName string) error
	registerSharedDrive func(dataDir string, cid driveid.CanonicalID, parentCID driveid.CanonicalID, displayName string) error
}

func defaultDriveAddDurableOps() driveAddDurableOps {
	return driveAddDurableOps{
		registerDrive:       config.RegisterDrive,
		registerSharedDrive: config.RegisterSharedDrive,
	}
}

// addNewDrive adds a new drive to the config with a computed default sync_dir.
// If the drive already exists, reports it as already configured. Token
// existence is verified as a precondition before writing config.
func addNewDrive(w io.Writer, cfgPath string, cid driveid.CanonicalID, logger *slog.Logger) error {
	return addNewDriveWithOps(w, cfgPath, cid, logger, defaultDriveAddDurableOps())
}

func addNewDriveWithOps(
	w io.Writer,
	cfgPath string,
	cid driveid.CanonicalID,
	logger *slog.Logger,
	ops driveAddDurableOps,
) error {
	// Verify a token exists for this drive's account.
	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if !managedPathExists(tokenPath) {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	dataDir := config.DefaultDataDir()
	priorCatalogDrive, hadPriorCatalogDrive, err := loadExistingCatalogDrive(dataDir, cid)
	if err != nil {
		return fmt.Errorf("loading existing catalog drive: %w", err)
	}

	ensureResult, err := config.EnsureDriveInConfigDetailed(cfgPath, cid, logger)
	if err != nil {
		return fmt.Errorf("adding drive to config: %w", err)
	}
	if ensureResult.Added {
		driveDisplayName := config.DefaultDisplayName(cid)
		if err := ops.registerDrive(dataDir, cid, driveDisplayName); err != nil {
			rollbackDriveAddConfig(cfgPath, cid, ensureResult, logger, "drive add rollback failed to restore config")
			return fmt.Errorf("updating catalog: %w", err)
		}
	}

	syncDir := ensureResult.SyncDir
	if err := materializeDriveSyncDir(syncDir); err != nil {
		configRolledBack := rollbackDriveAddConfig(
			cfgPath,
			cid,
			ensureResult,
			logger,
			"drive add rollback failed to restore config",
		)
		if ensureResult.Added && configRolledBack {
			rollbackDriveAddCatalog(dataDir, cid, priorCatalogDrive, hadPriorCatalogDrive, logger,
				"drive add rollback failed to restore catalog")
		}
		return fmt.Errorf("creating sync directory: %w", err)
	}

	if !ensureResult.Added {
		return writef(w, "Drive %s is already configured.\n", cid.String())
	}

	driveDisplayName := config.DefaultDisplayName(cid)
	return writef(w, "Added drive %s (%s) -> %s\n", driveDisplayName, cid.String(), syncDir)
}

func rollbackDriveAddConfig(
	cfgPath string,
	cid driveid.CanonicalID,
	ensureResult config.EnsureDriveInConfigResult,
	logger *slog.Logger,
	reason string,
) bool {
	rolledBack, err := restoreDriveAddConfigMutation(cfgPath, cid, ensureResult)
	if err != nil {
		logger.Warn(reason,
			"drive", cid.String(),
			"error", err,
		)
	}

	return rolledBack
}

func restoreDriveAddConfigMutation(
	cfgPath string,
	cid driveid.CanonicalID,
	ensureResult config.EnsureDriveInConfigResult,
) (bool, error) {
	switch {
	case ensureResult.Added:
		deleted, err := deleteDriveAddSectionIfUnchanged(cfgPath, cid, ensureResult.SyncDir)
		if err != nil {
			return false, fmt.Errorf("delete added drive section: %w", err)
		}
		return deleted, nil
	case ensureResult.BackfilledSyncDir:
		deleted, err := deleteDriveAddBackfilledSyncDirIfUnchanged(
			cfgPath,
			cid,
			ensureResult.SyncDir,
			ensureResult.DriveBeforeSyncDirBackfill,
		)
		if err != nil {
			return false, fmt.Errorf("delete backfilled sync_dir: %w", err)
		}
		return deleted, nil
	}

	return false, nil
}

func deleteDriveAddSectionIfUnchanged(
	cfgPath string,
	cid driveid.CanonicalID,
	syncDir string,
) (bool, error) {
	deleted, err := config.DeleteDriveSectionIfDriveEquals(cfgPath, cid, &config.Drive{SyncDir: syncDir})
	if err != nil {
		return false, fmt.Errorf("delete drive section: %w", err)
	}

	return deleted, nil
}

func deleteDriveAddBackfilledSyncDirIfUnchanged(
	cfgPath string,
	cid driveid.CanonicalID,
	syncDir string,
	driveBeforeBackfill *config.Drive,
) (bool, error) {
	if driveBeforeBackfill == nil {
		return false, nil
	}

	expected := *driveBeforeBackfill
	expected.SyncDir = syncDir
	deleted, err := config.DeleteDriveKeyIfDriveEquals(cfgPath, cid, "sync_dir", &expected)
	if err != nil {
		return false, fmt.Errorf("delete drive sync_dir key: %w", err)
	}

	return deleted, nil
}

func rollbackDriveAddCatalog(
	dataDir string,
	cid driveid.CanonicalID,
	priorCatalogDrive *config.CatalogDrive,
	hadPriorCatalogDrive bool,
	logger *slog.Logger,
	reason string,
) {
	if err := restoreDriveCatalogSnapshot(dataDir, cid, priorCatalogDrive, hadPriorCatalogDrive); err != nil {
		logger.Warn(reason,
			"drive", cid.String(),
			"error", err,
		)
	}
}

func restoreDriveCatalogSnapshot(
	dataDir string,
	cid driveid.CanonicalID,
	priorCatalogDrive *config.CatalogDrive,
	hadPriorCatalogDrive bool,
) error {
	if err := config.UpdateCatalogForDataDir(dataDir, func(catalog *config.Catalog) error {
		if hadPriorCatalogDrive {
			catalog.UpsertDrive(priorCatalogDrive)
			return nil
		}

		catalog.DeleteDrive(cid)
		return nil
	}); err != nil {
		return fmt.Errorf("update catalog: %w", err)
	}

	return nil
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
	ctx context.Context,
	cfgPath string,
	w io.Writer,
	cid driveid.CanonicalID,
	preResolvedName string,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
) error {
	return addSharedDriveWithOps(ctx, cfgPath, w, cid, preResolvedName, logger, runtime, defaultDriveAddDurableOps())
}

func addSharedDriveWithOps(
	ctx context.Context,
	cfgPath string,
	w io.Writer,
	cid driveid.CanonicalID,
	preResolvedName string,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
	ops driveAddDurableOps,
) error {
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if _, exists := cfg.Drives[cid]; exists {
		return writef(w, "Drive %s is already configured.\n", cid.String())
	}

	// Shared drives don't have their own token — find the parent account.
	parentCID, err := config.LoadAccountCanonicalIDByEmail(config.DefaultDataDir(), cid.Email())
	if err != nil {
		return fmt.Errorf("loading catalog owner: %w", err)
	}
	if parentCID.IsZero() {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	tokenPath := config.DriveTokenPath(parentCID)
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", cid.Email())
	}

	if !managedPathExists(tokenPath) {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	displayName := preResolvedName
	if displayName == "" {
		var resolveErr error

		displayName, resolveErr = resolveSharedDisplayName(ctx, cid, tokenPath, cfg, logger, runtime)
		if resolveErr != nil {
			displayName = config.DefaultDisplayName(cid)
			logger.Warn("could not derive shared drive display name, using fallback",
				"error", resolveErr, "fallback", displayName)
		}
	}

	syncDir := config.BaseSyncDir(cid, "", displayName)
	priorCatalogDrive, hadPriorCatalogDrive, err := loadExistingCatalogDrive(config.DefaultDataDir(), cid)
	if err != nil {
		return fmt.Errorf("loading existing catalog drive: %w", err)
	}

	dataDir := config.DefaultDataDir()
	if err := ops.registerSharedDrive(dataDir, cid, parentCID, displayName); err != nil {
		return fmt.Errorf("updating catalog: %w", err)
	}

	if err := config.AppendDriveSection(cfgPath, cid, syncDir); err != nil {
		rollbackSharedDriveAdd(cfgPath, cid, priorCatalogDrive, hadPriorCatalogDrive, nil, logger)
		return fmt.Errorf("writing drive config: %w", err)
	}

	expectedDrive := &config.Drive{SyncDir: syncDir}
	if err := config.SetDriveKey(cfgPath, cid, "display_name", displayName); err != nil {
		rollbackSharedDriveAdd(cfgPath, cid, priorCatalogDrive, hadPriorCatalogDrive, expectedDrive, logger)
		return fmt.Errorf("writing display_name to config: %w", err)
	}
	expectedDrive.DisplayName = displayName
	if err := materializeDriveSyncDir(syncDir); err != nil {
		rollbackSharedDriveAdd(cfgPath, cid, priorCatalogDrive, hadPriorCatalogDrive, expectedDrive, logger)
		return fmt.Errorf("creating sync directory: %w", err)
	}

	return writef(w, "Added drive %s (%s) -> %s\n", displayName, cid.String(), syncDir)
}

func loadExistingCatalogDrive(
	dataDir string,
	cid driveid.CanonicalID,
) (*config.CatalogDrive, bool, error) {
	catalog, err := config.LoadCatalogForDataDir(dataDir)
	if err != nil {
		return nil, false, fmt.Errorf("loading catalog: %w", err)
	}

	drive, found := catalog.DriveByCanonicalID(cid)
	if !found {
		return nil, false, nil
	}

	return &drive, true, nil
}

func rollbackSharedDriveAdd(
	cfgPath string,
	cid driveid.CanonicalID,
	priorCatalogDrive *config.CatalogDrive,
	hadPriorCatalogDrive bool,
	expectedConfigDrive *config.Drive,
	logger *slog.Logger,
) {
	restoreCatalog := expectedConfigDrive == nil
	if expectedConfigDrive != nil {
		deleted, err := config.DeleteDriveSectionIfDriveEquals(cfgPath, cid, expectedConfigDrive)
		if err != nil {
			logger.Warn("shared drive add rollback failed to remove config section",
				"drive", cid.String(),
				"error", err,
			)
			restoreCatalog = false
		} else {
			restoreCatalog = deleted
		}
	}

	if !restoreCatalog {
		return
	}

	if err := restoreDriveCatalogSnapshot(config.DefaultDataDir(), cid, priorCatalogDrive, hadPriorCatalogDrive); err != nil {
		logger.Warn("shared drive add rollback failed to restore catalog state",
			"drive", cid.String(),
			"error", err,
		)
	}
}

// resolveSharedDisplayName fetches the exact shared item and derives a
// collision-free display name from the authoritative source item.
func resolveSharedDisplayName(
	ctx context.Context,
	cid driveid.CanonicalID,
	tokenPath string,
	cfg *config.Config,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
) (string, error) {
	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		return "", fmt.Errorf("load token source: %w", err)
	}

	client, err := newGraphClientWithHTTP("", runtime.BootstrapMeta(), ts, logger)
	if err != nil {
		return "", err
	}
	existingNames := collectExistingDisplayNames(cfg)

	item, err := client.GetItem(ctx, driveid.New(cid.SourceDriveID()), cid.SourceItemID())
	if err != nil {
		return "", fmt.Errorf("fetch shared item: %w", err)
	}

	item.RemoteDriveID = cid.SourceDriveID()
	item.RemoteItemID = cid.SourceItemID()

	return deriveSharedDisplayName(sharedDisplayInput{
		Name:          item.Name,
		SharedByName:  item.SharedOwnerName,
		SharedByEmail: item.SharedOwnerEmail,
		RemoteDriveID: item.RemoteDriveID,
		RemoteItemID:  item.RemoteItemID,
	}, existingNames), nil
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

func sharedDiscoveryNoMatchesError(
	selector string,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	var buf strings.Builder

	if len(authRequired) > 0 {
		if err := printAccountAuthRequirementsText(&buf, authRequired); err != nil {
			return fmt.Errorf("render auth-required shared discovery error: %w", err)
		}
		_, _ = buf.WriteString("\n")
	}

	if len(degraded) > 0 {
		if err := printAccountDegradedText(&buf, "Accounts with degraded shared discovery:", degraded); err != nil {
			return fmt.Errorf("render degraded shared discovery error: %w", err)
		}
		_, _ = buf.WriteString("\n")
	}

	_, _ = fmt.Fprintf(
		&buf,
		"no shared folders matching %q found — Graph shared discovery also checks external shares, "+
			"but Microsoft can still omit some cross-org items; if you have the original share URL, "+
			"run 'onedrive-go drive add <share-url>' to bypass discovery, or use 'onedrive-go shared' "+
			"or 'onedrive-go drive list' to confirm what the API exposed",
		selector,
	)

	return fmt.Errorf("%s", strings.TrimSpace(buf.String()))
}

// addSharedDriveByName searches discovered shared folders for a match against
// the given search term (case-insensitive substring match against folder name
// and derived display name). Single match -> add. Multiple -> show list.
func addSharedDriveByName(
	ctx context.Context,
	cc *CLIContext,
	selector string,
) error {
	logger := cc.Logger
	cfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	snapshot, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return err
	}

	discovery := discoverSharedTargets(ctx, cc, filterAccountViews(snapshot.Accounts, cc.Flags.Account))
	matches := searchSharedDrives(selector, projectSharedFolders(cfg, discovery.Targets))

	switch len(matches) {
	case 0:
		return sharedDiscoveryNoMatchesError(selector, discovery.AccountsRequiringAuth, discovery.AccountsDegraded)

	case 1:
		return addSharedDrive(ctx, cc.CfgPath, cc.Output(), matches[0].cid, matches[0].displayName, logger, cc.runtime())

	default:
		if err := writef(cc.Output(), "Multiple shared folders match %q — be more specific:\n\n", selector); err != nil {
			return err
		}

		for i := range matches {
			ownerInfo := ""
			if matches[i].target.SharedByEmail != "" {
				ownerInfo = fmt.Sprintf(" (shared by %s)", matches[i].target.SharedByEmail)
			}

			viaInfo := ""
			if matches[i].target.AccountEmail != "" {
				viaInfo = fmt.Sprintf(" [via %s]", matches[i].target.AccountEmail)
			}

			if err := writef(
				cc.Output(),
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

		return writeln(cc.Output(), "\nRun 'onedrive-go drive add <canonical-id>' to add a specific drive.")
	}
}

// searchSharedDrives filters discovered shared folders against the user query.
func searchSharedDrives(selector string, folders []sharedFolderInfo) []sharedFolderInfo {
	lowerSelector := strings.ToLower(selector)
	var matches []sharedFolderInfo

	for i := range folders {
		if !strings.Contains(strings.ToLower(folders[i].target.Name), lowerSelector) &&
			!strings.Contains(strings.ToLower(folders[i].displayName), lowerSelector) {
			continue
		}

		matches = append(matches, folders[i])
	}

	return matches
}
