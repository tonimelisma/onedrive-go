// failure_messages.go — Human-readable failure messages for the issues display.
//
// Each issue type maps to a title, reason, and suggested action. This is the
// single source of truth for all user-facing failure text (R-2.3.8).
package sync

// IssueMessage holds the user-facing text for each issue type.
type IssueMessage struct {
	Title  string // e.g. "QUOTA EXCEEDED"
	Reason string // why it happened
	Action string // what user can do
}

// MessageForIssueType returns the human-readable message for a given issue type.
// Returns a generic message for unknown types.
func MessageForIssueType(issueType string) IssueMessage {
	switch issueType {
	case IssueQuotaExceeded:
		return IssueMessage{
			Title:  "QUOTA EXCEEDED",
			Reason: "Your OneDrive storage is full.",
			Action: "Free up space or upgrade your plan.",
		}
	case IssuePermissionDenied:
		return IssueMessage{
			Title:  "PERMISSION DENIED",
			Reason: "You don't have write access to this location.",
			Action: "Ask the drive owner to grant you edit permissions.",
		}
	case IssueLocalPermissionDenied:
		return IssueMessage{
			Title:  "LOCAL PERMISSION DENIED",
			Reason: "The local directory or file is not accessible.",
			Action: "Check filesystem permissions (e.g., chmod +r).",
		}
	case IssueInvalidFilename:
		return IssueMessage{
			Title:  "INVALID FILENAME",
			Reason: "The filename contains characters not allowed by OneDrive.",
			Action: "Rename the file to remove invalid characters.",
		}
	case IssuePathTooLong:
		return IssueMessage{
			Title:  "PATH TOO LONG",
			Reason: "The full path exceeds OneDrive's 400-character limit.",
			Action: "Shorten the path by renaming files or folders.",
		}
	case IssueFileTooLarge:
		return IssueMessage{
			Title:  "FILE TOO LARGE",
			Reason: "The file exceeds the maximum upload size.",
			Action: "Reduce the file size or exclude it via skip_files.",
		}
	case IssueBigDeleteHeld:
		return IssueMessage{
			Title:  "HELD DELETES",
			Reason: "Big-delete protection triggered — too many deletes in one batch.",
			Action: "Run `issues clear` to approve, or investigate first.",
		}
	case IssueCaseCollision:
		return IssueMessage{
			Title:  "CASE COLLISION",
			Reason: "Two files differ only in letter case, which OneDrive cannot distinguish.",
			Action: "Rename one of the conflicting files.",
		}
	case IssueDiskFull:
		return IssueMessage{
			Title:  "DISK FULL",
			Reason: "Local disk space is insufficient for downloads.",
			Action: "Free up local disk space.",
		}
	case IssueServiceOutage:
		return IssueMessage{
			Title:  "SERVICE OUTAGE",
			Reason: "OneDrive service is temporarily unavailable.",
			Action: "Wait for the service to recover (automatic retry in progress).",
		}
	case IssueHashPanic:
		return IssueMessage{
			Title:  "HASH ERROR",
			Reason: "File hashing failed unexpectedly.",
			Action: "Check file integrity and retry.",
		}
	case IssueFileTooLargeForSpace:
		return IssueMessage{
			Title:  "FILE TOO LARGE FOR SPACE",
			Reason: "The file is larger than available local disk space.",
			Action: "Free up local disk space to fit this file.",
		}
	default:
		return IssueMessage{
			Title:  "SYNC FAILURE",
			Reason: "An unexpected sync error occurred.",
			Action: "Check logs for details or retry.",
		}
	}
}

// HumanizeScopeKey translates internal scope keys to user-friendly descriptions.
// For shortcut scope keys, looks up the shortcut's local path from the provided
// shortcut list. For perm:dir:, strips the prefix. For global scopes, returns
// a plain English description (R-2.10.22).
func HumanizeScopeKey(key string, shortcuts []Shortcut) string {
	switch {
	case key == scopeKeyThrottleAccount:
		return "your OneDrive account (rate limited)"
	case key == scopeKeyService:
		return "OneDrive service"
	case key == scopeKeyQuotaOwn:
		return "your OneDrive storage"
	case len(key) > len(scopeKeyQuotaShortcut) && key[:len(scopeKeyQuotaShortcut)] == scopeKeyQuotaShortcut:
		// Try to find the shortcut's local path.
		scKey := key[len(scopeKeyQuotaShortcut):]
		for i := range shortcuts {
			if shortcuts[i].RemoteDrive+":"+shortcuts[i].RemoteItem == scKey {
				return shortcuts[i].LocalPath
			}
		}

		return scKey // fallback to internal key
	case len(key) > len(scopeKeyPermDir) && key[:len(scopeKeyPermDir)] == scopeKeyPermDir:
		return key[len(scopeKeyPermDir):]
	default:
		if key == "" {
			return ""
		}

		return key // fallback
	}
}
