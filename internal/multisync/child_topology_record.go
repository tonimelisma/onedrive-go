package multisync

import syncengine "github.com/tonimelisma/onedrive-go/internal/sync"

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
	state               syncengine.ShortcutChildTopologyState
	blockedDetail       string
}

func defaultChildMountTopology() *childMountTopology {
	return &childMountTopology{mounts: make(map[string]childTopologyRecord)}
}
