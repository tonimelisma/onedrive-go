package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// PermissionHandler encapsulates all permission-related logic extracted from
// Engine. It handles HTTP 403 responses, local permission denials, per-pass
// permission rechecks, and scanner-resolved permission clearing.
type PermissionHandler struct {
	baseline    synctypes.SyncFailureRecorder
	permChecker synctypes.PermissionChecker
	syncRoot    string
	driveID     driveid.ID
	logger      *slog.Logger
	nowFn       func() time.Time

	// scopeMgr abstracts Engine's scope lifecycle operations. Replaces
	// individual function callbacks with a single narrow interface.
	scopeMgr scopeManager
}

// HasPermChecker reports whether a remote permission checker is configured.
// Used by the engine to skip permission-related API calls when no checker
// is available (e.g., personal drives without shared folders).
func (ph *PermissionHandler) HasPermChecker() bool {
	return ph.permChecker != nil
}

// DeniedPrefixes returns all active remote read-only boundaries. The planner
// uses these prefixes to suppress remote-mutating actions under known
// read-only subtrees before they reach execution.
func (ph *PermissionHandler) DeniedPrefixes(ctx context.Context) []string {
	issues, err := ph.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssuePermissionDenied)
	if err != nil {
		ph.logger.Warn("DeniedPrefixes: failed to list permission failures",
			slog.String("error", err.Error()),
		)

		return nil
	}

	seen := make(map[string]bool, len(issues))
	var prefixes []string

	for i := range issues {
		if !issues[i].ScopeKey.IsPermRemote() {
			continue
		}

		boundary := issues[i].ScopeKey.RemotePath()
		if boundary == "" || seen[boundary] {
			continue
		}

		seen[boundary] = true
		prefixes = append(prefixes, boundary)
	}

	sort.Strings(prefixes)

	return prefixes
}

// handle403 is called when a worker reports an HTTP 403 on a write action.
// It queries the Graph API to determine if the folder is truly read-only,
// and if so, walks up the hierarchy to find the permission boundary and
// records a local_issue at the boundary folder.
// Returns true if permission-denied was confirmed and recorded (read-only),
// false if transient or unknown (caller should proceed with normal failure recording).
func (ph *PermissionHandler) handle403(
	ctx context.Context, bl *synctypes.Baseline, failedPath string, shortcuts []synctypes.Shortcut,
) bool {
	if ph.permChecker == nil {
		return false
	}

	if boundary, ok := ph.activeRemoteBoundary(ctx, failedPath); ok {
		ph.logger.Debug("handle403: path already under known remote read-only boundary",
			slog.String("path", failedPath),
			slog.String("boundary", boundary),
		)

		return true
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
	perms, err := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		return ph.handlePermissionCheckError(ctx, err, failedPath, parentFolder)
	}

	if graph.HasWriteAccess(perms) {
		ph.logger.Debug("handle403: transient 403, folder is writable",
			slog.String("path", failedPath),
		)

		return false
	}

	// Folder is read-only. Walk up to find the highest read-only ancestor.
	boundary := ph.walkPermissionBoundary(ctx, bl, parentFolder, sc, remoteDriveID)

	return ph.recordRemotePermissionBoundary(ctx, boundary, "folder is read-only (no write access)", http.StatusForbidden, failedPath)
}

// handlePermissionCheckError handles errors from ListItemPermissions during
// 403 processing. If the item is not found (404), records a permission issue
// to prevent infinite retries and returns true. Otherwise logs a warning and
// returns false (caller should proceed with normal failure recording).
func (ph *PermissionHandler) handlePermissionCheckError(ctx context.Context, err error, failedPath, parentFolder string) bool {
	if errors.Is(err, graph.ErrNotFound) {
		ph.logger.Warn("handle403: folder not found, recording as permission denied",
			slog.String("path", parentFolder),
		)

		return ph.recordRemotePermissionBoundary(
			ctx,
			parentFolder,
			"folder not found on remote (deleted or inaccessible)",
			http.StatusNotFound,
			failedPath,
		)
	}

	ph.logger.Warn("handle403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.String("error", err.Error()),
	)

	return false
}

func (ph *PermissionHandler) activeRemoteBoundary(ctx context.Context, failedPath string) (string, bool) {
	for _, boundary := range ph.DeniedPrefixes(ctx) {
		if failedPath == boundary || strings.HasPrefix(failedPath, boundary+"/") {
			return boundary, true
		}
	}

	return "", false
}

func (ph *PermissionHandler) recordRemotePermissionBoundary(
	ctx context.Context,
	boundary string,
	errMsg string,
	httpStatus int,
	failedPath string,
) bool {
	scopeKey := synctypes.SKPermRemote(boundary)

	if issueErr := ph.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       boundary,
		DriveID:    ph.driveID,
		Direction:  synctypes.DirectionUpload,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssuePermissionDenied,
		ErrMsg:     errMsg,
		HTTPStatus: httpStatus,
		ScopeKey:   scopeKey,
	}, nil); issueErr != nil {
		ph.logger.Warn("handle403: failed to record permission issue",
			slog.String("path", boundary),
			slog.String("error", issueErr.Error()),
		)

		return false
	}

	if ph.scopeMgr.isWatchMode() {
		// Remote permission recovery is recheck-driven, not trial-driven, so
		// the block has zero NextTrialAt and only acts as recursive admission control.
		ph.scopeMgr.setScopeBlock(scopeKey, &synctypes.ScopeBlock{
			Key:       scopeKey,
			IssueType: synctypes.IssuePermissionDenied,
			BlockedAt: ph.nowFn(),
		})
	}

	ph.logger.Info("handle403: read-only remote boundary detected, writes suppressed recursively",
		slog.String("boundary", boundary),
		slog.String("trigger_path", failedPath),
		slog.String("scope_key", scopeKey.String()),
	)

	return true
}

// walkPermissionBoundary walks UP the folder hierarchy to find the highest
// read-only ancestor. Returns the boundary folder path.
func (ph *PermissionHandler) walkPermissionBoundary(
	ctx context.Context, bl *synctypes.Baseline, startFolder string, sc *synctypes.Shortcut, remoteDriveID driveid.ID,
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

		parentPerms, parentErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentID)
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
func (ph *PermissionHandler) recheckPermissions(ctx context.Context, bl *synctypes.Baseline, shortcuts []synctypes.Shortcut) {
	if ph.permChecker == nil {
		return
	}

	issues, err := ph.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssuePermissionDenied)
	if err != nil || len(issues) == 0 {
		return
	}

	for i := range issues {
		issue := &issues[i]
		if !issue.ScopeKey.IsPermRemote() {
			continue
		}

		sc := findShortcutForPath(shortcuts, issue.Path)
		if sc == nil {
			ph.releaseRemotePermissionBoundary(ctx, issue, "shortcut no longer present; releasing remote permission boundary")

			continue
		}

		remoteDriveID := driveid.New(sc.RemoteDrive)
		remoteItemID := resolveRemoteItemID(bl, issue.Path, remoteDriveID)

		if remoteItemID == "" {
			ph.releaseRemotePermissionBoundary(ctx, issue, "remote permission boundary no longer resolvable; releasing stale scope")

			continue
		}

		perms, permErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
		if permErr != nil {
			ph.releaseRemotePermissionBoundary(ctx, issue, "permission recheck inconclusive; failing open")

			continue
		}

		if graph.HasWriteAccess(perms) {
			ph.releaseRemotePermissionBoundary(ctx, issue, "permission granted; releasing remote permission boundary")
			continue
		}

		if ph.scopeMgr.isWatchMode() {
			ph.scopeMgr.setScopeBlock(issue.ScopeKey, &synctypes.ScopeBlock{
				Key:       issue.ScopeKey,
				IssueType: synctypes.IssuePermissionDenied,
				BlockedAt: ph.nowFn(),
			})
		}
	}
}

func (ph *PermissionHandler) releaseRemotePermissionBoundary(
	ctx context.Context,
	issue *synctypes.SyncFailureRow,
	reason string,
) {
	if clearErr := ph.baseline.ClearSyncFailure(ctx, issue.Path, issue.DriveID); clearErr != nil {
		ph.logger.Warn("recheckPermissions: failed to clear failure",
			slog.String("path", issue.Path),
			slog.String("error", clearErr.Error()),
		)

		return
	}

	if ph.scopeMgr.isWatchMode() {
		ph.scopeMgr.onScopeClear(ctx, issue.ScopeKey)
	}

	ph.logger.Info(reason,
		slog.String("path", issue.Path),
		slog.String("scope_key", issue.ScopeKey.String()),
	)
}

// handleLocalPermission processes os.ErrPermission results from workers.
// It walks up from the failed path to find the deepest inaccessible ancestor
// directory, records a local_permission_denied failure, and creates a scope
// block for the directory subtree (R-2.10.12).
func (ph *PermissionHandler) handleLocalPermission(ctx context.Context, r *synctypes.WorkerResult) {
	// If the sync root itself is inaccessible, WARN loudly — don't silently
	// block everything behind a scope block. The sync root being inaccessible
	// is fundamentally different from a subdirectory denial: ALL operations
	// will fail, and the user needs a clear, actionable message.
	if !isDirAccessible(ph.syncRoot) {
		ph.logger.Warn("sync root directory is inaccessible",
			slog.String("path", ph.syncRoot),
			slog.String("error", r.ErrMsg),
		)

		// Record as a top-level actionable failure, not a scope block.
		if recErr := ph.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      r.Path,
			DriveID:   ph.driveID,
			Direction: directionFromAction(r.ActionType),
			IssueType: synctypes.IssueLocalPermissionDenied,
			Category:  synctypes.CategoryActionable,
			ErrMsg:    "sync root directory not accessible (check filesystem permissions)",
		}, nil); recErr != nil {
			ph.logger.Warn("handleLocalPermission: failed to record sync root issue",
				slog.String("path", r.Path),
				slog.String("error", recErr.Error()),
			)
		}

		return
	}

	// Walk up from the file's parent directory to find the deepest inaccessible ancestor.
	absPath := filepath.Join(ph.syncRoot, r.Path)
	parentDir := filepath.Dir(absPath)

	// Check if the parent directory is accessible (readable). os.Stat is
	// insufficient — it succeeds on chmod 000 dirs because stat() only needs
	// parent execute permission. os.Open tests actual read access.
	if isDirAccessible(parentDir) {
		// Parent directory is accessible — this is a file-level permission issue.
		// Record the failure at file level with no scope block.
		if recErr := ph.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      r.Path,
			DriveID:   ph.driveID,
			Direction: directionFromAction(r.ActionType),
			IssueType: synctypes.IssueLocalPermissionDenied,
			Category:  synctypes.CategoryActionable,
			ErrMsg:    "file not accessible (check filesystem permissions)",
		}, nil); recErr != nil {
			ph.logger.Warn("handleLocalPermission: failed to record file-level issue",
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
	relBoundary, relErr := filepath.Rel(ph.syncRoot, boundary)
	if relErr != nil {
		// Shouldn't happen — boundary is under syncRoot. Fall back to recording at file level.
		ph.logger.Warn("handleLocalPermission: failed to relativize boundary path",
			slog.String("boundary", boundary),
			slog.String("error", relErr.Error()),
		)

		return
	}

	scopeKey := synctypes.SKPermDir(relBoundary)

	// Record one failure at the boundary directory level.
	if recErr := ph.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      relBoundary,
		DriveID:   ph.driveID,
		Direction: directionFromAction(r.ActionType),
		IssueType: synctypes.IssueLocalPermissionDenied,
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "directory not accessible (check filesystem permissions)",
		ScopeKey:  scopeKey,
	}, nil); recErr != nil {
		ph.logger.Warn("handleLocalPermission: failed to record directory-level issue",
			slog.String("path", relBoundary),
			slog.String("error", recErr.Error()),
		)
	}

	// Create a scope block — no trials for permission blocks (rechecked per-pass
	// by recheckLocalPermissions instead).
	ph.scopeMgr.setScopeBlock(scopeKey, &synctypes.ScopeBlock{
		Key:       scopeKey,
		IssueType: synctypes.IssueLocalPermissionDenied,
		BlockedAt: ph.nowFn(),
	})

	ph.logger.Info("local permission denied: directory blocked",
		slog.String("boundary", relBoundary),
		slog.String("trigger_path", r.Path),
	)
}

// recheckLocalPermissions rechecks directory-level local permission denials
// at the start of each sync pass. If a directory is now accessible, clears
// the failure and releases the scope block (R-2.10.13).
func (ph *PermissionHandler) recheckLocalPermissions(ctx context.Context) {
	issues, err := ph.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
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
		absDir := filepath.Join(ph.syncRoot, dirPath)

		if !isDirAccessible(absDir) {
			// Still inaccessible — keep the block.
			continue
		}

		// Directory is accessible again — clear the failure and release the scope.
		if clearErr := ph.baseline.ClearSyncFailure(ctx, issue.Path, issue.DriveID); clearErr != nil {
			ph.logger.Warn("recheckLocalPermissions: failed to clear failure",
				slog.String("path", issue.Path),
				slog.String("error", clearErr.Error()),
			)

			continue
		}

		if ph.scopeMgr.isWatchMode() {
			ph.scopeMgr.onScopeClear(ctx, issue.ScopeKey)
		}

		ph.logger.Info("local permission restored, clearing denial",
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
func (ph *PermissionHandler) clearScannerResolvedPermissions(ctx context.Context, observedPaths map[string]bool) {
	if len(observedPaths) == 0 {
		return
	}

	issues, err := ph.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
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

		if clearErr := ph.baseline.ClearSyncFailure(ctx, issue.Path, issue.DriveID); clearErr != nil {
			ph.logger.Warn("clearScannerResolvedPermissions: failed to clear failure",
				slog.String("path", issue.Path),
				slog.String("error", clearErr.Error()),
			)

			continue
		}

		if !issue.ScopeKey.IsZero() && ph.scopeMgr.isWatchMode() {
			ph.scopeMgr.onScopeClear(ctx, issue.ScopeKey)
		}

		ph.logger.Info("scanner resolved permission denial",
			slog.String("path", issue.Path),
			slog.String("scope_key", issue.ScopeKey.String()),
		)
	}
}
