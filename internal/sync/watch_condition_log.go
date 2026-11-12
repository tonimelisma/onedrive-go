package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// logWatchSummary logs a periodic one-liner summary of active sync conditions
// in watch mode. The summary builder stays raw and machine-oriented; this
// boundary owns human-oriented log phrasing and churn suppression.
func (rt *watchRuntime) logWatchSummary(ctx context.Context) {
	snapshot, err := rt.deps.store.ReadDriveStatusSnapshot(ctx)
	if err != nil {
		return
	}
	summary := buildWatchConditionSummary(&snapshot)
	rt.logRemoteBlockedChanges(summary.RemoteBlocked)

	totalConditions := summary.ConditionTotal
	if totalConditions == 0 {
		if rt.loop.lastSummarySignature != "" {
			rt.deps.logger.Info("sync conditions cleared")
		}
		rt.loop.lastSummarySignature = ""
		return
	}

	signature := watchConditionSummaryFingerprint(summary)
	previous := rt.loop.lastSummarySignature
	breakdown := formatWatchConditionBreakdown(summary)
	if signature == previous {
		// Same kinds of condition, only counts moved: the 10 s maintenance
		// tick would otherwise re-warn on every count change.
		rt.deps.logger.Debug("sync conditions changed",
			slog.Int("total", totalConditions),
			slog.String("breakdown", breakdown),
		)
		return
	}

	rt.loop.lastSummarySignature = signature

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
// outage; conditionClearsAutomatically) never makes this summary a warning.
func watchConditionSummaryAddsActionable(summary watchConditionSummary, previousFingerprint string) bool {
	previous := make(map[string]struct{})
	for _, key := range strings.Split(previousFingerprint, "|") {
		previous[key] = struct{}{}
	}

	for i := range summary.Counts {
		key := summary.Counts[i].Key
		if conditionClearsAutomatically(key) {
			continue
		}
		if _, seen := previous[string(key)]; !seen {
			return true
		}
	}

	return false
}

// conditionLabelText names a condition kind in user words for console text;
// conditionLabel prepends the noun ("file"/"files"). The switch names every
// ConditionKey (the exhaustive linter holds it to that), so a new key is
// labeled on purpose; ok is false only for a key outside the set, which
// conditionLabel words by its own name.
//
//nolint:gocyclo,funlen // one case per member: the label table of the enum
func conditionLabelText(key ConditionKey) (label string, ok bool) {
	switch key {
	case ConditionInvalidFilename:
		return "with names OneDrive does not allow", true
	case ConditionPathTooLong:
		return "with paths too long for OneDrive", true
	case ConditionFileTooLarge:
		return "too large for OneDrive", true
	case ConditionCaseCollision:
		return "whose names OneDrive treats as the same name", true
	case ConditionRemoteNameTwin:
		return "that OneDrive keeps as two items under one name here", true
	case ConditionLocalReadDenied:
		return "that cannot be read locally", true
	case ConditionLocalWriteDenied:
		return "that cannot be written locally", true
	case ConditionRemoteWriteDenied:
		return "in read-only shared folders", true
	case ConditionRemoteReadDenied:
		return "in OneDrive folders you cannot read", true
	case ConditionQuotaExceeded:
		return "waiting for OneDrive storage space", true
	case ConditionDiskFull, ConditionFileTooLargeForSpace:
		return "waiting for local disk space", true
	case ConditionLocalNameTooLong:
		return "with names too long for this computer", true
	case ConditionLocalReadOnly:
		return "on a read-only local filesystem", true
	case ConditionDeletesHeld:
		return "whose deletion is held until you confirm it", true
	case ConditionLocalUnsyncable:
		return "renamed or replaced into something sync cannot use", true
	case ConditionLocalMountBoundary:
		return "on another filesystem inside the sync folder", true
	case ConditionLocalInterrupted:
		return "left set aside by an interrupted removal", true
	case ConditionRemoteFolderUnverified:
		return "too large to verify before deleting on OneDrive", true
	case ConditionLocalFileTooLarge:
		return "too large for the local filesystem", true
	case ConditionLocalBusy:
		return "busy on this computer", true
	case ConditionLocalCrossDevice:
		return "that would move across a mount point", true
	case ConditionUnsupportedOperation:
		return "that OneDrive refuses to handle", true
	case ConditionMalwareDetected:
		return "that OneDrive flagged as malware", true
	case ConditionLocalDatabaseOpen:
		return "that are databases open in an application", true
	case ConditionUploadVerification:
		return "that OneDrive stored differently than uploaded", true
	case ConditionDownloadIntegrity:
		return "that OneDrive served differently than it describes", true
	case ConditionRateLimited:
		return "waiting while OneDrive limits requests", true
	case ConditionServiceOutage:
		return "waiting for OneDrive to become available", true
	case ConditionAuthenticationRequired:
		return "waiting for sign-in", true
	case ConditionHashError:
		return "that could not be checked", true
	case ConditionUnexpectedCondition:
		return "with unexpected errors", true
	default:
		return "", false
	}
}

// conditionLabel names a condition kind in user words for console text.
func conditionLabel(key ConditionKey, count int) string {
	files := "files"
	if count == 1 {
		files = "file"
	}
	if label, ok := conditionLabelText(key); ok {
		return files + " " + label
	}

	return files + " with " + strings.ReplaceAll(string(key), "_", " ")
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
	current := make(map[ScopeKey]string, len(groups))

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
