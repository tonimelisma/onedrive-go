package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// graphRootID is the Graph API parent reference for top-level items.
// Distinct from strRoot in types.go which serializes the ItemTypeRoot enum.
const graphRootID = "root"

// Retry parameters for executor-level retries (separate from graph client retries).
const (
	executorMaxRetries = 3
	executorBaseDelay  = 1 * time.Second
	executorBackoffExp = 2.0
	executorJitter     = 0.25
)

// errClass classifies an error for the executor's retry/skip/fatal decision.
type errClass int

const (
	errClassSkip      errClass = iota // non-retryable, skip this action
	errClassRetryable                 // transient, retry with backoff
	errClassFatal                     // abort the entire sync cycle
)

// ExecutorConfig holds the immutable configuration for creating per-call
// Executor instances. Separated from mutable state to prevent temporal
// coupling and enable Phase 5 thread safety.
type ExecutorConfig struct {
	items     ItemClient
	downloads Downloader
	uploads   Uploader
	syncRoot  string     // absolute path to local sync directory
	driveID   driveid.ID // per-drive context (B-068)
	logger    *slog.Logger

	// sessionStore persists upload session URLs for cross-crash resume.
	// When non-nil and uploads satisfies SessionUploader, the executor uses
	// session-based uploads for large files (>SimpleUploadMaxSize).
	sessionStore *SessionStore

	// transferMgr handles unified download/upload with resume.
	transferMgr *TransferManager

	// Injectable for testing.
	nowFunc   func() time.Time
	hashFunc  func(filePath string) (string, error)
	sleepFunc func(ctx context.Context, d time.Duration) error
	trashFunc func(absPath string) error // nil = permanent delete (os.Remove)
}

// Executor executes individual actions against the Graph API and local
// filesystem, producing Outcomes. Created per-action via NewExecution.
// Thread-safe: all mutable state is per-call, no shared mutation.
type Executor struct {
	*ExecutorConfig
	baseline *Baseline
}

// NewExecutorConfig creates an immutable executor configuration bound to a
// specific drive and sync root. Use NewExecution to create per-call executors.
func NewExecutorConfig(
	items ItemClient, downloads Downloader, uploads Uploader,
	syncRoot string, driveID driveid.ID, logger *slog.Logger,
) *ExecutorConfig {
	cfg := &ExecutorConfig{
		items:     items,
		downloads: downloads,
		uploads:   uploads,
		syncRoot:  syncRoot,
		driveID:   driveID,
		logger:    logger,
		nowFunc:   time.Now,
		hashFunc:  computeQuickXorHash,
		sleepFunc: timeSleep,
	}

	cfg.transferMgr = NewTransferManager(downloads, uploads, nil, logger)

	return cfg
}

// NewExecution creates an ephemeral Executor for a single action execution.
// Baseline is used for parent ID resolution (thread-safe via locked accessors).
func NewExecution(cfg *ExecutorConfig, bl *Baseline) *Executor {
	return &Executor{
		ExecutorConfig: cfg,
		baseline:       bl,
	}
}

// executeFolderCreate dispatches to local or remote folder creation.
func (e *Executor) executeFolderCreate(ctx context.Context, action *Action) Outcome {
	if action.CreateSide == CreateLocal {
		return e.createLocalFolder(action)
	}

	return e.createRemoteFolder(ctx, action)
}

// createLocalFolder creates a directory on the local filesystem.
func (e *Executor) createLocalFolder(action *Action) Outcome {
	absPath := filepath.Join(e.syncRoot, action.Path)

	if err := os.MkdirAll(absPath, 0o755); err != nil { //nolint:mnd // standard dir perms
		return e.failedOutcome(action, ActionFolderCreate, fmt.Errorf("creating local folder %s: %w", action.Path, err))
	}

	// Set mtime from remote if available.
	if action.View != nil && action.View.Remote != nil && action.View.Remote.Mtime != 0 {
		mtime := time.Unix(0, action.View.Remote.Mtime)
		if err := os.Chtimes(absPath, mtime, mtime); err != nil {
			e.logger.Warn("failed to set folder mtime", slog.String("path", action.Path), slog.String("error", err.Error()))
		}
	}

	e.logger.Debug("created local folder", slog.String("path", action.Path))

	return e.folderOutcome(action)
}

// createRemoteFolder creates a folder on OneDrive. The DAG guarantees parent
// folder creates complete before children, so resolveParentID finds the parent
// in the baseline (committed by CommitOutcome before tracker.Complete).
func (e *Executor) createRemoteFolder(ctx context.Context, action *Action) Outcome {
	parentID, err := e.resolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionFolderCreate, err)
	}

	driveID := e.resolveDriveID(action)
	name := filepath.Base(action.Path)

	var item *graph.Item

	err = e.withRetry(ctx, "create folder "+action.Path, func() error {
		var createErr error
		item, createErr = e.items.CreateFolder(ctx, driveID, parentID, name)

		return createErr
	})
	if err != nil {
		return e.failedOutcome(action, ActionFolderCreate, fmt.Errorf("creating remote folder %s: %w", action.Path, err))
	}

	e.logger.Debug("created remote folder",
		slog.String("path", action.Path),
		slog.String("item_id", item.ID),
	)

	return Outcome{
		Action:     ActionFolderCreate,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     item.ID,
		ParentID:   parentID,
		ItemType:   ItemTypeFolder,
		RemoteHash: selectHash(item),
		ETag:       item.ETag,
	}
}

// executeMove dispatches to local or remote move.
func (e *Executor) executeMove(ctx context.Context, action *Action) Outcome {
	if action.Type == ActionLocalMove {
		return e.executeLocalMove(action)
	}

	return e.executeRemoteMove(ctx, action)
}

// executeLocalMove renames a local file/folder.
func (e *Executor) executeLocalMove(action *Action) Outcome {
	oldAbs := filepath.Join(e.syncRoot, action.OldPath)
	newAbs := filepath.Join(e.syncRoot, action.Path)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil { //nolint:mnd // standard dir perms
		return e.failedOutcome(action, ActionLocalMove, fmt.Errorf("creating parent for move target %s: %w", action.Path, err))
	}

	if err := os.Rename(oldAbs, newAbs); err != nil {
		return e.failedOutcome(action, ActionLocalMove, fmt.Errorf("renaming %s -> %s: %w", action.OldPath, action.Path, err))
	}

	e.logger.Debug("local move complete", slog.String("from", action.OldPath), slog.String("to", action.Path))

	return e.moveOutcome(action)
}

// executeRemoteMove renames/moves an item on OneDrive.
func (e *Executor) executeRemoteMove(ctx context.Context, action *Action) Outcome {
	driveID := e.resolveDriveID(action)

	newParentID, err := e.resolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionRemoteMove, err)
	}

	newName := filepath.Base(action.Path)

	var item *graph.Item

	err = e.withRetry(ctx, "remote move "+action.OldPath, func() error {
		var moveErr error
		item, moveErr = e.items.MoveItem(ctx, driveID, action.ItemID, newParentID, newName)

		return moveErr
	})
	if err != nil {
		return e.failedOutcome(action, ActionRemoteMove, fmt.Errorf("moving %s -> %s: %w", action.OldPath, action.Path, err))
	}

	e.logger.Debug("remote move complete",
		slog.String("from", action.OldPath),
		slog.String("to", action.Path),
		slog.String("item_id", item.ID),
	)

	o := e.moveOutcome(action)
	o.ItemID = item.ID
	o.ETag = item.ETag

	return o
}

// executeSyncedUpdate produces an Outcome from a PathView without I/O.
func (e *Executor) executeSyncedUpdate(action *Action) Outcome {
	o := Outcome{
		Action:   ActionUpdateSynced,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: ItemTypeFile,
	}

	if action.View != nil {
		if action.View.Remote != nil {
			o.RemoteHash = action.View.Remote.Hash
			o.Size = action.View.Remote.Size
			o.ETag = action.View.Remote.ETag
			o.ParentID = action.View.Remote.ParentID
			o.ItemType = action.View.Remote.ItemType
		}

		if action.View.Local != nil {
			o.LocalHash = action.View.Local.Hash
			o.Mtime = action.View.Local.Mtime
		}

		// Fall back to baseline ItemType when Remote is absent or had zero value.
		if o.ItemType == ItemTypeFile && action.View.Baseline != nil && action.View.Baseline.ItemType != ItemTypeFile {
			o.ItemType = action.View.Baseline.ItemType
		}
	}

	return o
}

// executeCleanup signals baseline removal without I/O.
func (e *Executor) executeCleanup(action *Action) Outcome {
	return Outcome{
		Action:   ActionCleanup,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: resolveActionItemType(action),
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveActionItemType extracts ItemType from the action's View, skipping
// zero values (ItemTypeFile) to find the actual type. Checks Remote → Baseline
// → Local, defaulting to ItemTypeFile if all are zero or View is nil.
func resolveActionItemType(action *Action) ItemType {
	if action.View != nil {
		if action.View.Remote != nil && action.View.Remote.ItemType != ItemTypeFile {
			return action.View.Remote.ItemType
		}

		if action.View.Baseline != nil && action.View.Baseline.ItemType != ItemTypeFile {
			return action.View.Baseline.ItemType
		}

		if action.View.Local != nil && action.View.Local.ItemType != ItemTypeFile {
			return action.View.Local.ItemType
		}
	}

	return ItemTypeFile
}

// resolveParentID determines the remote parent ID for a given relative path.
// Checks baseline (which includes items committed by earlier DAG actions),
// then falls back to "root" for top-level items.
func (e *Executor) resolveParentID(relPath string) (string, error) {
	parentDir := filepath.Dir(relPath)

	// Top-level item: parent is root.
	if parentDir == "." || parentDir == "" {
		return graphRootID, nil
	}

	// Normalize to forward slashes for map lookups.
	parentDir = filepath.ToSlash(parentDir)

	// Check baseline (DAG edges ensure parent folder creates commit before children).
	if entry, ok := e.baseline.GetByPath(parentDir); ok {
		return entry.ItemID, nil
	}

	return "", fmt.Errorf("sync: cannot resolve parent ID for %s (parent %s not in baseline)", relPath, parentDir)
}

// resolveDriveID returns the action's DriveID if set, otherwise the
// executor's default driveID (B-068: new local items have zero DriveID).
func (e *Executor) resolveDriveID(action *Action) driveid.ID {
	if !action.DriveID.IsZero() {
		return action.DriveID
	}

	return e.driveID
}

// classifyError determines whether an error is fatal, retryable, or skippable.
func classifyError(err error) errClass {
	if err == nil {
		return errClassSkip
	}

	// Context cancellation is always fatal.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errClassFatal
	}

	// GraphError carries a status code — classify by code for precision.
	// This must come before sentinel checks because 507 (fatal) wraps
	// ErrServerError (retryable), and the specific code wins.
	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return classifyStatusCode(ge.StatusCode)
	}

	// Bare sentinel errors (not wrapped in GraphError).
	if errors.Is(err, graph.ErrUnauthorized) {
		return errClassFatal
	}

	if errors.Is(err, graph.ErrThrottled) || errors.Is(err, graph.ErrServerError) {
		return errClassRetryable
	}

	// Non-graph errors (filesystem, etc.) are skippable.
	return errClassSkip
}

// classifyStatusCode maps HTTP status codes to error classes.
func classifyStatusCode(code int) errClass {
	switch code {
	case http.StatusUnauthorized:
		return errClassFatal
	case http.StatusInsufficientStorage:
		return errClassFatal
	case http.StatusRequestTimeout, http.StatusPreconditionFailed,
		http.StatusTooManyRequests, 509: //nolint:mnd // HTTP 509 Bandwidth Limit Exceeded (no stdlib constant)
		return errClassRetryable
	default:
		if code >= http.StatusInternalServerError {
			return errClassRetryable
		}

		return errClassSkip
	}
}

// withRetry retries a function with exponential backoff on retryable errors.
func (e *Executor) withRetry(ctx context.Context, desc string, fn func() error) error {
	var lastErr error

	for attempt := range executorMaxRetries + 1 {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if classifyError(lastErr) != errClassRetryable {
			return lastErr
		}

		if attempt < executorMaxRetries {
			backoff := calcExecBackoff(attempt)
			e.logger.Warn("retrying after transient error",
				slog.String("operation", desc),
				slog.Int("attempt", attempt+1),
				slog.Duration("backoff", backoff),
				slog.String("error", lastErr.Error()),
			)

			if err := e.sleepFunc(ctx, backoff); err != nil {
				return fmt.Errorf("sync: retry wait canceled: %w", err)
			}
		}
	}

	return lastErr
}

// calcExecBackoff computes exponential backoff with jitter for executor retries.
func calcExecBackoff(attempt int) time.Duration {
	backoff := float64(executorBaseDelay) * math.Pow(executorBackoffExp, float64(attempt))
	jitter := backoff * executorJitter * (rand.Float64()*2 - 1) //nolint:gosec // jitter does not need crypto rand
	backoff += jitter

	return time.Duration(backoff)
}

// failedOutcome builds an Outcome for a failed action.
func (e *Executor) failedOutcome(action *Action, actionType ActionType, err error) Outcome {
	e.logger.Warn("action failed",
		slog.String("action", actionType.String()),
		slog.String("path", action.Path),
		slog.String("error", err.Error()),
	)

	return Outcome{
		Action:  actionType,
		Success: false,
		Error:   err,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

// folderOutcome builds a successful Outcome for a local folder create.
func (e *Executor) folderOutcome(action *Action) Outcome {
	o := Outcome{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: ItemTypeFolder,
	}

	if action.View != nil && action.View.Remote != nil {
		o.ItemID = action.View.Remote.ItemID
		o.ParentID = action.View.Remote.ParentID
		o.ETag = action.View.Remote.ETag
	}

	return o
}

// moveOutcome builds a successful Outcome for a move action.
func (e *Executor) moveOutcome(action *Action) Outcome {
	o := Outcome{
		Action:  action.Type,
		Success: true,
		Path:    action.Path,
		OldPath: action.OldPath,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}

	if action.View != nil {
		if action.View.Remote != nil {
			o.RemoteHash = action.View.Remote.Hash
			o.Size = action.View.Remote.Size
			o.ETag = action.View.Remote.ETag
			o.ItemType = action.View.Remote.ItemType
		}

		if action.View.Local != nil {
			o.LocalHash = action.View.Local.Hash
			o.Mtime = action.View.Local.Mtime
		}
	}

	return o
}
