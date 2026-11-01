package sync

// Crash points (D-091) name the commit boundaries where a process killed at
// that instant leaves its effects half recorded: the mutation done on one
// side, the state database not yet told. A test hook (executorConfig.crashAt,
// SyncStore.crashAt) is called at each one; production leaves both nil, and
// each type's reachCrashPoint is then a no-op. Recovery from every point must
// converge with nothing lost when a fresh engine reopens the same database and
// tree.
const (
	// crashPointRemoteMoveDone: OneDrive moved the item; the outcome is not
	// committed.
	crashPointRemoteMoveDone = "exec.remote_move_done"
	// crashPointLocalMoveDone: the local rename happened; the outcome is not
	// committed.
	crashPointLocalMoveDone = "exec.local_move_done"
	// crashPointUploadRemoteDone: OneDrive holds the uploaded content; the
	// outcome is not committed.
	crashPointUploadRemoteDone = "upload.remote_done"
	// crashPointDownloadRenamed: the download is renamed into place; the
	// outcome is not committed.
	crashPointDownloadRenamed = "download.renamed"
	// crashPointLocalDeleteTrashed: the file left the sync folder for the
	// local trash, whose entry is still pending; neither the trash entry nor
	// the outcome is committed.
	crashPointLocalDeleteTrashed = "delete.local_trashed"
	// crashPointRemoteDeleteDone: OneDrive deleted the item; the outcome is
	// not committed.
	crashPointRemoteDeleteDone = "delete.remote_done"
	// crashPointCommitObservation: an observation window is written but its
	// transaction is not committed.
	crashPointCommitObservation = "store.commit_observation"
	// crashPointCommitOutcome: an action's outcome is written but its
	// transaction is not committed.
	crashPointCommitOutcome = "store.commit_outcome"
)
