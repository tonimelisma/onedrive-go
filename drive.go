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
func resumeSpecificDrive(cfgPath string, cfg *config.Config, driveID string, logger *slog.Logger) error {
	d, exists := cfg.Drives[driveID]
	if !exists {
		return fmt.Errorf("drive %q not found in config", driveID)
	}

	if d.Enabled == nil || *d.Enabled {
		fmt.Printf("Drive %s is already enabled.\n", driveID)

		return nil
	}

	logger.Info("resuming drive", "drive", driveID)

	if err := config.SetDriveKey(cfgPath, driveID, "enabled", "true"); err != nil {
		return fmt.Errorf("enabling drive: %w", err)
	}

	fmt.Printf("Resumed drive %s (%s).\n", driveID, d.SyncDir)

	return nil
}

// listPausedDrives prints all paused drives, or a message if none are paused.
func listPausedDrives(cfg *config.Config) error {
	var paused []string

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
		fmt.Printf("  %s (%s)\n", id, cfg.Drives[id].SyncDir)
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

	d, exists := cfg.Drives[flagDrive]
	if !exists {
		return fmt.Errorf("drive %q not found in config", flagDrive)
	}

	logger.Info("removing drive", "drive", flagDrive, "purge", purge)

	if purge {
		return purgeDrive(cfgPath, flagDrive, logger)
	}

	return pauseDrive(cfgPath, flagDrive, d.SyncDir)
}

// pauseDrive sets enabled = false for the drive, preserving all data.
func pauseDrive(cfgPath, driveID, syncDir string) error {
	if err := config.SetDriveKey(cfgPath, driveID, "enabled", "false"); err != nil {
		return fmt.Errorf("pausing drive: %w", err)
	}

	fmt.Printf("Paused drive %s.\n", driveID)
	fmt.Printf("Config, token, and state database kept for %s.\n", driveID)
	fmt.Printf("Sync directory untouched: %s\n", syncDir)
	fmt.Println("Run 'onedrive-go drive add --drive " + driveID + "' to resume.")

	return nil
}

// purgeDrive deletes the config section and state database for a drive.
// The token is NOT deleted here — it may be shared with other drives (SharePoint).
func purgeDrive(cfgPath, driveID string, logger *slog.Logger) error {
	cid, err := driveid.NewCanonicalID(driveID)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveID, err)
	}

	if err := purgeSingleDrive(cfgPath, cid, logger); err != nil {
		return err
	}

	fmt.Printf("Purged config and state for %s.\n", driveID)
	fmt.Println("Sync directory untouched — delete manually if desired.")

	return nil
}
