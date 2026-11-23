package cli

import (
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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

// statusStoreRoot is what the root of a state store is: a configured drive's
// own root, or a shared folder below a drive, brought in by its shortcut. It
// decides how a failure to read that root is told, because the next step
// differs: a shared folder ends with its shortcut.
type statusStoreRoot int

const (
	statusStoreRootDrive statusStoreRoot = iota
	statusStoreRootSharedFolder
)

// buildStatusConditionJSON is the CLI-owned presentation boundary for durable
// sync conditions. Sync owns raw snapshot reads and grouping; CLI owns user
// phrasing, scope-kind labels, ordering, truncation, and JSON shaping. root
// says what the snapshot's store syncs.
func buildStatusConditionJSON(
	snapshot *syncstore.DriveStatusSnapshot,
	root statusStoreRoot,
	verbose bool,
	examplesLimit int,
) []statusConditionJSON {
	if snapshot == nil {
		return nil
	}

	groups := syncstore.ProjectStoredConditionGroups(snapshot)
	if len(groups) == 0 {
		return nil
	}

	output := make([]statusConditionJSON, 0, len(groups))
	for i := range groups {
		group := groups[i]
		descriptor := describeStoredStatusCondition(&group, root)
		output = append(output, statusConditionJSON{
			ConditionKey:  string(group.ConditionKey),
			ConditionType: string(group.ConditionType),
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

func statusScopeKindFromScopeKey(scopeKey syncdomain.ScopeKey) string {
	if scopeKey.IsZero() {
		return ""
	}

	switch scopeKey.Kind {
	case syncdomain.ScopeThrottleTarget:
		return statusScopeDrive
	case syncdomain.ScopeService:
		return statusScopeService
	case syncdomain.ScopeQuotaOwn:
		return statusScopeDrive
	case syncdomain.ScopePermRemoteRead, syncdomain.ScopePermRemoteWrite:
		return statusScopeDirectory
	case syncdomain.ScopePermDirRead, syncdomain.ScopePermDirWrite, syncdomain.ScopeLocalReadOnly:
		return statusScopeDirectory
	case syncdomain.ScopeDiskLocal:
		return statusScopeDisk
	case syncdomain.ScopeDeleteSafety:
		return statusScopeDrive
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

// describeStoredStatusCondition is the descriptor of one stored condition
// group. OneDrive's refusal to read a shared folder's root is told as the
// shared folder's: gone ("not found") or no longer readable ("forbidden"),
// each with the next step that ends it. Everywhere else, a drive's own root
// included, the condition's own text stands.
func describeStoredStatusCondition(
	group *syncstore.StoredConditionGroup,
	root statusStoreRoot,
) statusConditionDescriptor {
	atSharedFolderRoot := root == statusStoreRootSharedFolder &&
		group.ScopeKey.Kind == syncdomain.ScopePermRemoteRead && group.ScopeKey.Param == ""
	if atSharedFolderRoot && group.ConditionKey == syncdomain.ConditionRemoteRootNotFound {
		return newStatusConditionDescriptor(
			"Shared folder gone",
			"This shared folder was deleted by its owner, or it is no longer shared with this account.",
			"Remove its shortcut from your OneDrive; that ends this report.",
		)
	}
	if atSharedFolderRoot && group.ConditionKey == syncdomain.ConditionRemoteReadDenied {
		return newStatusConditionDescriptor(
			"Shared folder cannot be read",
			"This account no longer has permission to read this shared folder.",
			"Ask the owner for access, or remove its shortcut from your OneDrive.",
		)
	}

	return describeStatusCondition(group.ConditionKey)
}

func describeStatusCondition(key syncdomain.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncdomain.ConditionLocalUnsyncable:
		return newStatusConditionDescriptor(
			"Synced item can no longer sync",
			"A synced file or folder was renamed or replaced into something sync cannot use (a name OneDrive "+
				"does not allow, an ignored location, a symbolic link or a special file). Its OneDrive copy is kept.",
			"Rename or move it back. If you meant to remove it, delete it on OneDrive.",
		)
	case syncdomain.ConditionRemoteFolderUnverified:
		return newStatusConditionDescriptor(
			"Folder too large to delete safely",
			"You deleted this folder here, but on OneDrive it holds more items than sync checks before a "+
				"recursive delete, so sync has not deleted it there.",
			"Delete the folder on the OneDrive website, or restore it here.",
		)
	case syncdomain.ConditionLocalInterrupted:
		return newStatusConditionDescriptor(
			"Interrupted removal",
			"Sync set this file aside while removing it, and the run stopped before it finished. "+
				"The file is your content; sync never deletes it on its own.",
			"Check the file. Rename it back if you want to keep it, or delete it.",
		)
	case syncdomain.ConditionRemovedFolderKept:
		return newStatusConditionDescriptor(
			"Kept in a removed shared folder",
			"This shared folder was removed from your OneDrive. Sync kept these items, and with them the folder: "+
				"they are not on OneDrive because sync does not sync them (a symbolic link, a special file, "+
				"something your filters exclude) or the folder can no longer be reached to upload them; or they "+
				"are folders that cannot be written to, so nothing in them could be removed; or they are synced "+
				"items that could not be moved to the trash or deleted.",
			"Move them somewhere else or delete them, or fix what keeps them (make a folder writable, free the "+
				"trash); the next sync then removes the folder.",
		)
	case syncdomain.ConditionLocalMountBoundary:
		return newStatusConditionDescriptor(
			"Another filesystem inside the sync folder",
			"This folder is a mount point for another disk or filesystem. Sync does not cross into it, so a "+
				"disk that is not mounted can never look like deleted files.",
			"Move the mount outside the sync folder, or set cross_filesystems = true to sync it.",
		)
	case syncdomain.ConditionLocalMountMissing:
		return newStatusConditionDescriptor(
			"Disk not mounted",
			"This folder was the mount point of another disk or filesystem, and that disk is not mounted now. "+
				"Its synced content is kept, and nothing is deleted on OneDrive.",
			"Mount the disk again; that clears it and loses nothing. 'onedrive-go drive rebind-root' does not "+
				"clear it: a rebind keeps every disk listed here. If the disk is gone for good, copy what you need "+
				"from this folder on OneDrive first, then remove the empty folder: sync holds the deletes of its "+
				"content on OneDrive until you run 'onedrive-go sync --confirm-deletes'.",
		)
	case syncdomain.ConditionDeletesHeld:
		return newStatusConditionDescriptor(
			"Deletes held for confirmation",
			"More was deleted than looks intended (a whole side is empty, more than 30% of synced files and at least "+
				"50, or the folder of a disk sync crossed into is gone), so sync held these deletes. Everything else keeps syncing.",
			"Check the sync folder and OneDrive. If the deletes are intended, run 'onedrive-go sync --confirm-deletes'; "+
				"while 'sync --watch' runs, it keeps running, and that command shows the held set and the command that "+
				"confirms exactly that set. Otherwise restore the missing files.",
		)
	case syncdomain.ConditionAuthenticationRequired:
		presentation := authstate.UnauthorizedIssuePresentation()
		return newStatusConditionDescriptor(
			"Sign-in required",
			presentation.Reason,
			presentation.Action,
		)
	case syncdomain.ConditionQuotaExceeded,
		syncdomain.ConditionServiceOutage,
		syncdomain.ConditionRateLimited,
		syncdomain.ConditionRemoteWriteDenied,
		syncdomain.ConditionRemoteReadDenied,
		syncdomain.ConditionRemoteRootNotFound:
		return describeRemoteStatusCondition(key)
	case syncdomain.ConditionLocalReadDenied,
		syncdomain.ConditionLocalWriteDenied,
		syncdomain.ConditionInvalidFilename,
		syncdomain.ConditionPathTooLong,
		syncdomain.ConditionFileTooLarge,
		syncdomain.ConditionCaseCollision,
		syncdomain.ConditionRemoteNameTwin:
		return describeFilesystemStatusCondition(key)
	case syncdomain.ConditionDiskFull,
		syncdomain.ConditionHashError,
		syncdomain.ConditionFileTooLargeForSpace,
		syncdomain.ConditionLocalNameTooLong,
		syncdomain.ConditionLocalReadOnly,
		syncdomain.ConditionLocalFileTooLarge,
		syncdomain.ConditionLocalBusy,
		syncdomain.ConditionLocalCrossDevice,
		syncdomain.ConditionUnsupportedOperation,
		syncdomain.ConditionMalwareDetected,
		syncdomain.ConditionLocalDatabaseOpen:
		return describeLocalRuntimeStatusCondition(key)
	case syncdomain.ConditionUploadVerification,
		syncdomain.ConditionDownloadIntegrity:
		return describeTransferStatusCondition(key)
	case syncdomain.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
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

func describeRemoteStatusCondition(key syncdomain.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncdomain.ConditionQuotaExceeded:
		return newStatusConditionDescriptor(
			"Storage full",
			"The OneDrive storage quota for this sync scope is full.",
			"Free up space in the owning drive, or ask the shared-folder owner to do so.",
		)
	case syncdomain.ConditionServiceOutage:
		return newStatusConditionDescriptor(
			"Service outage",
			"OneDrive service is temporarily unavailable.",
			"Wait for the service to recover (automatic retry in progress).",
		)
	case syncdomain.ConditionRateLimited:
		return newStatusConditionDescriptor(
			"Rate limited",
			"OneDrive asked this remote location to slow down.",
			"Wait for the retry window to expire (automatic retry in progress).",
		)
	case syncdomain.ConditionRemoteWriteDenied:
		return newStatusConditionDescriptor(
			"Shared folder writes blocked",
			"This shared folder is read-only for your current write attempts. Downloads continue normally.",
			"Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
		)
	case syncdomain.ConditionRemoteReadDenied:
		return newStatusConditionDescriptor(
			"Remote read blocked",
			"This remote content can no longer be downloaded with your current permissions.",
			"Restore access to the shared item, or remove the blocked content from this sync scope.",
		)
	case syncdomain.ConditionRemoteRootNotFound:
		// Only a root observation records it, and a shared folder's root is
		// told by describeStoredStatusCondition, so this is a drive's own root.
		return newStatusConditionDescriptor(
			"Synced folder not found",
			"OneDrive answered that the folder this drive syncs was not found.",
			"Check on the OneDrive website that the folder still exists. If it is gone, "+
				"remove the drive with 'onedrive-go drive remove --drive <drive>'.",
		)
	case syncdomain.ConditionAuthenticationRequired,
		syncdomain.ConditionLocalReadDenied,
		syncdomain.ConditionLocalWriteDenied,
		syncdomain.ConditionInvalidFilename,
		syncdomain.ConditionPathTooLong,
		syncdomain.ConditionFileTooLarge,
		syncdomain.ConditionCaseCollision,
		syncdomain.ConditionRemoteNameTwin,
		syncdomain.ConditionDiskFull,
		syncdomain.ConditionHashError,
		syncdomain.ConditionFileTooLargeForSpace,
		syncdomain.ConditionLocalNameTooLong,
		syncdomain.ConditionLocalReadOnly,
		syncdomain.ConditionLocalFileTooLarge,
		syncdomain.ConditionLocalBusy,
		syncdomain.ConditionLocalCrossDevice,
		syncdomain.ConditionUnsupportedOperation,
		syncdomain.ConditionMalwareDetected,
		syncdomain.ConditionLocalDatabaseOpen,
		syncdomain.ConditionUploadVerification,
		syncdomain.ConditionDownloadIntegrity,
		syncdomain.ConditionDeletesHeld,
		syncdomain.ConditionLocalUnsyncable,
		syncdomain.ConditionLocalMountBoundary,
		syncdomain.ConditionLocalMountMissing,
		syncdomain.ConditionLocalInterrupted,
		syncdomain.ConditionRemovedFolderKept,
		syncdomain.ConditionRemoteFolderUnverified,
		syncdomain.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
	}
}

func describeFilesystemStatusCondition(key syncdomain.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncdomain.ConditionLocalReadDenied:
		return newStatusConditionDescriptor(
			"Local read blocked",
			"The local file or directory can't be read.",
			"Restore read access (for example with chmod) so it can be uploaded.",
		)
	case syncdomain.ConditionLocalWriteDenied:
		return newStatusConditionDescriptor(
			"Local write blocked",
			"The local destination path can no longer be created, renamed, or updated.",
			"Restore local write access so downloads and local filesystem updates can complete.",
		)
	case syncdomain.ConditionInvalidFilename:
		return newStatusConditionDescriptor(
			"Invalid filename",
			"OneDrive does not allow these names: they contain a forbidden character, are reserved, or end in a space or period.",
			"Rename the files so OneDrive accepts their names.",
		)
	case syncdomain.ConditionPathTooLong:
		return newStatusConditionDescriptor(
			"Path too long",
			"The full path is longer than OneDrive allows: 400 characters, and in practice a few dozen fewer.",
			"Shorten the path by renaming files or folders.",
		)
	case syncdomain.ConditionFileTooLarge:
		return newStatusConditionDescriptor(
			"File too large",
			"The file exceeds the maximum upload size.",
			"Reduce the file size or move it out of the sync dir.",
		)
	case syncdomain.ConditionCaseCollision:
		return newStatusConditionDescriptor(
			"Case collision",
			"Two files have names OneDrive treats as the same (they differ only in letter case, in how accents are composed, or in emoji).",
			"Rename one of the conflicting files.",
		)
	case syncdomain.ConditionRemoteNameTwin:
		return newStatusConditionDescriptor(
			"Two OneDrive items, one name here",
			"OneDrive has two items that this computer would store under one name "+
				"(they differ only in how their characters are composed). Neither syncs.",
			"Rename one of them on OneDrive.",
		)
	case syncdomain.ConditionAuthenticationRequired,
		syncdomain.ConditionQuotaExceeded,
		syncdomain.ConditionServiceOutage,
		syncdomain.ConditionRateLimited,
		syncdomain.ConditionRemoteWriteDenied,
		syncdomain.ConditionRemoteReadDenied,
		syncdomain.ConditionRemoteRootNotFound,
		syncdomain.ConditionDiskFull,
		syncdomain.ConditionHashError,
		syncdomain.ConditionFileTooLargeForSpace,
		syncdomain.ConditionLocalNameTooLong,
		syncdomain.ConditionLocalReadOnly,
		syncdomain.ConditionLocalFileTooLarge,
		syncdomain.ConditionLocalBusy,
		syncdomain.ConditionLocalCrossDevice,
		syncdomain.ConditionUnsupportedOperation,
		syncdomain.ConditionMalwareDetected,
		syncdomain.ConditionLocalDatabaseOpen,
		syncdomain.ConditionUploadVerification,
		syncdomain.ConditionDownloadIntegrity,
		syncdomain.ConditionDeletesHeld,
		syncdomain.ConditionLocalUnsyncable,
		syncdomain.ConditionLocalMountBoundary,
		syncdomain.ConditionLocalMountMissing,
		syncdomain.ConditionLocalInterrupted,
		syncdomain.ConditionRemovedFolderKept,
		syncdomain.ConditionRemoteFolderUnverified,
		syncdomain.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
	}
}

func describeLocalRuntimeStatusCondition(key syncdomain.ConditionKey) statusConditionDescriptor {
	switch key {
	case syncdomain.ConditionDiskFull:
		return newStatusConditionDescriptor(
			"Disk full",
			"The local disk is full, out of inodes or quota, or below the min_free_space reserve (1 GB unless configured).",
			"Free up local disk space (or delete files if inodes ran out), or lower min_free_space in the config file.",
		)
	case syncdomain.ConditionLocalReadOnly:
		return newStatusConditionDescriptor(
			"Local folder read-only",
			"The local filesystem holding this folder is mounted read-only, so downloads and local changes cannot be written.",
			"Remount the filesystem read-write; syncing resumes automatically.",
		)
	case syncdomain.ConditionLocalFileTooLarge:
		return newStatusConditionDescriptor(
			"File too large for this filesystem",
			"The file is larger than the local filesystem can store (for example 4 GB on FAT32).",
			"Move the sync folder to a filesystem that supports large files, or exclude the file.",
		)
	case syncdomain.ConditionLocalBusy:
		return newStatusConditionDescriptor(
			"Local file busy",
			"The local file is in use, its network handle went stale, or a read of it timed out.",
			"Wait for the program using it to finish (automatic retry in progress).",
		)
	case syncdomain.ConditionUnsupportedOperation:
		return newStatusConditionDescriptor(
			"Not supported by OneDrive",
			"OneDrive refuses this operation here (for example creating a folder inside some shared folders).",
			"Make the change on the OneDrive website, or move the item somewhere OneDrive supports it.",
		)
	case syncdomain.ConditionLocalDatabaseOpen:
		return newStatusConditionDescriptor(
			"Database open locally",
			"A newer copy is on OneDrive, but the local database is open in an application, and replacing it now would corrupt it.",
			"Close the application using the database; the download is retried automatically.",
		)
	case syncdomain.ConditionMalwareDetected:
		return newStatusConditionDescriptor(
			"Blocked as malware",
			"OneDrive's scan flagged this file as malware and will not let it be downloaded.",
			"Check the file on the OneDrive website; delete it there if it is unwanted.",
		)
	case syncdomain.ConditionLocalCrossDevice:
		return newStatusConditionDescriptor(
			"Move across a mount point",
			"A move inside the sync folder would cross into a different filesystem, which a rename cannot do.",
			"Move the item yourself, or keep a single filesystem under the sync folder.",
		)
	case syncdomain.ConditionHashError:
		return newStatusConditionDescriptor(
			"Hash error",
			"File hashing failed unexpectedly.",
			"Check file integrity and retry.",
		)
	case syncdomain.ConditionFileTooLargeForSpace:
		return newStatusConditionDescriptor(
			"File too large for available space",
			"The file is larger than available local disk space.",
			"Free up local disk space to fit this file.",
		)
	case syncdomain.ConditionLocalNameTooLong:
		return newStatusConditionDescriptor(
			"Name too long for this computer",
			"OneDrive allows longer names than this computer's filesystem, which stores at most 255 bytes "+
				"per name (fewer characters for accented or non-Latin names).",
			"Rename the item on OneDrive to a shorter name.",
		)
	case syncdomain.ConditionAuthenticationRequired,
		syncdomain.ConditionQuotaExceeded,
		syncdomain.ConditionServiceOutage,
		syncdomain.ConditionRateLimited,
		syncdomain.ConditionRemoteWriteDenied,
		syncdomain.ConditionRemoteReadDenied,
		syncdomain.ConditionRemoteRootNotFound,
		syncdomain.ConditionLocalReadDenied,
		syncdomain.ConditionLocalWriteDenied,
		syncdomain.ConditionInvalidFilename,
		syncdomain.ConditionPathTooLong,
		syncdomain.ConditionFileTooLarge,
		syncdomain.ConditionCaseCollision,
		syncdomain.ConditionRemoteNameTwin,
		syncdomain.ConditionUploadVerification,
		syncdomain.ConditionDownloadIntegrity,
		syncdomain.ConditionDeletesHeld,
		syncdomain.ConditionLocalUnsyncable,
		syncdomain.ConditionLocalMountBoundary,
		syncdomain.ConditionLocalMountMissing,
		syncdomain.ConditionLocalInterrupted,
		syncdomain.ConditionRemovedFolderKept,
		syncdomain.ConditionRemoteFolderUnverified,
		syncdomain.ConditionUnexpectedCondition:
		return unexpectedStatusConditionDescriptor()
	default:
		return unexpectedStatusConditionDescriptor()
	}
}

// describeTransferStatusCondition names the transfer-integrity conditions:
// OneDrive stored or served different content than its own facts describe.
func describeTransferStatusCondition(key syncdomain.ConditionKey) statusConditionDescriptor {
	switch key { //nolint:exhaustive // only the transfer-integrity keys route here
	case syncdomain.ConditionDownloadIntegrity:
		return newStatusConditionDescriptor(
			"Download doesn't match OneDrive",
			"OneDrive served bytes that disagree with its own size or checksum, or an error page instead of the file.",
			"Nothing is lost: the local file is kept and the download is retried. If it keeps failing, check the file on the OneDrive website.",
		)
	case syncdomain.ConditionUploadVerification:
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

		leftKey := syncdomain.ConditionKey(groups[i].ConditionKey)
		rightKey := syncdomain.ConditionKey(groups[j].ConditionKey)
		if leftKey != rightKey {
			return syncdomain.ConditionKeyLess(leftKey, rightKey)
		}

		return groups[i].Scope < groups[j].Scope
	})
}
