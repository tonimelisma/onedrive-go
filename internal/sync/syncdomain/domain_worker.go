package syncdomain

import "time"

// OutcomeCommitTimeout bounds every store access the engine runs detached
// from shutdown: the commit of a completed action's outcome, the record of a
// created item, clearing a succeeded action's retry work, the findings
// observed before the shutdown, the invariant checks, and the records of the
// completions a stopping run drains, which share one such bound for the whole
// drain, measured from the cancel (engineFlow.stopDrainBound). One transaction is
// milliseconds (and waits at most SQLite's 5 s busy timeout for a lock); the
// bound only exists so a store whose disk stopped answering cannot block
// shutdown forever. It is the longest one detached wait of a canceled engine
// can take, so the watch control plane derives its bound on a drive's stop
// from it.
const OutcomeCommitTimeout = 30 * time.Second
