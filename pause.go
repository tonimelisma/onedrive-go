package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func newPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause [duration]",
		Short: "Pause syncing for a drive",
		Long: `Pause syncing for the specified drive. An optional duration argument
(e.g., "2h", "30m", "1d") schedules automatic resume after the interval.

Without a duration, the drive stays paused until manually resumed.
If a sync --watch daemon is running, it receives a SIGHUP to pick up the change.

Examples:
  onedrive-go pause --drive personal:user@example.com
  onedrive-go pause --drive personal:user@example.com 2h
  onedrive-go pause --drive personal:user@example.com 1d`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runPause,
		Args:        cobra.MaximumNArgs(1),
	}
}

func runPause(cmd *cobra.Command, args []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger

	if cc.Flags.Drive == "" {
		return fmt.Errorf("--drive is required (specify which drive to pause)")
	}

	cfgPath := resolveLoginConfigPath(cc.Flags.ConfigPath)

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, err := driveid.NewCanonicalID(cc.Flags.Drive)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", cc.Flags.Drive, err)
	}

	if _, exists := cfg.Drives[cid]; !exists {
		return fmt.Errorf("drive %q not found in config", cc.Flags.Drive)
	}

	// Set paused = true.
	if err := config.SetDriveKey(cfgPath, cid, "paused", "true"); err != nil {
		return fmt.Errorf("setting paused flag: %w", err)
	}

	// If a duration argument is provided, set paused_until.
	if len(args) > 0 {
		duration, err := parseDuration(args[0])
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", args[0], err)
		}

		until := time.Now().Add(duration).Format(time.RFC3339)
		if err := config.SetDriveKey(cfgPath, cid, "paused_until", until); err != nil {
			return fmt.Errorf("setting paused_until: %w", err)
		}

		statusf(cc.Flags.Quiet, "Drive %s paused until %s\n", cid.String(), until)
	} else {
		statusf(cc.Flags.Quiet, "Drive %s paused\n", cid.String())
	}

	// Notify running daemon, if any.
	notifyDaemon(cc.Flags.Quiet)

	return nil
}

// notifyDaemon attempts to send SIGHUP to a running sync --watch daemon.
// Non-fatal: if no daemon is running, prints a note instead.
func notifyDaemon(quiet bool) {
	pidPath := config.PIDFilePath()
	if pidPath == "" {
		return
	}

	if err := sendSIGHUP(pidPath); err != nil {
		statusf(quiet, "Note: %v — changes take effect on next daemon start\n", err)
	} else {
		statusf(quiet, "Notified running daemon to reload config\n")
	}
}

// hoursPerDay is used to convert day durations to hours.
const hoursPerDay = 24

// durationPattern matches durations like "30m", "2h", "1d", "1h30m".
var durationPattern = regexp.MustCompile(`^(\d+d)?(\d+h)?(\d+m)?(\d+s)?$`)

// parseDuration parses a human-friendly duration string. Supports Go duration
// syntax (e.g., "2h30m") plus a "d" suffix for days (converted to 24h).
func parseDuration(s string) (time.Duration, error) {
	// Try standard Go duration first.
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("duration must be positive")
		}

		return d, nil
	}

	// Try our extended format with "d" for days.
	if !durationPattern.MatchString(s) || s == "" {
		return 0, fmt.Errorf("expected format like 30m, 2h, 1d, or 1h30m")
	}

	var total time.Duration

	re := regexp.MustCompile(`(\d+)([dhms])`)
	for _, match := range re.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, fmt.Errorf("invalid number %q: %w", match[1], err)
		}

		switch match[2] {
		case "d":
			total += time.Duration(n) * hoursPerDay * time.Hour
		case "h":
			total += time.Duration(n) * time.Hour
		case "m":
			total += time.Duration(n) * time.Minute
		case "s":
			total += time.Duration(n) * time.Second
		}
	}

	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}

	return total, nil
}
