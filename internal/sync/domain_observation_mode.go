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

// fullRefreshAfterFolderDeleteDelay is how soon after an incremental delta
// deletes a synced folder the full remote refresh becomes due. Delta never
// reports an item moved into the folder in the same window, so only a full
// enumeration finds it gone (R-2.17.2). Deferring by this much, rather than
// running at once, lets one burst of folder deletions share one enumeration
// and keeps refreshes at least this far apart, so a tool churning folders
// cannot cost a whole-drive enumeration per poll. It matches the spacing of
// expired-delta recoveries (expiredDeltaRecoveryMinInterval): at most four
// enumerations an hour, and a missed delete resolved within a quarter hour
// instead of the 24-hour cadence.
const fullRefreshAfterFolderDeleteDelay = 15 * time.Minute
