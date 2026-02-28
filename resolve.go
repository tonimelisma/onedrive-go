package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// Resolution strategy aliases (re-export from sync package for CLI use).
const (
	resolutionKeepLocal  = sync.ResolutionKeepLocal
	resolutionKeepRemote = sync.ResolutionKeepRemote
	resolutionKeepBoth   = sync.ResolutionKeepBoth
)

func newResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve [path-or-id]",
		Short: "Resolve sync conflicts",
		Long: `Resolve sync conflicts with a chosen strategy.

Strategies:
  --keep-local   Upload the local file to overwrite remote
  --keep-remote  Download the remote file to overwrite local
  --keep-both    Keep both versions (conflict copies already saved)

Use --all to resolve all unresolved conflicts with the chosen strategy.
Without --all, a path or conflict ID argument is required.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runResolve,
	}

	cmd.Flags().Bool("keep-local", false, "upload local file to overwrite remote")
	cmd.Flags().Bool("keep-remote", false, "download remote file to overwrite local")
	cmd.Flags().Bool("keep-both", false, "keep both versions as-is")
	cmd.Flags().Bool("all", false, "resolve all unresolved conflicts")
	cmd.Flags().Bool("dry-run", false, "preview resolution without executing")

	cmd.MarkFlagsMutuallyExclusive("keep-local", "keep-remote", "keep-both")

	return cmd
}

func runResolve(cmd *cobra.Command, args []string) error {
	resolution, err := resolveStrategy(cmd)
	if err != nil {
		return err
	}

	resolveAll := cmd.Flags().Changed("all")

	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	if !resolveAll && len(args) == 0 {
		return fmt.Errorf("specify a conflict path or ID, or use --all to resolve all conflicts")
	}

	if resolveAll && len(args) > 0 {
		return fmt.Errorf("--all and a specific conflict argument are mutually exclusive")
	}

	ctx := cmd.Context()

	// keep_both doesn't need graph client — just DB update.
	if resolution == resolutionKeepBoth {
		return resolveKeepBothOnly(ctx, args, resolveAll, dryRun)
	}

	// keep_local and keep_remote need graph client for transfers.
	return resolveWithTransfers(ctx, args, resolution, resolveAll, dryRun)
}

// resolveStrategy returns the chosen resolution string from flags.
func resolveStrategy(cmd *cobra.Command) (string, error) {
	keepLocal := cmd.Flags().Changed("keep-local")
	keepRemote := cmd.Flags().Changed("keep-remote")
	keepBoth := cmd.Flags().Changed("keep-both")

	if !keepLocal && !keepRemote && !keepBoth {
		return "", fmt.Errorf("specify a resolution strategy: --keep-local, --keep-remote, or --keep-both")
	}

	switch {
	case keepLocal:
		return resolutionKeepLocal, nil
	case keepRemote:
		return resolutionKeepRemote, nil
	default:
		return resolutionKeepBoth, nil
	}
}

// resolveKeepBothOnly handles keep_both resolution which only needs the DB.
func resolveKeepBothOnly(ctx context.Context, args []string, all, dryRun bool) error {
	cc := mustCLIContext(ctx)

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	mgr, err := sync.NewBaselineManager(dbPath, cc.Logger)
	if err != nil {
		return err
	}
	defer mgr.Close()

	if all {
		return resolveAllKeepBoth(ctx, mgr, dryRun)
	}

	return resolveSingleKeepBoth(ctx, mgr, args[0], dryRun)
}

// resolveEachConflict iterates conflicts and calls resolveFn for each non-dry-run
// resolution. Extracted to deduplicate resolveAllKeepBoth and resolveAllWithEngine.
func resolveEachConflict(
	conflicts []sync.ConflictRecord, resolution string, dryRun bool,
	resolveFn func(id, resolution string) error,
) error {
	if len(conflicts) == 0 {
		fmt.Println("No unresolved conflicts.")
		return nil
	}

	for i := range conflicts {
		c := &conflicts[i]
		if dryRun {
			statusf("Would resolve %s (%s) as %s\n", c.Path, truncateID(c.ID), resolution)
			continue
		}

		if err := resolveFn(c.ID, resolution); err != nil {
			return fmt.Errorf("resolving %s: %w", c.Path, err)
		}

		statusf("Resolved %s as %s\n", c.Path, resolution)
	}

	return nil
}

func resolveAllKeepBoth(ctx context.Context, mgr *sync.BaselineManager, dryRun bool) error {
	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		return err
	}

	return resolveEachConflict(conflicts, resolutionKeepBoth, dryRun, func(id, resolution string) error {
		return mgr.ResolveConflict(ctx, id, resolution)
	})
}

func resolveSingleKeepBoth(ctx context.Context, mgr *sync.BaselineManager, idOrPath string, dryRun bool) error {
	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		return err
	}

	target, findErr := findConflict(conflicts, idOrPath)
	if findErr != nil {
		return findErr
	}

	if target == nil {
		return fmt.Errorf("conflict not found: %s", idOrPath)
	}

	if dryRun {
		statusf("Would resolve %s (%s) as keep_both\n", target.Path, truncateID(target.ID))
		return nil
	}

	if err := mgr.ResolveConflict(ctx, target.ID, resolutionKeepBoth); err != nil {
		return err
	}

	statusf("Resolved %s as keep_both\n", target.Path)

	return nil
}

// resolveWithTransfers handles keep_local and keep_remote which need graph client.
func resolveWithTransfers(
	ctx context.Context, args []string, resolution string, all, dryRun bool,
) error {
	cc := mustCLIContext(ctx)
	logger := cc.Logger

	client, ts, driveID, err := clientAndDrive(ctx, cc)
	if err != nil {
		return err
	}

	syncDir := cc.Cfg.SyncDir
	if syncDir == "" {
		return fmt.Errorf("sync_dir not configured")
	}

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	// Use transfer client (no timeout) for upload/download operations.
	transferClient := newTransferGraphClient(ts, logger)

	engine, err := sync.NewEngine(&sync.EngineConfig{
		DBPath:        dbPath,
		SyncRoot:      syncDir,
		DataDir:       config.DefaultDataDir(),
		DriveID:       driveID,
		Fetcher:       client,
		Items:         client,
		Downloads:     transferClient,
		Uploads:       transferClient,
		Logger:        logger,
		UseLocalTrash: cc.Cfg.UseLocalTrash,
	})
	if err != nil {
		return err
	}
	defer engine.Close()

	if all {
		return resolveAllWithEngine(ctx, engine, resolution, dryRun)
	}

	return resolveSingleWithEngine(ctx, engine, args[0], resolution, dryRun)
}

func resolveAllWithEngine(ctx context.Context, engine *sync.Engine, resolution string, dryRun bool) error {
	conflicts, err := engine.ListConflicts(ctx)
	if err != nil {
		return err
	}

	return resolveEachConflict(conflicts, resolution, dryRun, func(id, res string) error {
		return engine.ResolveConflict(ctx, id, res)
	})
}

func resolveSingleWithEngine(ctx context.Context, engine *sync.Engine, idOrPath, resolution string, dryRun bool) error {
	conflicts, err := engine.ListConflicts(ctx)
	if err != nil {
		return err
	}

	target, findErr := findConflict(conflicts, idOrPath)
	if findErr != nil {
		return findErr
	}

	if target == nil {
		return fmt.Errorf("conflict not found: %s", idOrPath)
	}

	if dryRun {
		statusf("Would resolve %s (%s) as %s\n", target.Path, truncateID(target.ID), resolution)
		return nil
	}

	err = engine.ResolveConflict(ctx, target.ID, resolution)
	if err != nil {
		return err
	}

	statusf("Resolved %s as %s\n", target.Path, resolution)

	return nil
}

// errAmbiguousPrefix is returned when a conflict ID prefix matches multiple
// conflicts and the user needs to provide a longer prefix.
var errAmbiguousPrefix = errors.New("ambiguous conflict ID prefix — provide more characters")

// findConflict searches a conflict list by exact ID, exact path, or ID prefix.
// Returns an error if an ID prefix matches multiple conflicts.
func findConflict(conflicts []sync.ConflictRecord, idOrPath string) (*sync.ConflictRecord, error) {
	// First pass: exact matches (ID or path) take priority.
	for i := range conflicts {
		c := &conflicts[i]
		if c.ID == idOrPath || c.Path == idOrPath {
			return c, nil
		}
	}

	// Second pass: prefix match with ambiguity detection.
	var match *sync.ConflictRecord

	for i := range conflicts {
		c := &conflicts[i]
		if len(c.ID) >= len(idOrPath) && c.ID[:len(idOrPath)] == idOrPath {
			if match != nil {
				return nil, errAmbiguousPrefix
			}

			match = c
		}
	}

	return match, nil
}
