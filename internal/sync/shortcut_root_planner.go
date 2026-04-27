package sync

// This file family is the functional core for parent-owned shortcut-root
// lifecycle. Engine code gathers remote, filesystem, child-drain, and cleanup
// facts; planner helpers turn those facts into next durable records without
// performing I/O.

type shortcutRootTopologyPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

type shortcutRootDrainAckPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

type shortcutRootArtifactCleanupAckPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

type shortcutRootReleaseCleanupPlan struct {
	Records []ShortcutRootRecord
	Changed bool
	Err     error
}
