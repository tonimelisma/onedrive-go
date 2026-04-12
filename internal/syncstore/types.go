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

type BaselineEntry = synctypes.BaselineEntry

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

// These aliases keep store row DTOs on a single canonical shape shared with
// legacy consumers, instead of maintaining duplicate struct definitions.
type (
	ObservedItem      = synctypes.ObservedItem
	RemoteStateRow    = synctypes.RemoteStateRow
	SyncFailureParams = synctypes.SyncFailureParams
	SyncFailureRow    = synctypes.SyncFailureRow
	ActionableFailure = synctypes.ActionableFailure
)

// RecoveryCandidate identifies one remote_state row that crash recovery must resolve.
type RecoveryCandidate struct {
	DriveID string
	ItemID  string
	Path    string
}

type (
	PendingRetryGroup = synctypes.PendingRetryGroup
	ConflictRecord    = synctypes.ConflictRecord
)

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

type ScopeBlock = synctypes.ScopeBlock

type (
	ScopeStateRecord       = synctypes.ScopeStateRecord
	ScopeStateApplyRequest = synctypes.ScopeStateApplyRequest
)

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
