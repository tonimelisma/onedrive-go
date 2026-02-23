package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Worker pool size for parallel download/upload phases.
const workerPoolSize = 8

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

// Executor takes an ActionPlan and executes each action against the Graph API
// and local filesystem, producing []Outcome. It never writes to the database.
type Executor struct {
	items     ItemClient
	transfers TransferClient
	syncRoot  string     // absolute path to local sync directory
	driveID   driveid.ID // per-drive context (B-068)
	logger    *slog.Logger

	// Mutable state per Execute() call.
	baseline       *Baseline
	createdFolders map[string]string // relative path -> remote item ID

	// Injectable for testing.
	nowFunc   func() time.Time
	hashFunc  func(filePath string) (string, error)
	sleepFunc func(ctx context.Context, d time.Duration) error
}

// NewExecutor creates an Executor bound to a specific drive and sync root.
func NewExecutor(
	items ItemClient, transfers TransferClient, syncRoot string, driveID driveid.ID, logger *slog.Logger,
) *Executor {
	return &Executor{
		items:     items,
		transfers: transfers,
		syncRoot:  syncRoot,
		driveID:   driveID,
		logger:    logger,
		nowFunc:   time.Now,
		hashFunc:  computeQuickXorHash,
		sleepFunc: timeSleepExec,
	}
}

// Execute runs all nine phases of the action plan in order and returns
// the collected outcomes. Context cancellation stops between phases.
func (e *Executor) Execute(ctx context.Context, plan *ActionPlan, baseline *Baseline) ([]Outcome, error) {
	e.baseline = baseline
	e.createdFolders = make(map[string]string)

	e.logger.Info("executor starting",
		slog.Int("folder_creates", len(plan.FolderCreates)),
		slog.Int("moves", len(plan.Moves)),
		slog.Int("downloads", len(plan.Downloads)),
		slog.Int("uploads", len(plan.Uploads)),
		slog.Int("local_deletes", len(plan.LocalDeletes)),
		slog.Int("remote_deletes", len(plan.RemoteDeletes)),
		slog.Int("conflicts", len(plan.Conflicts)),
		slog.Int("synced_updates", len(plan.SyncedUpdates)),
		slog.Int("cleanups", len(plan.Cleanups)),
	)

	var outcomes []Outcome

	// Phase 1: Folder creates (sequential — parent before child).
	for i := range plan.FolderCreates {
		if ctx.Err() != nil {
			return outcomes, ctx.Err()
		}

		outcomes = append(outcomes, e.executeFolderCreate(ctx, &plan.FolderCreates[i]))
	}

	// Phase 2: Moves (sequential — order matters for renames).
	for i := range plan.Moves {
		if ctx.Err() != nil {
			return outcomes, ctx.Err()
		}

		outcomes = append(outcomes, e.executeMove(ctx, &plan.Moves[i]))
	}

	// Phase 3: Downloads (parallel worker pool).
	if err := e.executeParallel(ctx, plan.Downloads, e.executeDownload, &outcomes); err != nil {
		return outcomes, err
	}

	// Phase 4: Uploads (parallel worker pool).
	if err := e.executeParallel(ctx, plan.Uploads, e.executeUpload, &outcomes); err != nil {
		return outcomes, err
	}

	// Phase 5: Local deletes (sequential — depth-first order from planner).
	for i := range plan.LocalDeletes {
		if ctx.Err() != nil {
			return outcomes, ctx.Err()
		}

		outcomes = append(outcomes, e.executeLocalDelete(ctx, &plan.LocalDeletes[i]))
	}

	// Phase 6: Remote deletes (sequential).
	for i := range plan.RemoteDeletes {
		if ctx.Err() != nil {
			return outcomes, ctx.Err()
		}

		outcomes = append(outcomes, e.executeRemoteDelete(ctx, &plan.RemoteDeletes[i]))
	}

	// Phase 7: Conflicts (sequential).
	for i := range plan.Conflicts {
		if ctx.Err() != nil {
			return outcomes, ctx.Err()
		}

		outcomes = append(outcomes, e.executeConflict(ctx, &plan.Conflicts[i]))
	}

	// Phase 8: Synced updates (no I/O).
	for i := range plan.SyncedUpdates {
		outcomes = append(outcomes, e.executeSyncedUpdate(&plan.SyncedUpdates[i]))
	}

	// Phase 9: Cleanups (no I/O).
	for i := range plan.Cleanups {
		outcomes = append(outcomes, e.executeCleanup(&plan.Cleanups[i]))
	}

	e.logger.Info("executor complete", slog.Int("outcomes", len(outcomes)))

	return outcomes, nil
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

// createRemoteFolder creates a folder on OneDrive and records its ID.
func (e *Executor) createRemoteFolder(ctx context.Context, action *Action) Outcome {
	parentID, err := e.resolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionFolderCreate, err)
	}

	driveID := e.resolveDriveID(action)
	name := filepath.Base(action.Path)

	item, err := e.items.CreateFolder(ctx, driveID, parentID, name)
	if err != nil {
		return e.failedOutcome(action, ActionFolderCreate, fmt.Errorf("creating remote folder %s: %w", action.Path, err))
	}

	// Track for later phases (uploads need parent IDs).
	e.createdFolders[action.Path] = item.ID

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
	oldAbs := filepath.Join(e.syncRoot, action.Path)
	newAbs := filepath.Join(e.syncRoot, action.NewPath)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil { //nolint:mnd // standard dir perms
		return e.failedOutcome(action, ActionLocalMove, fmt.Errorf("creating parent for move target %s: %w", action.NewPath, err))
	}

	if err := os.Rename(oldAbs, newAbs); err != nil {
		return e.failedOutcome(action, ActionLocalMove, fmt.Errorf("renaming %s -> %s: %w", action.Path, action.NewPath, err))
	}

	e.logger.Debug("local move complete", slog.String("from", action.Path), slog.String("to", action.NewPath))

	return e.moveOutcome(action)
}

// executeRemoteMove renames/moves an item on OneDrive.
func (e *Executor) executeRemoteMove(ctx context.Context, action *Action) Outcome {
	driveID := e.resolveDriveID(action)

	newParentID, err := e.resolveParentID(action.NewPath)
	if err != nil {
		return e.failedOutcome(action, ActionRemoteMove, err)
	}

	newName := filepath.Base(action.NewPath)

	item, err := e.items.MoveItem(ctx, driveID, action.ItemID, newParentID, newName)
	if err != nil {
		return e.failedOutcome(action, ActionRemoteMove, fmt.Errorf("moving %s -> %s: %w", action.Path, action.NewPath, err))
	}

	e.logger.Debug("remote move complete",
		slog.String("from", action.Path),
		slog.String("to", action.NewPath),
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
	}

	return o
}

// executeCleanup signals baseline removal without I/O.
func (e *Executor) executeCleanup(action *Action) Outcome {
	return Outcome{
		Action:  ActionCleanup,
		Success: true,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveParentID determines the remote parent ID for a given relative path.
// Checks createdFolders first (for items under newly-created folders), then
// baseline, then falls back to "root" for top-level items.
func (e *Executor) resolveParentID(relPath string) (string, error) {
	parentDir := filepath.Dir(relPath)

	// Top-level item: parent is root.
	if parentDir == "." || parentDir == "" {
		return "root", nil
	}

	// Normalize to forward slashes for map lookups.
	parentDir = filepath.ToSlash(parentDir)

	// Check recently-created folders first.
	if id, ok := e.createdFolders[parentDir]; ok {
		return id, nil
	}

	// Check baseline.
	if entry, ok := e.baseline.ByPath[parentDir]; ok {
		return entry.ItemID, nil
	}

	return "", fmt.Errorf("sync: cannot resolve parent ID for %s (parent %s not in baseline or createdFolders)", relPath, parentDir)
}

// resolveDriveID returns the action's DriveID if set, otherwise the
// executor's default driveID (B-068: new local items have zero DriveID).
func (e *Executor) resolveDriveID(action *Action) driveid.ID {
	if !action.DriveID.IsZero() {
		return action.DriveID
	}

	return e.driveID
}

// executeParallel runs actions concurrently with a bounded worker pool.
// Fatal errors cancel the pool; other errors are collected in outcomes.
func (e *Executor) executeParallel(
	ctx context.Context, actions []Action, fn func(context.Context, *Action) Outcome, outcomes *[]Outcome,
) error {
	if len(actions) == 0 {
		return nil
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workerPoolSize)

	results := make([]Outcome, len(actions))

	for i := range actions {
		g.Go(func() error {
			out := fn(gctx, &actions[i])
			results[i] = out

			if !out.Success && out.Error != nil && classifyError(out.Error) == errClassFatal {
				return out.Error
			}

			return nil
		})
	}

	err := g.Wait()

	// Collect all results in order.
	for i := range results {
		// Skip zero-value results from goroutines that never ran.
		if results[i].Path != "" || results[i].Success {
			*outcomes = append(*outcomes, results[i])
		}
	}

	return err
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
	case 401:
		return errClassFatal
	case 507: //nolint:mnd // HTTP 507 Insufficient Storage
		return errClassFatal
	case 408, 412, 429, 509: //nolint:mnd // transient HTTP codes
		return errClassRetryable
	default:
		if code >= 500 {
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
		Path:    action.NewPath,
		OldPath: action.Path,
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

// timeSleepExec waits for the given duration or until the context is canceled.
func timeSleepExec(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
