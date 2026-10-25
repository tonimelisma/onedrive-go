package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/sdnotify"
)

// watchStatusMaxRoots is how many sync roots STATUS= names before it only
// counts them; `systemctl status` shows one line.
const watchStatusMaxRoots = 3

// openSyncNotifier connects `sync --watch` to systemd's notify socket when
// the unit is Type=notify (R-6.6.32). Outside systemd it returns the no-op
// nil notifier. A socket that cannot be used is a warning, not a failure:
// syncing still works, and systemd reports the missing READY itself.
func openSyncNotifier(getenv func(string) string, logger *slog.Logger) *sdnotify.Notifier {
	notifier, err := sdnotify.Open(getenv, os.Getpid())
	if err != nil {
		logger.Warn("Couldn't connect to systemd's notify socket; the service manager won't see readiness or status",
			slog.String("error", err.Error()),
		)
		return nil
	}

	return notifier
}

// syncNotifier reports a watch's lifecycle to systemd. It is driven from the
// watch loop (WatchLifecycle) and from main's exit path; displayPath shows
// sync roots the way the console does.
type syncNotifier struct {
	notifier    *sdnotify.Notifier
	logger      *slog.Logger
	displayPath func(string) string
	// readyStatus is the last "Watching ..." line, reused after a reload
	// that kept the previous settings. Only the watch loop writes it.
	readyStatus string
}

func newSyncNotifier(notifier *sdnotify.Notifier, logger *slog.Logger, console *syncConsole) *syncNotifier {
	displayPath := func(p string) string { return p }
	if console != nil {
		displayPath = console.displayPath
	}

	return &syncNotifier{notifier: notifier, logger: logger, displayPath: displayPath}
}

// lifecycle is the multisync.OrchestratorConfig.WatchLifecycle hook.
func (s *syncNotifier) lifecycle(event multisync.WatchLifecycleEvent) {
	switch event.Phase {
	case multisync.WatchReady:
		s.readyStatus = s.watchingStatus(event)
		s.report(s.notifier.Ready(s.readyStatus))
	case multisync.WatchReloading:
		s.report(s.notifier.Reloading("Reloading the config"))
	case multisync.WatchReloaded:
		// systemd waits for READY=1 after RELOADING=1 whatever the outcome.
		if event.Err != nil {
			s.report(s.notifier.Ready(s.readyStatus + "; couldn't reload the config, kept the previous settings"))
			return
		}
		s.readyStatus = s.watchingStatus(event)
		s.report(s.notifier.Ready(s.readyStatus))
	case multisync.WatchStopping:
		s.report(s.notifier.Stopping("Stopping"))
	}
}

// failed sets the final STATUS= for a watch that ends with an error, in the
// words of the exit status, so `systemctl status` says why it stopped.
func (s *syncNotifier) failed(err error) {
	if err == nil {
		return
	}

	status := "Failed: " + firstLine(err.Error())
	switch {
	case isSignInRequired(err):
		status = "Sign-in required: run 'onedrive-go login'"
	case errors.Is(err, config.ErrInvalidConfig):
		status = "Invalid config: " + firstLine(err.Error())
	}
	s.report(s.notifier.Status(status))
}

// runWatchdog sends WATCHDOG=1 at the interval systemd asked for until ctx
// ends. The keepalive proves the process is scheduling goroutines; the watch
// loop's own health is reported through STATUS= and the exit status.
func (s *syncNotifier) runWatchdog(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := s.notifier.Watchdog(); err != nil {
				s.logger.Debug("systemd watchdog keepalive failed", slog.String("error", err.Error()))
			}
		}
	}
}

func (s *syncNotifier) report(err error) {
	if err != nil {
		s.logger.Warn("Couldn't send a status update to systemd", slog.String("error", err.Error()))
	}
}

func (s *syncNotifier) watchingStatus(event multisync.WatchLifecycleEvent) string {
	var status string
	switch roots := event.SyncRoots; {
	case len(roots) == 0:
		status = "Watching no drives"
	case len(roots) > watchStatusMaxRoots:
		status = fmt.Sprintf("Watching %d drives", len(roots))
	default:
		shown := make([]string, 0, len(roots))
		for _, root := range roots {
			shown = append(shown, s.displayPath(root))
		}
		status = "Watching " + strings.Join(shown, ", ")
	}

	signIn := make([]string, 0, len(event.Skipped))
	otherSkipped := 0
	for i := range event.Skipped {
		result := &event.Skipped[i]
		if result.Err != nil && isSignInRequired(result.Err) {
			signIn = append(signIn, startupResultLabel(result))
			continue
		}
		otherSkipped++
	}
	if len(signIn) > 0 {
		status += "; sign-in required for " + strings.Join(signIn, ", ")
	}
	switch {
	case otherSkipped == 1:
		status += "; 1 drive could not start"
	case otherSkipped > 1:
		status += fmt.Sprintf("; %d drives could not start", otherSkipped)
	}

	return status
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// startSyncWatchdog runs the keepalive loop when the unit set WatchdogSec=.
// The returned stop ends the goroutine and waits for it, so the caller owns
// its whole lifetime.
func startSyncWatchdog(ctx context.Context, s *syncNotifier) (stop func()) {
	interval := s.notifier.WatchdogInterval()
	if interval <= 0 {
		return func() {}
	}

	ticker := time.NewTicker(interval)
	watchdogCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runWatchdog(watchdogCtx, ticker.C)
	}()

	return func() {
		cancel()
		<-done
		ticker.Stop()
	}
}
