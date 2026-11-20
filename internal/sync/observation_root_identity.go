package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// The local root identity check is an observation precondition: it decides
// whether a local scan may be trusted at all, before any snapshot is produced.
//
// Every engine has an expected identity. A shortcut child engine gets it from
// its parent, which owns the alias folder. A primary engine gets it from its
// own store: the fingerprint recorded with the first committed local
// snapshot. Without that, an unmounted disk, a wiped tmpfs, or a symlinked
// root re-pointed at an empty folder scans successfully with zero entries,
// and every synced file is planned as a remote delete (D-001).

// ResolveSyncRootPath returns the folder the sync tree is opened on. When the
// configured sync folder is itself a symbolic link, the tree is opened on its
// target, resolved once at startup: re-pointing the link while the engine runs
// must not silently move every later operation to a different folder. Only the
// final component is resolved, so platform aliases in ancestors (such as
// macOS's /var -> /private/var) keep the configured spelling.
func ResolveSyncRootPath(syncRoot string) (string, error) {
	info, err := localpath.Lstat(syncRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return syncRoot, nil
		}
		return "", fmt.Errorf("checking sync folder %s: %w", syncRoot, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return syncRoot, nil
	}

	resolved, err := filepath.EvalSymlinks(syncRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolving symbolic link %s: %w", ErrMountRootUnavailable, syncRoot, err)
	}

	return resolved, nil
}
