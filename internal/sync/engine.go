package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type reconcileResult struct {
	events    []synctypes.ChangeEvent
	shortcuts []synctypes.Shortcut
}

type driveIdentityProof struct {
	attempted bool
	drive     *graph.Drive
}

// Engine orchestrates a complete sync pass: observe → plan → execute → commit.
// Single-drive only; multi-drive orchestration is handled by internal/multisync.
type Engine struct {
	baseline           *syncstore.SyncStore
	planner            *syncplan.Planner
	execCfg            *syncexec.ExecutorConfig
	fetcher            synctypes.DeltaFetcher
	socketIOFetcher    synctypes.SocketIOEndpointFetcher
	itemsClient        synctypes.ItemClient
	driveVerifier      synctypes.DriveVerifier      // optional (B-074)
	folderDelta        synctypes.FolderDeltaFetcher // optional: for shortcut observation (6.4b)
	recursiveLister    synctypes.RecursiveLister    // optional: for shortcut observation (6.4b)
	permHandler        *PermissionHandler           // encapsulates all permission logic (6.4c)
	syncRoot           string
	syncTree           *synctree.Root
	driveID            driveid.ID
	driveType          string
	rootItemID         string
	logger             *slog.Logger
	sessionStore       *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers    int                    // goroutine count for the worker pool
	checkWorkers       int                    // goroutine limit for parallel file hashing
	localFilter        synctypes.LocalFilterConfig
	localRules         synctypes.LocalObservationRules
	syncScopeConfig    syncscope.Config
	enableWebsocket    bool
	bigDeleteThreshold int   // from config; 0 means use default
	minFreeSpace       int64 // startup disk-scope revalidation threshold
	diskAvailableFn    func(string) (uint64, error)

	// Test/debug-only invariant checks. Production keeps this disabled;
	// tests enable it to catch lifecycle and scope regressions immediately.
	assertInvariants bool
	debugEventHook   func(DebugEvent)

	// nowFn is the engine's clock. Defaults to time.Now. Tests inject a
	// controllable clock for deterministic trial timer and scope timing.
	nowFn func() time.Time

	// localWatcherFactory overrides the default fsnotify watcher factory
	// for the local observer. Tests inject a mock factory to simulate
	// inotify watch limit exhaustion (ENOSPC).
	localWatcherFactory func() (syncobserve.FsWatcher, error)

	// retryBatchLimit lets tests lower the retrier sweep batch size so stress
	// runs can exercise the batching contract without seeding thousands of
	// durable failures per iteration. Production leaves this zero and uses the
	// compiled default in engine_retry_trial.go.
	retryBatchLimit int

	// socketIOWakeSourceFactory is a test seam for watch-mode websocket
	// wakeups. Production uses syncobserve.NewSocketIOWakeSource.
	socketIOWakeSourceFactory func(
		synctypes.SocketIOEndpointFetcher,
		driveid.ID,
		syncobserve.SocketIOWakeSourceOptions,
	) socketIOWakeSourceRunner

	// watchRuntimeHook is a test-only seam that exposes the newly constructed
	// watch runtime before initWatchInfra wires timers, observers, and workers.
	// Production keeps this nil.
	watchRuntimeHook func(*watchRuntime)

	afterFunc func(time.Duration, func()) syncTimer
	newTicker func(time.Duration) syncTicker
	sleepFn   func(context.Context, time.Duration) error
	jitterFn  func(time.Duration) time.Duration
	nextRunID atomic.Int64
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

	syncTree, err := synctree.Open(cfg.SyncRoot)
	if err != nil {
		return nil, fmt.Errorf("sync: opening sync tree: %w", err)
	}

	execCfg := syncexec.NewExecutorConfig(cfg.Items, cfg.Downloads, cfg.Uploads, syncTree, cfg.DriveID, cfg.Logger)
	execCfg.SetRootItemID(cfg.RootItemID)

	if cfg.UseLocalTrash {
		execCfg.SetTrashFunc(localtrash.Default)
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
		socketIOFetcher:    cfg.SocketIOFetcher,
		itemsClient:        cfg.Items,
		driveVerifier:      cfg.DriveVerifier,
		folderDelta:        cfg.FolderDelta,
		recursiveLister:    cfg.RecursiveLister,
		sessionStore:       sessionStore,
		syncRoot:           cfg.SyncRoot,
		syncTree:           syncTree,
		driveID:            cfg.DriveID,
		driveType:          cfg.DriveType,
		rootItemID:         cfg.RootItemID,
		logger:             cfg.Logger,
		transferWorkers:    cfg.TransferWorkers,
		checkWorkers:       cfg.CheckWorkers,
		localFilter:        cfg.LocalFilter,
		localRules:         cfg.LocalRules,
		syncScopeConfig:    cfg.SyncScope,
		enableWebsocket:    cfg.EnableWebsocket,
		bigDeleteThreshold: bdThreshold,
		minFreeSpace:       cfg.MinFreeSpace,
		diskAvailableFn:    driveops.DiskAvailable,
		nowFn:              time.Now,
		afterFunc:          realAfterFunc,
		newTicker:          realNewTicker,
		sleepFn:            realSleep,
		jitterFn:           realJitter,
		socketIOWakeSourceFactory: func(
			fetcher synctypes.SocketIOEndpointFetcher,
			driveID driveid.ID,
			opts syncobserve.SocketIOWakeSourceOptions,
		) socketIOWakeSourceRunner {
			return syncobserve.NewSocketIOWakeSourceWithOptions(fetcher, driveID, opts)
		},
	}

	e.permHandler = &PermissionHandler{
		baseline:     e.baseline,
		permChecker:  cfg.PermChecker,
		syncTree:     syncTree,
		driveID:      cfg.DriveID,
		accountEmail: cfg.AccountEmail,
		rootItemID:   cfg.RootItemID,
		logger:       cfg.Logger,
		nowFn:        e.nowFunc,
	}

	return e, nil
}

func (e *Engine) nextRuntimeRunID() string {
	return fmt.Sprintf("run-%d", e.nextRunID.Add(1))
}

// Close releases resources held by the engine. Nil-safe for observer
// references set during RunWatch, cleans stale upload sessions, and
// closes the database connection last. Safe to call more than once.
func (e *Engine) Close(ctx context.Context) error {
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

// proveDriveIdentity performs the single startup proof call used by one-shot
// and watch-mode bootstrap. The proof remains ephemeral; persisted auth state
// is still owned by scope_blocks.
func (e *Engine) proveDriveIdentity(ctx context.Context) (driveIdentityProof, error) {
	if e.driveVerifier == nil {
		return driveIdentityProof{}, nil
	}

	drive, err := e.driveVerifier.Drive(ctx, e.driveID)
	if err != nil {
		return driveIdentityProof{attempted: true}, fmt.Errorf("sync: verifying drive identity: %w", err)
	}

	if drive.ID != e.driveID {
		return driveIdentityProof{attempted: true, drive: drive}, fmt.Errorf(
			"sync: drive ID mismatch: configured %s, remote returned %s", e.driveID, drive.ID)
	}

	return driveIdentityProof{attempted: true, drive: drive}, nil
}

func (e *Engine) logVerifiedDrive(proof driveIdentityProof) {
	if !proof.attempted || proof.drive == nil {
		return
	}

	e.logger.Info("drive identity verified",
		slog.String("drive_id", proof.drive.ID.String()),
		slog.String("drive_type", proof.drive.DriveType),
	)
}

func (e *Engine) hasPersistedAuthScope(ctx context.Context) (bool, error) {
	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: listing scope blocks: %w", err)
	}

	for i := range blocks {
		if blocks[i].Key == synctypes.SKAuthAccount() {
			return true, nil
		}
	}

	return false, nil
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
	matches, err := e.syncTree.Glob(conflictCopyGlob(c.Path))
	if err != nil {
		return fmt.Errorf("glob conflict copies for keep-local: %w", err)
	}

	// Restore the first conflict copy to the original path. If multiple
	// conflict copies exist (shouldn't normally happen), the first one is
	// the user's local version.
	if len(matches) > 0 {
		if renameErr := e.syncTree.Rename(matches[0], c.Path); renameErr != nil {
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
	// Update baseline for the original file with its current on-disk hash.
	if err := e.refreshLocalBaselineFromDisk(ctx, c.DriveID, c.ItemID, c.Path); err != nil {
		return fmt.Errorf("updating baseline for original: %w", err)
	}

	// Find conflict copies and create baseline entries for each. A synthetic
	// item ID is used because the conflict copy has no remote counterpart yet.
	// The next upload-capable sync or full reconciliation will upload the file
	// and replace this entry with a real item ID.
	matches, err := e.syncTree.Glob(conflictCopyGlob(c.Path))
	if err != nil {
		return fmt.Errorf("glob conflict copies: %w", err)
	}

	for _, m := range matches {
		syntheticID := conflictCopyPlaceholderItemID(m)
		if upsertErr := e.refreshLocalBaselineFromDisk(ctx, c.DriveID, syntheticID, m); upsertErr != nil {
			return fmt.Errorf("updating baseline for conflict copy %s: %w", filepath.Base(m), upsertErr)
		}
	}

	return nil
}

func conflictCopyPlaceholderItemID(relPath string) string {
	return "conflict-copy:" + relPath
}

// refreshLocalBaselineFromDisk computes the QuickXorHash of a file on disk and
// explicitly refreshes the local-side baseline tuple without fabricating a
// transfer outcome.
func (e *Engine) refreshLocalBaselineFromDisk(ctx context.Context, driveID driveid.ID, itemID, relPath string) error {
	absPath, err := e.syncTree.Abs(relPath)
	if err != nil {
		return fmt.Errorf("resolving %s under sync tree: %w", relPath, err)
	}

	info, err := e.syncTree.Stat(relPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", relPath, err)
	}

	hash, err := driveops.ComputeQuickXorHash(absPath)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", relPath, err)
	}

	if err := e.baseline.RefreshLocalBaseline(ctx, syncstore.LocalBaselineRefresh{
		Path:           relPath,
		DriveID:        driveID,
		ItemID:         itemID,
		ItemType:       synctypes.ItemTypeFile,
		LocalHash:      hash,
		LocalSize:      info.Size(),
		LocalSizeKnown: true,
		LocalMtime:     info.ModTime().UnixNano(),
	}); err != nil {
		return fmt.Errorf("refreshing local baseline for %s: %w", relPath, err)
	}

	return nil
}

// cleanupConflictCopies deletes all conflict copy files for the given
// relative path. Called after keep-local or keep-remote resolution — the
// user has chosen one side, so the other side's content is no longer needed.
func (e *Engine) cleanupConflictCopies(relPath string) {
	matches, err := e.syncTree.Glob(conflictCopyGlob(relPath))
	if err != nil {
		e.logger.Warn("glob for conflict copies",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)

		return
	}

	for _, m := range matches {
		if removeErr := e.syncTree.Remove(m); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
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

func conflictCopyGlob(relPath string) string {
	dir := filepath.Dir(relPath)
	name := filepath.Base(relPath)
	stem, ext := syncexec.ConflictStemExt(name)
	pattern := fmt.Sprintf("%s.conflict-*%s", stem, ext)
	if dir == "." {
		return pattern
	}

	return filepath.Join(dir, pattern)
}
