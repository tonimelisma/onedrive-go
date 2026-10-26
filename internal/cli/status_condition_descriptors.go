package cli

import (
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	statusScopeDrive     = "drive"
	statusScopeDirectory = "directory"
	statusScopeService   = "service"
	statusScopeDisk      = "disk"
	statusScopeFile      = "file"
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
	case syncengine.ScopePermDirRead, syncengine.ScopePermDirWrite, syncengine.ScopeLocalReadOnly:
		return statusScopeDirectory
	case syncengine.ScopeDiskLocal:
		return statusScopeDisk
	default:
		return statusScopeFile
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
			"Sign-in required",
			presentation.Reason,
			presentation.Action,
		)
	case syncengine.ConditionQuotaExceeded,
		syncengine.ConditionServiceOutage,
		syncengine.ConditionRateLimited,
		syncengine.ConditionRemoteWriteDenied,
		syncengine.ConditionRemoteReadDenied:
		return describeRemoteStatusCondition(key)
	case syncengine.ConditionLocalReadDenied,
		syncengine.ConditionLocalWriteDenied,
		syncengine.ConditionInvalidFilename,
		syncengine.ConditionPathTooLong,
		syncengine.ConditionFileTooLarge,
		syncengine.ConditionCaseCollision:
		return describeFilesystemStatusCondition(key)
	case syncengine.ConditionDiskFull,
		syncengine.ConditionHashError,
		syncengine.ConditionFileTooLargeForSpace,
		syncengine.ConditionLocalNameTooLong,
		syncengine.ConditionLocalReadOnly,
		syncengine.ConditionLocalFileTooLarge,
		syncengine.ConditionLocalBusy,
		syncengine.ConditionLocalCrossDevice,
		syncengine.ConditionUnsupportedOperation,
		syncengine.ConditionMalwareDetected,
		syncengine.ConditionLocalDatabaseOpen:
		return describeLocalRuntimeStatusCondition(key)
	case syncengine.ConditionUploadVerification,
		syncengine.ConditionDownloadIntegrity:
		return describeTransferStatusCondition(key)
	case syncengine.ConditionUnexpectedCondition:
		return newStatusConditionDescriptor(
			"Sync issue",
			"An unexpected sync issue needs attention.",
			"Check logs for details or rerun status after the next sync pass.",
		)
	default:
		return newStatusConditionDescriptor(
			"Sync issue",
			"An unexpected sync issue needs attention.",
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

func unexpectedStatusConditionDescriptor() statusConditionDescriptor {
	return newStatusConditionDescriptor(
		"Sync issue",
		"An unexpected sync issue needs attention.",
		"Check logs for details or rerun status after the next sync pass.",
	)
}

func describeRemoteStatusCondition(key syncengine.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncengine.ConditionQuotaExceeded:
		return newStatusConditionDescriptor(
			"Storage full",
			"The OneDrive storage quota for this sync scope is full.",
			"Free up space in the owning drive, or ask the shared-folder owner to do so.",
		)
	case syncengine.ConditionServiceOutage:
		return newStatusConditionDescriptor(
			"Service outage",
			"OneDrive service is temporarily unavailable.",
			"Wait for the service to recover (automatic retry in progress).",
		)
	case syncengine.ConditionRateLimited:
		return newStatusConditionDescriptor(
			"Rate limited",
			"OneDrive asked this remote location to slow down.",
			"Wait for the retry window to expire (automatic retry in progress).",
		)
	case syncengine.ConditionRemoteWriteDenied:
		return newStatusConditionDescriptor(
			"Shared folder writes blocked",
			"This shared folder is read-only for your current write attempts. Downloads continue normally.",
			"Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
		)
	case syncengine.ConditionRemoteReadDenied:
		return newStatusConditionDescriptor(
			"Remote read blocked",
			"This remote content can no longer be downloaded with your current permissions.",
			"Restore access to the shared item, or remove the blocked content from this sync scope.",
		)
	case syncengine.ConditionAuthenticationRequired,
		syncengine.ConditionLocalReadDenied,
		syncengine.ConditionLocalWriteDenied,
		syncengine.ConditionInvalidFilename,
		syncengine.ConditionPathTooLong,
		syncengine.ConditionFileTooLarge,
		syncengine.ConditionCaseCollision,
		syncengine.ConditionDiskFull,
		syncengine.ConditionHashError,
		syncengine.ConditionFileTooLargeForSpace,
		syncengine.ConditionLocalNameTooLong,
		syncengine.ConditionLocalReadOnly,
		syncengine.ConditionLocalFileTooLarge,
		syncengine.ConditionLocalBusy,
		syncengine.ConditionLocalCrossDevice,
		syncengine.ConditionUnsupportedOperation,
		syncengine.ConditionMalwareDetected,
		syncengine.ConditionLocalDatabaseOpen,
		syncengine.ConditionUploadVerification,
		syncengine.ConditionDownloadIntegrity,
		syncengine.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
	}
}

func describeFilesystemStatusCondition(key syncengine.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncengine.ConditionLocalReadDenied:
		return newStatusConditionDescriptor(
			"Local read blocked",
			"The local file or directory can't be read.",
			"Restore read access (for example with chmod) so it can be uploaded.",
		)
	case syncengine.ConditionLocalWriteDenied:
		return newStatusConditionDescriptor(
			"Local write blocked",
			"The local destination path can no longer be created, renamed, or updated.",
			"Restore local write access so downloads and local filesystem updates can complete.",
		)
	case syncengine.ConditionInvalidFilename:
		return newStatusConditionDescriptor(
			"Invalid filename",
			"OneDrive does not allow these names: they contain a forbidden character, are reserved, or end in a space or period.",
			"Rename the files so OneDrive accepts their names.",
		)
	case syncengine.ConditionPathTooLong:
		return newStatusConditionDescriptor(
			"Path too long",
			"The full path is longer than OneDrive allows: 400 characters, and in practice a few dozen fewer.",
			"Shorten the path by renaming files or folders.",
		)
	case syncengine.ConditionFileTooLarge:
		return newStatusConditionDescriptor(
			"File too large",
			"The file exceeds the maximum upload size.",
			"Reduce the file size or move it out of the sync dir.",
		)
	case syncengine.ConditionCaseCollision:
		return newStatusConditionDescriptor(
			"Case collision",
			"Two files differ only in letter case, which OneDrive cannot distinguish.",
			"Rename one of the conflicting files.",
		)
	case syncengine.ConditionAuthenticationRequired,
		syncengine.ConditionQuotaExceeded,
		syncengine.ConditionServiceOutage,
		syncengine.ConditionRateLimited,
		syncengine.ConditionRemoteWriteDenied,
		syncengine.ConditionRemoteReadDenied,
		syncengine.ConditionDiskFull,
		syncengine.ConditionHashError,
		syncengine.ConditionFileTooLargeForSpace,
		syncengine.ConditionLocalNameTooLong,
		syncengine.ConditionLocalReadOnly,
		syncengine.ConditionLocalFileTooLarge,
		syncengine.ConditionLocalBusy,
		syncengine.ConditionLocalCrossDevice,
		syncengine.ConditionUnsupportedOperation,
		syncengine.ConditionMalwareDetected,
		syncengine.ConditionLocalDatabaseOpen,
		syncengine.ConditionUploadVerification,
		syncengine.ConditionDownloadIntegrity,
		syncengine.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
	}
}

func describeLocalRuntimeStatusCondition(key syncengine.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncengine.ConditionDiskFull:
		return newStatusConditionDescriptor(
			"Disk full",
			"The local disk is full, out of inodes or quota, or below the min_free_space reserve (1 GB unless configured).",
			"Free up local disk space (or delete files if inodes ran out), or lower min_free_space in the config file.",
		)
	case syncengine.ConditionLocalReadOnly:
		return newStatusConditionDescriptor(
			"Local folder read-only",
			"The local filesystem holding this folder is mounted read-only, so downloads and local changes cannot be written.",
			"Remount the filesystem read-write; syncing resumes automatically.",
		)
	case syncengine.ConditionLocalFileTooLarge:
		return newStatusConditionDescriptor(
			"File too large for this filesystem",
			"The file is larger than the local filesystem can store (for example 4 GB on FAT32).",
			"Move the sync folder to a filesystem that supports large files, or exclude the file.",
		)
	case syncengine.ConditionLocalBusy:
		return newStatusConditionDescriptor(
			"Local file busy",
			"The local file is in use or its network handle went stale.",
			"Wait for the program using it to finish (automatic retry in progress).",
		)
	case syncengine.ConditionUnsupportedOperation:
		return newStatusConditionDescriptor(
			"Not supported by OneDrive",
			"OneDrive refuses this operation here (for example creating a folder inside some shared folders).",
			"Make the change on the OneDrive website, or move the item somewhere OneDrive supports it.",
		)
	case syncengine.ConditionLocalDatabaseOpen:
		return newStatusConditionDescriptor(
			"Database open locally",
			"A newer copy is on OneDrive, but the local database is open in an application, and replacing it now would corrupt it.",
			"Close the application using the database; the download is retried automatically.",
		)
	case syncengine.ConditionMalwareDetected:
		return newStatusConditionDescriptor(
			"Blocked as malware",
			"OneDrive's scan flagged this file as malware and will not let it be downloaded.",
			"Check the file on the OneDrive website; delete it there if it is unwanted.",
		)
	case syncengine.ConditionLocalCrossDevice:
		return newStatusConditionDescriptor(
			"Move across a mount point",
			"A move inside the sync folder would cross into a different filesystem, which a rename cannot do.",
			"Move the item yourself, or keep a single filesystem under the sync folder.",
		)
	case syncengine.ConditionHashError:
		return newStatusConditionDescriptor(
			"Hash error",
			"File hashing failed unexpectedly.",
			"Check file integrity and retry.",
		)
	case syncengine.ConditionFileTooLargeForSpace:
		return newStatusConditionDescriptor(
			"File too large for available space",
			"The file is larger than available local disk space.",
			"Free up local disk space to fit this file.",
		)
	case syncengine.ConditionLocalNameTooLong:
		return newStatusConditionDescriptor(
			"Name too long for this computer",
			"OneDrive allows longer names than this computer's filesystem, which stores at most 255 bytes "+
				"per name (fewer characters for accented or non-Latin names).",
			"Rename the item on OneDrive to a shorter name.",
		)
	case syncengine.ConditionAuthenticationRequired,
		syncengine.ConditionQuotaExceeded,
		syncengine.ConditionServiceOutage,
		syncengine.ConditionRateLimited,
		syncengine.ConditionRemoteWriteDenied,
		syncengine.ConditionRemoteReadDenied,
		syncengine.ConditionLocalReadDenied,
		syncengine.ConditionLocalWriteDenied,
		syncengine.ConditionInvalidFilename,
		syncengine.ConditionPathTooLong,
		syncengine.ConditionFileTooLarge,
		syncengine.ConditionCaseCollision,
		syncengine.ConditionUploadVerification,
		syncengine.ConditionDownloadIntegrity,
		syncengine.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
	}
}

// describeTransferStatusCondition names the transfer-integrity conditions:
// OneDrive stored or served different content than its own facts describe.
func describeTransferStatusCondition(key syncengine.ConditionKey) statusConditionDescriptor {
	switch key { //nolint:exhaustive // only the transfer-integrity keys route here
	case syncengine.ConditionDownloadIntegrity:
		return newStatusConditionDescriptor(
			"Download doesn't match OneDrive",
			"OneDrive served bytes that disagree with its own size or checksum, or an error page instead of the file.",
			"Nothing is lost: the local file is kept and the download is retried. If it keeps failing, check the file on the OneDrive website.",
		)
	case syncengine.ConditionUploadVerification:
		return newStatusConditionDescriptor(
			"Upload not stored as sent",
			"OneDrive answered the upload with a different size or checksum than the file that was sent.",
			"Nothing is lost: the local file is kept and the upload is retried. If it keeps failing, upload the file again with put.",
		)
	default:
		return unexpectedStatusConditionDescriptor()
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
