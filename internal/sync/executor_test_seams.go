package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// The executor's test seams: what the engine's tests reach in an executor,
// its configuration, its worker pool and its scope state. A ...ForTest
// function is used only by tests and the sync test-helper packages.

// SetCrashHookForTest installs the hook the executor's crash points call.
func (cfg *ExecutorConfig) SetCrashHookForTest(hook func(point string) error) {
	cfg.crashAt = hook
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
func (cfg *ExecutorConfig) SetItemsForTest(items ItemClient) {
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

// SetStoreAccessTimeoutForTest bounds the executor's detached store access.
func (cfg *ExecutorConfig) SetStoreAccessTimeoutForTest(timeout time.Duration) {
	cfg.storeAccessTimeout = timeout
}

// UploadNewFileForTest uploads localPath as a new item (uploadNewFile).
func (e *Executor) UploadNewFileForTest(
	ctx context.Context, action *Action, driveID driveid.ID, parentID, source, localPath string,
) (*driveops.UploadResult, error) {
	return e.uploadNewFile(ctx, action, driveID, parentID, source, localPath)
}

// FinishIntoPlaceholderForTest finishes a create into its placeholder
// (finishIntoPlaceholder).
func (e *Executor) FinishIntoPlaceholderForTest(
	ctx context.Context, action *Action, driveID driveid.ID, parentID, itemID, source, localPath string,
) (*driveops.UploadResult, bool, error) {
	return e.finishIntoPlaceholder(ctx, action, driveID, parentID, itemID, source, localPath)
}

// ExecuteActionForTest runs one action on the calling goroutine, as a worker
// does (executeAction).
func (wp *WorkerPool) ExecuteActionForTest(ctx context.Context, ta *TrackedAction) {
	wp.executeAction(ctx, ta)
}

// DispatchActionForTest runs one action's executor call (dispatchAction).
func (wp *WorkerPool) DispatchActionForTest(ctx context.Context, exec *Executor, ta *TrackedAction) ActionOutcome {
	return wp.dispatchAction(ctx, exec, ta)
}

// SetOutcomeCommitTimeoutForTest bounds the detached commit of an outcome.
func (wp *WorkerPool) SetOutcomeCommitTimeoutForTest(timeout time.Duration) {
	wp.outcomeCommitTimeout = timeout
}

// HasWindowForTest reports whether scope detection holds a window for key.
func (ss *ScopeState) HasWindowForTest(key ScopeKey) bool {
	_, ok := ss.windows[key]
	return ok
}

// WindowPathCountForTest is how many distinct paths the window for key holds
// at now; open is false when there is no window.
func (ss *ScopeState) WindowPathCountForTest(key ScopeKey, now time.Time) (count int, open bool) {
	window, ok := ss.windows[key]
	if !ok {
		return 0, false
	}

	return window.uniqueCount(now), true
}
