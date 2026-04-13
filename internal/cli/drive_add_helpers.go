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
)

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
	ctx context.Context,
	cfgPath string,
	w io.Writer,
	cid driveid.CanonicalID,
	preResolvedName string,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
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

		displayName, resolveErr = resolveSharedDisplayName(ctx, cid, tokenPath, cfg, logger, runtime)
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
		if err := printAccountAuthRequirementsText(&buf, "Authentication required:", authRequired); err != nil {
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

	snapshot, err := loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return err
	}

	discovery := discoverSharedTargets(ctx, cc, filterAccountCatalog(snapshot.Catalog, cc.Flags.Account))
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
