package sync

import (
	"net/http"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ScopeKeyKind discriminates the type of block scope. Value-typed (usable
// as map key), exhaustive via switch. Zero value is invalid by construction.
type ScopeKeyKind int

const (
	ScopeThrottleTarget  ScopeKeyKind = iota + 1 // Param = "drive:<targetDriveID>"
	ScopeService                                 // no Param
	ScopeQuotaOwn                                // no Param
	ScopePermDirRead                             // Param = relative directory path
	ScopePermDirWrite                            // Param = relative directory path
	ScopePermRemoteRead                          // Param = local boundary path
	ScopePermRemoteWrite                         // Param = local boundary path
	ScopeDiskLocal                               // no Param
)

// ScopeKey identifies a block scope. The Kind discriminator determines the
// semantics; Param carries per-instance data for parameterized scopes
// (ScopeThrottleTarget, local permission scopes, remote permission scopes).
// Comparable, so usable as a map key.
type ScopeKey struct {
	Kind  ScopeKeyKind
	Param string
}

// Fixed scope-key constructors for non-parameterized scopes. Use these
// instead of constructing ScopeKey{Kind: ...} literals for readability.
func SKService() ScopeKey   { return ScopeKey{Kind: ScopeService} }
func SKQuotaOwn() ScopeKey  { return ScopeKey{Kind: ScopeQuotaOwn} }
func SKDiskLocal() ScopeKey { return ScopeKey{Kind: ScopeDiskLocal} }

// SKThrottleDrive returns the target-scoped throttle key for one drive.
func SKThrottleDrive(targetDriveID driveid.ID) ScopeKey {
	return ScopeKey{Kind: ScopeThrottleTarget, Param: throttleDriveParam(targetDriveID)}
}

// SKPermLocalRead returns the scope key for a local read-denied directory.
func SKPermLocalRead(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDirRead, Param: dirPath}
}

// SKPermLocalWrite returns the scope key for a local write-denied directory.
func SKPermLocalWrite(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDirWrite, Param: dirPath}
}

// SKPermRemoteRead returns the scope key for a remote read-denied boundary.
func SKPermRemoteRead(boundaryPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermRemoteRead, Param: boundaryPath}
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
	WireThrottleTarget  = "throttle:target:"
	WireService         = "service"
	WireQuotaOwn        = "quota:own"
	WirePermDirRead     = "perm:dir:read:"
	WirePermDirWrite    = "perm:dir:write:"
	WirePermRemoteRead  = "perm:remote:read:"
	WirePermRemoteWrite = "perm:remote:write:"
	WireDiskLocal       = "disk:local"
)

// String serializes to the wire format stored in SQLite scope_key columns.
// ParseScopeKey is the inverse.
func (sk ScopeKey) String() string {
	switch sk.Kind {
	case ScopeThrottleTarget:
		return WireThrottleTarget + sk.Param
	case ScopeService:
		return WireService
	case ScopeQuotaOwn:
		return WireQuotaOwn
	case ScopePermDirRead:
		return WirePermDirRead + sk.Param
	case ScopePermDirWrite:
		return WirePermDirWrite + sk.Param
	case ScopePermRemoteRead:
		return WirePermRemoteRead + sk.Param
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
	case strings.HasPrefix(s, WireThrottleTarget):
		return ScopeKey{Kind: ScopeThrottleTarget, Param: strings.TrimPrefix(s, WireThrottleTarget)}
	case s == WireService:
		return SKService()
	case s == WireQuotaOwn:
		return SKQuotaOwn()
	case s == WireDiskLocal:
		return SKDiskLocal()
	case strings.HasPrefix(s, WirePermDirRead):
		return SKPermLocalRead(strings.TrimPrefix(s, WirePermDirRead))
	case strings.HasPrefix(s, WirePermDirWrite):
		return SKPermLocalWrite(strings.TrimPrefix(s, WirePermDirWrite))
	case strings.HasPrefix(s, WirePermRemoteRead):
		return SKPermRemoteRead(strings.TrimPrefix(s, WirePermRemoteRead))
	case strings.HasPrefix(s, WirePermRemoteWrite):
		return SKPermRemoteWrite(strings.TrimPrefix(s, WirePermRemoteWrite))
	default:
		return ScopeKey{}
	}
}

// IsGlobal returns true for block scopes that affect ALL actions. Target-scoped
// throttles are intentionally not global; only service remains process-wide.
func (sk ScopeKey) IsGlobal() bool {
	return sk.Kind == ScopeService
}

// IsPermDir returns true for local directory permission block scopes.
func (sk ScopeKey) IsPermDir() bool {
	return sk.IsPermLocalRead() || sk.IsPermLocalWrite()
}

// IsPermLocalRead returns true for local read-denied directory scopes.
func (sk ScopeKey) IsPermLocalRead() bool {
	return sk.Kind == ScopePermDirRead
}

// IsPermLocalWrite returns true for local write-denied directory scopes.
func (sk ScopeKey) IsPermLocalWrite() bool {
	return sk.Kind == ScopePermDirWrite
}

// IsPermRemote returns true for remote read- or write-denied subtree scopes.
func (sk ScopeKey) IsPermRemote() bool {
	return sk.IsPermRemoteRead() || sk.IsPermRemoteWrite()
}

// IsPermRemoteRead returns true for remote read-denied subtree scopes.
func (sk ScopeKey) IsPermRemoteRead() bool {
	return sk.Kind == ScopePermRemoteRead
}

// IsPermRemoteWrite returns true for remote write-denied subtree scopes.
func (sk ScopeKey) IsPermRemoteWrite() bool {
	return sk.Kind == ScopePermRemoteWrite
}

// DirPath returns the directory path for a local permission scope key.
// Panics if called on a non-local-permission key (defensive — caller bug).
func (sk ScopeKey) DirPath() string {
	descriptor := DescribeScopeKey(sk)
	if descriptor.Family != ScopeFamilyPermDir {
		panic("ScopeKey.DirPath() called on non-local-permission key")
	}
	return descriptor.ScopePath()
}

// RemotePath returns the local boundary path for a remote permission scope key.
// Panics if called on a non-remote-permission key (defensive — caller bug).
func (sk ScopeKey) RemotePath() string {
	descriptor := DescribeScopeKey(sk)
	if descriptor.Family != ScopeFamilyPermRemote {
		panic("ScopeKey.RemotePath() called on non-remote-permission key")
	}
	return descriptor.ScopePath()
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
// Used to derive a stable default issue type from a scope key.
func (sk ScopeKey) IssueType() string {
	return DescribeScopeKey(sk).DefaultIssueType
}

// Humanize translates a scope key to a user-friendly description (R-2.10.22).
// For directory- and subtree-scoped blocks, returns the stored local path. For
// global scopes, returns a plain English description.
func (sk ScopeKey) Humanize() string {
	return DescribeScopeKey(sk).Humanize()
}

// BlocksAction returns true if this scope key blocks the given action.
// Replaces the scattered string-matching logic from blockedScope().
func (sk ScopeKey) BlocksAction(
	path string,
	throttleTargetKey string,
	actionType ActionType,
) bool {
	return DescribeScopeKey(sk).BlocksAction(path, throttleTargetKey, actionType)
}

func scopePathMatches(path, boundary string) bool {
	if boundary == "" {
		return true
	}

	return path == boundary || strings.HasPrefix(path, boundary+"/")
}

// ScopeKeyForResult maps one action completion target and HTTP status code to a
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
