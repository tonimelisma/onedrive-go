package sync

// remoteObservationBatch is the value remote observation hands the watch loop
// to apply. It lives with observation rather than the engine because
// observation produces it; the loop is only its consumer.
type remoteObservationBatchSource string

const (
	remoteObservationBatchPrimaryWatch remoteObservationBatchSource = "primary_watch"
	remoteObservationBatchMountRoot    remoteObservationBatchSource = "mount_root_watch"
	remoteObservationBatchFullRefresh  remoteObservationBatchSource = "full_refresh"
)

type remoteObservationBatch struct {
	source                remoteObservationBatchSource
	observationMode       remoteObservationMode
	observed              []observedItem
	emitted               []changeEvent
	cursorToken           string
	markFullRemoteRefresh bool
	findings              ObservationFindingsBatch
	shortcutTopology      shortcutTopologyBatch
	armFullRefreshTimer   bool
	markFullRefreshIfIdle bool
	applyAck              chan error
}
