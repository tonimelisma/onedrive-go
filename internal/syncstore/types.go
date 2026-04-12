package syncstore

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// DirLowerKey groups baseline entries by (directory, lowercase name) for
// case-insensitive sibling lookups.
type DirLowerKey struct {
	Dir     string
	LowName string
}

// DirLowerKeyFromPath computes the case-insensitive grouping key for a path.
func DirLowerKeyFromPath(path string) DirLowerKey {
	return DirLowerKey{
		Dir:     filepath.Dir(path),
		LowName: strings.ToLower(filepath.Base(path)),
	}
}

// BaselineEntry represents the confirmed synced state of a single path.
type BaselineEntry struct {
	Path            string
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	ItemType        synctypes.ItemType
	LocalHash       string
	RemoteHash      string
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64
	RemoteMtime     int64
	SyncedAt        int64
	ETag            string
}

// Baseline is the in-memory container for all baseline entries.
type Baseline struct {
	mu         sync.RWMutex
	ByPath     map[string]*BaselineEntry
	ByID       map[driveid.ItemKey]*BaselineEntry
	ByDirLower map[DirLowerKey][]*BaselineEntry
}

func (b *Baseline) GetByPath(path string) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.ByPath[path]
	return entry, ok
}

func (b *Baseline) GetByID(key driveid.ItemKey) (*BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.ByID[key]
	return entry, ok
}

func (b *Baseline) GetCaseVariants(dir, name string) []*BaselineEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ByDirLower[DirLowerKey{Dir: dir, LowName: strings.ToLower(name)}]
}

func (b *Baseline) Put(entry *BaselineEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	newKey := driveid.NewItemKey(entry.DriveID, entry.ItemID)
	if old, ok := b.ByPath[entry.Path]; ok {
		oldKey := driveid.NewItemKey(old.DriveID, old.ItemID)
		if oldKey != newKey {
			delete(b.ByID, oldKey)
		}
	}

	b.ByPath[entry.Path] = entry
	b.ByID[newKey] = entry

	dlk := DirLowerKeyFromPath(entry.Path)
	found := false
	for i, existing := range b.ByDirLower[dlk] {
		if existing.Path == entry.Path {
			b.ByDirLower[dlk][i] = entry
			found = true
			break
		}
	}
	if !found {
		b.ByDirLower[dlk] = append(b.ByDirLower[dlk], entry)
	}
}

func (b *Baseline) Delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if entry, ok := b.ByPath[path]; ok {
		delete(b.ByID, driveid.NewItemKey(entry.DriveID, entry.ItemID))
	}
	delete(b.ByPath, path)

	dlk := DirLowerKeyFromPath(path)
	entries := b.ByDirLower[dlk]
	for i, entry := range entries {
		if entry.Path == path {
			b.ByDirLower[dlk] = append(entries[:i], entries[i+1:]...)
			if len(b.ByDirLower[dlk]) == 0 {
				delete(b.ByDirLower, dlk)
			}
			break
		}
	}
}

func (b *Baseline) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.ByPath)
}

func (b *Baseline) ForEachPath(fn func(string, *BaselineEntry)) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for path, entry := range b.ByPath {
		fn(path, entry)
	}
}

func (b *Baseline) DescendantsOf(prefix string) []*BaselineEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	childPrefix := prefix + "/"
	var descendants []*BaselineEntry
	for path, entry := range b.ByPath {
		if strings.HasPrefix(path, childPrefix) {
			descendants = append(descendants, entry)
		}
	}
	return descendants
}

// NewBaselineForTest creates a Baseline pre-populated with entries.
func NewBaselineForTest(entries []*BaselineEntry) *Baseline {
	bl := &Baseline{
		ByPath:     make(map[string]*BaselineEntry, len(entries)),
		ByID:       make(map[driveid.ItemKey]*BaselineEntry, len(entries)),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry, len(entries)),
	}
	for _, entry := range entries {
		bl.ByPath[entry.Path] = entry
		bl.ByID[driveid.NewItemKey(entry.DriveID, entry.ItemID)] = entry
		dlk := DirLowerKeyFromPath(entry.Path)
		bl.ByDirLower[dlk] = append(bl.ByDirLower[dlk], entry)
	}
	return bl
}

// ObservedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type ObservedItem struct {
	DriveID          driveid.ID
	ItemID           string
	ParentID         string
	Path             string
	ItemType         synctypes.ItemType
	Hash             string
	Size             int64
	Mtime            int64
	ETag             string
	IsDeleted        bool
	Filtered         bool
	FilterGeneration int64
	FilterReason     synctypes.RemoteFilterReason
}

// RemoteStateRow represents a row from the remote_state table.
type RemoteStateRow struct {
	DriveID          driveid.ID
	ItemID           string
	Path             string
	ParentID         string
	ItemType         synctypes.ItemType
	Hash             string
	Size             int64
	Mtime            int64
	ETag             string
	PreviousPath     string
	SyncStatus       synctypes.SyncStatus
	ObservedAt       int64
	FilterGeneration int64
	FilterReason     synctypes.RemoteFilterReason
}

// SyncFailureParams bundles all inputs for RecordFailure into a single struct.
type SyncFailureParams struct {
	Path       string
	DriveID    driveid.ID
	Direction  synctypes.Direction
	Role       synctypes.FailureRole
	IssueType  string
	Category   synctypes.FailureCategory
	ErrMsg     string
	HTTPStatus int
	ActionType synctypes.ActionType
	FileSize   int64
	LocalHash  string
	ItemID     string
	ScopeKey   synctypes.ScopeKey
}

// SyncFailureRow represents a row from the sync_failures table.
type SyncFailureRow struct {
	Path         string
	DriveID      driveid.ID
	Direction    synctypes.Direction
	Role         synctypes.FailureRole
	Category     synctypes.FailureCategory
	IssueType    string
	ItemID       string
	ActionType   synctypes.ActionType
	FailureCount int
	NextRetryAt  int64
	LastError    string
	HTTPStatus   int
	FirstSeenAt  int64
	LastSeenAt   int64
	FileSize     int64
	LocalHash    string
	ScopeKey     synctypes.ScopeKey
}

// ActionableFailure represents a scanner-detected issue to batch-upsert.
type ActionableFailure struct {
	Path       string
	DriveID    driveid.ID
	Direction  synctypes.Direction
	ActionType synctypes.ActionType
	Role       synctypes.FailureRole
	IssueType  string
	Error      string
	ScopeKey   synctypes.ScopeKey
	FileSize   int64
}

// RecoveryCandidate identifies one remote_state row that crash recovery must resolve.
type RecoveryCandidate struct {
	DriveID string
	ItemID  string
	Path    string
}

// PendingRetryGroup aggregates transient failures by scope_key.
type PendingRetryGroup struct {
	ScopeKey     synctypes.ScopeKey
	Count        int
	EarliestNext time.Time
}

// ConflictRecord holds metadata about a detected conflict.
type ConflictRecord struct {
	ID           string
	DriveID      driveid.ID
	ItemID       string
	Path         string
	Name         string
	ConflictType string
	DetectedAt   int64
	LocalHash    string
	RemoteHash   string
	LocalMtime   int64
	RemoteMtime  int64
	Resolution   string
	ResolvedAt   int64
	ResolvedBy   string
}

// ConflictRequestRecord is the durable user-intent workflow for one conflict.
type ConflictRequestRecord struct {
	ConflictRecord
	State               string
	RequestedResolution string
	RequestedAt         int64
	ApplyingAt          int64
	LastError           string
}

// HeldDeleteRecord is the durable user-approval ledger for delete safety threshold holds.
type HeldDeleteRecord struct {
	DriveID       driveid.ID
	ItemID        string
	Path          string
	ActionType    synctypes.ActionType
	State         string
	HeldAt        int64
	ApprovedAt    int64
	LastPlannedAt int64
	LastError     string
}

// ScopeBlock represents an active scope-level block.
type ScopeBlock struct {
	Key           synctypes.ScopeKey
	IssueType     string
	TimingSource  synctypes.ScopeTimingSource
	BlockedAt     time.Time
	TrialInterval time.Duration
	NextTrialAt   time.Time
	PreserveUntil time.Time
	TrialCount    int
}

// ScopeStateRecord is the durable store-owned projection of the current sync scope.
type ScopeStateRecord struct {
	Generation            int64
	EffectiveSnapshotJSON string
	ObservationPlanHash   string
	ObservationMode       synctypes.ScopeObservationMode
	WebsocketEnabled      bool
	PendingReentry        bool
	LastReconcileKind     synctypes.ScopeReconcileKind
	UpdatedAt             int64
}

// ScopeStateApplyRequest is one atomic scope-state transition.
type ScopeStateApplyRequest struct {
	State ScopeStateRecord
}

// BaselineMutation is the store-owned persistence input produced from one
// executed action result.
type BaselineMutation struct {
	Action synctypes.ActionType
	// Success is carried for safety and for tests that seed mixed result sets.
	// CommitMutation no-ops failed mutations so store persistence stays aligned
	// with the engine's success-only commit contract.
	Success         bool
	Path            string
	OldPath         string
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	ItemType        synctypes.ItemType
	LocalHash       string
	RemoteHash      string
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64
	RemoteMtime     int64
	ETag            string
	ConflictType    string
	ResolvedBy      string
}

// SyncMetadata is the store-owned projection persisted after a completed sync pass.
type SyncMetadata struct {
	Duration  time.Duration
	Succeeded int
	Failed    int
	Errors    []error
}
