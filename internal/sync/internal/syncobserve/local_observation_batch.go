package syncobserve

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// LocalObservation is one unit of local observation the watch runtime applies
// to local truth. Local observers build it from re-observed filesystem facts;
// the watch loop owns its durable application and dirty scheduling. Its kind
// says which of three it is, and the runtime's apply switches over every kind
// (the exhaustive linter holds it to that).
type LocalObservation struct {
	Kind localObservationKind

	// rows are the patch's upserts, or every row of the snapshot's scan.
	Rows []syncdomain.LocalStateRow
	// DeletedPrefixes are the patch's deletes. Every local delete removes the
	// path's whole subtree (a path is its own prefix), so there is no
	// separate list of exact deletes.
	DeletedPrefixes []string
	// dirty asks the patch's apply to mark the runtime dirty.
	Dirty bool

	// findings and crossedMounts are the snapshot's scan findings (the whole
	// set, the empty one included) and the mount points it crossed.
	Findings      []syncdomain.SkippedItem
	CrossedMounts []string

	// recoveryReason is why local truth is suspect.
	RecoveryReason string
}

// localObservationKind says what a LocalObservation carries.
type localObservationKind int

const (
	// LocalPatch writes rows and removes paths and subtrees of local truth.
	LocalPatch localObservationKind = iota
	// LocalSnapshot is every row of one full scan with that scan's findings
	// and crossed mounts: the three commit together, in one transaction.
	LocalSnapshot
	// LocalTruthSuspect states that local truth may have missed a change.
	LocalTruthSuspect
)

func (k localObservationKind) String() string {
	switch k {
	case LocalPatch:
		return "patch"
	case LocalSnapshot:
		return "snapshot"
	case LocalTruthSuspect:
		return "suspect"
	default:
		return unknownLocalObservationKind
	}
}

// unknownLocalObservationKind names a kind outside the constants above.
const unknownLocalObservationKind = "unknown"

// localObservationBatchForEvent is the patch for one path a watch handler
// observed: the path's row, or, for a removed path, the removal of the path
// and every row below it. A deleted path takes its subtree whatever it was: a
// folder neither the baseline nor the watch set knows can still have rows
// below it.
func localObservationBatchForEvent(event *syncdomain.LocalEntry) LocalObservation {
	if event == nil || event.Path == "" {
		return LocalObservation{}
	}

	batch := LocalObservation{Dirty: true}
	if event.IsDeleted {
		batch.DeletedPrefixes = []string{event.Path}
	} else {
		batch.Rows = []syncdomain.LocalStateRow{localStateRowFromEvent(event)}
	}

	return batch
}

func localStateRowFromEvent(event *syncdomain.LocalEntry) syncdomain.LocalStateRow {
	return syncdomain.LocalStateRow{
		Path:             event.Path,
		ItemType:         event.ItemType,
		Hash:             event.Hash,
		Size:             event.Size,
		Mtime:            event.Mtime,
		Ctime:            event.LocalCtime,
		LocalDevice:      event.LocalDevice,
		LocalInode:       event.LocalInode,
		LocalBirthTime:   event.LocalBirthTime,
		LocalHasIdentity: event.LocalHasIdentity,
	}
}
