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
	ScopeThrottleTarget                          // Param = "drive:<targetDriveID>" or "shared:<remoteDrive>:<remoteItem>"
	ScopeService                                 // no Param
	ScopeQuotaOwn                                // no Param
	ScopeQuotaShortcut                           // Param = "remoteDrive:remoteItem"
	ScopePermLocalRead                           // Param = relative directory path
	ScopePermLocalWrite                          // Param = relative directory path
	ScopePermRemoteWrite                         // Param = local boundary path
	ScopeDiskLocal                               // no Param
)

// ScopeKey identifies a scope block. The Kind discriminator determines the
// semantics; Param carries per-instance data for parameterized scopes
// (ScopeQuotaShortcut, ScopePermLocalRead, ScopePermLocalWrite,
// ScopePermRemoteWrite). Comparable, so usable
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

// SKThrottleShared returns the target-scoped throttle key for one shared root/item.
func SKThrottleShared(remoteDriveID, remoteItemID string) ScopeKey {
	return ScopeKey{Kind: ScopeThrottleTarget, Param: throttleSharedParam(remoteDriveID, remoteItemID)}
}

// SKQuotaShortcut returns the scope key for a shortcut quota block.
func SKQuotaShortcut(compositeKey string) ScopeKey {
	return ScopeKey{Kind: ScopeQuotaShortcut, Param: compositeKey}
}

// SKPermLocalRead returns the scope key for a local read-denied directory scope.
func SKPermLocalRead(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermLocalRead, Param: dirPath}
}

// SKPermLocalWrite returns the scope key for a local write-denied directory scope.
func SKPermLocalWrite(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermLocalWrite, Param: dirPath}
}

// SKPermRemoteWrite returns the scope key for a remote write-denied boundary.
func SKPermRemoteWrite(boundaryPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermRemoteWrite, Param: boundaryPath}
}

// IsZero returns true for the zero-value ScopeKey (Kind == 0).
func (sk ScopeKey) IsZero() bool {
	return sk.Kind == 0
}

// Wire-format strings for scope keys stored in SQLite scope_key columns.
// Used by String() and ParseScopeKey() — the only serialization boundary.
const (
	WireAuthAccount      = "auth:account"
	WireThrottleAccount  = "throttle:account"
	WireThrottleTarget   = "throttle:target:"
	WireService          = "service"
	WireQuotaOwn         = "quota:own"
	WireQuotaShortcut    = "quota:shortcut:"    // prefix for parameterized key
	WirePermLocalRead    = "perm:local-read:"   // prefix for parameterized key
	WirePermLocalWrite   = "perm:local-write:"  // prefix for parameterized key
	WirePermRemoteWrite  = "perm:remote-write:" // prefix for parameterized key
	WireLegacyPermDir    = "perm:dir:"          // legacy startup cleanup only
	WireLegacyPermRemote = "perm:remote:"       // legacy startup cleanup only
	WireDiskLocal        = "disk:local"
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
	case ScopeQuotaShortcut:
		return WireQuotaShortcut + sk.Param
	case ScopePermLocalRead:
		return WirePermLocalRead + sk.Param
	case ScopePermLocalWrite:
		return WirePermLocalWrite + sk.Param
	case ScopePermRemoteWrite:
		return WirePermRemoteWrite + sk.Param
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
	case strings.HasPrefix(s, WireQuotaShortcut):
		return SKQuotaShortcut(strings.TrimPrefix(s, WireQuotaShortcut))
	case strings.HasPrefix(s, WirePermLocalRead):
		return SKPermLocalRead(strings.TrimPrefix(s, WirePermLocalRead))
	case strings.HasPrefix(s, WirePermLocalWrite):
		return SKPermLocalWrite(strings.TrimPrefix(s, WirePermLocalWrite))
	case strings.HasPrefix(s, WirePermRemoteWrite):
		return SKPermRemoteWrite(strings.TrimPrefix(s, WirePermRemoteWrite))
	case strings.HasPrefix(s, WireLegacyPermDir):
		return SKPermLocalWrite(strings.TrimPrefix(s, WireLegacyPermDir))
	case strings.HasPrefix(s, WireLegacyPermRemote):
		return SKPermRemoteWrite(strings.TrimPrefix(s, WireLegacyPermRemote))
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

// IsPermLocalRead returns true for local read-denied directory scope blocks.
func (sk ScopeKey) IsPermLocalRead() bool {
	return sk.Kind == ScopePermLocalRead
}

// IsPermLocalWrite returns true for local write-denied directory scope blocks.
func (sk ScopeKey) IsPermLocalWrite() bool {
	return sk.Kind == ScopePermLocalWrite
}

// IsPermRemoteWrite returns true for remote write-denied subtree scopes.
func (sk ScopeKey) IsPermRemoteWrite() bool {
	return sk.Kind == ScopePermRemoteWrite
}

// DirPath returns the directory path for a local permission scope key.
// Panics if called on a non-local-permission key (defensive — caller bug).
func (sk ScopeKey) DirPath() string {
	if sk.Kind != ScopePermLocalRead && sk.Kind != ScopePermLocalWrite {
		panic("ScopeKey.DirPath() called on non-local-permission key")
	}
	return sk.Param
}

// RemotePath returns the local boundary path for a ScopePermRemoteWrite key.
// Panics if called on a non-remote-write key (defensive — caller bug).
func (sk ScopeKey) RemotePath() string {
	if sk.Kind != ScopePermRemoteWrite {
		panic("ScopeKey.RemotePath() called on non-remote-write key")
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

// IsThrottleShared returns true when the throttle scope applies to one shared target.
func (sk ScopeKey) IsThrottleShared() bool {
	return sk.Kind == ScopeThrottleTarget && strings.HasPrefix(sk.Param, throttleSharedPrefix)
}

// ThrottleTargetKey returns the normalized target key for a target-scoped throttle.
// Panics if called on a non-target throttle key.
func (sk ScopeKey) ThrottleTargetKey() string {
	if sk.Kind != ScopeThrottleTarget {
		panic("ScopeKey.ThrottleTargetKey() called on non-target throttle key")
	}
	return sk.Param
}

// ThrottleShortcutKey returns the "remoteDrive:remoteItem" suffix for a shared
// target throttle key. Panics if called on a non-shared throttle key.
func (sk ScopeKey) ThrottleShortcutKey() string {
	if !sk.IsThrottleShared() {
		panic("ScopeKey.ThrottleShortcutKey() called on non-shared throttle key")
	}
	return strings.TrimPrefix(sk.Param, throttleSharedPrefix)
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
	case ScopeQuotaOwn, ScopeQuotaShortcut:
		return IssueQuotaExceeded
	case ScopePermLocalRead:
		return IssueLocalReadDenied
	case ScopePermLocalWrite:
		return IssueLocalWriteDenied
	case ScopePermRemoteWrite:
		return IssueRemoteWriteDenied
	case ScopeDiskLocal:
		return IssueDiskFull
	default:
		return ""
	}
}

// Humanize translates a scope key to a user-friendly description (R-2.10.22).
// For shortcut scopes, looks up the shortcut's local path from the provided
// list. For permission scopes, returns the directory/boundary path. For global
// scopes, returns a plain English description.
func (sk ScopeKey) Humanize(shortcuts []Shortcut) string {
	switch sk.Kind {
	case ScopeAuthAccount:
		return "your OneDrive account authorization"
	case ScopeThrottleAccount:
		return "your OneDrive account (rate limited)"
	case ScopeThrottleTarget:
		if sk.IsThrottleShared() {
			shortcutKey := sk.ThrottleShortcutKey()
			for i := range shortcuts {
				if shortcuts[i].RemoteDrive+":"+shortcuts[i].RemoteItem == shortcutKey {
					return shortcuts[i].LocalPath + " (rate limited)"
				}
			}
			return shortcutKey
		}
		return "this drive (rate limited)"
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
	case ScopePermLocalRead, ScopePermLocalWrite, ScopePermRemoteWrite:
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

// BlocksTrackedAction returns true if this scope key blocks the given tracked
// action based on the concrete capabilities the action instance requires.
func (sk ScopeKey) BlocksTrackedAction(ta *TrackedAction) bool {
	if ta == nil {
		return false
	}

	action := &ta.Action

	switch sk.Kind {
	case ScopeAuthAccount, ScopeThrottleAccount, ScopeService:
		return true // global blocks
	case ScopeThrottleTarget:
		throttleTargetKey := action.ThrottleTargetKey()
		return throttleTargetKey != "" && throttleTargetKey == sk.Param
	case ScopeDiskLocal:
		return action.Type == ActionDownload
	case ScopeQuotaOwn:
		return action.TargetsOwnDrive() && action.Type == ActionUpload
	case ScopeQuotaShortcut:
		return action.ShortcutKey() == sk.Param && action.Type == ActionUpload
	case ScopePermLocalRead:
		return scopeBlocksCapability(sk.Param, action, PermissionCapabilityLocalRead)
	case ScopePermLocalWrite:
		return scopeBlocksCapability(sk.Param, action, PermissionCapabilityLocalWrite)
	case ScopePermRemoteWrite:
		return scopeBlocksCapability(sk.Param, action, PermissionCapabilityRemoteWrite)
	default:
		return false
	}
}

func scopeBlocksCapability(boundary string, action *Action, capability PermissionCapability) bool {
	if action == nil {
		return false
	}

	required := action.PermissionCapabilities()
	matchedCapability := false
	for _, cap := range required {
		if cap == capability {
			matchedCapability = true
			break
		}
	}
	if !matchedCapability {
		return false
	}

	for _, path := range action.ScopePathsForCapability(capability) {
		if scopePathMatches(path, boundary) {
			return true
		}
	}

	return false
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
func ScopeKeyForResult(httpStatus int, targetDriveID driveid.ID, shortcutKey string) ScopeKey {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		if shortcutKey != "" {
			remoteDriveID, remoteItemID, ok := strings.Cut(shortcutKey, ":")
			if !ok || remoteDriveID == "" || remoteItemID == "" {
				return ScopeKey{}
			}
			return SKThrottleShared(remoteDriveID, remoteItemID)
		}
		if targetDriveID.IsZero() {
			return ScopeKey{}
		}
		return SKThrottleDrive(targetDriveID)
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

const (
	throttleDrivePrefix  = "drive:"
	throttleSharedPrefix = "shared:"
)

func throttleDriveParam(targetDriveID driveid.ID) string {
	return throttleDrivePrefix + targetDriveID.String()
}

func throttleSharedParam(remoteDriveID, remoteItemID string) string {
	return throttleSharedPrefix + remoteDriveID + ":" + remoteItemID
}
