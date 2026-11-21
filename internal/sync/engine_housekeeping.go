package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// housekeeper runs the engine's non-critical cleanup: leftover download
// files, expired upload session files and expired trash entries, after each
// sync pass, and the watch's hourly trash purge. It owns when the trash was
// last purged and the failure streak of each step. Only the goroutine
// running the engine's RunOnce or RunWatch calls it, so its state is plain
// fields.
type housekeeper struct {
	syncTree      *synctree.Root
	sessionStore  *driveops.SessionStore
	trash         *localtrash.Trash // nil purges nothing
	contentFilter syncdomain.ContentFilterConfig
	roots         *protectedRootSet
	logger        *slog.Logger
	now           func() time.Time

	// trashPurgedAt is when the trash was last purged; the watch's
	// maintenance purges at most once per trashPurgeInterval. It holds no
	// monotonic reading, so the interval is measured in wall time.
	trashPurgedAt time.Time

	// These keep each step's failure to one Warn per failure streak
	// (R-6.6.30): a protected-root refresh, a leftover download file
	// cleanup, an expired upload session cleanup and a trash purge that
	// keeps failing. Each clears when its step succeeds again, so the next
	// streak is reported once more.
	rootRefreshFailing    ownedStreak
	partialCleanupFailing ownedStreak
	sessionCleanupFailing ownedStreak
	trashPurgeFailing     ownedStreak
}

// afterSync runs the cleanup after a sync pass. It runs synchronously on the
// run's goroutine, so a one-shot sync that runs it has cleaned up before
// RunOnce returns.
//
// The transfer-artifact cleanup walks everything but the protected roots, and
// removes every owned partial it finds with no age check. It therefore runs
// only when the protected-root list was refreshed for this pass: a stale list
// could let the parent delete a child mount's in-flight partials.
func (h *housekeeper) afterSync(ctx context.Context) {
	if h.refreshRoots(ctx) {
		h.cleanTransferArtifacts()
	}
	h.purgeExpiredTrash(true)
}

// streakReport names one repeated step's failure streak and the three lines
// it can log.
type streakReport struct {
	failing   streakFlag
	warn      string // the first failure of a streak
	repeat    string // every later failure of the streak, at Debug
	recovered string // the first success after a streak, at Info
}

// streakFlag is whether a repeating step is in a failure streak. Swap sets
// it and returns what it was. A flag that one goroutine owns is an
// ownedStreak; one that two goroutines write is an atomic.Bool.
type streakFlag interface {
	Swap(failing bool) bool
}

// ownedStreak is a streakFlag that only one goroutine reads and writes.
type ownedStreak bool

// Swap sets the flag to failing and returns what it was.
func (f *ownedStreak) Swap(failing bool) bool {
	was := bool(*f)
	*f = ownedStreak(failing)

	return was
}

// reportStreakOutcome keeps the failure of a step that repeats to one Warn
// per streak (R-6.6.30). A step can run many times in one engine's life (the
// watch purges its trash every hour, and every final drain of a removed
// shared folder releases its tree), and a condition that stays would
// otherwise warn each time: the first failure warns, repeats are Debug, and
// the first success after a streak says so at Info and ends it, so the next
// streak warns once more. attrs go on every line.
func reportStreakOutcome(logger *slog.Logger, report streakReport, err error, attrs ...any) {
	if err == nil {
		if report.failing.Swap(false) {
			logger.Info(report.recovered, attrs...)
		}

		return
	}

	failed := make([]any, 0, len(attrs)+1)
	failed = append(failed, attrs...)
	failed = append(failed, slog.Any("error", err))
	if report.failing.Swap(true) {
		logger.Debug(report.repeat, failed...)
		return
	}
	logger.Warn(report.warn, failed...)
}

// protectedRootRefreshRecoveredMessage is the line that ends a failure streak
// of the housekeeping refresh. It is logged after the refresh and before the
// cleanup reads the list.
const protectedRootRefreshRecoveredMessage = "shared-folder list refreshed again; transfer cleanup resumes"

// refreshRoots refreshes the protected-root list and
// reports whether housekeeping may walk the tree. A failure is one Warn per
// streak (R-6.6.30), Debug on repeats, and the streak ends when a refresh
// succeeds; the cleanup it guards is skipped for the pass, not run stale.
//
// A refresh that failed because ctx is done is a shutdown, not a failure: a
// watch bootstrap whose actions were canceled still reaches housekeeping, and
// a clean stop warns about nothing (R-6.6.21). It skips the cleanup at Debug
// and leaves the streak as it was, so a real failure later still warns once.
func (h *housekeeper) refreshRoots(ctx context.Context) bool {
	err := h.roots.refresh(ctx)
	if err != nil && ctx.Err() != nil {
		h.logger.Debug("skipping transfer cleanup: shutting down", slog.Any("error", err))
		return false
	}

	reportStreakOutcome(h.logger, streakReport{
		failing:   &h.rootRefreshFailing,
		warn:      "Could not refresh the shared-folder list; skipping transfer-leftover cleanup until it can be read",
		repeat:    "refresh protected roots before housekeeping failed again; skipping transfer cleanup",
		recovered: protectedRootRefreshRecoveredMessage,
	}, err)

	return err == nil
}

// cleanTransferArtifacts removes leftover download files from the sync tree
// and expired upload session files from the session store. Neither can fail
// a pass. Each of the two reports everything it could not remove in one line,
// once per streak (R-6.6.30): what cannot be removed now is still there the
// next time. What it did remove is said whenever there was something. It
// reads the protected roots through their set's lock: a parent's final-drain
// acknowledgement can rederive them from the orchestrator's goroutine while
// this runs.
func (h *housekeeper) cleanTransferArtifacts() {
	result := driveops.CleanTransferArtifacts(h.syncTree, h.sessionStore, h.logger,
		driveops.TransferArtifactCleanupOptions{
			SkipDirs:            transferArtifactCleanupSkipDirs(h.contentFilter, h.roots.current()),
			IncludedDirs:        h.contentFilter.IncludedDirs,
			IncludeJunkPartials: h.contentFilter.IgnoreJunkFiles,
		},
	)

	reportStreakOutcome(h.logger, streakReport{
		failing:   &h.partialCleanupFailing,
		warn:      "Could not remove every leftover download file; the rest stay until this is fixed",
		repeat:    "could not remove every leftover download file (still failing)",
		recovered: "leftover download files can be removed again",
	}, result.PartialsErr)
	if result.PartialsRemoved > 0 {
		h.logger.Info("cleaned stale partial files", slog.Int("count", result.PartialsRemoved))
	}

	reportStreakOutcome(h.logger, streakReport{
		failing:   &h.sessionCleanupFailing,
		warn:      "Could not remove every expired upload session file; the rest stay until this is fixed",
		repeat:    "could not remove every expired upload session file (still failing)",
		recovered: "expired upload session files can be removed again",
	}, result.SessionsErr)
	if result.SessionsRemoved > 0 {
		h.logger.Info("cleaned stale upload sessions", slog.Int("count", result.SessionsRemoved))
	}
}

// trashPurgeInterval bounds how often a long-running watch session scans the
// trash for expired entries.
const trashPurgeInterval = time.Hour

// purgeExpiredTrash removes trash entries older than the retention period.
// It is best effort: a failure leaves entries for the next attempt. A purge
// that keeps failing is reported once per streak at Warn, naming the trash
// directory, because the trash then grows without bound and nothing else
// says so; repeats are Debug and recovery ends the streak (R-6.6.30).
func (h *housekeeper) purgeExpiredTrash(force bool) {
	if h.trash == nil {
		return
	}
	now := h.now()
	if !force && !h.trashPurgedAt.IsZero() && now.Sub(h.trashPurgedAt) < trashPurgeInterval {
		return
	}
	h.trashPurgedAt = now.Round(0)

	removed, err := h.trash.Purge()
	reportStreakOutcome(h.logger, streakReport{
		failing:   &h.trashPurgeFailing,
		warn:      "Could not remove expired files from the sync trash; it will keep growing until this is fixed",
		repeat:    "could not purge expired trash entries (still failing)",
		recovered: "expired files can be removed from the sync trash again",
	}, err, slog.String("trash", h.trash.Dir()))
	if removed > 0 {
		h.logger.Info("removed expired files from the sync trash",
			slog.Int("removed", removed), slog.String("trash", h.trash.Dir()))
	}
}

func transferArtifactCleanupSkipDirs(filter syncdomain.ContentFilterConfig, protectedRoots []syncshortcut.ProtectedRoot) []string {
	skipDirs := append([]string(nil), filter.IgnoredDirs...)
	for i := range protectedRoots {
		if protectedRoots[i].Path == "" {
			continue
		}
		skipDirs = append(skipDirs, protectedRoots[i].Path)
	}
	return skipDirs
}
