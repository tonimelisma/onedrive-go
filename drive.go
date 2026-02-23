package main

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func newDriveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drive",
		Short: "Manage drives (add, remove)",
		Long:  "Add or remove drives from the configuration.",
	}

	cmd.AddCommand(newDriveAddCmd())
	cmd.AddCommand(newDriveRemoveCmd())

	return cmd
}

func newDriveAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add",
		Short: "Resume a paused drive or add a new one",
		Long: `Resume a paused drive by setting enabled = true.

If --drive is specified, resumes that specific drive.
Without --drive, lists paused drives that can be resumed.

To add SharePoint libraries, use:
  onedrive-go drive add --drive sharepoint:email:site:library`,
		RunE: runDriveAdd,
	}
}

func newDriveRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Pause or purge a drive",
		Long: `Pause a drive by setting enabled = false in the config.

The drive's token, state database, config section, and sync directory are preserved.
With --purge, the config section and state database are deleted.
The sync directory is never deleted automatically.`,
		RunE: runDriveRemove,
	}

	cmd.Flags().Bool("purge", false, "delete config section and state database")

	return cmd
}

func runDriveAdd(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	cfgPath := resolveLoginConfigPath()

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Drives) == 0 {
		fmt.Println("No drives configured. Run 'onedrive-go login' to get started.")

		return nil
	}

	// If --drive is specified, resume that specific drive.
	if flagDrive != "" {
		return resumeSpecificDrive(cfgPath, cfg, flagDrive, logger)
	}

	// Otherwise, list paused drives.
	return listPausedDrives(cfg)
}

// resumeSpecificDrive enables a specific drive by setting enabled = true.
// The driveSelector is raw user input that gets resolved to a CanonicalID.
func resumeSpecificDrive(cfgPath string, cfg *config.Config, driveSelector string, logger *slog.Logger) error {
	cid, err := driveid.NewCanonicalID(driveSelector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveSelector, err)
	}

	d, exists := cfg.Drives[cid]
	if !exists {
		return fmt.Errorf("drive %q not found in config", driveSelector)
	}

	if d.Enabled == nil || *d.Enabled {
		fmt.Printf("Drive %s is already enabled.\n", cid.String())

		return nil
	}

	logger.Info("resuming drive", "drive", cid.String())

	if err := config.SetDriveKey(cfgPath, cid, "enabled", "true"); err != nil {
		return fmt.Errorf("enabling drive: %w", err)
	}

	fmt.Printf("Resumed drive %s (%s).\n", cid.String(), d.SyncDir)

	return nil
}

// listPausedDrives prints all paused drives, or a message if none are paused.
func listPausedDrives(cfg *config.Config) error {
	var paused []driveid.CanonicalID

	for id := range cfg.Drives {
		d := cfg.Drives[id]
		if d.Enabled != nil && !*d.Enabled {
			paused = append(paused, id)
		}
	}

	if len(paused) == 0 {
		fmt.Println("No paused drives to resume.")
		fmt.Println("To add SharePoint libraries, use: onedrive-go drive add --drive sharepoint:email:site:library")

		return nil
	}

	fmt.Println("Paused drives (use --drive to resume):")

	for _, id := range paused {
		fmt.Printf("  %s (%s)\n", id.String(), cfg.Drives[id].SyncDir)
	}

	return nil
}

func runDriveRemove(cmd *cobra.Command, _ []string) error {
	logger := buildLogger()

	if flagDrive == "" {
		return fmt.Errorf("--drive is required (specify which drive to remove)")
	}

	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	cfgPath := resolveLoginConfigPath()

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, cidErr := driveid.NewCanonicalID(flagDrive)
	if cidErr != nil {
		return fmt.Errorf("invalid drive ID %q: %w", flagDrive, cidErr)
	}

	d, exists := cfg.Drives[cid]
	if !exists {
		return fmt.Errorf("drive %q not found in config", flagDrive)
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)

	if purge {
		return purgeDrive(cfgPath, cid, logger)
	}

	return pauseDrive(cfgPath, cid, d.SyncDir)
}

// pauseDrive sets enabled = false for the drive, preserving all data.
func pauseDrive(cfgPath string, driveID driveid.CanonicalID, syncDir string) error {
	if err := config.SetDriveKey(cfgPath, driveID, "enabled", "false"); err != nil {
		return fmt.Errorf("pausing drive: %w", err)
	}

	idStr := driveID.String()
	fmt.Printf("Paused drive %s.\n", idStr)
	fmt.Printf("Config, token, and state database kept for %s.\n", idStr)
	fmt.Printf("Sync directory untouched: %s\n", syncDir)
	fmt.Println("Run 'onedrive-go drive add --drive " + idStr + "' to resume.")

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
