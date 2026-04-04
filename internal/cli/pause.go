package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
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
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runPause,
		Args:        cobra.MaximumNArgs(1),
	}
}

func runPause(cmd *cobra.Command, args []string) error {
	return newSyncControlService(mustCLIContext(cmd.Context())).runPause(args)
}

// notifyDaemon attempts to send SIGHUP to a running sync --watch daemon.
// Non-fatal: if no daemon is running, prints a note instead.
func notifyDaemon(cc *CLIContext) {
	pidPath := config.PIDFilePath()
	if pidPath == "" {
		return
	}

	if err := sendSIGHUP(pidPath); err != nil {
		cc.Statusf("Note: %v — changes take effect on next daemon start\n", err)
	} else {
		cc.Statusf("Notified running daemon to reload config\n")
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
