package sync

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
//
// The transfer-artifact cleanup walks everything but the protected roots, and
// removes every owned partial it finds with no age check. It therefore runs
// only when the protected-root list was refreshed for this pass: a stale list
// could let the parent delete a child mount's in-flight partials.
func (flow *engineFlow) postSyncHousekeeping(ctx context.Context) {
	if flow.engine.refreshProtectedRootsForHousekeeping(ctx) {
		flow.engine.cleanTransferArtifacts()
	}
	flow.engine.purgeExpiredTrash(true)
}

// streakReport names one repeated step's failure streak and the three lines
// it can log.
type streakReport struct {
	failing   *atomic.Bool
	warn      string // the first failure of a streak
	repeat    string // every later failure of the streak, at Debug
	recovered string // the first success after a streak, at Info
}

// reportStreakOutcome keeps the failure of a step that repeats to one Warn
// per streak (R-6.6.30). A step can run many times in one engine's life (the
// watch purges its trash every hour, and every final drain of a removed
// shared folder releases its tree), and a condition that stays would
// otherwise warn each time: the first failure warns, repeats are Debug, and
// the first success after a streak says so at Info and ends it, so the next
// streak warns once more. attrs go on every line.
func (e *Engine) reportStreakOutcome(report streakReport, err error, attrs ...any) {
	if err == nil {
		if report.failing.Swap(false) {
			e.logger.Info(report.recovered, attrs...)
		}

		return
	}

	failed := make([]any, 0, len(attrs)+1)
	failed = append(failed, attrs...)
	failed = append(failed, slog.Any("error", err))
	if report.failing.Swap(true) {
		e.logger.Debug(report.repeat, failed...)
		return
	}
	e.logger.Warn(report.warn, failed...)
}

// refreshProtectedRootsForHousekeeping refreshes the protected-root list and
// reports whether housekeeping may walk the tree. A failure is one Warn per
// streak (R-6.6.30), Debug on repeats, and the streak ends when a refresh
// succeeds; the cleanup it guards is skipped for the pass, not run stale.
//
// A refresh that failed because ctx is done is a shutdown, not a failure: a
// watch bootstrap whose actions were canceled still reaches housekeeping, and
// a clean stop warns about nothing (R-6.6.21). It skips the cleanup at Debug
// and leaves the streak as it was, so a real failure later still warns once.
func (e *Engine) refreshProtectedRootsForHousekeeping(ctx context.Context) bool {
	err := e.refreshProtectedRootsFromStore(ctx)
	if err != nil && ctx.Err() != nil {
		e.logger.Debug("skipping transfer cleanup: shutting down", slog.Any("error", err))
		return false
	}

	e.reportStreakOutcome(streakReport{
		failing:   &e.protectedRootRefreshFailing,
		warn:      "Could not refresh the shared-folder list; skipping transfer-leftover cleanup until it can be read",
		repeat:    "refresh protected roots before housekeeping failed again; skipping transfer cleanup",
		recovered: "shared-folder list refreshed again; transfer cleanup resumes",
	}, err)

	return err == nil
}

// cleanTransferArtifacts removes leftover download files from the sync tree
// and expired upload session files from the session store. Neither can fail
// a pass. Each of the two reports everything it could not remove in one line,
// once per streak (R-6.6.30): what cannot be removed now is still there the
// next time. What it did remove is said whenever there was something.
func (e *Engine) cleanTransferArtifacts() {
	result := driveops.CleanTransferArtifacts(e.syncTree, e.sessionStore, e.logger,
		driveops.TransferArtifactCleanupOptions{
			SkipDirs:            transferArtifactCleanupSkipDirs(e.contentFilter, e.protectedRoots),
			IncludedDirs:        e.contentFilter.IncludedDirs,
			IncludeJunkPartials: e.contentFilter.IgnoreJunkFiles,
		},
	)

	e.reportStreakOutcome(streakReport{
		failing:   &e.partialCleanupFailing,
		warn:      "Could not remove every leftover download file; the rest stay until this is fixed",
		repeat:    "could not remove every leftover download file (still failing)",
		recovered: "leftover download files can be removed again",
	}, result.PartialsErr)
	if result.PartialsRemoved > 0 {
		e.logger.Info("cleaned stale partial files", slog.Int("count", result.PartialsRemoved))
	}

	e.reportStreakOutcome(streakReport{
		failing:   &e.sessionCleanupFailing,
		warn:      "Could not remove every expired upload session file; the rest stay until this is fixed",
		repeat:    "could not remove every expired upload session file (still failing)",
		recovered: "expired upload session files can be removed again",
	}, result.SessionsErr)
	if result.SessionsRemoved > 0 {
		e.logger.Info("cleaned stale upload sessions", slog.Int("count", result.SessionsRemoved))
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
func (e *Engine) purgeExpiredTrash(force bool) {
	if e == nil || e.trash == nil {
		return
	}
	now := e.nowFunc()
	if last := e.trashPurgedAt.Load(); !force && last != 0 && now.Sub(time.Unix(0, last)) < trashPurgeInterval {
		return
	}
	e.trashPurgedAt.Store(now.UnixNano())

	removed, err := e.trash.Purge()
	e.reportStreakOutcome(streakReport{
		failing:   &e.trashPurgeFailing,
		warn:      "Could not remove expired files from the sync trash; it will keep growing until this is fixed",
		repeat:    "could not purge expired trash entries (still failing)",
		recovered: "expired files can be removed from the sync trash again",
	}, err, slog.String("trash", e.trash.Dir()))
	if removed > 0 {
		e.logger.Info("removed expired files from the sync trash",
			slog.Int("removed", removed), slog.String("trash", e.trash.Dir()))
	}
}

func transferArtifactCleanupSkipDirs(filter ContentFilterConfig, protectedRoots []ProtectedRoot) []string {
	skipDirs := append([]string(nil), filter.IgnoredDirs...)
	for i := range protectedRoots {
		if protectedRoots[i].Path == "" {
			continue
		}
		skipDirs = append(skipDirs, protectedRoots[i].Path)
	}
	return skipDirs
}
