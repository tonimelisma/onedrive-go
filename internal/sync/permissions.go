package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// PermissionChecker provides permission queries on drive items.
// Satisfied by *graph.Client.
type PermissionChecker interface {
	ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)
}

// permissionCache is an in-memory cache of folder path → canWrite.
// Built from local_issues + API queries each cycle. Not persisted.
type permissionCache struct {
	cache map[string]bool
}

func newPermissionCache() *permissionCache {
	return &permissionCache{cache: make(map[string]bool)}
}

// reset clears all cached entries. Called at the start of each sync cycle
// to prevent stale entries from persisting when permissions change.
func (pc *permissionCache) reset() {
	pc.cache = make(map[string]bool)
}

func (pc *permissionCache) get(folderPath string) (canWrite bool, ok bool) {
	canWrite, ok = pc.cache[folderPath]
	return canWrite, ok
}

func (pc *permissionCache) set(folderPath string, canWrite bool) {
	pc.cache[folderPath] = canWrite
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
func (e *Engine) handle403(ctx context.Context, failedPath string, shortcuts []Shortcut) {
	if e.permChecker == nil {
		return
	}

	sc := findShortcutForPath(shortcuts, failedPath)
	if sc == nil {
		return
	}

	remoteDriveID := driveid.New(sc.RemoteDrive)

	// Resolve the parent folder's remote item ID from baseline.
	// If not in baseline (e.g., brand-new local file), fall back to the
	// shortcut root. This means the boundary walk won't find intermediate
	// read-only folders for brand-new content, but will still correctly
	// suppress at the shortcut root level.
	parentFolder := filepath.Dir(failedPath)
	parentItemID := e.resolveRemoteItemID(parentFolder, remoteDriveID)

	if parentItemID == "" {
		parentFolder = sc.LocalPath
		parentItemID = sc.RemoteItem
	}

	// Query permissions on the parent folder.
	perms, err := e.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		e.handlePermissionCheckError(ctx, err, failedPath, parentFolder)
		return
	}

	if graph.HasWriteAccess(perms) {
		e.logger.Debug("handle403: transient 403, folder is writable",
			slog.String("path", failedPath),
		)

		return
	}

	// Folder is read-only. Walk up to find the highest read-only ancestor.
	boundary := e.walkPermissionBoundary(ctx, parentFolder, sc, remoteDriveID)

	// Record ONE issue for the boundary folder.
	if issueErr := e.baseline.RecordLocalIssue(
		ctx, boundary, IssuePermissionDenied,
		"folder is read-only (no write access)", http.StatusForbidden, 0, "",
	); issueErr != nil {
		e.logger.Warn("handle403: failed to record permission issue",
			slog.String("path", boundary),
			slog.String("error", issueErr.Error()),
		)
	}

	e.logger.Info("handle403: read-only folder detected, writes suppressed",
		slog.String("boundary", boundary),
		slog.String("trigger_path", failedPath),
	)
}

// handlePermissionCheckError handles errors from ListItemPermissions during
// 403 processing. If the item is not found (404), records a permission issue
// to prevent infinite retries. Otherwise logs a warning and does not suppress.
func (e *Engine) handlePermissionCheckError(ctx context.Context, err error, failedPath, parentFolder string) {
	if errors.Is(err, graph.ErrNotFound) {
		e.logger.Warn("handle403: folder not found, recording as permission denied",
			slog.String("path", parentFolder),
		)

		if issueErr := e.baseline.RecordLocalIssue(
			ctx, parentFolder, IssuePermissionDenied,
			"folder not found on remote (deleted or inaccessible)", http.StatusNotFound, 0, "",
		); issueErr != nil {
			e.logger.Warn("handle403: failed to record issue for missing folder",
				slog.String("path", parentFolder),
				slog.String("error", issueErr.Error()),
			)
		}

		return
	}

	e.logger.Warn("handle403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.String("error", err.Error()),
	)
}

// walkPermissionBoundary walks UP the folder hierarchy to find the highest
// read-only ancestor. Returns the boundary folder path.
func (e *Engine) walkPermissionBoundary(
	ctx context.Context, startFolder string, sc *Shortcut, remoteDriveID driveid.ID,
) string {
	boundary := startFolder

	for boundary != sc.LocalPath && boundary != "." && boundary != "" {
		parent := filepath.Dir(boundary)
		if parent == boundary {
			break
		}

		parentID := e.resolveRemoteItemID(parent, remoteDriveID)
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

// resolveRemoteItemID looks up the remote item ID for a local path from the
// baseline. Returns empty string if not found.
func (e *Engine) resolveRemoteItemID(localPath string, driveID driveid.ID) string {
	bl, err := e.baseline.Load(context.Background())
	if err != nil {
		return ""
	}

	entry, ok := bl.GetByPath(localPath)
	if !ok {
		return ""
	}

	if entry.DriveID != driveID {
		return ""
	}

	return entry.ItemID
}

// recheckPermissions re-queries all permission_denied local_issues at the
// start of each sync cycle. If a folder is now writable, the issue is cleared
// and writes resume. Runs every cycle (typically 5 min in watch mode).
func (e *Engine) recheckPermissions(ctx context.Context, shortcuts []Shortcut) {
	if e.permChecker == nil {
		return
	}

	// Reset the in-memory cache at the start of each cycle to prevent
	// stale entries from persisting when permissions change.
	e.permCache.reset()

	issues, err := e.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	if err != nil || len(issues) == 0 {
		return
	}

	for i := range issues {
		issue := &issues[i]

		sc := findShortcutForPath(shortcuts, issue.Path)
		if sc == nil {
			continue
		}

		remoteDriveID := driveid.New(sc.RemoteDrive)
		remoteItemID := e.resolveRemoteItemID(issue.Path, remoteDriveID)

		if remoteItemID == "" {
			continue
		}

		perms, permErr := e.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
		if permErr != nil {
			continue
		}

		canWrite := graph.HasWriteAccess(perms)
		e.permCache.set(issue.Path, canWrite)

		if canWrite {
			if clearErr := e.baseline.ClearLocalIssue(ctx, issue.Path); clearErr != nil {
				e.logger.Warn("recheckPermissions: failed to clear issue",
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

// isWriteSuppressed checks if a write action should be suppressed because
// its path is under a permission_denied folder. Used by the engine when
// filtering actions before execution.
func isWriteSuppressed(path string, deniedPrefixes []string) bool {
	for _, prefix := range deniedPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}

	return false
}

// isRemoteWriteAction returns true for action types that write to the remote
// drive and would fail with 403 on a read-only folder.
func isRemoteWriteAction(at ActionType) bool {
	switch at { //nolint:exhaustive // only write actions are relevant
	case ActionUpload, ActionRemoteDelete, ActionRemoteMove, ActionFolderCreate:
		return true
	default:
		return false
	}
}

// filterDeniedWrites removes write actions whose paths fall under
// permission-denied folders. Downloads continue normally.
func (e *Engine) filterDeniedWrites(ctx context.Context, plan *ActionPlan) *ActionPlan {
	denied := e.loadDeniedPrefixes(ctx)
	if len(denied) == 0 {
		return plan
	}

	var filtered []Action

	var filteredDeps [][]int

	// Map old indices → new indices for dependency remapping.
	oldToNew := make(map[int]int, len(plan.Actions))

	for i, action := range plan.Actions {
		if isRemoteWriteAction(action.Type) && isWriteSuppressed(action.Path, denied) {
			e.logger.Debug("suppressing write under denied folder",
				slog.String("path", action.Path),
				slog.String("action", action.Type.String()),
			)

			continue
		}

		oldToNew[i] = len(filtered)
		filtered = append(filtered, action)
		filteredDeps = append(filteredDeps, plan.Deps[i])
	}

	// Remap dependency indices.
	for i := range filteredDeps {
		var remapped []int
		for _, dep := range filteredDeps[i] {
			if newIdx, ok := oldToNew[dep]; ok {
				remapped = append(remapped, newIdx)
			}
			// Drop dependencies on suppressed actions — they no longer exist.
		}

		filteredDeps[i] = remapped
	}

	return &ActionPlan{
		Actions: filtered,
		Deps:    filteredDeps,
	}
}

// loadDeniedPrefixes loads all permission_denied local_issue paths.
func (e *Engine) loadDeniedPrefixes(ctx context.Context) []string {
	issues, err := e.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	if err != nil {
		return nil
	}

	prefixes := make([]string, 0, len(issues))
	for i := range issues {
		prefixes = append(prefixes, issues[i].Path)
	}

	return prefixes
}
