package sync

// Crash points (D-091) name the commit boundaries where a process killed at
// that instant leaves its effects half recorded: the mutation done on one
// side, the state database not yet told. A test hook (ExecutorConfig.crashAt,
// SyncStore.crashAt) is called at each one; production leaves both nil, and
// each type's reachCrashPoint is then a no-op. Recovery from every point must
// converge with nothing lost when a fresh engine reopens the same database and
// tree.
const (
	// CrashPointRemoteMoveDone: OneDrive moved the item; the outcome is not
	// committed.
	CrashPointRemoteMoveDone = "exec.remote_move_done"
	// CrashPointLocalMoveDone: the local rename happened; the outcome is not
	// committed.
	CrashPointLocalMoveDone = "exec.local_move_done"
	// CrashPointLocalMoveDestinationTrashed: a local move that replaces its
	// destination took the destination's file out of the sync folder into
	// the local trash, whose entry is still pending; the rename has not
	// happened and neither the trash entry nor the outcome is committed.
	CrashPointLocalMoveDestinationTrashed = "exec.local_move_destination_trashed"
	// CrashPointUploadCreatedUnrecorded: OneDrive holds the new file's empty
	// item; the pending-create record naming it is not written (the creating
	// request was in flight when the process died). Only the intent row,
	// written before the request, knows the empty item is ours.
	CrashPointUploadCreatedUnrecorded = "upload.created_unrecorded"
	// CrashPointUploadRemoteDone: OneDrive holds the uploaded content; the
	// outcome is not committed.
	CrashPointUploadRemoteDone = "upload.remote_done"
	// CrashPointDownloadRenamed: the download is renamed into place; the
	// outcome is not committed.
	CrashPointDownloadRenamed = "download.renamed"
	// CrashPointLocalDeleteTrashed: the file left the sync folder for the
	// local trash, whose entry is still pending; neither the trash entry nor
	// the outcome is committed.
	CrashPointLocalDeleteTrashed = "delete.local_trashed"
	// CrashPointRemoteDeleteDone: OneDrive deleted the item; the outcome is
	// not committed.
	CrashPointRemoteDeleteDone = "delete.remote_done"
	// CrashPointCommitObservation: an observation window is written but its
	// transaction is not committed.
	CrashPointCommitObservation = "store.commit_observation"
	// CrashPointCommitOutcome: an action's outcome is written but its
	// transaction is not committed.
	CrashPointCommitOutcome = "store.commit_outcome"
)
