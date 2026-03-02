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

	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if driveSelector := cc.Flags.SingleDrive(); driveSelector != "" {
		return resumeSingleDrive(cc, cfg, driveSelector)
	}

	return resumeAllDrives(cc, cfg)
}

// resumeSingleDrive resumes a specific drive by canonical ID.
func resumeSingleDrive(cc *CLIContext, cfg *config.Config, selector string) error {
	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", selector, err)
	}

	d, exists := cfg.Drives[cid]
	if !exists {
		return fmt.Errorf("drive %q not found in config", selector)
	}

	if d.Paused == nil || !*d.Paused {
		cc.Statusf("Drive %s is not paused\n", cid.String())

		return nil
	}

	if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
		return err
	}

	cc.Statusf("Drive %s resumed\n", cid.String())
	notifyDaemon(cc)

	return nil
}

// resumeAllDrives resumes every paused drive in the config.
func resumeAllDrives(cc *CLIContext, cfg *config.Config) error {
	if len(cfg.Drives) == 0 {
		return fmt.Errorf("no drives configured")
	}

	resumed := 0

	for cid := range cfg.Drives {
		d := cfg.Drives[cid]
		if d.Paused == nil || !*d.Paused {
			continue
		}

		if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
			return fmt.Errorf("resuming %s: %w", cid.String(), err)
		}

		cc.Statusf("Drive %s resumed\n", cid.String())
		resumed++
	}

	if resumed == 0 {
		cc.Statusf("No paused drives found\n")

		return nil
	}

	notifyDaemon(cc)

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
