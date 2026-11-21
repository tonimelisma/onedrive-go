package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// watchCreateAttempts is the watch loop's memory of the file creates it ran
// that did not succeed, by path, so the loop can tell when a remote poll
// began after the last attempt at a path ended (R-5.6.15). A watch replan
// observes the local tree only, so the step after planning keeps a fresh
// create intent whose file is gone: the create may have landed without a
// change batch showing it yet. Once a poll that began after the attempt ended
// has been applied, a create of that attempt that landed has been claimed by
// then, and an intent still without an item at a path the local tree no
// longer holds is dropped (SyncStore.DropGoneCreateIntentsAt).
//
// It is held in memory only, by the loop goroutine, which owns both dispatch
// and completions: the row has no column for it, and after a restart the
// start pass, which observes OneDrive before the local tree, judges the
// intents an earlier process left. A path no attempt of this process ended
// at is never judged here. The end of an attempt is the instant its
// completion is handled, which is never before the attempt really ended, so
// a poll can only be judged too early, never too late.
//
// Accepted exposure, the same as the start rule's: a create whose answer was
// lost and whose change OneDrive's change log shows only after that poll
// comes back as an empty remote file at a path the user emptied, and is
// downloaded. A fixed wait instead was rejected, since nothing measured
// grounds its length.
type watchCreateAttempts struct {
	// inFlight is the path of each dispatched create, by action ID.
	inFlight map[int64]string
	// endedAt is when the last create at a path that did not succeed ended.
	endedAt map[string]time.Time
	// dropFailing is set while the drop after a poll keeps failing, so the
	// failure is a Warn once per streak (R-6.6.30).
	dropFailing bool
}

// dispatched notes ta when it is the create of a new file: an upload with no
// remote item to overwrite.
func (a *watchCreateAttempts) dispatched(ta *syncdomain.TrackedAction) {
	if ta == nil || ta.Action.Type != syncdomain.ActionUpload || syncexec.ShouldOverwriteKnownRemoteItem(&ta.Action) {
		return
	}
	if a.inFlight == nil {
		a.inFlight = make(map[int64]string)
	}
	a.inFlight[ta.ID] = ta.Action.Path
}

// completed notes the end, at now, of the create c completes. One that
// succeeded left no intent (the hook named its item, or a zero-byte file
// cleared it), so its path is forgotten; one that did not keeps its intent
// for a poll that begins after now.
func (a *watchCreateAttempts) completed(c *syncdomain.ActionCompletion, now time.Time) {
	if c == nil {
		return
	}
	path, ok := a.inFlight[c.ActionID]
	if !ok {
		return
	}
	delete(a.inFlight, c.ActionID)
	if c.Success {
		delete(a.endedAt, path)
		return
	}
	if a.endedAt == nil {
		a.endedAt = make(map[string]time.Time)
	}
	a.endedAt[path] = now
}

// endedBefore returns the paths whose last create ended before startedAt and
// at which no create is in flight now.
func (a *watchCreateAttempts) endedBefore(startedAt time.Time) []string {
	var paths []string
	for path, endedAt := range a.endedAt {
		if endedAt.Before(startedAt) && !a.inFlightAt(path) {
			paths = append(paths, path)
		}
	}

	return paths
}

func (a *watchCreateAttempts) inFlightAt(path string) bool {
	for _, inFlight := range a.inFlight {
		if inFlight == path {
			return true
		}
	}

	return false
}

// settled forgets every one of judged except the paths in kept, whose intent
// stays for a later poll.
func (a *watchCreateAttempts) settled(judged, kept []string) {
	stays := make(map[string]bool, len(kept))
	for _, path := range kept {
		stays[path] = true
	}
	for _, path := range judged {
		if !stays[path] {
			delete(a.endedAt, path)
		}
	}
}

// dropGoneCreateIntentsAfterPoll runs, once a remote batch whose poll began
// at pollStartedAt has been applied, the drop of the fresh create intents of
// gone files at the paths whose last create attempt ended before then. It
// touches the store only when such a path exists, so a poll that returned no
// items costs nothing else. A failure is retried at the next poll: the paths
// stay in memory.
func (rt *watchRuntime) dropGoneCreateIntentsAfterPoll(ctx context.Context, pollStartedAt time.Time) {
	attempts := &rt.loop.createAttempts
	if pollStartedAt.IsZero() {
		return
	}
	paths := attempts.endedBefore(pollStartedAt)
	if len(paths) == 0 {
		return
	}

	kept, err := rt.deps.store.DropGoneCreateIntentsAt(ctx, paths, rt.localScanCoverage())
	if err != nil {
		level := slog.LevelDebug
		if !attempts.dropFailing && ctx.Err() == nil {
			level = slog.LevelWarn
			attempts.dropFailing = true
		}
		rt.deps.logger.Log(ctx, level,
			"Could not drop the intents of creates whose file is gone; will retry after the next check of OneDrive",
			slog.Int("paths", len(paths)), slog.Any("error", err))
		return
	}
	attempts.dropFailing = false
	attempts.settled(paths, kept)
}
