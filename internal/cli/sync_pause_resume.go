package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func runPauseCommand(ctx context.Context, cc *CLIContext, now func() time.Time, args []string) error {
	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	// Parse the duration before touching the config: a typo must not leave
	// the drive paused with no end time.
	var duration time.Duration
	if len(args) > 0 {
		parsed, err := parseDuration(args[0])
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", args[0], err)
		}
		duration = parsed
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, drive, err := config.MatchDrive(cfg, driveSelector, cc.Logger)
	if err != nil {
		return fmt.Errorf("choosing the drive to pause: %w", err)
	}

	if err := config.SetDriveKey(cc.CfgPath, cid, "paused", "true"); err != nil {
		return fmt.Errorf("setting paused flag: %w", err)
	}

	result := pauseJSONOutput{Drive: cid.String(), Paused: true}

	if duration > 0 {
		until := now().Add(duration)
		if err := config.SetDriveKey(cc.CfgPath, cid, "paused_until", until.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("setting paused_until: %w", err)
		}

		result.PausedUntil = until.UTC().Format(time.RFC3339)
		if !cc.Flags.JSON {
			cc.Statusf("Paused %s until %s.\n", configuredDriveLabel(cid, &drive), until.Local().Format("Mon Jan 2 15:04"))
		}
	} else if !cc.Flags.JSON {
		cc.Statusf("Paused %s until you run 'onedrive-go resume'.\n", configuredDriveLabel(cid, &drive))
	}

	notifyDaemon(ctx, cc)

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), result)
	}

	return nil
}

// pauseJSONOutput is `pause --json`: the drive and the pause now recorded for
// it. paused_until is omitted for a pause that lasts until `resume`.
type pauseJSONOutput struct {
	Drive       string `json:"drive"`
	Paused      bool   `json:"paused"`
	PausedUntil string `json:"paused_until,omitempty"`
}

// resumeJSONOutput is `resume --json`: one entry per drive the command looked
// at, saying what it did there.
type resumeJSONOutput struct {
	Drives []resumeJSONDrive `json:"drives"`
}

type resumeJSONDrive struct {
	Drive  string        `json:"drive"`
	Result resumeOutcome `json:"result"`
	Error  string        `json:"error,omitempty"`
}

// resumeOutcome says what resume did to one drive.
type resumeOutcome string

const (
	resumeOutcomeResumed      resumeOutcome = "resumed"
	resumeOutcomeExpiredPause resumeOutcome = "expired_pause_cleared"
	resumeOutcomeNotPaused    resumeOutcome = "not_paused"
	resumeOutcomeResumeFailed resumeOutcome = "failed"
)

func (o resumeOutcome) cleared() bool {
	return o == resumeOutcomeResumed || o == resumeOutcomeExpiredPause
}

func runResumeCommand(ctx context.Context, cc *CLIContext, now func() time.Time) error {
	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector != "" {
		return resumeSingleDriveWithNow(ctx, cc, now, cfg, driveSelector)
	}

	return resumeAllDrivesWithNow(ctx, cc, now, cfg)
}

func resumeSingleDriveWithNow(ctx context.Context, cc *CLIContext, now func() time.Time, cfg *config.Config, selector string) error {
	cid, d, err := config.MatchDrive(cfg, selector, cc.Logger)
	if err != nil {
		return fmt.Errorf("choosing the drive to resume: %w", err)
	}

	outcome, err := resumeOneDrive(cc, now, cid, &d)
	if err != nil {
		return err
	}

	if outcome == resumeOutcomeNotPaused && !cc.Flags.JSON {
		cc.Statusf("Drive %s is not paused\n", cid.String())
	}

	if outcome == resumeOutcomeResumed {
		notifyDaemon(ctx, cc)
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), resumeJSONOutput{
			Drives: []resumeJSONDrive{{Drive: cid.String(), Result: outcome}},
		})
	}

	return nil
}

// resumeAllDrivesWithNow resumes every paused drive.
//
// One drive failing must not strand the rest. "Resume all" is a bulk request,
// and returning at the first failure leaves an arbitrary subset still paused
// with nothing saying which -- the drives the loop had not reached yet look
// exactly like the ones that resumed. Every drive is attempted and the
// failures are reported together.
func resumeAllDrivesWithNow(ctx context.Context, cc *CLIContext, now func() time.Time, cfg *config.Config) error {
	if len(cfg.Drives) == 0 {
		return fmt.Errorf("no drives configured")
	}

	resumed := 0
	report := resumeJSONOutput{Drives: []resumeJSONDrive{}}

	var errs []error

	// Sorted rather than map order: this prints a line per drive, and a bulk
	// command that reports its drives in a different order every run is hard
	// to read and harder to diff. It also makes a partial failure reproducible
	// instead of depending on map iteration.
	for _, cid := range sortedDriveCanonicalIDs(cfg.Drives) {
		drive := cfg.Drives[cid]

		outcome, err := resumeOneDrive(cc, now, cid, &drive)
		if err != nil {
			errs = append(errs, err)
			report.Drives = append(report.Drives, resumeJSONDrive{
				Drive: cid.String(), Result: resumeOutcomeResumeFailed, Error: err.Error(),
			})

			continue
		}

		report.Drives = append(report.Drives, resumeJSONDrive{Drive: cid.String(), Result: outcome})
		if outcome.cleared() {
			resumed++
		}
	}

	if resumed > 0 {
		notifyDaemon(ctx, cc)
	}

	// Under --json the document reports every drive, including the ones that
	// failed; the exit status still says the run failed.
	if cc.Flags.JSON {
		if err := writeJSONDocument(cc.Output(), report); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if resumed == 0 && !cc.Flags.JSON {
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

// resumeOneDrive clears any pause on a single drive and reports what it did.
// The human confirmation is printed here; under --json the caller's document
// carries the outcome instead.
func resumeOneDrive(
	cc *CLIContext,
	now func() time.Time,
	cid driveid.CanonicalID,
	drive *config.Drive,
) (resumeOutcome, error) {
	if !drive.IsPaused(now()) {
		if drive.Paused == nil || !*drive.Paused {
			return resumeOutcomeNotPaused, nil
		}

		if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
			return resumeOutcomeResumeFailed, fmt.Errorf("clearing expired pause for %s: %w", cid.String(), err)
		}

		if !cc.Flags.JSON {
			cc.Statusf("Drive %s: expired timed pause cleared\n", cid.String())
		}

		return resumeOutcomeExpiredPause, nil
	}

	if err := clearPausedKeys(cc.CfgPath, cid); err != nil {
		return resumeOutcomeResumeFailed, fmt.Errorf("resuming %s: %w", cid.String(), err)
	}

	if !cc.Flags.JSON {
		cc.Statusf("Drive %s resumed\n", cid.String())
	}

	return resumeOutcomeResumed, nil
}

// configuredDriveLabel names a drive the way a person configured it: its display name
// when it has one, otherwise its canonical ID.
func configuredDriveLabel(cid driveid.CanonicalID, drive *config.Drive) string {
	if drive != nil && drive.DisplayName != "" {
		return drive.DisplayName
	}

	return cid.String()
}
