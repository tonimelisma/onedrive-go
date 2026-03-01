package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume syncing for a paused drive",
		Long: `Resume syncing for the specified drive. With --drive, resumes that drive.
Without --drive, resumes ALL paused drives.

If a sync --watch daemon is running, it receives a SIGHUP to pick up the change.

Examples:
  onedrive-go resume --drive personal:user@example.com
  onedrive-go resume`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runResume,
	}
}

func runResume(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	cfgPath := resolveLoginConfigPath(cc.Flags.ConfigPath)

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if cc.Flags.Drive != "" {
		return resumeSingleDrive(cfgPath, cfg, cc.Flags.Drive, cc.Flags.Quiet)
	}

	return resumeAllDrives(cfgPath, cfg, cc.Flags.Quiet)
}

// resumeSingleDrive resumes a specific drive by canonical ID.
func resumeSingleDrive(cfgPath string, cfg *config.Config, selector string, quiet bool) error {
	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", selector, err)
	}

	d, exists := cfg.Drives[cid]
	if !exists {
		return fmt.Errorf("drive %q not found in config", selector)
	}

	if d.Paused == nil || !*d.Paused {
		statusf(quiet, "Drive %s is not paused\n", cid.String())

		return nil
	}

	if err := clearPausedKeys(cfgPath, cid); err != nil {
		return err
	}

	statusf(quiet, "Drive %s resumed\n", cid.String())
	notifyDaemon(quiet)

	return nil
}

// resumeAllDrives resumes every paused drive in the config.
func resumeAllDrives(cfgPath string, cfg *config.Config, quiet bool) error {
	if len(cfg.Drives) == 0 {
		return fmt.Errorf("no drives configured")
	}

	resumed := 0

	for cid := range cfg.Drives {
		d := cfg.Drives[cid]
		if d.Paused == nil || !*d.Paused {
			continue
		}

		if err := clearPausedKeys(cfgPath, cid); err != nil {
			return fmt.Errorf("resuming %s: %w", cid.String(), err)
		}

		statusf(quiet, "Drive %s resumed\n", cid.String())
		resumed++
	}

	if resumed == 0 {
		statusf(quiet, "No paused drives found\n")

		return nil
	}

	notifyDaemon(quiet)

	return nil
}

// clearPausedKeys removes both paused and paused_until keys from a drive section.
func clearPausedKeys(cfgPath string, cid driveid.CanonicalID) error {
	if err := config.DeleteDriveKey(cfgPath, cid, "paused"); err != nil {
		return fmt.Errorf("clearing paused flag: %w", err)
	}

	if err := config.DeleteDriveKey(cfgPath, cid, "paused_until"); err != nil {
		return fmt.Errorf("clearing paused_until: %w", err)
	}

	return nil
}
