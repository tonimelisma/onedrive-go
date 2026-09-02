package sync

import "time"

// How remote observation is being performed, and how often a full refresh is
// due. These are domain vocabulary rather than watch internals: the store
// persists the cadence decision in observation_state and reads the mode back,
// so keeping them in the watch loop made the store depend on the engine.

type remoteObservationMode string

const (
	remoteObservationModeDelta     remoteObservationMode = "delta"
	remoteObservationModeEnumerate remoteObservationMode = "enumerate"
)

// fullRemoteRefreshInterval is how long delta-based observation waits between
// full remote refreshes. The store persists the resulting deadline in
// observation_state, so the constant belongs with the vocabulary rather than
// with the watch loop that happens to schedule it.
const fullRemoteRefreshInterval = 24 * time.Hour
