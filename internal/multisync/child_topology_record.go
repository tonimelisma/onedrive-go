package multisync

type childTopologyState string

const (
	childTopologyStateActive         childTopologyState = "active"
	childTopologyStatePendingRemoval childTopologyState = "pending_removal"
	childTopologyStateConflict       childTopologyState = "conflict"
	childTopologyStateUnavailable    childTopologyState = "unavailable"
)

type childTopologyStateReason string

const (
	childTopologyStateReasonDuplicateContentRoot          childTopologyStateReason = "duplicate_content_root"
	childTopologyStateReasonExplicitStandaloneContentRoot childTopologyStateReason = "explicit_standalone_content_root"
	childTopologyStateReasonShortcutRemoved               childTopologyStateReason = "shortcut_removed"
	childTopologyStateReasonShortcutBindingUnavailable    childTopologyStateReason = "shortcut_binding_unavailable"
)

type childMountTopology struct {
	mounts map[string]childTopologyRecord
}

type childTopologyRecord struct {
	mountID             string
	namespaceID         string
	bindingItemID       string
	localAlias          string
	relativeLocalPath   string
	reservedLocalPaths  []string
	tokenOwnerCanonical string
	remoteDriveID       string
	remoteItemID        string
	state               childTopologyState
	stateReason         childTopologyStateReason
}

func defaultChildMountTopology() *childMountTopology {
	return &childMountTopology{mounts: make(map[string]childTopologyRecord)}
}
