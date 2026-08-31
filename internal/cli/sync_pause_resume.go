package cli

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func runPauseCommand(cc *CLIContext, now func() time.Time, args []string) error {
	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive to pause)")
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, err := driveid.NewCanonicalID(driveSelector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveSelector, err)
	}

	if _, exists := cfg.Drives[cid]; !exists {
		return fmt.Errorf("drive %q not found in config", driveSelector)
	}

	if err := config.SetDriveKey(cc.CfgPath, cid, "paused", "true"); err != nil {
		return fmt.Errorf("setting paused flag: %w", err)
	}

	if len(args) > 0 {
		duration, err := parseDuration(args[0])
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", args[0], err)
		}

		until := now().Add(duration).Format(time.RFC3339)
		if err := config.SetDriveKey(cc.CfgPath, cid, "paused_until", until); err != nil {
			return fmt.Errorf("setting paused_until: %w", err)
		}

		cc.Statusf("Drive %s paused until %s\n", cid.String(), until)
	} else {
		cc.Statusf("Drive %s paused\n", cid.String())
	}

	notifyDaemon(cc)

	return nil
}

func runResumeCommand(cc *CLIContext, now func() time.Time) error {
	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector != "" {
		return resumeSingleDriveWithNow(cc, now, cfg, driveSelector)
	}

	return resumeAllDrivesWithNow(cc, now, cfg)
}

func resumeSingleDriveWithNow(cc *CLIContext, now func() time.Time, cfg *config.Config, selector string) error {
	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", selector, err)
	}

	d, exists := cfg.Drives[cid]
	if !exists {
		return fmt.Errorf("drive %q not found in config", selector)
	}

	if !d.IsPaused(now()) {
		if d.Paused != nil && *d.Paused {
			if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
				return err
			}

			cc.Statusf("Drive %s: expired timed pause cleared\n", cid.String())
			return nil
		}

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

// resumeAllDrivesWithNow resumes every paused drive.
//
// One drive failing must not strand the rest. "Resume all" is a bulk request,
// and returning at the first failure leaves an arbitrary subset still paused
// with nothing saying which -- the drives the loop had not reached yet look
// exactly like the ones that resumed. Every drive is attempted and the
// failures are reported together.
func resumeAllDrivesWithNow(cc *CLIContext, now func() time.Time, cfg *config.Config) error {
	if len(cfg.Drives) == 0 {
		return fmt.Errorf("no drives configured")
	}

	resumed := 0

	var errs []error

	// Sorted rather than map order: this prints a line per drive, and a bulk
	// command that reports its drives in a different order every run is hard
	// to read and harder to diff. It also makes a partial failure reproducible
	// instead of depending on map iteration.
	for _, cid := range sortedDriveCanonicalIDs(cfg.Drives) {
		drive := cfg.Drives[cid]

		cleared, err := resumeOneDrive(cc, now, cid, &drive)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		if cleared {
			resumed++
		}
	}

	if resumed > 0 {
		notifyDaemon(cc)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if resumed == 0 {
		cc.Statusf("No paused drives found\n")
	}

	return nil
}

// sortedDriveCanonicalIDs returns the configured drives in a stable order.
func sortedDriveCanonicalIDs(drives map[driveid.CanonicalID]config.Drive) []driveid.CanonicalID {
	ids := make([]driveid.CanonicalID, 0, len(drives))
	for cid := range drives {
		ids = append(ids, cid)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	return ids
}

// resumeOneDrive clears any pause on a single drive, reporting whether it
// cleared one.
func resumeOneDrive(
	cc *CLIContext,
	now func() time.Time,
	cid driveid.CanonicalID,
	drive *config.Drive,
) (bool, error) {
	if !drive.IsPaused(now()) {
		if drive.Paused == nil || !*drive.Paused {
			return false, nil
		}

		if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
			return false, fmt.Errorf("clearing expired pause for %s: %w", cid.String(), err)
		}

		cc.Statusf("Drive %s: expired timed pause cleared\n", cid.String())

		return true, nil
	}

	if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
		return false, fmt.Errorf("resuming %s: %w", cid.String(), err)
	}

	cc.Statusf("Drive %s resumed\n", cid.String())

	return true, nil
}
