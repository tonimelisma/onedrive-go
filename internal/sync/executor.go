package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// dirPermissions is the Unix permission mode for newly created sync directories.
const dirPermissions = 0o755

// uploadChunkSize is the default chunk size per upload call (10 MiB).
// Value is 32 × 320 KiB = 10485760 bytes, satisfying the Graph API 320 KiB chunk alignment requirement.
const uploadChunkSize = 10_485_760

// simpleUploadMax is the maximum file size for simple (single-request) upload in bytes (4 MiB).
// Larger files require a chunked upload session.
const simpleUploadMax = 4_194_304

// Executor dispatches validated ActionPlan operations: filesystem I/O, API calls, state updates.
// It processes phases sequentially (sync-algorithm.md section 9.1).
// Fatal errors abort the sync; skip-tier errors are recorded and execution continues.
type Executor struct {
	store           ExecutorStore
	items           ItemClient
	transfer        TransferClient
	syncRoot        string
	cfg             *config.SafetyConfig
	logger          *slog.Logger
	conflictHandler *ConflictHandler
}

// NewExecutor creates an Executor with the given dependencies.
// syncRoot must be an absolute path to the local sync directory.
func NewExecutor(
	store ExecutorStore,
	items ItemClient,
	transfer TransferClient,
	syncRoot string,
	cfg *config.SafetyConfig,
	logger *slog.Logger,
) *Executor {
	if logger == nil {
		logger = slog.Default()
	}

	return &Executor{
		store:           store,
		items:           items,
		transfer:        transfer,
		syncRoot:        syncRoot,
		cfg:             cfg,
		logger:          logger,
		conflictHandler: NewConflictHandler(syncRoot, logger),
	}
}

// Execute processes the validated action plan in 9 sequential phases.
// Returns a SyncReport summarizing results. Fatal errors abort immediately;
// skip/deferred errors are recorded in the report and execution continues.
func (e *Executor) Execute(ctx context.Context, plan *ActionPlan) (*SyncReport, error) {
	e.logger.Info("executor: starting", "total_actions", plan.TotalActions())

	r := &SyncReport{}

	phases := []struct {
		name string
		run  func(context.Context, *SyncReport) error
	}{
		{"folder_creates", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.FolderCreates, "folder create",
				func(c2 context.Context, a *Action) error { return e.executeFolderCreate(c2, a) },
				func(s2 *SyncReport) { s2.FoldersCreated++ })
		}},
		{"moves", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.Moves, "move",
				func(c2 context.Context, a *Action) error { return e.executeMove(c2, a) },
				func(s2 *SyncReport) { s2.Moved++ })
		}},
		{"downloads", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.Downloads, "download",
				func(c2 context.Context, a *Action) error {
					n, err := e.executeDownload(c2, a)
					if err == nil {
						s.BytesDownloaded += n
					}

					return err
				},
				func(s2 *SyncReport) { s2.Downloaded++ })
		}},
		{"uploads", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.Uploads, "upload",
				func(c2 context.Context, a *Action) error {
					n, err := e.executeUpload(c2, a)
					if err == nil {
						s.BytesUploaded += n
					}

					return err
				},
				func(s2 *SyncReport) { s2.Uploaded++ })
		}},
		{"local_deletes", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.LocalDeletes, "local delete",
				func(c2 context.Context, a *Action) error { return e.executeLocalDelete(c2, a) },
				func(s2 *SyncReport) { s2.LocalDeleted++ })
		}},
		{"remote_deletes", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.RemoteDeletes, "remote delete",
				func(c2 context.Context, a *Action) error { return e.executeRemoteDelete(c2, a) },
				func(s2 *SyncReport) { s2.RemoteDeleted++ })
		}},
		{"conflicts", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.Conflicts, "conflict",
				func(c2 context.Context, a *Action) error { return e.executeConflict(c2, a) },
				func(s2 *SyncReport) { s2.Conflicts++ })
		}},
		{"synced_updates", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.SyncedUpdates, "synced update",
				func(c2 context.Context, a *Action) error { return e.executeSyncedUpdate(c2, a) },
				func(s2 *SyncReport) { s2.SyncedUpdates++ })
		}},
		{"cleanups", func(c context.Context, s *SyncReport) error {
			return e.dispatchPhase(c, s, plan.Cleanups, "cleanup",
				func(c2 context.Context, a *Action) error { return e.executeCleanup(c2, a) },
				func(s2 *SyncReport) { s2.Cleanups++ })
		}},
	}

	for _, phase := range phases {
		if err := ctx.Err(); err != nil {
			return r, err
		}

		e.logger.Debug("executor: phase starting", "phase", phase.name)

		if err := phase.run(ctx, r); err != nil {
			return r, err
		}
	}

	if err := e.store.Checkpoint(); err != nil {
		// Checkpoint failure is non-fatal: the sync data is already committed.
		// A failed WAL flush is recovered on the next successful open.
		e.logger.Warn("executor: checkpoint failed", "error", err)
	}

	e.logger.Info("executor: done",
		"downloaded", r.Downloaded,
		"uploaded", r.Uploaded,
		"errors", len(r.Errors),
	)

	return r, nil
}

// dispatchPhase loops over actions, calls handler for each, and routes errors.
// Fatal errors propagate immediately. Skip/deferred errors are recorded in the report.
// onSuccess is called (with the report) when an action completes without error.
func (e *Executor) dispatchPhase(
	ctx context.Context,
	report *SyncReport,
	actions []Action,
	label string,
	handler func(context.Context, *Action) error,
	onSuccess func(*SyncReport),
) error {
	for i := range actions {
		if err := handler(ctx, &actions[i]); err != nil {
			tier := classifyError(err)
			if tier == ErrorFatal {
				return err
			}

			report.Skipped++
			report.Errors = append(report.Errors, ActionError{Action: actions[i], Err: err, Tier: tier})
			e.logger.Warn("executor: "+label+" skipped", "path", actions[i].Path, "error", err)
		} else {
			onSuccess(report)
		}
	}

	return nil
}

// --- Individual action handlers ---

// executeFolderCreate creates a folder on the local filesystem or via the Graph API.
func (e *Executor) executeFolderCreate(ctx context.Context, action *Action) error {
	e.logger.Info("executor: folder create",
		"path", action.Path,
		"side", action.CreateSide,
	)

	if action.CreateSide == FolderCreateLocal {
		localPath := filepath.Join(e.syncRoot, action.Path)
		if err := os.MkdirAll(localPath, dirPermissions); err != nil {
			return fmt.Errorf("executor: mkdir %s: %w", action.Path, err)
		}

		if action.Item != nil {
			now := NowNano()
			action.Item.LocalMtime = Int64Ptr(now)
			action.Item.UpdatedAt = now

			return e.store.UpsertItem(ctx, action.Item)
		}

		return nil
	}

	// FolderCreateRemote: create folder via Graph API.
	if action.Item == nil {
		return fmt.Errorf("executor: remote folder create for %s has nil item", action.Path)
	}

	created, err := e.items.CreateFolder(ctx, action.DriveID, action.Item.ParentID, action.Item.Name)
	if err != nil {
		return fmt.Errorf("executor: create remote folder %s: %w", action.Path, err)
	}

	action.Item.ItemID = created.ID
	action.Item.ETag = created.ETag
	action.Item.CTag = created.CTag
	action.Item.UpdatedAt = NowNano()

	return e.store.UpsertItem(ctx, action.Item)
}

// executeMove renames or moves a remote item via the Graph API, then updates path state.
func (e *Executor) executeMove(ctx context.Context, action *Action) error {
	if action.Item == nil {
		return fmt.Errorf("executor: move action for %s has nil item", action.ItemID)
	}

	newName := filepath.Base(action.NewPath)
	newParentPath := filepath.Dir(action.NewPath)

	e.logger.Info("executor: move", "from", action.Path, "to", action.NewPath)

	// Resolve the new parent's ItemID from the DB. The FolderCreates phase ensures
	// remote parent folders exist before any moves are dispatched.
	parent, err := e.store.GetItemByPath(ctx, newParentPath)
	if err != nil {
		return fmt.Errorf("executor: lookup new parent %s: %w", newParentPath, err)
	}

	newParentID := action.Item.ParentID // default: same parent (rename only)
	if parent != nil && parent.ItemID != "" {
		newParentID = parent.ItemID
	}

	moved, err := e.items.MoveItem(ctx, action.DriveID, action.ItemID, newParentID, newName)
	if err != nil {
		return fmt.Errorf("executor: move item %s -> %s: %w", action.Path, action.NewPath, err)
	}

	// For folder moves, cascade path updates to all descendant items in the DB.
	if action.Item.ItemType == ItemTypeFolder {
		if err := e.store.CascadePathUpdate(ctx, action.Path, action.NewPath); err != nil {
			return fmt.Errorf("executor: cascade path update %s -> %s: %w", action.Path, action.NewPath, err)
		}
	}

	action.Item.Path = action.NewPath
	action.Item.Name = newName
	action.Item.ParentID = newParentID
	action.Item.ETag = moved.ETag
	action.Item.UpdatedAt = NowNano()

	return e.store.UpsertItem(ctx, action.Item)
}

// executeDownload streams a remote file to disk via a .partial temp file.
// Verifies QuickXorHash before the atomic rename, then restores the remote mtime.
func (e *Executor) executeDownload(ctx context.Context, action *Action) (int64, error) {
	if action.Item == nil {
		return 0, fmt.Errorf("executor: download action for %s has nil item", action.ItemID)
	}

	e.logger.Info("executor: download", "path", action.Path, "item_id", action.ItemID)

	localPath := filepath.Join(e.syncRoot, action.Path)
	partialPath := localPath + ".partial"

	if err := os.MkdirAll(filepath.Dir(localPath), dirPermissions); err != nil {
		return 0, fmt.Errorf("executor: mkdir for download %s: %w", action.Path, err)
	}

	n, gotHash, err := e.downloadToPartial(ctx, action, partialPath)
	if err != nil {
		_ = os.Remove(partialPath) // best-effort cleanup; error is non-actionable here
		return 0, err
	}

	if action.Item.QuickXorHash != "" && gotHash != action.Item.QuickXorHash {
		_ = os.Remove(partialPath)
		return 0, fmt.Errorf("executor: hash mismatch for %s: got %s want %s",
			action.Path, gotHash, action.Item.QuickXorHash)
	}

	if err := os.Rename(partialPath, localPath); err != nil {
		_ = os.Remove(partialPath)
		return 0, fmt.Errorf("executor: rename partial %s: %w", action.Path, err)
	}

	// Restore remote mtime (best-effort: filesystem precision may differ from OneDrive precision).
	if action.Item.RemoteMtime != nil {
		mtime := time.Unix(0, *action.Item.RemoteMtime)
		_ = os.Chtimes(localPath, time.Now(), mtime) //nolint:errcheck // best-effort mtime restore
	}

	return n, e.updateDownloadState(ctx, action, n, gotHash)
}

// downloadToPartial writes download content to the partial file and returns bytes/hash.
func (e *Executor) downloadToPartial(ctx context.Context, action *Action, partialPath string) (int64, string, error) {
	f, err := os.Create(partialPath)
	if err != nil {
		return 0, "", fmt.Errorf("executor: create partial file %s: %w", partialPath, err)
	}
	defer f.Close()

	hasher := quickxorhash.New()
	mw := io.MultiWriter(f, hasher)

	n, err := e.transfer.Download(ctx, action.DriveID, action.ItemID, mw)
	if err != nil {
		return 0, "", fmt.Errorf("executor: download %s: %w", action.Path, err)
	}

	hash := base64.StdEncoding.EncodeToString(hasher.Sum(nil))

	return n, hash, nil
}

// updateDownloadState writes local+synced fields back to the DB after a successful download.
func (e *Executor) updateDownloadState(ctx context.Context, action *Action, n int64, hash string) error {
	now := NowNano()
	action.Item.LocalSize = Int64Ptr(n)
	action.Item.LocalMtime = action.Item.RemoteMtime
	action.Item.LocalHash = hash
	action.Item.SyncedSize = action.Item.Size
	action.Item.SyncedMtime = action.Item.RemoteMtime
	action.Item.SyncedHash = hash
	action.Item.LastSyncedAt = Int64Ptr(now)
	action.Item.UpdatedAt = now

	return e.store.UpsertItem(ctx, action.Item)
}

// executeUpload uploads a local file to OneDrive. Uses SimpleUpload for files ≤4 MiB;
// chunked upload session for larger files. Hashes the content during upload.
func (e *Executor) executeUpload(ctx context.Context, action *Action) (int64, error) {
	if action.Item == nil {
		return 0, fmt.Errorf("executor: upload action for %s has nil item", action.ItemID)
	}

	localPath := filepath.Join(e.syncRoot, action.Path)

	f, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("executor: open %s for upload: %w", action.Path, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("executor: stat %s for upload: %w", action.Path, err)
	}

	size := stat.Size()
	mtime := stat.ModTime()
	hasher := quickxorhash.New()

	e.logger.Info("executor: upload", "path", action.Path, "size", size)

	var uploaded *graph.Item
	if size <= simpleUploadMax {
		tee := io.TeeReader(f, hasher)
		uploaded, err = e.transfer.SimpleUpload(ctx, action.DriveID, action.Item.ParentID, action.Item.Name, tee, size)
	} else {
		uploaded, err = e.uploadChunked(ctx, action.DriveID, action.Item.ParentID, action.Item.Name, f, size, mtime, hasher)
	}

	if err != nil {
		return 0, fmt.Errorf("executor: upload %s: %w", action.Path, err)
	}

	hash := base64.StdEncoding.EncodeToString(hasher.Sum(nil))

	return size, e.updateUploadState(ctx, action, uploaded, size, mtime, hash)
}

// updateUploadState writes remote+synced fields to the DB after a successful upload.
func (e *Executor) updateUploadState(
	ctx context.Context,
	action *Action,
	uploaded *graph.Item,
	size int64,
	mtime time.Time,
	hash string,
) error {
	now := NowNano()
	mtimeNano := Int64Ptr(mtime.UnixNano())

	if uploaded != nil {
		action.Item.ItemID = uploaded.ID
		action.Item.ETag = uploaded.ETag
		action.Item.CTag = uploaded.CTag
	}

	action.Item.SyncedSize = Int64Ptr(size)
	action.Item.SyncedMtime = mtimeNano
	action.Item.SyncedHash = hash
	action.Item.LocalHash = hash
	action.Item.LastSyncedAt = Int64Ptr(now)
	action.Item.UpdatedAt = now

	return e.store.UpsertItem(ctx, action.Item)
}

// executeLocalDelete removes a local file after verifying its hash matches the last-synced
// baseline (S4 runtime check). If the file was modified, it is backed up as a conflict.
func (e *Executor) executeLocalDelete(ctx context.Context, action *Action) error {
	if action.Item == nil {
		return fmt.Errorf("executor: local delete for %s has nil item", action.ItemID)
	}

	localPath := filepath.Join(e.syncRoot, action.Path)

	e.logger.Info("executor: local delete", "path", action.Path)

	currentHash, err := computeLocalHash(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			// File already gone; treat as success and update DB.
			e.logger.Warn("executor: local delete: file already absent", "path", action.Path)
			return e.store.MarkDeleted(ctx, action.DriveID, action.ItemID, NowNano())
		}

		return fmt.Errorf("executor: hash before delete %s: %w", action.Path, err)
	}

	// S4: if content changed since last sync, back up rather than silently delete.
	if currentHash != action.Item.SyncedHash {
		return e.handleLocalDeleteConflict(ctx, action, localPath, currentHash)
	}

	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("executor: remove %s: %w", action.Path, err)
	}

	return e.store.MarkDeleted(ctx, action.DriveID, action.ItemID, NowNano())
}

// handleLocalDeleteConflict backs up a locally-modified file that was scheduled for deletion.
// The file is renamed to a conflict copy using generateConflictPath, and the conflict is
// recorded in the DB. This is a safety-check (S4) backup, not a full keep-both resolution.
func (e *Executor) handleLocalDeleteConflict(
	ctx context.Context,
	action *Action,
	localPath, currentHash string,
) error {
	conflictPath := generateConflictPath(localPath)

	e.logger.Warn("executor: local delete conflict: file changed since sync, backing up",
		"path", action.Path,
		"conflict_path", conflictPath,
	)

	if err := os.Rename(localPath, conflictPath); err != nil {
		return fmt.Errorf("executor: backup conflict file %s: %w", action.Path, err)
	}

	now := NowNano()

	record := &ConflictRecord{
		ID:         fmt.Sprintf("conflict-%d", now),
		DriveID:    action.DriveID,
		ItemID:     action.ItemID,
		Path:       action.Path,
		DetectedAt: now,
		LocalHash:  currentHash,
		RemoteHash: action.Item.QuickXorHash,
		LocalMtime: action.Item.LocalMtime,
		Resolution: ConflictUnresolved,
	}

	if err := e.store.RecordConflict(ctx, record); err != nil {
		return fmt.Errorf("executor: record delete conflict for %s: %w", action.Path, err)
	}

	return e.store.MarkDeleted(ctx, action.DriveID, action.ItemID, now)
}

// executeRemoteDelete deletes a remote item via the Graph API.
// 404 responses are treated as success (item already gone).
func (e *Executor) executeRemoteDelete(ctx context.Context, action *Action) error {
	e.logger.Info("executor: remote delete", "item_id", action.ItemID)

	err := e.items.DeleteItem(ctx, action.DriveID, action.ItemID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			// Already deleted on remote — treat as success.
			e.logger.Warn("executor: remote delete: item already gone", "item_id", action.ItemID)
		} else {
			return fmt.Errorf("executor: delete remote item %s: %w", action.ItemID, err)
		}
	}

	return e.store.MarkDeleted(ctx, action.DriveID, action.ItemID, NowNano())
}

// executeConflict resolves a conflict using the keep-both policy (4.8).
// The conflict handler renames local files and produces sub-actions (downloads/uploads)
// that are dispatched inline before recording the resolved conflict.
func (e *Executor) executeConflict(ctx context.Context, action *Action) error {
	if action.ConflictInfo == nil {
		return fmt.Errorf("executor: conflict action for %s has nil ConflictInfo", action.ItemID)
	}

	e.logger.Info("executor: resolving conflict", "path", action.Path, "type", action.ConflictInfo.Type)

	result, err := e.conflictHandler.Resolve(ctx, action)
	if err != nil {
		return fmt.Errorf("executor: resolve conflict %s: %w", action.Path, err)
	}

	if err := e.store.RecordConflict(ctx, result.Record); err != nil {
		return fmt.Errorf("executor: record conflict %s: %w", action.Path, err)
	}

	// Dispatch resolution sub-actions (download or upload) to complete keep-both.
	// Only ActionDownload and ActionUpload are valid here; other types indicate a bug.
	for i := range result.SubActions {
		sub := &result.SubActions[i]
		switch sub.Type { //nolint:exhaustive // conflict handler only returns download/upload
		case ActionDownload:
			if _, err := e.executeDownload(ctx, sub); err != nil {
				return fmt.Errorf("executor: conflict download %s: %w", sub.Path, err)
			}
		case ActionUpload:
			if _, err := e.executeUpload(ctx, sub); err != nil {
				return fmt.Errorf("executor: conflict upload %s: %w", sub.Path, err)
			}
		default:
			// Conflict handler only returns download/upload sub-actions; other types are a bug.
			return fmt.Errorf("executor: conflict %s: unexpected sub-action type %d", action.Path, sub.Type)
		}
	}

	return nil
}

// executeSyncedUpdate snapshots the current item state as the new synced base.
// Used for false conflicts where local and remote agree on content.
func (e *Executor) executeSyncedUpdate(ctx context.Context, action *Action) error {
	if action.Item == nil {
		return fmt.Errorf("executor: synced update for %s has nil item", action.ItemID)
	}

	e.logger.Debug("executor: synced update", "path", action.Path)

	now := NowNano()
	action.Item.SyncedSize = action.Item.LocalSize
	action.Item.SyncedMtime = action.Item.LocalMtime
	action.Item.SyncedHash = action.Item.LocalHash
	action.Item.LastSyncedAt = Int64Ptr(now)
	action.Item.UpdatedAt = now

	return e.store.UpsertItem(ctx, action.Item)
}

// executeCleanup removes a stale DB entry. No filesystem operation is needed
// because the item was already absent from both sides when the cleanup was planned.
func (e *Executor) executeCleanup(ctx context.Context, action *Action) error {
	e.logger.Debug("executor: cleanup", "path", action.Path, "item_id", action.ItemID)

	return e.store.MarkDeleted(ctx, action.DriveID, action.ItemID, NowNano())
}

// --- Upload helpers ---

// uploadChunked uploads a large file (>4 MiB) using a resumable upload session.
// io.TeeReader feeds every byte read from file into hasher simultaneously,
// so the final hash reflects the complete file content.
func (e *Executor) uploadChunked(
	ctx context.Context,
	driveID, parentID, name string,
	file *os.File,
	size int64,
	mtime time.Time,
	hasher io.Writer,
) (*graph.Item, error) {
	session, err := e.transfer.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
	if err != nil {
		return nil, fmt.Errorf("executor: create upload session for %s: %w", name, err)
	}

	// TeeReader causes every byte read from file to also be written to hasher.
	tee := io.TeeReader(file, hasher)

	var offset int64

	for offset < size {
		length := int64(uploadChunkSize)
		if remaining := size - offset; remaining < length {
			length = remaining
		}

		item, err := e.transfer.UploadChunk(ctx, session, io.LimitReader(tee, length), offset, length, size)
		if err != nil {
			return nil, fmt.Errorf("executor: upload chunk at offset %d: %w", offset, err)
		}

		offset += length

		if item != nil {
			return item, nil // final chunk: upload complete
		}
	}

	return nil, fmt.Errorf("executor: upload session for %s completed without final item response", name)
}

// --- Utilities ---

// computeLocalHash hashes a local file with QuickXorHash and returns base64.
func computeLocalHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := quickxorhash.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("executor: hashing %s: %w", path, err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// classifyError maps an error to an ErrorTier for recovery decisions.
// Fatal: abort sync. Retryable: retry with backoff (engine 4.10). Skip: log and continue.
func classifyError(err error) ErrorTier {
	if err == nil {
		return ErrorSkip
	}

	// Context cancellation or deadline: abort immediately.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorFatal
	}

	// Auth failures: cannot proceed without re-authentication.
	if errors.Is(err, graph.ErrUnauthorized) || errors.Is(err, graph.ErrNotLoggedIn) {
		return ErrorFatal
	}

	// Throttling and server errors: safe to retry with backoff.
	if errors.Is(err, graph.ErrThrottled) || errors.Is(err, graph.ErrServerError) {
		return ErrorRetryable
	}

	// Client errors where the specific item cannot be synced: skip and continue.
	if errors.Is(err, graph.ErrForbidden) ||
		errors.Is(err, graph.ErrBadRequest) ||
		errors.Is(err, graph.ErrLocked) ||
		errors.Is(err, graph.ErrNotFound) ||
		errors.Is(err, graph.ErrNoDownloadURL) {
		return ErrorSkip
	}

	// Unknown errors default to skip to prevent one bad item from aborting the sync.
	return ErrorSkip
}
