package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// deleteStateDB deletes the state DB family at statePath for a purge, under
// the locks an engine using that store holds (changeDriveStateUnderLock):
// the folder at syncDir ("" when it is no longer known), then the store. It
// runs after the drive left the config and a running watch was told, so the
// watch's reload has retired the drive's runner. It reports whether any file
// was removed.
//
// While an engine still has the store open (a runner that did not stop in
// time, another configuration's engine sharing the data directory), the
// store is kept: a purge never deletes it under a live engine. The returned
// error says so and names rerun, the command that finishes the purge once
// that sync has stopped.
func deleteStateDB(ctx context.Context, statePath, syncDir, rerun string, logger *slog.Logger) (bool, error) {
	removed := false
	refused := func(inUse error) error {
		return fmt.Errorf("kept the sync state, because %w; once that sync has stopped, run '%s' again to delete it",
			inUse, rerun)
	}
	err := changeDriveStateUnderLock(ctx, syncDir, statePath, refused, logger, func() error {
		var removeErr error
		removed, removeErr = removeStateDBFamily(statePath)
		return removeErr
	})
	if err != nil {
		return removed, err
	}
	if removed {
		logger.Info("removed state database family", "path", statePath)
	}

	return removed, nil
}

// removeStateDBFamily removes every file of the store at statePath
// (config.StateDBFamily), each through the managed-path boundary, and
// reports whether any was there.
func removeStateDBFamily(statePath string) (bool, error) {
	removedAny := false

	for _, candidate := range config.StateDBFamily(statePath) {
		removedPath, err := removeManagedPath(candidate)
		if err != nil {
			return removedAny, fmt.Errorf("delete the sync state: %w", err)
		}
		if removedPath {
			removedAny = true
		}
	}

	return removedAny, nil
}
