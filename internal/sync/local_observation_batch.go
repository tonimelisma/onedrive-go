package sync

// localObservationBatch is the engine-applied local watch observation unit.
// Local observers build it from re-observed filesystem facts; the watch loop
// owns durable application to local_state and dirty scheduling.
type localObservationBatch struct {
	rows            []LocalStateRow
	deletedPaths    []string
	deletedPrefixes []string
	fullSnapshot    bool
	markSuspect     bool
	recoveryReason  string
	dirty           bool
}

func localObservationBatchForEvent(event *ChangeEvent) localObservationBatch {
	if event == nil {
		return localObservationBatch{}
	}

	batch := localObservationBatch{dirty: event.Path != "" || event.OldPath != ""}

	switch event.Type {
	case ChangeDelete:
		if event.ItemType == ItemTypeFolder {
			batch.deletedPrefixes = append(batch.deletedPrefixes, event.Path)
			return batch
		}
		batch.deletedPaths = append(batch.deletedPaths, event.Path)
	case ChangeMove:
		if event.OldPath != "" {
			batch.deletedPaths = append(batch.deletedPaths, event.OldPath)
		}
		if event.Path != "" && !event.IsDeleted {
			batch.rows = append(batch.rows, localStateRowFromEvent(event))
		}
	case ChangeCreate, ChangeModify:
		if event.Path != "" && !event.IsDeleted {
			batch.rows = append(batch.rows, localStateRowFromEvent(event))
		}
	}

	return batch
}

func localStateRowFromEvent(event *ChangeEvent) LocalStateRow {
	return LocalStateRow{
		Path:             event.Path,
		ItemType:         event.ItemType,
		Hash:             event.Hash,
		Size:             event.Size,
		Mtime:            event.Mtime,
		LocalDevice:      event.LocalDevice,
		LocalInode:       event.LocalInode,
		LocalHasIdentity: event.LocalHasIdentity,
	}
}
