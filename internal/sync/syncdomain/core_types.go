package syncdomain

import (
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// SkippedItem records a local filesystem entry that was rejected at
// observation time. The scanner collects these alongside events so the
// engine can persist them as durable observation issues.
type SkippedItem struct {
	Path               string    // NFC-normalized, relative to sync root
	Reason             IssueType // the observation issue type it is stored under
	Detail             string    // human-readable explanation
	FileSize           int64     // populated for IssueFileTooLarge (after stat)
	BlocksReadBoundary bool      // true when observation proved an unreadable subtree boundary
}

// LocalEntry is one entry the local observer saw: a scan's change list
// entry (Path and ItemType), a watch handler's row facts (ItemType, Size,
// Hash, Mtime, filesystem identity when available), or a removal (Path and
// IsDeleted). It is not durable sync truth and is never stored; it reaches
// the runtime only through the local observer's own paths (scan results,
// LocalObservation). OneDrive's side reports RemoteItem values instead, so
// the type carries no remote-only field.
type LocalEntry struct {
	Path     string // NFC-normalized, relative to sync root
	ItemType ItemType
	Size     int64
	// Hash is a QuickXorHash (base64), the hash OneDrive's side is compared
	// in. Empty for folders.
	Hash             string
	Mtime            int64 // Unix nanoseconds
	LocalCtime       int64 // local inode change time, files only; zero means unknown
	LocalDevice      uint64
	LocalInode       uint64
	LocalBirthTime   int64 // entry birth time (Unix nanoseconds); zero means unknown
	LocalHasIdentity bool
	IsDeleted        bool
}

// BaselineEntry represents the confirmed synced state of a single path.
// This is the ONLY durable per-item state in the system.
type BaselineEntry struct {
	Path            string
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	ItemType        ItemType
	LocalHash       string // per-side: handles SharePoint enrichment natively
	RemoteHash      string // for normal files, LocalHash == RemoteHash
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64 // local mtime at sync time (Unix nanoseconds)
	RemoteMtime     int64 // remote mtime at sync time; zero means unknown
	// LocalCtime is the local inode change time at sync time (Unix
	// nanoseconds); zero means unknown. The hash fast path requires it
	// unchanged, because an in-place edit can restore the old mtime but
	// never the old ctime (R-2.2.4).
	LocalCtime       int64
	LocalDevice      uint64
	LocalInode       uint64
	LocalBirthTime   int64 // entry birth time (Unix nanoseconds); zero means unknown
	LocalHasIdentity bool
	ETag             string
}

// StoredPathOf turns a path relative to the sync tree, as spelled on disk
// with the host's separator, into the form sync stores paths in:
// slash-separated, in stored form (namekey.StoredPath, R-2.13.1). It is the
// one place that conversion is written.
func StoredPathOf(rel string) string {
	return namekey.StoredPath(filepath.ToSlash(rel))
}

// RemoteState captures the current state of a path as observed from
// the Graph API delta response.
type RemoteState struct {
	ItemID   string
	DriveID  driveid.ID // normalized (lowercase, zero-padded to 16 chars)
	Name     string
	ItemType ItemType
	Size     int64
	// SizeKnown is true when Size is the item's real size (zero included)
	// rather than an absent value; downloads verify against it.
	SizeKnown bool
	Hash      string
	Mtime     int64
	ETag      string
	CTag      string
	IsDeleted bool
}

// LocalState captures the current state of a path as observed from
// the local filesystem.
type LocalState struct {
	Name             string
	ItemType         ItemType
	Size             int64
	Hash             string
	Mtime            int64
	LocalCtime       int64 // inode change time (files only); zero means unknown
	LocalDevice      uint64
	LocalInode       uint64
	LocalBirthTime   int64 // entry birth time (Unix nanoseconds); zero means unknown
	LocalHasIdentity bool
}

// truthAvailability records whether one side of current path truth is safe for
// the planner to interpret structurally.
type truthAvailability string

const (
	TruthAvailabilityAvailable               truthAvailability = "available"
	TruthAvailabilityBlockedObservationIssue truthAvailability = "blocked_observation_issue"
)

// pathTruthSource captures which durable authority proved that one side of
// truth is currently unavailable to the planner.
type pathTruthSource string

const (
	pathTruthSourceNone             pathTruthSource = ""
	PathTruthSourceObservationIssue pathTruthSource = "observation_issue"
)

// pathTruthSideStatus is the raw current-truth availability status for one
// side of a path. The zero value is intentionally healthy so callers can use
// it as a derived read-side value without manual initialization.
type pathTruthSideStatus struct {
	Availability truthAvailability
	Source       pathTruthSource
	IssueType    IssueType
	ScopeKey     ScopeKey
}

func (s pathTruthSideStatus) IsAvailable() bool {
	return s.Availability == "" || s.Availability == TruthAvailabilityAvailable
}

// PathTruthStatus carries the raw current-truth availability status for both
// the local and remote side of one path. Planner attaches it after SQLite
// computes structural diff so structural absence stays distinct from
// unavailable truth, but the type itself is not planner-owned policy.
type PathTruthStatus struct {
	Local  pathTruthSideStatus
	Remote pathTruthSideStatus
}

func (s *PathTruthStatus) SuppressesStructuralActions() bool {
	if s == nil {
		return false
	}

	return !s.Local.IsAvailable() || !s.Remote.IsAvailable()
}

// PathView is a unified three-way view of a single path built from the
// authoritative snapshots plus baseline. It is the in-memory planning view the
// current actionable-set builder reasons over after SQLite computes structural
// diff and reconciliation outcomes.
type PathView struct {
	Path        string
	Remote      *RemoteState   // nil = absent from current remote snapshot
	Local       *LocalState    // nil = absent from current local snapshot
	Baseline    *BaselineEntry // nil = never synced
	TruthStatus PathTruthStatus
}

// RemoteContentDiffersFromBaseline reports whether remote content facts prove
// the item's content changed since the baseline. eTag and mtime churn alone
// (a Graph move rewrites both) are not content changes.
func RemoteContentDiffersFromBaseline(remote *RemoteState, baseline *BaselineEntry) bool {
	if remote == nil || baseline == nil || baseline.ItemType != ItemTypeFile {
		return false
	}
	if remote.Hash != "" && baseline.RemoteHash != "" && remote.Hash != baseline.RemoteHash {
		return true
	}

	return baseline.RemoteSizeKnown && remote.Size != baseline.RemoteSize
}

// LocalFileDiffersFromBaseline reports whether a local file's content facts
// show it changed since the baseline: the hash when either side has one,
// otherwise size, then modification time.
func LocalFileDiffersFromBaseline(local *LocalState, baseline *BaselineEntry) bool {
	if local == nil || baseline == nil {
		return false
	}
	if baseline.ItemType != ItemTypeFile || local.ItemType != ItemTypeFile {
		return false
	}
	if baseline.LocalHash != "" || local.Hash != "" {
		return baseline.LocalHash != local.Hash
	}
	if baseline.LocalSizeKnown && baseline.LocalSize != local.Size {
		return true
	}

	return baseline.LocalMtime != local.Mtime
}
