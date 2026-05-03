package cli

import (
	"fmt"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

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
