package cli

import (
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	statusScopeAccount   = "account"
	statusScopeDrive     = "drive"
	statusScopeDirectory = "directory"
	statusScopeService   = "service"
	statusScopeDisk      = "disk"
)

type statusConditionDescriptor struct {
	Title  string
	Reason string
	Action string
}

// buildStatusConditionJSON is the CLI-owned presentation boundary for durable
// sync conditions. Sync owns raw snapshot reads and grouping; CLI owns user
// phrasing, scope-kind labels, ordering, truncation, and JSON shaping.
func buildStatusConditionJSON(
	snapshot *syncengine.DriveStatusSnapshot,
	verbose bool,
	examplesLimit int,
) []statusConditionJSON {
	if snapshot == nil {
		return nil
	}

	groups := syncengine.ProjectStoredConditionGroups(snapshot)
	if len(groups) == 0 {
		return nil
	}

	output := make([]statusConditionJSON, 0, len(groups))
	for i := range groups {
		group := groups[i]
		descriptor := describeStatusCondition(group.ConditionKey)
		output = append(output, statusConditionJSON{
			ConditionKey:  string(group.ConditionKey),
			ConditionType: group.ConditionType,
			Title:         descriptor.Title,
			Reason:        descriptor.Reason,
			Action:        descriptor.Action,
			ScopeKind:     statusScopeKindFromScopeKey(group.ScopeKey),
			Scope:         group.ScopeKey.Humanize(),
			Count:         group.Count,
			Paths:         sampleStrings(group.Paths, verbose, examplesLimit),
		})
	}

	sortStatusConditions(output)

	return output
}

func conditionTotal(groups []statusConditionJSON) int {
	total := 0
	for i := range groups {
		total += groups[i].Count
	}

	return total
}

func statusScopeKindFromScopeKey(scopeKey syncengine.ScopeKey) string {
	if scopeKey.IsZero() {
		return ""
	}

	switch scopeKey.Kind {
	case syncengine.ScopeThrottleTarget:
		return statusScopeDrive
	case syncengine.ScopeService:
		return statusScopeService
	case syncengine.ScopeQuotaOwn:
		return statusScopeDrive
	case syncengine.ScopePermRemoteRead, syncengine.ScopePermRemoteWrite:
		return statusScopeDirectory
	case syncengine.ScopePermDirRead, syncengine.ScopePermDirWrite:
		return statusScopeDirectory
	case syncengine.ScopeDiskLocal:
		return statusScopeDisk
	default:
		return "file"
	}
}

func sampleStrings(values []string, verbose bool, examplesLimit int) []string {
	if len(values) == 0 {
		return nil
	}
	if verbose || len(values) <= examplesLimit {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}

	out := make([]string, examplesLimit)
	copy(out, values[:examplesLimit])
	return out
}

func describeStatusCondition(key syncengine.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncengine.ConditionAuthenticationRequired:
		presentation := authstate.UnauthorizedIssuePresentation()
		return newStatusConditionDescriptor(
			"AUTHENTICATION REQUIRED",
			presentation.Reason,
			presentation.Action,
		)
	case syncengine.ConditionQuotaExceeded,
		syncengine.ConditionServiceOutage,
		syncengine.ConditionRateLimited,
		syncengine.ConditionRemoteWriteDenied,
		syncengine.ConditionRemoteReadDenied:
		return describeRemoteStatusCondition(string(key))
	case syncengine.ConditionLocalReadDenied,
		syncengine.ConditionLocalWriteDenied,
		syncengine.ConditionInvalidFilename,
		syncengine.ConditionPathTooLong,
		syncengine.ConditionFileTooLarge,
		syncengine.ConditionCaseCollision:
		return describeFilesystemStatusCondition(string(key))
	case syncengine.ConditionDiskFull,
		syncengine.ConditionHashError,
		syncengine.ConditionFileTooLargeForSpace:
		return describeLocalRuntimeStatusCondition(string(key))
	case syncengine.ConditionUnexpectedCondition:
		return newStatusConditionDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	default:
		return newStatusConditionDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	}
}

func newStatusConditionDescriptor(title, reason, action string) statusConditionDescriptor {
	return statusConditionDescriptor{
		Title:  title,
		Reason: reason,
		Action: action,
	}
}

func describeRemoteStatusCondition(key string) statusConditionDescriptor {
	switch key {
	case string(syncengine.ConditionQuotaExceeded):
		return newStatusConditionDescriptor(
			"QUOTA EXCEEDED",
			"The OneDrive storage quota for this sync scope is full.",
			"Free up space in the owning drive, or ask the shared-folder owner to do so.",
		)
	case string(syncengine.ConditionServiceOutage):
		return newStatusConditionDescriptor(
			"SERVICE OUTAGE",
			"OneDrive service is temporarily unavailable.",
			"Wait for the service to recover (automatic retry in progress).",
		)
	case string(syncengine.ConditionRateLimited):
		return newStatusConditionDescriptor(
			"RATE LIMITED",
			"OneDrive asked this remote location to slow down.",
			"Wait for the retry window to expire (automatic retry in progress).",
		)
	case string(syncengine.ConditionRemoteWriteDenied):
		return newStatusConditionDescriptor(
			"SHARED FOLDER WRITES BLOCKED",
			"This shared folder is read-only for your current write attempts. Downloads continue normally.",
			"Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
		)
	case string(syncengine.ConditionRemoteReadDenied):
		return newStatusConditionDescriptor(
			"REMOTE READ BLOCKED",
			"This remote content can no longer be downloaded with your current permissions.",
			"Restore access to the shared item, or remove the blocked content from this sync scope.",
		)
	default:
		return newStatusConditionDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	}
}

func describeFilesystemStatusCondition(key string) statusConditionDescriptor {
	switch key {
	case string(syncengine.ConditionLocalReadDenied):
		return newStatusConditionDescriptor(
			"LOCAL READ BLOCKED",
			"The local source file or directory can no longer be read.",
			"Restore local read access so uploads and conflict recovery can read the source content.",
		)
	case string(syncengine.ConditionLocalWriteDenied):
		return newStatusConditionDescriptor(
			"LOCAL WRITE BLOCKED",
			"The local destination path can no longer be created, renamed, or updated.",
			"Restore local write access so downloads and local filesystem updates can complete.",
		)
	case string(syncengine.ConditionInvalidFilename):
		return newStatusConditionDescriptor(
			"INVALID FILENAME",
			"The filename contains characters not allowed by OneDrive.",
			"Rename the file to remove invalid characters.",
		)
	case string(syncengine.ConditionPathTooLong):
		return newStatusConditionDescriptor(
			"PATH TOO LONG",
			"The full path exceeds OneDrive's 400-character limit.",
			"Shorten the path by renaming files or folders.",
		)
	case string(syncengine.ConditionFileTooLarge):
		return newStatusConditionDescriptor(
			"FILE TOO LARGE",
			"The file exceeds the maximum upload size.",
			"Reduce the file size or move it out of the sync dir.",
		)
	case string(syncengine.ConditionCaseCollision):
		return newStatusConditionDescriptor(
			"CASE COLLISION",
			"Two files differ only in letter case, which OneDrive cannot distinguish.",
			"Rename one of the conflicting files.",
		)
	default:
		return newStatusConditionDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	}
}

func describeLocalRuntimeStatusCondition(key string) statusConditionDescriptor {
	switch key {
	case string(syncengine.ConditionDiskFull):
		return newStatusConditionDescriptor(
			"DISK FULL",
			"Local disk space is insufficient for downloads.",
			"Free up local disk space.",
		)
	case string(syncengine.ConditionHashError):
		return newStatusConditionDescriptor(
			"HASH ERROR",
			"File hashing failed unexpectedly.",
			"Check file integrity and retry.",
		)
	case string(syncengine.ConditionFileTooLargeForSpace):
		return newStatusConditionDescriptor(
			"FILE TOO LARGE FOR SPACE",
			"The file is larger than available local disk space.",
			"Free up local disk space to fit this file.",
		)
	default:
		return newStatusConditionDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	}
}

func sortStatusConditions(groups []statusConditionJSON) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}

		leftKey := syncengine.ConditionKey(groups[i].ConditionKey)
		rightKey := syncengine.ConditionKey(groups[j].ConditionKey)
		if leftKey != rightKey {
			return syncengine.ConditionKeyLess(leftKey, rightKey)
		}

		return groups[i].Scope < groups[j].Scope
	})
}
