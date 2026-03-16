package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// resolveRemoteItemID looks up the remote item ID for a local path from the
// baseline. Pure data lookup — no store access needed.
func resolveRemoteItemID(bl *Baseline, localPath string, driveID driveid.ID) string {
	entry, ok := bl.GetByPath(localPath)
	if !ok {
		return ""
	}

	if entry.DriveID != driveID {
		return ""
	}

	return entry.ItemID
}

// permissionCache is a thread-safe in-memory cache of folder path → canWrite.
// Built from sync_failures + API queries each pass. Not persisted.
// Accessed concurrently by the main sync goroutine (recheckPermissions,
// deniedPrefixes) and the drain goroutine (handle403 → set).
type permissionCache struct {
	mu    stdsync.RWMutex
	cache map[string]bool
}

func newPermissionCache() *permissionCache {
	return &permissionCache{cache: make(map[string]bool)}
}

// reset clears all cached entries. Called at the start of each sync pass
// to prevent stale entries from persisting when permissions change.
func (pc *permissionCache) reset() {
	if pc == nil {
		return
	}

	pc.mu.Lock()
	pc.cache = make(map[string]bool)
	pc.mu.Unlock()
}

func (pc *permissionCache) get(folderPath string) (canWrite bool, ok bool) {
	if pc == nil {
		return false, false
	}

	pc.mu.RLock()
	canWrite, ok = pc.cache[folderPath]
	pc.mu.RUnlock()

	return canWrite, ok
}

func (pc *permissionCache) set(folderPath string, canWrite bool) {
	if pc == nil {
		return
	}

	pc.mu.Lock()
	pc.cache[folderPath] = canWrite
	pc.mu.Unlock()
}

// deniedPrefixes returns all folder paths cached as read-only (canWrite == false).
func (pc *permissionCache) deniedPrefixes() []string {
	if pc == nil {
		return nil
	}

	pc.mu.RLock()
	defer pc.mu.RUnlock()

	var prefixes []string
	for path, canWrite := range pc.cache {
		if !canWrite {
			prefixes = append(prefixes, path)
		}
	}

	return prefixes
}

// findShortcutForPath returns the first shortcut whose LocalPath is a prefix
// of (or equal to) the given path. Returns nil if no shortcut matches.
func findShortcutForPath(shortcuts []Shortcut, filePath string) *Shortcut {
	for i := range shortcuts {
		sc := &shortcuts[i]
		if filePath == sc.LocalPath || strings.HasPrefix(filePath, sc.LocalPath+"/") {
			return sc
		}
	}

	return nil
}

// handle403 is called when a worker reports an HTTP 403 on a write action.
// It queries the Graph API to determine if the folder is truly read-only,
// and if so, walks up the hierarchy to find the permission boundary and
// records a local_issue at the boundary folder.
// Returns true if permission-denied was confirmed and recorded (read-only),
// false if transient or unknown (caller should proceed with normal failure recording).
func (e *Engine) handle403(ctx context.Context, bl *Baseline, failedPath string, shortcuts []Shortcut) bool {
	if e.permChecker == nil {
		return false
	}

	sc := findShortcutForPath(shortcuts, failedPath)
	if sc == nil {
		return false
	}

	remoteDriveID := driveid.New(sc.RemoteDrive)

	// Resolve the parent folder's remote item ID from baseline.
	// If not in baseline (e.g., brand-new local file), fall back to the
	// shortcut root. This means the boundary walk won't find intermediate
	// read-only folders for brand-new content, but will still correctly
	// suppress at the shortcut root level.
	parentFolder := filepath.Dir(failedPath)
	parentItemID := resolveRemoteItemID(bl, parentFolder, remoteDriveID)

	if parentItemID == "" {
		parentFolder = sc.LocalPath
		parentItemID = sc.RemoteItem
	}

	// Query permissions on the parent folder.
	perms, err := e.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		return e.handlePermissionCheckError(ctx, err, failedPath, parentFolder)
	}

	if graph.HasWriteAccess(perms) {
		e.logger.Debug("handle403: transient 403, folder is writable",
			slog.String("path", failedPath),
		)

		return false
	}

	// Folder is read-only. Walk up to find the highest read-only ancestor.
	boundary := e.walkPermissionBoundary(ctx, bl, parentFolder, sc, remoteDriveID)

	// Record ONE issue for the boundary folder.
	if issueErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       boundary,
		DriveID:    e.driveID,
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only (no write access)",
		HTTPStatus: http.StatusForbidden,
	}, nil); issueErr != nil {
		e.logger.Warn("handle403: failed to record permission issue",
			slog.String("path", boundary),
			slog.String("error", issueErr.Error()),
		)
	}

	e.permCache.set(boundary, false)

	e.logger.Info("handle403: read-only folder detected, writes suppressed",
		slog.String("boundary", boundary),
		slog.String("trigger_path", failedPath),
	)

	return true
}

// handlePermissionCheckError handles errors from ListItemPermissions during
// 403 processing. If the item is not found (404), records a permission issue
// to prevent infinite retries and returns true. Otherwise logs a warning and
// returns false (caller should proceed with normal failure recording).
func (e *Engine) handlePermissionCheckError(ctx context.Context, err error, failedPath, parentFolder string) bool {
	if errors.Is(err, graph.ErrNotFound) {
		e.logger.Warn("handle403: folder not found, recording as permission denied",
			slog.String("path", parentFolder),
		)

		if issueErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:       parentFolder,
			DriveID:    e.driveID,
			Direction:  "upload",
			IssueType:  IssuePermissionDenied,
			ErrMsg:     "folder not found on remote (deleted or inaccessible)",
			HTTPStatus: http.StatusNotFound,
		}, nil); issueErr != nil {
			e.logger.Warn("handle403: failed to record issue for missing folder",
				slog.String("path", parentFolder),
				slog.String("error", issueErr.Error()),
			)
		}

		e.permCache.set(parentFolder, false)

		return true
	}

	e.logger.Warn("handle403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.String("error", err.Error()),
	)

	return false
}

// walkPermissionBoundary walks UP the folder hierarchy to find the highest
// read-only ancestor. Returns the boundary folder path.
func (e *Engine) walkPermissionBoundary(
	ctx context.Context, bl *Baseline, startFolder string, sc *Shortcut, remoteDriveID driveid.ID,
) string {
	boundary := startFolder

	for boundary != sc.LocalPath && boundary != "." && boundary != "" {
		parent := filepath.Dir(boundary)
		if parent == boundary {
			break
		}

		parentID := resolveRemoteItemID(bl, parent, remoteDriveID)
		if parentID == "" {
			break
		}

		parentPerms, parentErr := e.permChecker.ListItemPermissions(ctx, remoteDriveID, parentID)
		if parentErr != nil {
			break
		}

		if graph.HasWriteAccess(parentPerms) {
			break
		}

		boundary = parent
	}

	return boundary
}

// recheckPermissions re-queries all permission_denied sync_failures at the
// start of each sync pass. If a folder is now writable, the issue is cleared
// and writes resume. Runs every pass (typically 5 min in watch mode).
func (e *Engine) recheckPermissions(ctx context.Context, bl *Baseline, shortcuts []Shortcut) {
	if e.permChecker == nil {
		return
	}

	// Reset the in-memory cache at the start of each pass to prevent
	// stale entries from persisting when permissions change.
	e.permCache.reset()

	issues, err := e.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	if err != nil || len(issues) == 0 {
		return
	}

	for i := range issues {
		issue := &issues[i]

		sc := findShortcutForPath(shortcuts, issue.Path)
		if sc == nil {
			e.permCache.set(issue.Path, false)

			continue
		}

		remoteDriveID := driveid.New(sc.RemoteDrive)
		remoteItemID := resolveRemoteItemID(bl, issue.Path, remoteDriveID)

		if remoteItemID == "" {
			e.permCache.set(issue.Path, false)

			continue
		}

		perms, permErr := e.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
		if permErr != nil {
			e.permCache.set(issue.Path, false)

			continue
		}

		canWrite := graph.HasWriteAccess(perms)
		e.permCache.set(issue.Path, canWrite)

		if canWrite {
			if clearErr := e.baseline.ClearSyncFailure(ctx, issue.Path, issue.DriveID); clearErr != nil {
				e.logger.Warn("recheckPermissions: failed to clear failure",
					slog.String("path", issue.Path),
					slog.String("error", clearErr.Error()),
				)

				continue
			}

			e.logger.Info("permission granted, clearing denial",
				slog.String("path", issue.Path),
			)
		}
	}
}

// isDirAccessible returns true if the directory can be opened for reading.
// os.Stat is insufficient — it succeeds on chmod 000 dirs because stat()
// only requires execute on the parent. os.Open tests actual read access.
func isDirAccessible(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}

	f.Close()

	return true
}

// handleLocalPermission processes os.ErrPermission results from workers.
// It walks up from the failed path to find the deepest inaccessible ancestor
// directory, records a local_permission_denied failure, and creates a scope
// block for the directory subtree (R-2.10.12).
func (e *Engine) handleLocalPermission(ctx context.Context, r *WorkerResult) {
	// If the sync root itself is inaccessible, WARN loudly — don't silently
	// block everything behind a scope block. The sync root being inaccessible
	// is fundamentally different from a subdirectory denial: ALL operations
	// will fail, and the user needs a clear, actionable message.
	if !isDirAccessible(e.syncRoot) {
		e.logger.Warn("sync root directory is inaccessible",
			slog.String("path", e.syncRoot),
			slog.String("error", r.ErrMsg),
		)

		// Record as a top-level actionable failure, not a scope block.
		if recErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      r.Path,
			DriveID:   e.driveID,
			Direction: directionFromAction(r.ActionType),
			IssueType: IssueLocalPermissionDenied,
			Category:  "actionable",
			ErrMsg:    "sync root directory not accessible (check filesystem permissions)",
		}, nil); recErr != nil {
			e.logger.Warn("handleLocalPermission: failed to record sync root issue",
				slog.String("path", r.Path),
				slog.String("error", recErr.Error()),
			)
		}

		return
	}

	// Walk up from the file's parent directory to find the deepest inaccessible ancestor.
	absPath := filepath.Join(e.syncRoot, r.Path)
	parentDir := filepath.Dir(absPath)

	// Check if the parent directory is accessible (readable). os.Stat is
	// insufficient — it succeeds on chmod 000 dirs because stat() only needs
	// parent execute permission. os.Open tests actual read access.
	if isDirAccessible(parentDir) {
		// Parent directory is accessible — this is a file-level permission issue.
		// Record the failure at file level with no scope block.
		if recErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      r.Path,
			DriveID:   e.driveID,
			Direction: directionFromAction(r.ActionType),
			IssueType: IssueLocalPermissionDenied,
			Category:  "actionable",
			ErrMsg:    "file not accessible (check filesystem permissions)",
		}, nil); recErr != nil {
			e.logger.Warn("handleLocalPermission: failed to record file-level issue",
				slog.String("path", r.Path),
				slog.String("error", recErr.Error()),
			)
		}

		return
	}

	// Parent directory is inaccessible — walk up to find the deepest denied ancestor.
	boundary := parentDir
	for {
		parent := filepath.Dir(boundary)
		if parent == boundary {
			break // reached filesystem root
		}

		if isDirAccessible(parent) {
			// Parent is accessible — boundary is the deepest inaccessible dir.
			break
		}

		boundary = parent
	}

	// Convert boundary to relative path for recording.
	relBoundary, relErr := filepath.Rel(e.syncRoot, boundary)
	if relErr != nil {
		// Shouldn't happen — boundary is under syncRoot. Fall back to recording at file level.
		e.logger.Warn("handleLocalPermission: failed to relativize boundary path",
			slog.String("boundary", boundary),
			slog.String("error", relErr.Error()),
		)

		return
	}

	scopeKey := SKPermDir(relBoundary)

	// Record one failure at the boundary directory level.
	if recErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      relBoundary,
		DriveID:   e.driveID,
		Direction: directionFromAction(r.ActionType),
		IssueType: IssueLocalPermissionDenied,
		Category:  "actionable",
		ErrMsg:    "directory not accessible (check filesystem permissions)",
		ScopeKey:  scopeKey,
	}, nil); recErr != nil {
		e.logger.Warn("handleLocalPermission: failed to record directory-level issue",
			slog.String("path", relBoundary),
			slog.String("error", recErr.Error()),
		)
	}

	// Create a scope block — no trials for permission blocks (rechecked per-pass
	// by recheckLocalPermissions instead).
	e.setScopeBlock(scopeKey, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueLocalPermissionDenied,
		BlockedAt: e.nowFunc(),
	})

	e.logger.Info("local permission denied: directory blocked",
		slog.String("boundary", relBoundary),
		slog.String("trigger_path", r.Path),
	)
}

// recheckLocalPermissions rechecks directory-level local permission denials
// at the start of each sync pass. If a directory is now accessible, clears
// the failure and releases the scope block (R-2.10.13).
func (e *Engine) recheckLocalPermissions(ctx context.Context) {
	issues, err := e.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	if err != nil || len(issues) == 0 {
		return
	}

	for i := range issues {
		issue := &issues[i]

		// Only recheck directory-level issues (those with a perm:dir: scope key).
		if !issue.ScopeKey.IsPermDir() {
			continue
		}

		dirPath := issue.ScopeKey.DirPath()
		absDir := filepath.Join(e.syncRoot, dirPath)

		if !isDirAccessible(absDir) {
			// Still inaccessible — keep the block.
			continue
		}

		// Directory is accessible again — clear the failure and release the scope.
		if clearErr := e.baseline.ClearSyncFailure(ctx, issue.Path, issue.DriveID); clearErr != nil {
			e.logger.Warn("recheckLocalPermissions: failed to clear failure",
				slog.String("path", issue.Path),
				slog.String("error", clearErr.Error()),
			)

			continue
		}

		if e.watch != nil {
			e.onScopeClear(ctx, issue.ScopeKey)
		}

		e.logger.Info("local permission restored, clearing denial",
			slog.String("path", issue.Path),
			slog.String("scope_key", issue.ScopeKey.String()),
		)
	}
}

// clearScannerResolvedPermissions checks whether the scanner observed paths
// that were previously blocked by local_permission_denied failures. If the
// scanner successfully accessed a path (it appeared in events), the
// permission issue is resolved — clear the failure and release any scope block.
//
// Implements R-2.10.10. Complements recheckLocalPermissions (R-2.10.13).
func (e *Engine) clearScannerResolvedPermissions(ctx context.Context, observedPaths map[string]bool) {
	if len(observedPaths) == 0 {
		return
	}

	issues, err := e.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	if err != nil || len(issues) == 0 {
		return
	}

	for i := range issues {
		issue := &issues[i]

		resolved := false
		if issue.ScopeKey.IsPermDir() {
			// Directory-level: resolved if any observed path falls under the directory.
			dirPath := issue.ScopeKey.DirPath()
			for p := range observedPaths {
				if p == dirPath || strings.HasPrefix(p, dirPath+"/") {
					resolved = true
					break
				}
			}
		} else {
			// File-level: resolved if the file itself was observed.
			resolved = observedPaths[issue.Path]
		}

		if !resolved {
			continue
		}

		if clearErr := e.baseline.ClearSyncFailure(ctx, issue.Path, issue.DriveID); clearErr != nil {
			e.logger.Warn("clearScannerResolvedPermissions: failed to clear failure",
				slog.String("path", issue.Path),
				slog.String("error", clearErr.Error()),
			)

			continue
		}

		if !issue.ScopeKey.IsZero() && e.watch != nil {
			e.onScopeClear(ctx, issue.ScopeKey)
		}

		e.logger.Info("scanner resolved permission denial",
			slog.String("path", issue.Path),
			slog.String("scope_key", issue.ScopeKey.String()),
		)
	}
}

// pathSetFromEvents builds a set of paths from scanner change events.
func pathSetFromEvents(events []ChangeEvent) map[string]bool {
	if len(events) == 0 {
		return nil
	}

	paths := make(map[string]bool, len(events))
	for i := range events {
		if events[i].Path != "" {
			paths[events[i].Path] = true
		}
	}

	return paths
}

// pathSetFromBatch builds a set of paths from watch-mode batch entries.
func pathSetFromBatch(batch []PathChanges) map[string]bool {
	if len(batch) == 0 {
		return nil
	}

	paths := make(map[string]bool, len(batch))
	for i := range batch {
		if batch[i].Path != "" {
			paths[batch[i].Path] = true
		}
	}

	return paths
}
