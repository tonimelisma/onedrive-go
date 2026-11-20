package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// logWatchSummary logs a periodic one-liner summary of active sync conditions
// in watch mode. The summary builder stays raw and machine-oriented; this
// boundary owns human-oriented log phrasing and churn suppression: each
// summary line is written only when what it says changed, never once per 10 s
// tick. A failing read of the conditions is logged like a failing housekeeping
// step instead: a warning per failure streak, repeats at Debug.
func (rt *watchRuntime) logWatchSummary(ctx context.Context) {
	snapshot, err := rt.deps.store.ReadDriveStatusSnapshot(ctx)
	if err != nil {
		// The next tick reads again. A failure warns once per failure streak
		// and logs its repeats at Debug (R-6.6.30).
		level := slog.LevelDebug
		if !rt.loop.summaryReadFailing {
			rt.loop.summaryReadFailing = true
			level = slog.LevelWarn
		}
		rt.deps.logger.Log(ctx, level, "could not read the sync conditions for the watch summary",
			slog.Any("error", err))
		return
	}
	if rt.loop.summaryReadFailing {
		rt.loop.summaryReadFailing = false
		rt.deps.logger.Info("reading the sync conditions for the watch summary works again")
	}
	summary := buildWatchConditionSummary(&snapshot)
	rt.logRemoteBlockedChanges(summary.RemoteBlocked)

	totalConditions := summary.ConditionTotal
	if totalConditions == 0 {
		if rt.loop.lastSummarySignature != "" {
			rt.deps.logger.Info("sync conditions cleared")
		}
		rt.loop.lastSummarySignature = ""
		rt.loop.lastSummaryBreakdown = ""
		return
	}

	signature := watchConditionSummaryFingerprint(summary)
	previous := rt.loop.lastSummarySignature
	breakdown := formatWatchConditionBreakdown(summary)
	if signature == previous {
		// Same kinds of condition: a count that moved is said at Debug, so
		// the summary is not re-warned on every count change, and an
		// unchanged breakdown is not said at all.
		if breakdown != rt.loop.lastSummaryBreakdown {
			rt.loop.lastSummaryBreakdown = breakdown
			rt.deps.logger.Debug("sync conditions changed",
				slog.Int("total", totalConditions),
				slog.String("breakdown", breakdown),
			)
		}
		return
	}

	rt.loop.lastSummarySignature = signature
	rt.loop.lastSummaryBreakdown = breakdown

	// The breakdown and the next step are already the message; repeating
	// them as attributes printed everything twice on the console. The count
	// stays as condition_total, a log-file-only field.
	total := slog.Int("condition_total", totalConditions)
	if watchConditionSummaryAddsActionable(summary, previous) {
		rt.deps.logger.Warn("Some files are not syncing: "+breakdown+". Run 'onedrive-go status' for details.", total)
		return
	}

	rt.deps.logger.Info("Sync conditions: "+breakdown+". Run 'onedrive-go status' for details.", total)
}

// watchConditionSummaryFingerprint is the churn-suppression key for one raw
// summary: the set of condition kinds present, not their counts, so the
// summary is reported when a kind of problem appears or clears rather than
// every time a count moves.
func watchConditionSummaryFingerprint(summary watchConditionSummary) string {
	keys := make([]string, 0, len(summary.Counts))
	for i := range summary.Counts {
		keys = append(keys, string(summary.Counts[i].Key))
	}

	return strings.Join(keys, "|")
}

// watchConditionSummaryAddsActionable reports whether the summary contains a
// condition kind absent from the previous fingerprint that the user may need
// to act on. A condition that clears automatically (throttling, a service
// outage; ConditionClearsAutomatically) never makes this summary a warning.
func watchConditionSummaryAddsActionable(summary watchConditionSummary, previousFingerprint string) bool {
	previous := make(map[string]struct{})
	for _, key := range strings.Split(previousFingerprint, "|") {
		previous[key] = struct{}{}
	}

	for i := range summary.Counts {
		key := summary.Counts[i].Key
		if syncdomain.ConditionClearsAutomatically(key) {
			continue
		}
		if _, seen := previous[string(key)]; !seen {
			return true
		}
	}

	return false
}

// conditionLabelText names a condition kind in user words for console text;
// conditionLabel prepends the noun ("file"/"files", or "folder"/"folders" for
// the conditions of a mount point, which is one folder). The switch names every
// ConditionKey (the exhaustive linter holds it to that), so a new key is
// labeled on purpose; ok is false only for a key outside the set, which
// conditionLabel words by its own name.
//
//nolint:gocyclo,funlen // one case per member: the label table of the enum
func conditionLabelText(key syncdomain.ConditionKey) (label string, ok bool) {
	switch key {
	case syncdomain.ConditionInvalidFilename:
		return "with names OneDrive does not allow", true
	case syncdomain.ConditionPathTooLong:
		return "with paths too long for OneDrive", true
	case syncdomain.ConditionFileTooLarge:
		return "too large for OneDrive", true
	case syncdomain.ConditionCaseCollision:
		return "whose names OneDrive treats as the same name", true
	case syncdomain.ConditionRemoteNameTwin:
		return "that OneDrive keeps as two items under one name here", true
	case syncdomain.ConditionLocalReadDenied:
		return "that cannot be read locally", true
	case syncdomain.ConditionLocalWriteDenied:
		return "that cannot be written locally", true
	case syncdomain.ConditionRemoteWriteDenied:
		return "in read-only shared folders", true
	case syncdomain.ConditionRemoteReadDenied:
		return "in OneDrive folders you cannot read", true
	case syncdomain.ConditionRemoteRootNotFound:
		return "in OneDrive folders that were not found", true
	case syncdomain.ConditionQuotaExceeded:
		return "waiting for OneDrive storage space", true
	case syncdomain.ConditionDiskFull, syncdomain.ConditionFileTooLargeForSpace:
		return "waiting for local disk space", true
	case syncdomain.ConditionLocalNameTooLong:
		return "with names too long for this computer", true
	case syncdomain.ConditionLocalReadOnly:
		return "on a read-only local filesystem", true
	case syncdomain.ConditionDeletesHeld:
		return "whose deletion is held until you confirm it", true
	case syncdomain.ConditionLocalUnsyncable:
		return "renamed or replaced into something sync cannot use", true
	case syncdomain.ConditionLocalMountBoundary:
		return "on another filesystem inside the sync folder", true
	case syncdomain.ConditionLocalMountMissing:
		return "whose disk is not mounted", true
	case syncdomain.ConditionLocalInterrupted:
		return "left set aside by an interrupted removal", true
	case syncdomain.ConditionRemovedFolderKept:
		return "kept in the local folder of a removed shared folder", true
	case syncdomain.ConditionRemoteFolderUnverified:
		return "too large to verify before deleting on OneDrive", true
	case syncdomain.ConditionLocalFileTooLarge:
		return "too large for the local filesystem", true
	case syncdomain.ConditionLocalBusy:
		return "busy on this computer", true
	case syncdomain.ConditionLocalCrossDevice:
		return "that would move across a mount point", true
	case syncdomain.ConditionUnsupportedOperation:
		return "that OneDrive refuses to handle", true
	case syncdomain.ConditionMalwareDetected:
		return "that OneDrive flagged as malware", true
	case syncdomain.ConditionLocalDatabaseOpen:
		return "that are databases open in an application", true
	case syncdomain.ConditionUploadVerification:
		return "that OneDrive stored differently than uploaded", true
	case syncdomain.ConditionDownloadIntegrity:
		return "that OneDrive served differently than it describes", true
	case syncdomain.ConditionRateLimited:
		return "waiting while OneDrive limits requests", true
	case syncdomain.ConditionServiceOutage:
		return "waiting for OneDrive to become available", true
	case syncdomain.ConditionAuthenticationRequired:
		return "waiting for sign-in", true
	case syncdomain.ConditionHashError:
		return "that could not be checked", true
	case syncdomain.ConditionUnexpectedCondition:
		return "with unexpected errors", true
	default:
		return "", false
	}
}

// The nouns conditionLabel counts a condition kind in. They are console
// words, not the item types of the same spelling.
const (
	conditionNounFile   = "file"
	conditionNounFolder = "folder"
)

// conditionLabel names a condition kind in user words for console text.
func conditionLabel(key syncdomain.ConditionKey, count int) string {
	noun := conditionNounFile
	if key == syncdomain.ConditionLocalMountBoundary || key == syncdomain.ConditionLocalMountMissing {
		noun = conditionNounFolder
	}
	if count != 1 {
		noun += "s"
	}
	if label, ok := conditionLabelText(key); ok {
		return noun + " " + label
	}

	return noun + " with " + strings.ReplaceAll(string(key), "_", " ")
}

func formatWatchConditionBreakdown(summary watchConditionSummary) string {
	parts := make([]string, 0, len(summary.Counts))
	for i := range summary.Counts {
		parts = append(parts, fmt.Sprintf("%d %s", summary.Counts[i].Count,
			conditionLabel(summary.Counts[i].Key, summary.Counts[i].Count)))
	}

	return strings.Join(parts, ", ")
}

func (rt *watchRuntime) logRemoteBlockedChanges(groups []watchRemoteBlockedGroup) {
	current := make(map[syncdomain.ScopeKey]string, len(groups))

	for i := range groups {
		group := groups[i]
		if !group.ScopeKey.IsPermRemoteWrite() {
			continue
		}
		boundary := group.ScopeKey.Humanize()

		signature := strings.Join(group.BlockedPaths, "\x00")
		current[group.ScopeKey] = signature

		switch previous, ok := rt.loop.lastRemoteBlocked[group.ScopeKey]; {
		case !ok:
			rt.deps.logger.Warn("Can't upload changes in "+boundary+": you have read-only access to this shared folder. "+
				"Ask the owner for edit access or move the files elsewhere",
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		case previous != signature:
			// Info: the block was already reported; only its file set moved.
			rt.deps.logger.Info("shared-folder writes still blocked",
				slog.String("boundary", boundary),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		}
	}

	for scopeKey := range rt.loop.lastRemoteBlocked {
		if _, ok := current[scopeKey]; ok {
			continue
		}
		rt.deps.logger.Info("shared-folder write block cleared",
			slog.String("boundary", scopeKey.Humanize()),
		)
	}

	rt.loop.lastRemoteBlocked = current
}
