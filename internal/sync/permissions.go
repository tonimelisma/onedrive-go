package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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

// PermissionChecker provides permission queries on drive items.
// Satisfied by *graph.Client.
type PermissionChecker interface {
	ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)
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
	if issueErr := e.baseline.RecordSyncFailure(
		ctx, boundary, e.driveID, "upload", IssuePermissionDenied,
		"folder is read-only (no write access)", http.StatusForbidden, 0, "", "",
	); issueErr != nil {
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

		if issueErr := e.baseline.RecordSyncFailure(
			ctx, parentFolder, e.driveID, "upload", IssuePermissionDenied,
			"folder not found on remote (deleted or inaccessible)", http.StatusNotFound, 0, "", "",
		); issueErr != nil {
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
