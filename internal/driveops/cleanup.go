package driveops

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// TransferArtifactCleanupOptions controls local transfer artifact cleanup.
// SkipDirs entries are rooted slash paths relative to the sync root and are
// left untouched during partial-file cleanup. IncludedDirs narrows cleanup to
// configured sync-visible containers and their descendants, with the same
// root-relative ancestor/container semantics as sync include filters.
// IncludeJunkPartials also removes arbitrary user-named *.partial files.
type TransferArtifactCleanupOptions struct {
	SkipDirs            []string
	IncludedDirs        []string
	IncludeJunkPartials bool
}

// TransferArtifactCleanup is what one post-sync cleanup did. Each half goes
// on past what it cannot remove, so its count and its error can both be set.
type TransferArtifactCleanup struct {
	PartialsRemoved int
	PartialsErr     error
	SessionsRemoved int
	SessionsErr     error
}

// CleanTransferArtifacts runs the non-critical post-sync housekeeping:
//   - deletes orphaned owned partial files under the sync tree
//   - deletes expired upload session files from the session store, if any
//
// It never fails a sync run, and it reports no failure itself: the engine that
// called it does, in one line per cleanup and once per failure streak.
func CleanTransferArtifacts(
	tree *synctree.Root,
	sessionStore *SessionStore,
	logger *slog.Logger,
	opts TransferArtifactCleanupOptions,
) TransferArtifactCleanup {
	var result TransferArtifactCleanup

	result.PartialsRemoved, result.PartialsErr = CleanStalePartials(tree, logger, opts)
	if sessionStore != nil {
		result.SessionsRemoved, result.SessionsErr = sessionStore.CleanStale(StaleSessionAge)
	}

	return result
}
