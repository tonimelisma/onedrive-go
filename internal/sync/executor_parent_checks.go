package sync

import (
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// remoteParentChecks remembers, for the plan being executed, which remote
// parents the executor has already read live: paths whose visibility
// converged, and item IDs read back as folders. Every create into a folder
// used to make both reads again, so a tree of 6,000 new files in 62 folders
// cost 12,000 reads that all answered the same, half of the requests of the
// whole upload (soak of 2026-11-02). The first create into a parent in each
// plan still pays both reads (R-2.8.10); its siblings rely on the create
// request itself, whose 404 names the same parent and is reported as the
// same parent-gone fact. The engine clears the memo when it registers a plan.
type remoteParentChecks struct {
	// mu guards visible and folders: executor workers add concurrently.
	mu      stdsync.Mutex
	visible map[string]struct{}
	folders map[string]struct{}
}

func newRemoteParentChecks() *remoteParentChecks {
	return &remoteParentChecks{
		visible: make(map[string]struct{}),
		folders: make(map[string]struct{}),
	}
}

// reset forgets every read. Called when a plan is registered: the next
// create into each parent reads it live again.
func (c *remoteParentChecks) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.visible = make(map[string]struct{})
	c.folders = make(map[string]struct{})
}

// pathVisible reports whether remotePath converged earlier in this plan.
func (c *remoteParentChecks) pathVisible(remotePath string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.visible[remotePath]

	return ok
}

func (c *remoteParentChecks) markPathVisible(remotePath string) {
	if c == nil || remotePath == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.visible[remotePath] = struct{}{}
}

// folderRead reports whether the item was read live as a folder earlier in
// this plan.
func (c *remoteParentChecks) folderRead(driveID driveid.ID, itemID string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.folders[folderReadKey(driveID, itemID)]

	return ok
}

func (c *remoteParentChecks) markFolderRead(driveID driveid.ID, itemID string) {
	if c == nil || itemID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.folders[folderReadKey(driveID, itemID)] = struct{}{}
}

func folderReadKey(driveID driveid.ID, itemID string) string {
	return driveID.String() + "/" + itemID
}

// resetRemoteParentChecks forgets the parents read for the previous plan.
func (cfg *executorConfig) resetRemoteParentChecks() {
	if cfg == nil {
		return
	}
	cfg.parentChecks.reset()
}
