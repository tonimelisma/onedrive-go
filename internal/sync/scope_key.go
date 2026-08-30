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
// (ScopeThrottleTarget, local directory permission scopes, remote read
// boundaries, and remote write block scopes).
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

// sKPermLocalRead returns the scope key for a local read-denied directory.
func sKPermLocalRead(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDirRead, Param: dirPath}
}

// SKPermLocalWrite returns the scope key for a local write-denied directory.
func SKPermLocalWrite(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDirWrite, Param: dirPath}
}

// sKPermRemoteRead returns the scope key for a remote read-denied boundary.
func sKPermRemoteRead(boundaryPath string) ScopeKey {
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
	wireThrottleTarget  = "throttle:target:"
	wireService         = "service"
	wireQuotaOwn        = "quota:own"
	wirePermDirRead     = "perm:dir:read:"
	wirePermDirWrite    = "perm:dir:write:"
	wirePermRemoteRead  = "perm:remote:read:"
	wirePermRemoteWrite = "perm:remote:write:"
	wireDiskLocal       = "disk:local"
)

// String serializes to the wire format stored in SQLite scope_key columns.
// ParseScopeKey is the inverse.
func (sk ScopeKey) String() string {
	switch sk.Kind {
	case ScopeThrottleTarget:
		return wireThrottleTarget + sk.Param
	case ScopeService:
		return wireService
	case ScopeQuotaOwn:
		return wireQuotaOwn
	case ScopePermDirRead:
		return wirePermDirRead + sk.Param
	case ScopePermDirWrite:
		return wirePermDirWrite + sk.Param
	case ScopePermRemoteRead:
		return wirePermRemoteRead + sk.Param
	case ScopePermRemoteWrite:
		return wirePermRemoteWrite + sk.Param
	case ScopeDiskLocal:
		return wireDiskLocal
	default:
		return ""
	}
}

// parseScopeKey deserializes a wire-format string into a ScopeKey.
// Returns the zero-value ScopeKey for unknown formats.
func parseScopeKey(s string) ScopeKey {
	switch {
	case strings.HasPrefix(s, wireThrottleTarget):
		return ScopeKey{Kind: ScopeThrottleTarget, Param: strings.TrimPrefix(s, wireThrottleTarget)}
	case s == wireService:
		return SKService()
	case s == wireQuotaOwn:
		return SKQuotaOwn()
	case s == wireDiskLocal:
		return SKDiskLocal()
	case strings.HasPrefix(s, wirePermDirRead):
		return sKPermLocalRead(strings.TrimPrefix(s, wirePermDirRead))
	case strings.HasPrefix(s, wirePermDirWrite):
		return SKPermLocalWrite(strings.TrimPrefix(s, wirePermDirWrite))
	case strings.HasPrefix(s, wirePermRemoteRead):
		return sKPermRemoteRead(strings.TrimPrefix(s, wirePermRemoteRead))
	case strings.HasPrefix(s, wirePermRemoteWrite):
		return SKPermRemoteWrite(strings.TrimPrefix(s, wirePermRemoteWrite))
	default:
		return ScopeKey{}
	}
}

// IsGlobal returns true for block scopes that affect ALL actions. Target-scoped
// throttles are intentionally not global; only service remains process-wide.
func (sk ScopeKey) IsGlobal() bool {
	return sk.Kind == ScopeService
}

// IsPermDir returns true for local directory permission scopes.
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

// CoveredPath returns the subtree or path covered by this scope key when it is
// path-scoped. Generic callers should use this accessor; DirPath and RemotePath
// remain family-asserting helpers for callers that need to prove the scope
// family before proceeding. Non-path scopes return the empty string.
func (sk ScopeKey) CoveredPath() string {
	return describeScopeKey(sk).ScopePath()
}

// DirPath returns the directory path for a local directory permission scope key.
// Panics if called on a non-local-permission key (defensive — caller bug).
func (sk ScopeKey) DirPath() string {
	descriptor := describeScopeKey(sk)
	if descriptor.Family != scopeFamilyPermDir {
		panic("ScopeKey.DirPath() called on non-local-permission key")
	}
	return sk.CoveredPath()
}

// RemotePath returns the local boundary path for a remote read boundary or
// remote write block scope key.
// Panics if called on a non-remote-permission key (defensive — caller bug).
func (sk ScopeKey) RemotePath() string {
	descriptor := describeScopeKey(sk)
	if descriptor.Family != scopeFamilyPermRemote {
		panic("ScopeKey.RemotePath() called on non-remote-permission key")
	}
	return sk.CoveredPath()
}

// PersistsInBlockScopes reports whether this scope is a timed blocked-work
// scope that belongs in block_scopes.
func (sk ScopeKey) PersistsInBlockScopes() bool {
	return describeScopeKey(sk).PersistsInBlockScopes()
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

// ConditionType returns the condition_type constant for this scope key's kind.
// Used to derive a stable default condition type from a scope key.
func (sk ScopeKey) ConditionType() string {
	return describeScopeKey(sk).DefaultConditionType
}

// Humanize translates a scope key to a user-friendly description (R-2.10.22).
// For directory- and subtree-scoped blocks, returns the stored local path. For
// global scopes, returns a plain English description.
func (sk ScopeKey) Humanize() string {
	return describeScopeKey(sk).Humanize()
}

// BlocksAction returns true if this scope key blocks the given action.
// Replaces the scattered string-matching logic from blockedScope().
func (sk ScopeKey) BlocksAction(
	path string,
	throttleTargetKey string,
	actionType actionType,
) bool {
	return describeScopeKey(sk).BlocksAction(path, throttleTargetKey, actionType)
}

func scopePathMatches(path, boundary string) bool {
	if boundary == "" {
		return true
	}

	return path == boundary || strings.HasPrefix(path, boundary+"/")
}

// scopeKeyForResult maps one action completion target and HTTP status code to a
// ScopeKey. Returns the zero-value for non-scope statuses. This is the single
// source of truth for HTTP status → scope key classification.
func scopeKeyForResult(httpStatus int, targetDriveID driveid.ID) ScopeKey {
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
