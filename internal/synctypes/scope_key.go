package synctypes

import (
	"net/http"
	"strings"
)

// ScopeKeyKind discriminates the type of scope block. Value-typed (usable
// as map key), exhaustive via switch. Zero value is invalid by construction.
type ScopeKeyKind int

const (
	ScopeAuthAccount     ScopeKeyKind = iota + 1 // no Param
	ScopeThrottleAccount                         // no Param
	ScopeService                                 // no Param
	ScopeQuotaOwn                                // no Param
	ScopeQuotaShortcut                           // Param = "remoteDrive:remoteItem"
	ScopePermDir                                 // Param = relative directory path
	ScopePermRemote                              // Param = local boundary path
	ScopeDiskLocal                               // no Param
)

// ScopeKey identifies a scope block. The Kind discriminator determines the
// semantics; Param carries per-instance data for parameterized scopes
// (ScopeQuotaShortcut, ScopePermDir, ScopePermRemote). Comparable, so usable
// as a map key.
type ScopeKey struct {
	Kind  ScopeKeyKind
	Param string
}

// Fixed scope-key constructors for non-parameterized scopes. Use these
// instead of constructing ScopeKey{Kind: ...} literals for readability.
func SKThrottleAccount() ScopeKey { return ScopeKey{Kind: ScopeThrottleAccount} }
func SKService() ScopeKey         { return ScopeKey{Kind: ScopeService} }
func SKQuotaOwn() ScopeKey        { return ScopeKey{Kind: ScopeQuotaOwn} }
func SKDiskLocal() ScopeKey       { return ScopeKey{Kind: ScopeDiskLocal} }
func SKAuthAccount() ScopeKey     { return ScopeKey{Kind: ScopeAuthAccount} }

// SKQuotaShortcut returns the scope key for a shortcut quota block.
func SKQuotaShortcut(compositeKey string) ScopeKey {
	return ScopeKey{Kind: ScopeQuotaShortcut, Param: compositeKey}
}

// SKPermDir returns the scope key for a local directory permission block.
func SKPermDir(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDir, Param: dirPath}
}

// SKPermRemote returns the scope key for a remote read-only boundary.
func SKPermRemote(boundaryPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermRemote, Param: boundaryPath}
}

// IsZero returns true for the zero-value ScopeKey (Kind == 0).
func (sk ScopeKey) IsZero() bool {
	return sk.Kind == 0
}

// Wire-format strings for scope keys stored in SQLite scope_key columns.
// Used by String() and ParseScopeKey() — the only serialization boundary.
const (
	WireAuthAccount     = "auth:account"
	WireThrottleAccount = "throttle:account"
	WireService         = "service"
	WireQuotaOwn        = "quota:own"
	WireQuotaShortcut   = "quota:shortcut:" // prefix for parameterized key
	WirePermDir         = "perm:dir:"       // prefix for parameterized key
	WirePermRemote      = "perm:remote:"    // prefix for parameterized key
	WireDiskLocal       = "disk:local"
)

// String serializes to the wire format stored in SQLite scope_key columns.
// ParseScopeKey is the inverse.
func (sk ScopeKey) String() string {
	switch sk.Kind {
	case ScopeAuthAccount:
		return WireAuthAccount
	case ScopeThrottleAccount:
		return WireThrottleAccount
	case ScopeService:
		return WireService
	case ScopeQuotaOwn:
		return WireQuotaOwn
	case ScopeQuotaShortcut:
		return WireQuotaShortcut + sk.Param
	case ScopePermDir:
		return WirePermDir + sk.Param
	case ScopePermRemote:
		return WirePermRemote + sk.Param
	case ScopeDiskLocal:
		return WireDiskLocal
	default:
		return ""
	}
}

// ParseScopeKey deserializes a wire-format string into a ScopeKey.
// Returns the zero-value ScopeKey for unknown formats.
func ParseScopeKey(s string) ScopeKey {
	switch {
	case s == WireAuthAccount:
		return SKAuthAccount()
	case s == WireThrottleAccount:
		return SKThrottleAccount()
	case s == WireService:
		return SKService()
	case s == WireQuotaOwn:
		return SKQuotaOwn()
	case s == WireDiskLocal:
		return SKDiskLocal()
	case len(s) > len(WireQuotaShortcut) && s[:len(WireQuotaShortcut)] == WireQuotaShortcut:
		return SKQuotaShortcut(s[len(WireQuotaShortcut):])
	case len(s) > len(WirePermDir) && s[:len(WirePermDir)] == WirePermDir:
		return SKPermDir(s[len(WirePermDir):])
	case len(s) > len(WirePermRemote) && s[:len(WirePermRemote)] == WirePermRemote:
		return SKPermRemote(s[len(WirePermRemote):])
	default:
		return ScopeKey{}
	}
}

// IsGlobal returns true for scope blocks that affect ALL actions (throttle,
// service). Used by isObservationSuppressed to skip API calls during outages.
func (sk ScopeKey) IsGlobal() bool {
	return sk.Kind == ScopeAuthAccount || sk.Kind == ScopeThrottleAccount || sk.Kind == ScopeService
}

// IsPermDir returns true for local directory permission scope blocks.
func (sk ScopeKey) IsPermDir() bool {
	return sk.Kind == ScopePermDir
}

// IsPermRemote returns true for remote read-only subtree scopes.
func (sk ScopeKey) IsPermRemote() bool {
	return sk.Kind == ScopePermRemote
}

// DirPath returns the directory path for a ScopePermDir key.
// Panics if called on a non-PermDir key (defensive — caller bug).
func (sk ScopeKey) DirPath() string {
	if sk.Kind != ScopePermDir {
		panic("ScopeKey.DirPath() called on non-PermDir key")
	}
	return sk.Param
}

// RemotePath returns the local boundary path for a ScopePermRemote key.
// Panics if called on a non-PermRemote key (defensive — caller bug).
func (sk ScopeKey) RemotePath() string {
	if sk.Kind != ScopePermRemote {
		panic("ScopeKey.RemotePath() called on non-PermRemote key")
	}
	return sk.Param
}

// IssueType returns the issue_type constant for this scope key's kind.
// Used to populate sync_failures.issue_type consistently.
func (sk ScopeKey) IssueType() string {
	switch sk.Kind {
	case ScopeAuthAccount:
		return IssueUnauthorized
	case ScopeThrottleAccount:
		return IssueRateLimited
	case ScopeService:
		return IssueServiceOutage
	case ScopeQuotaOwn, ScopeQuotaShortcut:
		return IssueQuotaExceeded
	case ScopePermDir:
		return IssueLocalPermissionDenied
	case ScopePermRemote:
		return IssueSharedFolderBlocked
	case ScopeDiskLocal:
		return IssueDiskFull
	default:
		return ""
	}
}

// Humanize translates a scope key to a user-friendly description (R-2.10.22).
// For shortcut scopes, looks up the shortcut's local path from the provided
// list. For perm:dir, returns the directory path. For global scopes, returns
// a plain English description.
func (sk ScopeKey) Humanize(shortcuts []Shortcut) string {
	switch sk.Kind {
	case ScopeAuthAccount:
		return "your OneDrive account authorization"
	case ScopeThrottleAccount:
		return "your OneDrive account (rate limited)"
	case ScopeService:
		return "OneDrive service"
	case ScopeQuotaOwn:
		return "your OneDrive storage"
	case ScopeQuotaShortcut:
		for i := range shortcuts {
			if shortcuts[i].RemoteDrive+":"+shortcuts[i].RemoteItem == sk.Param {
				return shortcuts[i].LocalPath
			}
		}
		return sk.Param // fallback to composite key
	case ScopePermDir, ScopePermRemote:
		return sk.Param
	case ScopeDiskLocal:
		return "local disk"
	default:
		return sk.String()
	}
}

// BlocksAction returns true if this scope key blocks the given action.
// Replaces the scattered string-matching logic from blockedScope().
func (sk ScopeKey) BlocksAction(path, shortcutKey string, actionType ActionType, targetsOwnDrive bool) bool {
	switch sk.Kind {
	case ScopeAuthAccount, ScopeThrottleAccount, ScopeService:
		return true // global blocks
	case ScopeDiskLocal:
		return actionType == ActionDownload
	case ScopeQuotaOwn:
		return targetsOwnDrive && actionType == ActionUpload
	case ScopeQuotaShortcut:
		return shortcutKey == sk.Param && actionType == ActionUpload
	case ScopePermDir:
		return path == sk.Param || strings.HasPrefix(path, sk.Param+"/")
	case ScopePermRemote:
		if path != sk.Param && !strings.HasPrefix(path, sk.Param+"/") {
			return false
		}
		switch actionType {
		case ActionUpload, ActionRemoteDelete, ActionRemoteMove, ActionFolderCreate:
			return true
		case ActionDownload, ActionLocalDelete, ActionLocalMove, ActionConflict, ActionUpdateSynced, ActionCleanup:
			return false
		default:
			return false
		}
	default:
		return false
	}
}

// ScopeKeyForStatus maps an HTTP status code and shortcut context to a
// ScopeKey. Returns the zero-value for non-scope statuses. Single source
// of truth for HTTP status → scope key classification.
func ScopeKeyForStatus(httpStatus int, shortcutKey string) ScopeKey {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		return SKThrottleAccount()
	case httpStatus == http.StatusServiceUnavailable:
		return SKService()
	case httpStatus == http.StatusInsufficientStorage:
		if shortcutKey != "" {
			return SKQuotaShortcut(shortcutKey)
		}
		return SKQuotaOwn()
	case httpStatus >= http.StatusInternalServerError:
		return SKService()
	default:
		return ScopeKey{}
	}
}
