package syncexec

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The executor's test seams: what the engine's tests reach in an executor,
// its configuration, its worker pool and its scope state. A ...ForTest
// function is used only by tests and the sync test-helper packages.

// Hooks are the test seams of the executor and of the worker pools built on
// its configuration, passed in when the configuration is built. The zero
// value is production behavior, and production code passes a Hooks value
// through without setting a field (R-6.10.29).
type Hooks struct {
	// CrashAt is called at each crash point (crash_points.go): an error
	// stops the operation there as a crash would (D-091).
	CrashAt func(point string) error
	// HashFile replaces the QuickXorHash of a file by path.
	HashFile func(ctx context.Context, filePath string) (string, error)
	// HashReader replaces the QuickXorHash of an open descriptor, which
	// destructive actions verify the exact inode they then remove or
	// replace through (D-010).
	HashReader func(ctx context.Context, r io.Reader) (string, error)
	// StoreAccessTimeout, when positive, shortens the bound of every store
	// access the executor runs detached from its action's context.
	StoreAccessTimeout time.Duration
	// OutcomeCommitTimeout, when positive, shortens the bound of a worker's
	// detached commit of a completed action's outcome.
	OutcomeCommitTimeout time.Duration
}

// hashFile hashes the file at filePath. It stops when ctx ends.
func (cfg *ExecutorConfig) hashFile(ctx context.Context, filePath string) (string, error) {
	hash := cfg.hooks.HashFile
	if hash == nil {
		hash = driveops.ComputeQuickXorHashContext
	}

	return hash(ctx, filePath)
}

// hashOpenFile hashes an open descriptor. It stops when ctx ends.
func (cfg *ExecutorConfig) hashOpenFile(ctx context.Context, r io.Reader) (string, error) {
	hash := cfg.hooks.HashReader
	if hash == nil {
		hash = driveops.ComputeQuickXorHashFromReaderContext
	}

	return hash(ctx, r)
}

// outcomeCommitBound is the bound of a worker's detached outcome commit:
// OutcomeCommitTimeout unless a test shortened it.
func (wp *WorkerPool) outcomeCommitBound() time.Duration {
	if wp.cfg.hooks.OutcomeCommitTimeout > 0 {
		return wp.cfg.hooks.OutcomeCommitTimeout
	}

	return syncdomain.OutcomeCommitTimeout
}

// NowForTest is the executor's clock reading.
func (cfg *ExecutorConfig) NowForTest() time.Time {
	return cfg.nowFunc()
}

// PathConvergenceForTest is the path convergence the executor checks
// remote paths with.
func (cfg *ExecutorConfig) PathConvergenceForTest() driveops.PathConvergence {
	return cfg.pathConvergence
}

// SetItemsForTest replaces the executor's item client.
func (cfg *ExecutorConfig) SetItemsForTest(items syncdomain.ItemClient) {
	cfg.items = items
}

// SetLoggerForTest replaces the executor's logger.
func (cfg *ExecutorConfig) SetLoggerForTest(logger *slog.Logger) {
	cfg.logger = logger
}

// LoggerForTest is the executor's logger.
func (cfg *ExecutorConfig) LoggerForTest() *slog.Logger {
	return cfg.logger
}

// UploadNewFileForTest uploads localPath as a new item (uploadNewFile).
func (e *Executor) UploadNewFileForTest(
	ctx context.Context, action *syncdomain.Action, driveID driveid.ID, parentID, source, localPath string,
) (*driveops.UploadResult, error) {
	return e.uploadNewFile(ctx, action, driveID, parentID, source, localPath)
}

// FinishIntoPlaceholderForTest finishes a create into its placeholder
// (finishIntoPlaceholder).
func (e *Executor) FinishIntoPlaceholderForTest(
	ctx context.Context, action *syncdomain.Action, driveID driveid.ID, parentID, itemID, source, localPath string,
) (*driveops.UploadResult, bool, error) {
	return e.finishIntoPlaceholder(ctx, action, driveID, parentID, itemID, source, localPath)
}

// ExecuteActionForTest runs one action on the calling goroutine, as a worker
// does (executeAction).
func (wp *WorkerPool) ExecuteActionForTest(ctx context.Context, ta *syncdomain.TrackedAction) {
	wp.executeAction(ctx, ta)
}

// DispatchActionForTest runs one action's executor call (dispatchAction).
func (wp *WorkerPool) DispatchActionForTest(ctx context.Context, exec *Executor, ta *syncdomain.TrackedAction) syncdomain.ActionOutcome {
	return wp.dispatchAction(ctx, exec, ta)
}

// HasWindowForTest reports whether scope detection holds a window for key.
func (ss *ScopeState) HasWindowForTest(key syncdomain.ScopeKey) bool {
	_, ok := ss.windows[key]
	return ok
}

// WindowPathCountForTest is how many distinct paths the window for key holds
// at now; open is false when there is no window.
func (ss *ScopeState) WindowPathCountForTest(key syncdomain.ScopeKey, now time.Time) (count int, open bool) {
	window, ok := ss.windows[key]
	if !ok {
		return 0, false
	}

	return window.uniqueCount(now), true
}
