package sync

// localObservation is one unit of local observation the watch runtime applies
// to local truth. Local observers build it from re-observed filesystem facts;
// the watch loop owns its durable application and dirty scheduling. Its kind
// says which of three it is, and the runtime's apply switches over every kind
// (the exhaustive linter holds it to that).
type localObservation struct {
	kind localObservationKind

	// rows are the patch's upserts, or every row of the snapshot's scan.
	rows []localStateRow
	// deletedPaths and deletedPrefixes are the patch's exact and subtree
	// deletes.
	deletedPaths    []string
	deletedPrefixes []string
	// dirty asks the patch's apply to mark the runtime dirty.
	dirty bool

	// findings and crossedMounts are the snapshot's scan findings (the whole
	// set, the empty one included) and the mount points it crossed.
	findings      []skippedItem
	crossedMounts []string

	// recoveryReason is why local truth is suspect.
	recoveryReason string
}

// localObservationKind says what a localObservation carries.
type localObservationKind int

const (
	// localPatch writes rows and removes paths and subtrees of local truth.
	localPatch localObservationKind = iota
	// localSnapshot is every row of one full scan with that scan's findings
	// and crossed mounts: the three commit together, in one transaction.
	localSnapshot
	// localTruthSuspect states that local truth may have missed a change.
	localTruthSuspect
)

func (k localObservationKind) String() string {
	switch k {
	case localPatch:
		return "patch"
	case localSnapshot:
		return "snapshot"
	case localTruthSuspect:
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
func localObservationBatchForEvent(event *changeEvent) localObservation {
	if event == nil || event.Path == "" {
		return localObservation{}
	}

	batch := localObservation{dirty: true}
	if event.IsDeleted {
		batch.deletedPrefixes = []string{event.Path}
	} else {
		batch.rows = []localStateRow{localStateRowFromEvent(event)}
	}

	return batch
}

func localStateRowFromEvent(event *changeEvent) localStateRow {
	return localStateRow{
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
