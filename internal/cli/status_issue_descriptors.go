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

func describeStatusSummary(key syncengine.SummaryKey) statusSummaryDescriptor {
	switch key {
	case syncengine.SummaryAuthenticationRequired:
		presentation := authstate.UnauthorizedIssuePresentation()
		return newStatusSummaryDescriptor(
			"AUTHENTICATION REQUIRED",
			presentation.Reason,
			presentation.Action,
		)
	case syncengine.SummaryConflictUnresolved:
		return newStatusSummaryDescriptor(
			"UNRESOLVED CONFLICT",
			"Local and remote changes diverged and both versions were preserved.",
			"Review the conflict copies, then run 'onedrive-go resolve local|remote|both <path>'.",
		)
	case syncengine.SummaryQuotaExceeded,
		syncengine.SummaryServiceOutage,
		syncengine.SummaryRateLimited,
		syncengine.SummarySharedFolderWritesBlocked,
		syncengine.SummaryRemotePermissionDenied:
		return describeRemoteStatusSummary(string(key))
	case syncengine.SummaryLocalPermissionDenied,
		syncengine.SummaryInvalidFilename,
		syncengine.SummaryPathTooLong,
		syncengine.SummaryFileTooLarge,
		syncengine.SummaryCaseCollision:
		return describeFilesystemStatusSummary(string(key))
	case syncengine.SummaryHeldDeletes,
		syncengine.SummaryDiskFull,
		syncengine.SummaryHashError,
		syncengine.SummaryFileTooLargeForSpace:
		return describeLocalRuntimeStatusSummary(string(key))
	case syncengine.SummarySyncFailure:
		return newStatusSummaryDescriptor(
			"SYNC FAILURE",
			"An unexpected sync error occurred.",
			"Check logs for details or retry.",
		)
	default:
		return newStatusSummaryDescriptor(
			"SYNC FAILURE",
			"An unexpected sync error occurred.",
			"Check logs for details or retry.",
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
	case string(syncengine.SummaryQuotaExceeded):
		return newStatusSummaryDescriptor(
			"QUOTA EXCEEDED",
			"The OneDrive storage quota for this sync scope is full.",
			"Free up space in the owning drive, or ask the shared-folder owner to do so.",
		)
	case string(syncengine.SummaryServiceOutage):
		return newStatusSummaryDescriptor(
			"SERVICE OUTAGE",
			"OneDrive service is temporarily unavailable.",
			"Wait for the service to recover (automatic retry in progress).",
		)
	case string(syncengine.SummaryRateLimited):
		return newStatusSummaryDescriptor(
			"RATE LIMITED",
			"OneDrive asked this remote location to slow down.",
			"Wait for the retry window to expire (automatic retry in progress).",
		)
	case string(syncengine.SummarySharedFolderWritesBlocked):
		return newStatusSummaryDescriptor(
			"SHARED FOLDER WRITES BLOCKED",
			"This shared folder is read-only for your current write attempts. Downloads continue normally.",
			"Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
		)
	case string(syncengine.SummaryRemotePermissionDenied):
		return newStatusSummaryDescriptor(
			"PERMISSION DENIED",
			"You don't have write access to this location.",
			"Ask the drive owner to grant you edit permissions.",
		)
	default:
		return newStatusSummaryDescriptor(
			"SYNC FAILURE",
			"An unexpected sync error occurred.",
			"Check logs for details or retry.",
		)
	}
}

func describeFilesystemStatusSummary(key string) statusSummaryDescriptor {
	switch key {
	case string(syncengine.SummaryLocalPermissionDenied):
		return newStatusSummaryDescriptor(
			"LOCAL PERMISSION DENIED",
			"The local directory or file is not accessible.",
			"Check filesystem permissions (e.g., chmod +r).",
		)
	case string(syncengine.SummaryInvalidFilename):
		return newStatusSummaryDescriptor(
			"INVALID FILENAME",
			"The filename contains characters not allowed by OneDrive.",
			"Rename the file to remove invalid characters.",
		)
	case string(syncengine.SummaryPathTooLong):
		return newStatusSummaryDescriptor(
			"PATH TOO LONG",
			"The full path exceeds OneDrive's 400-character limit.",
			"Shorten the path by renaming files or folders.",
		)
	case string(syncengine.SummaryFileTooLarge):
		return newStatusSummaryDescriptor(
			"FILE TOO LARGE",
			"The file exceeds the maximum upload size.",
			"Reduce the file size or exclude it via skip_files.",
		)
	case string(syncengine.SummaryCaseCollision):
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
	case string(syncengine.SummaryHeldDeletes):
		return newStatusSummaryDescriptor(
			"HELD DELETES",
			"Delete safety threshold triggered - too many deletes in one batch.",
			"Run `resolve deletes` to approve, or investigate first.",
		)
	case string(syncengine.SummaryDiskFull):
		return newStatusSummaryDescriptor(
			"DISK FULL",
			"Local disk space is insufficient for downloads.",
			"Free up local disk space.",
		)
	case string(syncengine.SummaryHashError):
		return newStatusSummaryDescriptor(
			"HASH ERROR",
			"File hashing failed unexpectedly.",
			"Check file integrity and retry.",
		)
	case string(syncengine.SummaryFileTooLargeForSpace):
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

func sortStatusFailureGroups(groups []failureGroupJSON) {
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
