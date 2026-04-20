package cli

import (
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type statusSummaryDescriptor struct {
	Title  string
	Reason string
	Action string
}

func describeStatusCondition(key syncengine.ConditionKey) statusSummaryDescriptor {
	switch key {
	case syncengine.ConditionAuthenticationRequired:
		presentation := authstate.UnauthorizedIssuePresentation()
		return newStatusSummaryDescriptor(
			"AUTHENTICATION REQUIRED",
			presentation.Reason,
			presentation.Action,
		)
	case syncengine.ConditionQuotaExceeded,
		syncengine.ConditionServiceOutage,
		syncengine.ConditionRateLimited,
		syncengine.ConditionRemoteWriteDenied,
		syncengine.ConditionRemoteReadDenied:
		return describeRemoteStatusSummary(string(key))
	case syncengine.ConditionLocalReadDenied,
		syncengine.ConditionLocalWriteDenied,
		syncengine.ConditionInvalidFilename,
		syncengine.ConditionPathTooLong,
		syncengine.ConditionFileTooLarge,
		syncengine.ConditionCaseCollision:
		return describeFilesystemStatusSummary(string(key))
	case syncengine.ConditionDiskFull,
		syncengine.ConditionHashError,
		syncengine.ConditionFileTooLargeForSpace:
		return describeLocalRuntimeStatusSummary(string(key))
	case syncengine.ConditionUnexpectedCondition:
		return newStatusSummaryDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	default:
		return newStatusSummaryDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	}
}

func newStatusSummaryDescriptor(title, reason, action string) statusSummaryDescriptor {
	return statusSummaryDescriptor{
		Title:  title,
		Reason: reason,
		Action: action,
	}
}

func describeRemoteStatusSummary(key string) statusSummaryDescriptor {
	switch key {
	case string(syncengine.ConditionQuotaExceeded):
		return newStatusSummaryDescriptor(
			"QUOTA EXCEEDED",
			"The OneDrive storage quota for this sync scope is full.",
			"Free up space in the owning drive, or ask the shared-folder owner to do so.",
		)
	case string(syncengine.ConditionServiceOutage):
		return newStatusSummaryDescriptor(
			"SERVICE OUTAGE",
			"OneDrive service is temporarily unavailable.",
			"Wait for the service to recover (automatic retry in progress).",
		)
	case string(syncengine.ConditionRateLimited):
		return newStatusSummaryDescriptor(
			"RATE LIMITED",
			"OneDrive asked this remote location to slow down.",
			"Wait for the retry window to expire (automatic retry in progress).",
		)
	case string(syncengine.ConditionRemoteWriteDenied):
		return newStatusSummaryDescriptor(
			"SHARED FOLDER WRITES BLOCKED",
			"This shared folder is read-only for your current write attempts. Downloads continue normally.",
			"Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
		)
	case string(syncengine.ConditionRemoteReadDenied):
		return newStatusSummaryDescriptor(
			"REMOTE READ BLOCKED",
			"This remote content can no longer be downloaded with your current permissions.",
			"Restore access to the shared item, or remove the blocked content from this sync scope.",
		)
	default:
		return newStatusSummaryDescriptor(
			"SYNC CONDITION",
			"An unexpected sync condition needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	}
}

func describeFilesystemStatusSummary(key string) statusSummaryDescriptor {
	switch key {
	case string(syncengine.ConditionLocalReadDenied):
		return newStatusSummaryDescriptor(
			"LOCAL READ BLOCKED",
			"The local source file or directory can no longer be read.",
			"Restore local read access so uploads and conflict recovery can read the source content.",
		)
	case string(syncengine.ConditionLocalWriteDenied):
		return newStatusSummaryDescriptor(
			"LOCAL WRITE BLOCKED",
			"The local destination path can no longer be created, renamed, or updated.",
			"Restore local write access so downloads and local filesystem updates can complete.",
		)
	case string(syncengine.ConditionInvalidFilename):
		return newStatusSummaryDescriptor(
			"INVALID FILENAME",
			"The filename contains characters not allowed by OneDrive.",
			"Rename the file to remove invalid characters.",
		)
	case string(syncengine.ConditionPathTooLong):
		return newStatusSummaryDescriptor(
			"PATH TOO LONG",
			"The full path exceeds OneDrive's 400-character limit.",
			"Shorten the path by renaming files or folders.",
		)
	case string(syncengine.ConditionFileTooLarge):
		return newStatusSummaryDescriptor(
			"FILE TOO LARGE",
			"The file exceeds the maximum upload size.",
			"Reduce the file size or move it out of the sync dir.",
		)
	case string(syncengine.ConditionCaseCollision):
		return newStatusSummaryDescriptor(
			"CASE COLLISION",
			"Two files differ only in letter case, which OneDrive cannot distinguish.",
			"Rename one of the conflicting files.",
		)
	default:
		return newStatusSummaryDescriptor(
			"SYNC FAILURE",
			"An unexpected sync error occurred.",
			"Check logs for details or retry.",
		)
	}
}

func describeLocalRuntimeStatusSummary(key string) statusSummaryDescriptor {
	switch key {
	case string(syncengine.ConditionDiskFull):
		return newStatusSummaryDescriptor(
			"DISK FULL",
			"Local disk space is insufficient for downloads.",
			"Free up local disk space.",
		)
	case string(syncengine.ConditionHashError):
		return newStatusSummaryDescriptor(
			"HASH ERROR",
			"File hashing failed unexpectedly.",
			"Check file integrity and retry.",
		)
	case string(syncengine.ConditionFileTooLargeForSpace):
		return newStatusSummaryDescriptor(
			"FILE TOO LARGE FOR SPACE",
			"The file is larger than available local disk space.",
			"Free up local disk space to fit this file.",
		)
	default:
		return newStatusSummaryDescriptor(
			"SYNC FAILURE",
			"An unexpected sync error occurred.",
			"Check logs for details or retry.",
		)
	}
}

func sortStatusConditions(groups []statusConditionJSON) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		if groups[i].Title != groups[j].Title {
			return groups[i].Title < groups[j].Title
		}
		return groups[i].Scope < groups[j].Scope
	})
}
