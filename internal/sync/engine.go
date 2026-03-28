package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// trialEntry tracks a pending trial action in the pipeline. Created when
// the trial timer fires and reobserve succeeds, consumed when the planner's
// fresh action arrives at admitAndDispatch or admitReady.
type trialEntry struct {
	scopeKey synctypes.ScopeKey
	created  time.Time
}

// watchState bundles all watch-mode-only state. Nil in one-shot mode.
// Active scope runtime state lives here as a plain engine-owned slice; the
// database remains the durable record for restart/recovery.
type watchState struct {
	// Active scope blocks owned by the watch control flow. The slice is tiny
	// (usually 0-5 entries), so linear scans keep the logic simple and avoid a
	// second mirrored subsystem.
	activeScopes []synctypes.ScopeBlock

	// Scope detection — sliding window failure tracking.
	scopeState *syncdispatch.ScopeState

	// Event buffer — watch-loop retry/trial work injects events via buf.Add().
	buf *syncobserve.Buffer

	// Big-delete protection: rolling counter + external change detection.
	// deleteCounter is nil even in watch mode when force=true.
	deleteCounter   *syncdispatch.DeleteCounter
	lastDataVersion int64

	// Trial management (watch-loop-owned state).
	trialPending map[string]trialEntry
	trialTimer   *time.Timer

	// Retry timer — watch loop retrier sweeps sync_failures on each tick.
	retryTimer   *time.Timer
	retryTimerCh chan struct{} // persistent, buffered(1)

	// Observer references — set in startObservers, nil'd on Close.
	remoteObs *syncobserve.RemoteObserver
	localObs  *syncobserve.LocalObserver

	// Monotonic action ID counter owned by the watch control flow. Prevents
	// ID collisions across batches without introducing cross-goroutine sync.
	nextActionID int64

	// Throttling: tracks last recheckPermissions call time (R-2.10.9).
	lastPermRecheck time.Time

	// Deduplication: caches last actionable issue count for watch summaries.
	lastSummaryTotal int

	// Async reconciliation guard — prevents concurrent full reconciliations.
	// CompareAndSwap(false, true) at start, Store(false) in defer.
	// Zero value (false) is the correct initial state.
	reconcileRunning atomic.Bool

	// afterReconcileCommit is a test-only hook called after CommitObservation
	// succeeds in runFullReconciliationAsync. Nil in production. Allows tests
	// to inject actions (e.g. context cancellation) at an otherwise unreachable
	// point between commit and buffer feeding.
	afterReconcileCommit func()
}

// Engine orchestrates a complete sync pass: observe → plan → execute → commit.
// Single-drive only; multi-drive orchestration is handled by the Orchestrator.
type Engine struct {
	baseline           *syncstore.SyncStore
	planner            *syncplan.Planner
	execCfg            *syncexec.ExecutorConfig
	fetcher            synctypes.DeltaFetcher
	driveVerifier      synctypes.DriveVerifier      // optional (B-074)
	folderDelta        synctypes.FolderDeltaFetcher // optional: for shortcut observation (6.4b)
	recursiveLister    synctypes.RecursiveLister    // optional: for shortcut observation (6.4b)
	permHandler        *PermissionHandler           // encapsulates all permission logic (6.4c)
	syncRoot           string
	driveID            driveid.ID
	logger             *slog.Logger
	sessionStore       *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers    int                    // goroutine count for the worker pool
	checkWorkers       int                    // goroutine limit for parallel file hashing
	bigDeleteThreshold int                    // from config; 0 means use default
	minFreeSpace       int64                  // startup disk-scope revalidation threshold
	diskAvailableFn    func(string) (uint64, error)

	// watchShortcuts holds the latest shortcuts for permission and shortcut
	// handling. Stays on Engine because both one-shot result draining and the
	// watch loop need access, while setShortcuts is also called from RunOnce
	// where e.watch is nil.
	watchShortcuts   []synctypes.Shortcut
	watchShortcutsMu stdsync.RWMutex

	// depGraph is the pure dependency graph. Tracks action dependencies and
	// readiness. Set during executePlan (one-shot) or initWatchPipeline
	// (watch mode).
	depGraph *syncdispatch.DepGraph

	// readyCh feeds admitted actions to the worker pool. In watch mode actions
	// pass through active-scope admission first; one-shot mode bypasses that
	// runtime check. Workers read from this channel via the WorkerPool.
	readyCh chan *synctypes.TrackedAction

	// trialCh is the persistent, buffered(1) channel for trial timer signals.
	// Created in NewEngine. In one-shot mode, no writer → harmlessly blocks
	// in select.
	trialCh chan struct{}

	// watch bundles all watch-mode-only state. Nil in one-shot mode.
	watch *watchState

	// Test/debug-only invariant checks. Production keeps this disabled;
	// tests enable it to catch scope lifecycle regressions immediately.
	assertScopeInvariants bool
	debugEventHook        func(engineDebugEvent)

	// Engine-owned result counters. Workers are pure executors — the engine
	// classifies results and owns all final disposition counts (R-6.8.9).
	succeeded    atomic.Int32
	failed       atomic.Int32
	syncErrors   []error
	syncErrorsMu stdsync.Mutex

	// nowFn is the engine's clock. Defaults to time.Now. Tests inject a
	// controllable clock for deterministic trial timer and scope timing.
	nowFn func() time.Time

	// localWatcherFactory overrides the default fsnotify watcher factory
	// for the local observer. Tests inject a mock factory to simulate
	// inotify watch limit exhaustion (ENOSPC).
	localWatcherFactory func() (syncobserve.FsWatcher, error)
}

// NewEngine creates an Engine, initializing the SyncStore (which opens
// the SQLite database and applies the canonical schema). Returns an error if DB init fails
// or if DriveID is zero (indicates a config/login issue).
func NewEngine(ctx context.Context, cfg *synctypes.EngineConfig) (*Engine, error) {
	if cfg.DriveID.IsZero() {
		return nil, fmt.Errorf("sync: engine requires non-zero drive ID")
	}

	bm, err := syncstore.NewSyncStore(ctx, cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}

	execCfg := syncexec.NewExecutorConfig(cfg.Items, cfg.Downloads, cfg.Uploads, cfg.SyncRoot, cfg.DriveID, cfg.Logger)

	if cfg.UseLocalTrash {
		execCfg.SetTrashFunc(syncstore.DefaultTrashFunc)
	}

	// Construct sessionStore and TransferManager together so the TM is
	// immutable after creation (no post-hoc field mutation). Disk space
	// checking is configured via WithDiskCheck so every download (sync
	// and CLI) gets automatic protection (R-6.2.6).
	var sessionStore *driveops.SessionStore
	if cfg.DataDir != "" {
		sessionStore = driveops.NewSessionStore(cfg.DataDir, cfg.Logger)
	}

	execCfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads, cfg.Uploads, sessionStore, cfg.Logger,
		driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable),
	))

	// Default threshold if not set by config.
	bdThreshold := cfg.BigDeleteThreshold
	if bdThreshold == 0 {
		bdThreshold = synctypes.DefaultBigDeleteThreshold
	}

	e := &Engine{
		baseline:           bm,
		planner:            syncplan.NewPlanner(cfg.Logger),
		execCfg:            execCfg,
		fetcher:            cfg.Fetcher,
		driveVerifier:      cfg.DriveVerifier,
		folderDelta:        cfg.FolderDelta,
		recursiveLister:    cfg.RecursiveLister,
		sessionStore:       sessionStore,
		syncRoot:           cfg.SyncRoot,
		driveID:            cfg.DriveID,
		logger:             cfg.Logger,
		transferWorkers:    cfg.TransferWorkers,
		checkWorkers:       cfg.CheckWorkers,
		bigDeleteThreshold: bdThreshold,
		minFreeSpace:       cfg.MinFreeSpace,
		diskAvailableFn:    driveops.DiskAvailable,
		nowFn:              time.Now,
		trialCh:            make(chan struct{}, 1),
	}

	e.permHandler = &PermissionHandler{
		baseline:    e.baseline,
		permChecker: cfg.PermChecker,
		syncRoot:    cfg.SyncRoot,
		driveID:     cfg.DriveID,
		logger:      cfg.Logger,
		nowFn:       e.nowFunc,
	}

	return e, nil
}

// Close releases resources held by the engine. Nil-safe for observer
// references set during RunWatch, cleans stale upload sessions, and
// closes the database connection last. Safe to call more than once.
func (e *Engine) Close(ctx context.Context) error {
	// Nil out observer references to prevent dangling reads after Close.
	if e.watch != nil {
		e.watch.remoteObs = nil
		e.watch.localObs = nil
	}

	// Clean stale upload session files (best-effort).
	if e.sessionStore != nil {
		if n, err := e.sessionStore.CleanStale(driveops.StaleSessionAge); err != nil {
			e.logger.Warn("failed to clean stale sessions on close",
				slog.String("error", err.Error()),
			)
		} else if n > 0 {
			e.logger.Info("cleaned stale upload sessions on close",
				slog.Int("deleted", n),
			)
		}
	}

	if e.baseline == nil {
		return nil
	}

	if err := e.baseline.Close(ctx); err != nil {
		return fmt.Errorf("sync: closing state store: %w", err)
	}

	return nil
}

// verifyDriveIdentity checks that the configured drive ID matches the remote
// API (B-074). Returns nil if no synctypes.DriveVerifier is configured (optional check).
func (e *Engine) verifyDriveIdentity(ctx context.Context) error {
	if e.driveVerifier == nil {
		return nil
	}

	drive, err := e.driveVerifier.Drive(ctx, e.driveID)
	if err != nil {
		return fmt.Errorf("sync: verifying drive identity: %w", err)
	}

	if drive.ID != e.driveID {
		return fmt.Errorf("sync: drive ID mismatch: configured %s, remote returned %s", e.driveID, drive.ID)
	}

	e.logger.Info("drive identity verified",
		slog.String("drive_id", drive.ID.String()),
		slog.String("drive_type", drive.DriveType),
	)

	return nil
}

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Observe remote (skip if upload-only)
//  3. Observe local (skip if download-only)
//  4. Buffer and flush changes
//  5. Early return if no changes
//  6. Plan actions (flat list + dependency edges)
//  7. Return early if dry-run
//  8. Build DepGraph, start worker pool
//  9. Wait for completion, commit delta token

// ListConflicts returns all unresolved conflicts from the database.
func (e *Engine) ListConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	conflicts, err := e.baseline.ListConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing unresolved conflicts: %w", err)
	}

	return conflicts, nil
}

// ListAllConflicts returns all conflicts (resolved and unresolved) from the
// database. Used by 'conflicts --history'.
func (e *Engine) ListAllConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	conflicts, err := e.baseline.ListAllConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing conflict history: %w", err)
	}

	return conflicts, nil
}

// ResolveConflict resolves a single conflict by ID. For keep_both, this is
// a DB-only update. For keep_local, the local file is uploaded to overwrite
// the remote. For keep_remote, the remote file is downloaded to overwrite
// the local. The conflict record and baseline are updated atomically.
func (e *Engine) ResolveConflict(ctx context.Context, conflictID, resolution string) error {
	c, err := e.baseline.GetConflict(ctx, conflictID)
	if err != nil {
		return fmt.Errorf("sync: loading conflict %s: %w", conflictID, err)
	}

	switch resolution {
	case synctypes.ResolutionKeepBoth:
		// Update baseline entries for both the original file and the conflict
		// copy so the next sync sees them as unchanged. Without this, the
		// scanner would flag the original (stale hash) and the conflict copy
		// (no baseline entry) as needing action.
		if err := e.resolveKeepBoth(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, synctypes.ResolutionKeepBoth, err)
		}

		if err := e.baseline.ResolveConflict(ctx, c.ID, resolution); err != nil {
			return fmt.Errorf("sync: marking conflict %s resolved as %s: %w", c.ID, resolution, err)
		}

		return nil

	case synctypes.ResolutionKeepLocal:
		if err := e.resolveKeepLocal(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, synctypes.ResolutionKeepLocal, err)
		}

		if err := e.baseline.ResolveConflict(ctx, c.ID, resolution); err != nil {
			return fmt.Errorf("sync: marking conflict %s resolved as %s: %w", c.ID, resolution, err)
		}

		return nil

	case synctypes.ResolutionKeepRemote:
		if err := e.resolveKeepRemote(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, synctypes.ResolutionKeepRemote, err)
		}

		if err := e.baseline.ResolveConflict(ctx, c.ID, resolution); err != nil {
			return fmt.Errorf("sync: marking conflict %s resolved as %s: %w", c.ID, resolution, err)
		}

		return nil

	default:
		return fmt.Errorf("sync: unknown resolution strategy %q", resolution)
	}
}

// resolveKeepLocal restores the conflict copy (which holds the user's local
// version) to the original path and uploads it. During conflict detection,
// ExecuteConflict renamed the local file to a conflict copy and downloaded
// the remote content to the original path. "Keep local" means the user wants
// their pre-conflict local content — which lives in the conflict copy.
func (e *Engine) resolveKeepLocal(ctx context.Context, c *synctypes.ConflictRecord) error {
	absPath := filepath.Join(e.syncRoot, c.Path)
	pattern := syncexec.ConflictCopyGlob(absPath)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob conflict copies for keep-local: %w", err)
	}

	// Restore the first conflict copy to the original path. If multiple
	// conflict copies exist (shouldn't normally happen), the first one is
	// the user's local version.
	if len(matches) > 0 {
		if renameErr := os.Rename(matches[0], absPath); renameErr != nil {
			return fmt.Errorf("restoring conflict copy to %s: %w", c.Path, renameErr)
		}

		e.logger.Debug("restored conflict copy for keep-local",
			slog.String("from", filepath.Base(matches[0])),
			slog.String("to", c.Path),
		)
	}

	return e.resolveTransfer(ctx, c, synctypes.ActionUpload)
}

// resolveKeepRemote downloads the remote file to overwrite the local version.
func (e *Engine) resolveKeepRemote(ctx context.Context, c *synctypes.ConflictRecord) error {
	return e.resolveTransfer(ctx, c, synctypes.ActionDownload)
}

// resolveTransfer executes a single transfer (upload or download) for conflict
// resolution and commits the result to the baseline.
//
// Hash verification is intentionally skipped here (B-153): conflict resolution
// is a user-initiated override ("keep mine" / "keep theirs") where the intent
// is to force one side to match the other, not to verify content integrity.
// The executor's per-action functions already verify hashes for normal syncs.
func (e *Engine) resolveTransfer(ctx context.Context, c *synctypes.ConflictRecord, actionType synctypes.ActionType) error {
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline for resolve: %w", err)
	}

	exec := syncexec.NewExecution(e.execCfg, bl)

	action := &synctypes.Action{
		Type:    actionType,
		DriveID: c.DriveID,
		ItemID:  c.ItemID,
		Path:    c.Path,
		View:    &synctypes.PathView{Path: c.Path},
	}

	var outcome synctypes.Outcome
	if actionType == synctypes.ActionUpload {
		outcome = exec.ExecuteUpload(ctx, action)
	} else {
		outcome = exec.ExecuteDownload(ctx, action)
	}

	if !outcome.Success {
		return fmt.Errorf("transfer failed: %w", outcome.Error)
	}

	if err := e.baseline.CommitOutcome(ctx, &outcome); err != nil {
		return fmt.Errorf("committing transfer outcome: %w", err)
	}

	// Clean up conflict copies — the user has chosen one side, so the
	// conflict copy (holding the other side's content) serves no purpose.
	e.cleanupConflictCopies(c.Path)

	return nil
}

// resolveKeepBoth updates baseline entries for both the original file and its
// conflict copies so that the next sync treats them as unchanged. The original
// file's baseline was not updated during conflict detection (unresolved
// conflicts intentionally skip baseline upsert), so it still has a stale hash.
// Conflict copies have no baseline entry at all.
func (e *Engine) resolveKeepBoth(ctx context.Context, c *synctypes.ConflictRecord) error {
	absPath := filepath.Join(e.syncRoot, c.Path)

	// Update baseline for the original file with its current on-disk hash.
	if err := e.upsertBaselineFromDisk(ctx, c.DriveID, c.ItemID, c.Path, absPath); err != nil {
		return fmt.Errorf("updating baseline for original: %w", err)
	}

	// Find conflict copies and create baseline entries for each. A synthetic
	// item ID is used because the conflict copy has no remote counterpart yet.
	// The next upload-capable sync or full reconciliation will upload the file
	// and replace this entry with a real item ID.
	pattern := syncexec.ConflictCopyGlob(absPath)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob conflict copies: %w", err)
	}

	for _, m := range matches {
		relPath, relErr := filepath.Rel(e.syncRoot, m)
		if relErr != nil {
			return fmt.Errorf("computing relative path for conflict copy: %w", relErr)
		}

		syntheticID := "conflict-copy-placeholder"
		if upsertErr := e.upsertBaselineFromDisk(ctx, c.DriveID, syntheticID, relPath, m); upsertErr != nil {
			return fmt.Errorf("updating baseline for conflict copy %s: %w", filepath.Base(m), upsertErr)
		}
	}

	return nil
}

// upsertBaselineFromDisk computes the QuickXorHash of a file on disk and
// commits a synthetic UpdateSynced outcome to the baseline. Used during
// conflict resolution to bring baseline entries up to date without a real
// transfer.
func (e *Engine) upsertBaselineFromDisk(ctx context.Context, driveID driveid.ID, itemID, relPath, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", relPath, err)
	}

	hash, err := driveops.ComputeQuickXorHash(absPath)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", relPath, err)
	}

	outcome := &synctypes.Outcome{
		Action:    synctypes.ActionUpdateSynced,
		Success:   true,
		Path:      relPath,
		DriveID:   driveID,
		ItemID:    itemID,
		ItemType:  synctypes.ItemTypeFile,
		LocalHash: hash,
		Size:      info.Size(),
		Mtime:     info.ModTime().UnixNano(),
	}

	if err := e.baseline.CommitOutcome(ctx, outcome); err != nil {
		return fmt.Errorf("committing baseline update for %s: %w", relPath, err)
	}

	return nil
}

// cleanupConflictCopies deletes all conflict copy files for the given
// relative path. Called after keep-local or keep-remote resolution — the
// user has chosen one side, so the other side's content is no longer needed.
func (e *Engine) cleanupConflictCopies(relPath string) {
	absPath := filepath.Join(e.syncRoot, relPath)
	pattern := syncexec.ConflictCopyGlob(absPath)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		e.logger.Warn("glob for conflict copies",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)

		return
	}

	for _, m := range matches {
		if removeErr := os.Remove(m); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			e.logger.Warn("removing conflict copy",
				slog.String("file", filepath.Base(m)),
				slog.String("error", removeErr.Error()),
			)
		} else {
			e.logger.Debug("removed conflict copy",
				slog.String("file", filepath.Base(m)),
			)
		}
	}
}
