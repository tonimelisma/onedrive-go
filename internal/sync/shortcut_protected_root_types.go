package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Protected roots are the parent-local alias paths a shortcut root reserves.
// They are shortcut vocabulary: observation consumes them as a suppression
// filter, but only the shortcut lifecycle creates or retires them.

// ProtectedRoot marks a parent-engine protected subtree inside this
// mount. The sync engine suppresses normal local content events for these roots
// and, when it can identify the same directory at a sibling path, wakes the
// parent engine shortcut-root lifecycle instead of uploading a duplicate folder.
type ProtectedRoot struct {
	Path           string
	MountID        string
	BindingID      string
	RemoteDriveID  driveid.ID
	RemoteItemID   string
	RemoteIsFolder bool
	Device         uint64
	Inode          uint64
	BirthTime      int64 // zero means unknown; see synctree.FileIdentity
	HasIdentity    bool
}

// protectedRootEventType identifies the lifecycle fact the local observer found.
type protectedRootEventType string

// protectedRootEvent is a narrow parent-engine-internal notification from the
// local observer to the watch runtime. It never plans child content work; it
// only wakes the parent shortcut-root lifecycle so parent-owned state can
// publish updated child work commands promptly.
const (
	protectedRootEventPathReserved  protectedRootEventType = "path_reserved"
	protectedRootEventIdentityMatch protectedRootEventType = "identity_match"
)

type protectedRootEvent struct {
	Type         protectedRootEventType
	Path         string
	ReservedPath string
	MountID      string
	BindingID    string
}

type protectedRootEventSink func(protectedRootEvent)

// fileIdentity is the identity the shortcut root recorded for the reserved
// directory; meaningful only when HasIdentity.
func (r ProtectedRoot) fileIdentity() synctree.FileIdentity {
	return synctree.FileIdentity{Device: r.Device, Inode: r.Inode, BirthTime: r.BirthTime}
}
