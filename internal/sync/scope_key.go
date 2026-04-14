package sync

import (
	"net/http"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ScopeKeyKind discriminates the type of scope block. Value-typed (usable
// as map key), exhaustive via switch. Zero value is invalid by construction.
type ScopeKeyKind int

const (
	ScopeAuthAccount     ScopeKeyKind = iota + 1 // no Param
	ScopeThrottleAccount                         // legacy only; no Param
	ScopeThrottleTarget                          // Param = "drive:<targetDriveID>"
	ScopeService                                 // no Param
	ScopeQuotaOwn                                // no Param
	ScopePermDir                                 // Param = relative directory path
	ScopePermRemote                              // Param = local boundary path
	ScopeDiskLocal                               // no Param
)

// ScopeKey identifies a scope block. The Kind discriminator determines the
// semantics; Param carries per-instance data for parameterized scopes
// (ScopeThrottleTarget, ScopePermDir, ScopePermRemote). Comparable, so usable
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

// SKThrottleDrive returns the target-scoped throttle key for one drive.
func SKThrottleDrive(targetDriveID driveid.ID) ScopeKey {
	return ScopeKey{Kind: ScopeThrottleTarget, Param: throttleDriveParam(targetDriveID)}
}

// SKPermDir returns the scope key for a local directory permission block.
func SKPermDir(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDir, Param: dirPath}
}

// SKPermLocalRead returns the scope key for a local read-denied directory.
func SKPermLocalRead(dirPath string) ScopeKey {
	return SKPermDir(dirPath)
}

// SKPermLocalWrite returns the scope key for a local write-denied directory.
func SKPermLocalWrite(dirPath string) ScopeKey {
	return SKPermDir(dirPath)
}

// SKPermRemote returns the scope key for a remote read-only boundary.
func SKPermRemote(boundaryPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermRemote, Param: boundaryPath}
}

// SKPermRemoteWrite returns the scope key for a remote write-denied boundary.
func SKPermRemoteWrite(boundaryPath string) ScopeKey {
	return SKPermRemote(boundaryPath)
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
	WireThrottleTarget  = "throttle:target:"
	WireService         = "service"
	WireQuotaOwn        = "quota:own"
	WirePermDir         = "perm:dir:"    // prefix for parameterized key
	WirePermRemote      = "perm:remote:" // prefix for parameterized key
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
	case ScopeThrottleTarget:
		return WireThrottleTarget + sk.Param
	case ScopeService:
		return WireService
	case ScopeQuotaOwn:
		return WireQuotaOwn
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
	case strings.HasPrefix(s, WireThrottleTarget):
		return ScopeKey{Kind: ScopeThrottleTarget, Param: strings.TrimPrefix(s, WireThrottleTarget)}
	case s == WireService:
		return SKService()
	case s == WireQuotaOwn:
		return SKQuotaOwn()
	case s == WireDiskLocal:
		return SKDiskLocal()
	case strings.HasPrefix(s, WirePermDir):
		return SKPermDir(strings.TrimPrefix(s, WirePermDir))
	case strings.HasPrefix(s, WirePermRemote):
		return SKPermRemote(strings.TrimPrefix(s, WirePermRemote))
	default:
		return ScopeKey{}
	}
}

// IsGlobal returns true for scope blocks that affect ALL actions. Target-scoped
// throttles are intentionally not global; only the legacy throttle:account key,
// auth:account, and service remain process-wide.
func (sk ScopeKey) IsGlobal() bool {
	return sk.Kind == ScopeAuthAccount || sk.Kind == ScopeThrottleAccount || sk.Kind == ScopeService
}

// IsPermDir returns true for local directory permission scope blocks.
func (sk ScopeKey) IsPermDir() bool {
	return sk.Kind == ScopePermDir
}

// IsPermLocalRead returns true for local read-denied directory scopes.
func (sk ScopeKey) IsPermLocalRead() bool {
	return sk.IsPermDir()
}

// IsPermLocalWrite returns true for local write-denied directory scopes.
func (sk ScopeKey) IsPermLocalWrite() bool {
	return sk.IsPermDir()
}

// IsPermRemote returns true for remote read-only subtree scopes.
func (sk ScopeKey) IsPermRemote() bool {
	return sk.Kind == ScopePermRemote
}

// IsPermRemoteWrite returns true for remote write-denied subtree scopes.
func (sk ScopeKey) IsPermRemoteWrite() bool {
	return sk.IsPermRemote()
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

// IsThrottleTarget returns true for target-scoped throttle keys.
func (sk ScopeKey) IsThrottleTarget() bool {
	return sk.Kind == ScopeThrottleTarget
}

// IsThrottleDrive returns true when the throttle scope applies to one drive.
func (sk ScopeKey) IsThrottleDrive() bool {
	return sk.Kind == ScopeThrottleTarget && strings.HasPrefix(sk.Param, throttleDrivePrefix)
}

// ThrottleTargetKey returns the normalized target key for a target-scoped throttle.
// Panics if called on a non-target throttle key.
func (sk ScopeKey) ThrottleTargetKey() string {
	if sk.Kind != ScopeThrottleTarget {
		panic("ScopeKey.ThrottleTargetKey() called on non-target throttle key")
	}
	return sk.Param
}

// IssueType returns the issue_type constant for this scope key's kind.
// Used to populate sync_failures.issue_type consistently.
func (sk ScopeKey) IssueType() string {
	switch sk.Kind {
	case ScopeAuthAccount:
		return IssueUnauthorized
	case ScopeThrottleAccount, ScopeThrottleTarget:
		return IssueRateLimited
	case ScopeService:
		return IssueServiceOutage
	case ScopeQuotaOwn:
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
// For directory- and subtree-scoped blocks, returns the stored local path. For
// global scopes, returns a plain English description.
func (sk ScopeKey) Humanize() string {
	switch sk.Kind {
	case ScopeAuthAccount:
		return "your OneDrive account authorization"
	case ScopeThrottleAccount:
		return "your OneDrive account (rate limited)"
	case ScopeThrottleTarget:
		return "this drive (rate limited)"
	case ScopeService:
		return "OneDrive service"
	case ScopeQuotaOwn:
		return "this drive storage"
	case ScopePermDir, ScopePermRemote:
		if sk.Param == "" {
			return "/"
		}
		return sk.Param
	case ScopeDiskLocal:
		return "local disk"
	default:
		return sk.String()
	}
}

// BlocksAction returns true if this scope key blocks the given action.
// Replaces the scattered string-matching logic from blockedScope().
func (sk ScopeKey) BlocksAction(
	path string,
	throttleTargetKey string,
	actionType ActionType,
) bool {
	switch sk.Kind {
	case ScopeAuthAccount, ScopeThrottleAccount, ScopeService:
		return true // global blocks
	case ScopeThrottleTarget:
		return throttleTargetKey != "" && throttleTargetKey == sk.Param
	case ScopeDiskLocal:
		return actionType == ActionDownload
	case ScopeQuotaOwn:
		return actionType == ActionUpload
	case ScopePermDir:
		return path == sk.Param || strings.HasPrefix(path, sk.Param+"/")
	case ScopePermRemote:
		if !scopePathMatches(path, sk.Param) {
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

func scopePathMatches(path, boundary string) bool {
	if boundary == "" {
		return true
	}

	return path == boundary || strings.HasPrefix(path, boundary+"/")
}

// ScopeKeyForResult maps one worker result target and HTTP status code to a
// ScopeKey. Returns the zero-value for non-scope statuses. This is the single
// source of truth for HTTP status → scope key classification.
func ScopeKeyForResult(httpStatus int, targetDriveID driveid.ID) ScopeKey {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		if targetDriveID.IsZero() {
			return ScopeKey{}
		}
		return SKThrottleDrive(targetDriveID)
	case httpStatus == http.StatusServiceUnavailable:
		return SKService()
	case httpStatus == http.StatusInsufficientStorage:
		return SKQuotaOwn()
	case httpStatus >= http.StatusInternalServerError:
		return SKService()
	default:
		return ScopeKey{}
	}
}

const (
	throttleDrivePrefix = "drive:"
)

func throttleDriveParam(targetDriveID driveid.ID) string {
	return throttleDrivePrefix + targetDriveID.String()
}
