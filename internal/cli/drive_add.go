package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

func runDriveAddWithContext(ctx context.Context, cc *CLIContext, args []string) error {
	if cc.SharedTarget != nil {
		result, err := addDriveFromSharedTarget(ctx, cc)
		return finishDriveAdd(ctx, cc, result, err)
	}

	selector, err := driveAddSelector(cc, args)
	if err != nil {
		return err
	}

	if selector == "" {
		if cc.Flags.JSON {
			// Without a selector, drive add only prints guidance; the
			// listing a script wants is `drive list --json`.
			return &usageError{
				commandPath: "onedrive-go drive add",
				err:         fmt.Errorf("drive add --json needs a drive to add; run 'onedrive-go drive list --json' to see available drives"),
			}
		}

		return listAvailableDrives(cc.Output())
	}

	cid, parseErr := driveid.NewCanonicalID(selector)
	if parseErr != nil {
		if strings.Contains(selector, ":") {
			return fmt.Errorf("invalid canonical ID %q: %w\n"+
				"Run 'onedrive-go drive list' to see valid canonical IDs", selector, parseErr)
		}

		result, addErr := addSharedDriveByName(ctx, cc, selector)
		return finishDriveAdd(ctx, cc, result, addErr)
	}

	if cid.IsShared() {
		result, addErr := addSharedDriveByCanonicalID(ctx, cc, cid)
		return finishDriveAdd(ctx, cc, result, addErr)
	}

	result, addErr := addOwnDrive(cc.resultWriter(), cc.CfgPath, cid, cc.Logger)
	return finishDriveAdd(ctx, cc, result, addErr)
}

// driveAddSelector is the drive to add: the argument, else a single --drive.
func driveAddSelector(cc *CLIContext, args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}

	return cc.Flags.SingleDrive()
}

// addDriveFromSharedTarget adds the shared folder named by a shared-target
// selector (shared:<recipient>:<drive>:<item>) resolved at bootstrap.
func addDriveFromSharedTarget(ctx context.Context, cc *CLIContext) (driveAddResult, error) {
	item, _, err := cc.resolveSharedItem(ctx)
	if err != nil {
		return driveAddResult{}, err
	}
	if !item.IsFolder {
		return driveAddResult{}, fmt.Errorf("shared files are direct stat/get/put targets, not drives")
	}

	cid, err := driveid.NewCanonicalID(cc.SharedTarget.Selector())
	if err != nil {
		return driveAddResult{}, fmt.Errorf("parse shared drive identity: %w", err)
	}

	return addSharedDrive(ctx, cc.CfgPath, cc.resultWriter(), cid, "", cc.Logger, cc.runtime())
}

// addSharedDriveByCanonicalID adds a shared folder given as a shared
// canonical ID, after checking it is a folder rather than a file.
func addSharedDriveByCanonicalID(ctx context.Context, cc *CLIContext, cid driveid.CanonicalID) (driveAddResult, error) {
	clients, err := cc.sharedTargetClients(ctx, sharedref.Ref{
		AccountEmail:  cid.Email(),
		RemoteDriveID: cid.SourceDriveID(),
		RemoteItemID:  cid.SourceItemID(),
	})
	if err != nil {
		return driveAddResult{}, err
	}

	item, err := clients.Meta.GetItem(ctx, driveid.New(cid.SourceDriveID()), cid.SourceItemID())
	if err != nil {
		return driveAddResult{}, fmt.Errorf("loading shared item: %w", err)
	}
	if !item.IsFolder {
		return driveAddResult{}, fmt.Errorf("shared files are direct stat/get/put targets, not drives")
	}

	return addSharedDrive(ctx, cc.CfgPath, cc.resultWriter(), cid, "", cc.Logger, cc.runtime())
}

// driveAddResult is what drive add did, and is also `drive add --json`.
// added is false when the drive was already configured; completed is true
// when it was configured but not fully set up (a section written by hand or by
// an earlier version of the client, or a sync folder removed since) and this
// run finished it.
type driveAddResult struct {
	Drive       string `json:"drive"`
	DisplayName string `json:"display_name,omitempty"`
	SyncDir     string `json:"sync_dir"`
	Added       bool   `json:"added"`
	Completed   bool   `json:"completed"`
}

// finishDriveAdd tells a running sync about the new drive and, under --json,
// prints the result.
func finishDriveAdd(ctx context.Context, cc *CLIContext, result driveAddResult, err error) error {
	if err != nil {
		return err
	}
	notifyDaemonIfRunning(ctx, cc)

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), result)
	}

	return nil
}

// addOwnDrive enrolls one of a signed-in account's own drives (its primary
// drive or a SharePoint library) through config.AddDrive. The account's
// token must exist.
func addOwnDrive(w io.Writer, cfgPath string, cid driveid.CanonicalID, logger *slog.Logger) (driveAddResult, error) {
	if err := requireSavedLogin(config.DriveTokenPath(cid), cid.Email()); err != nil {
		return driveAddResult{}, err
	}

	enrolled, err := config.AddDrive(&config.AddDriveRequest{
		ConfigPath:  cfgPath,
		DataDir:     config.DefaultDataDir(),
		Drive:       cid,
		MakeSyncDir: materializeDriveSyncDir,
	}, logger)
	if err != nil {
		return driveAddResult{}, fmt.Errorf("adding drive %s: %w", cid, err)
	}

	return reportDriveAdd(w, cid, &enrolled)
}

// requireSavedLogin fails unless the token file at tokenPath exists: drive
// add only enrolls a drive of an account that is signed in.
func requireSavedLogin(tokenPath, email string) error {
	if tokenPath == "" {
		return fmt.Errorf("cannot determine data directory for %s", email)
	}

	tokenExists, err := managedPathExists(tokenPath)
	if err != nil {
		return fmt.Errorf("checking for the token file of %s: %w", email, err)
	}
	if !tokenExists {
		return fmt.Errorf("no token file for %s — run 'onedrive-go login' first", email)
	}

	return nil
}

// reportDriveAdd prints what the enrollment did and returns it as the
// command's result.
func reportDriveAdd(w io.Writer, cid driveid.CanonicalID, enrolled *config.AddDriveResult) (driveAddResult, error) {
	result := driveAddResult{
		Drive:       cid.String(),
		DisplayName: enrolled.DisplayName,
		SyncDir:     enrolled.SyncDir,
		Added:       enrolled.Added,
		Completed:   enrolled.Completed,
	}

	switch {
	case enrolled.Added:
		return result, writef(w, "Added drive %s (%s) -> %s\n", result.DisplayName, result.Drive, result.SyncDir)
	case enrolled.Completed:
		return result, writef(w, "Completed drive %s (%s) -> %s: it was configured but not fully set up.\n",
			result.DisplayName, result.Drive, result.SyncDir)
	default:
		return result, writef(w, "Drive %s is already configured.\n", result.Drive)
	}
}

// listAvailableDrives lists drives that can be added. Shows usage guidance
// when no canonical ID argument is provided.
func listAvailableDrives(w io.Writer) error {
	if err := writeln(w, "Run 'onedrive-go drive add <canonical-id>' to add a drive."); err != nil {
		return err
	}

	return writeln(w, "Run 'onedrive-go drive list' to see available drives.")
}

// addSharedDrive enrolls a shared folder through config.AddDrive, under the
// signed-in account it is shared with. If preResolvedName is non-empty, it is
// used directly (avoids re-querying the API when the caller already has the
// display name from search results).
func addSharedDrive(
	ctx context.Context,
	cfgPath string,
	w io.Writer,
	cid driveid.CanonicalID,
	preResolvedName string,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
) (driveAddResult, error) {
	// Shared drives don't have their own token — find the account they are
	// shared with.
	ownerCID, err := config.LoadAccountCanonicalIDByEmail(config.DefaultDataDir(), cid.Email())
	if err != nil {
		return driveAddResult{}, fmt.Errorf("loading catalog owner: %w", err)
	}
	if ownerCID.IsZero() {
		return driveAddResult{}, fmt.Errorf("no token file for %s — run 'onedrive-go login' first", cid.Email())
	}

	tokenPath := config.DriveTokenPath(ownerCID)
	if loginErr := requireSavedLogin(tokenPath, cid.Email()); loginErr != nil {
		return driveAddResult{}, loginErr
	}

	displayName, err := sharedDriveDisplayName(ctx, cfgPath, cid, preResolvedName, tokenPath, logger, runtime)
	if err != nil {
		return driveAddResult{}, err
	}

	enrolled, err := config.AddDrive(&config.AddDriveRequest{
		ConfigPath:  cfgPath,
		DataDir:     config.DefaultDataDir(),
		Drive:       cid,
		Owner:       ownerCID,
		DisplayName: displayName,
		MakeSyncDir: materializeDriveSyncDir,
	}, logger)
	if err != nil {
		return driveAddResult{}, fmt.Errorf("adding drive %s: %w", cid, err)
	}

	return reportDriveAdd(w, cid, &enrolled)
}

// sharedDriveDisplayName is the name a shared drive's section gets when it
// has none yet. It is resolved before the enrollment because it may take a
// Graph request. The config read here only spares that request when the
// section already has a name; AddDrive decides from its own locked read and
// never replaces a configured name.
func sharedDriveDisplayName(
	ctx context.Context,
	cfgPath string,
	cid driveid.CanonicalID,
	preResolvedName string,
	tokenPath string,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
) (string, error) {
	if preResolvedName != "" {
		return preResolvedName, nil
	}

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	if existing, found := cfg.Drives[cid]; found && existing.DisplayName != "" {
		return existing.DisplayName, nil
	}

	displayName, err := resolveSharedDisplayName(ctx, cid, tokenPath, cfg, runtime)
	if err != nil {
		fallback := config.DefaultDisplayName(cid)
		logger.Warn("could not derive shared drive display name, using fallback",
			"error", err, "fallback", fallback)

		return fallback, nil
	}

	return displayName, nil
}

// resolveSharedDisplayName fetches the exact shared item and derives a
// collision-free display name from the authoritative source item.
func resolveSharedDisplayName(
	ctx context.Context,
	cid driveid.CanonicalID,
	tokenPath string,
	cfg *config.Config,
	runtime *driveops.SessionRuntime,
) (string, error) {
	client, err := runtime.BootstrapClient(ctx, tokenPath)
	if err != nil {
		return "", fmt.Errorf("load sign-in for shared drive name: %w", err)
	}
	existingNames := collectExistingDisplayNames(cfg)

	item, err := client.GetItem(ctx, driveid.New(cid.SourceDriveID()), cid.SourceItemID())
	if err != nil {
		return "", fmt.Errorf("fetch shared item: %w", err)
	}

	return deriveSharedDisplayName(sharedDisplayInput{
		Name:          item.Name,
		SharedByName:  item.SharedOwnerName,
		SharedByEmail: item.SharedOwnerEmail,
		RemoteDriveID: cid.SourceDriveID(),
		RemoteItemID:  cid.SourceItemID(),
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

	return errors.New(strings.TrimSpace(buf.String()))
}

// addSharedDriveByName searches discovered shared folders for a match against
// the given search term (case-insensitive substring match against folder name
// and derived display name). Single match -> add. Multiple -> show list.
func addSharedDriveByName(
	ctx context.Context,
	cc *CLIContext,
	selector string,
) (driveAddResult, error) {
	logger := cc.Logger
	cfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return driveAddResult{}, fmt.Errorf("loading config: %w", err)
	}

	snapshot, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return driveAddResult{}, err
	}

	discovery := discoverSharedTargets(ctx, cc, filterAccountViews(snapshot.Accounts, cc.Flags.Account))
	matches := searchSharedDrives(selector, projectSharedFolders(cfg, discovery.Targets))

	switch len(matches) {
	case 0:
		return driveAddResult{}, sharedDiscoveryNoMatchesError(selector, discovery.AccountsRequiringAuth, discovery.AccountsDegraded)

	case 1:
		return addSharedDrive(ctx, cc.CfgPath, cc.resultWriter(), matches[0].cid, matches[0].displayName, logger, cc.runtime())

	default:
		if err := writef(cc.Status(), "Multiple shared folders match %q — be more specific:\n\n", selector); err != nil {
			return driveAddResult{}, err
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
				cc.Status(),
				"  %d. %s%s%s\n     %s\n",
				i+1,
				matches[i].displayName,
				ownerInfo,
				viaInfo,
				matches[i].cid.String(),
			); err != nil {
				return driveAddResult{}, err
			}
		}

		// Nothing was added, so this is a failure for scripts, and the list
		// is guidance rather than the command's result: stderr.
		if err := writeln(cc.Status(), "\nRun 'onedrive-go drive add <canonical-id>' to add a specific drive."); err != nil {
			return driveAddResult{}, err
		}

		return driveAddResult{}, fmt.Errorf("%d shared folders match %q; nothing was added", len(matches), selector)
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
