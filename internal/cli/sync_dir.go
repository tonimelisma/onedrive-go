package cli

import (
	"fmt"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// syncRootDirPerms is the mode of a sync folder the client creates. Unlike a
// folder inside it (localpath.UserTreeDirPerm, 0777 minus the umask), the sync
// folder is a container the client makes on its own initiative for what it
// downloads, which is personal, so it is private. An existing folder keeps
// its mode.
const syncRootDirPerms = 0o700

func materializeDriveSyncDir(syncDir string) error {
	if syncDir == "" {
		return fmt.Errorf("sync_dir is empty")
	}

	expanded := config.ExpandTilde(syncDir)
	if !filepath.IsAbs(expanded) {
		return fmt.Errorf("sync_dir %q must be absolute after tilde expansion", syncDir)
	}
	if err := localpath.MkdirAll(expanded, syncRootDirPerms); err != nil {
		return fmt.Errorf("create sync_dir %q: %w", syncDir, err)
	}

	return nil
}
