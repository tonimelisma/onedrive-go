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
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// PermissionHandler encapsulates all permission-related logic extracted from
// Engine. It handles HTTP 403 responses, local permission denials, per-pass
// permission rechecks, and scanner-resolved permission clearing.
type PermissionHandler struct {
	store        *SyncStore
	permChecker  PermissionChecker
	syncTree     *synctree.Root
	driveID      driveid.ID
	accountEmail string
	rootItemID   string
	logger       *slog.Logger
	nowFn        func() time.Time
}

type remoteBoundaryRoot struct {
	remoteDrive string
	remoteItem  string
	localPath   string
}

// HasPermChecker reports whether a remote permission checker is configured.
// Used by the engine to skip permission-related API calls when no checker
// is available (e.g., personal drives without shared folders).
func (ph *PermissionHandler) HasPermChecker() bool {
	return ph.permChecker != nil
}

// ActiveRemoteBlockedBoundaries returns all persisted remote permission
// boundaries currently blocking write admission under read-only subtrees.
// The watch/runtime path uses these boundaries for active-scope checks;
// the legacy dry-run planner still consumes the same path list until that
// preview path is moved onto snapshot reconciliation.
func (ph *PermissionHandler) ActiveRemoteBlockedBoundaries(ctx context.Context) []string {
	issues, err := ph.store.ListRemoteBlockedFailures(ctx)
	if err != nil {
		ph.logger.Warn("ActiveRemoteBlockedBoundaries: failed to list remote blocked failures",
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
		if seen[boundary] {
			continue
		}

		seen[boundary] = true
		prefixes = append(prefixes, boundary)
	}

	sort.Strings(prefixes)

	return prefixes
}

// handle403 is called when a worker reports an HTTP 403 on a write action.
// It queries the Graph API to determine whether the folder is truly read-only
// and returns a decision for the engine to apply.
func (ph *PermissionHandler) handle403(
	ctx context.Context,
	bl *Baseline,
	failedPath string,
	actionType ActionType,
) PermissionCheckDecision {
	if ph.permChecker == nil {
		return PermissionCheckDecision{}
	}

	if boundary, ok := ph.activeRemoteBoundary(ctx, failedPath); ok {
		ph.logger.Debug("handle403: path already under known remote read-only boundary",
			slog.String("path", failedPath),
			slog.String("boundary", boundary),
		)

		return PermissionCheckDecision{
			Matched:      true,
			Kind:         permissionCheckNone,
			BoundaryPath: boundary,
			TriggerPath:  failedPath,
		}
	}

	root := ph.permissionRoot()
	if root == nil {
		return PermissionCheckDecision{}
	}

	remoteDriveID := driveid.New(root.remoteDrive)

	// Resolve the parent folder's remote item ID from baseline.
	// If not in baseline (e.g., brand-new local file), fall back to the
	// configured root. This means the boundary walk won't find intermediate
	// read-only folders for brand-new content, but will still correctly
	// suppress at the drive root level.
	parentFolder := remoteParentPath(failedPath, root.localPath)
	parentItemID := resolveBoundaryRemoteItemID(bl, parentFolder, remoteDriveID, root)

	if parentItemID == "" {
		parentFolder = root.localPath
		parentItemID = root.remoteItem
	}

	// Query permissions on the parent folder.
	perms, err := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		return ph.handlePermissionCheckError(ctx, err, failedPath, parentFolder, actionType, remoteDriveID)
	}

	access := graph.EvaluateWriteAccess(perms, ph.accountEmail)
	ph.logger.Debug("handle403: evaluated folder permissions",
		slog.String("path", failedPath),
		slog.String("account_email", ph.accountEmail),
		slog.String("access", access.String()),
		slog.Int("permission_count", len(perms)),
	)

	switch access {
	case graph.PermissionWriteAccessWritable:
		ph.logger.Debug("handle403: transient 403, folder is writable",
			slog.String("path", failedPath),
		)

		return PermissionCheckDecision{}
	case graph.PermissionWriteAccessInconclusive:
		ph.logger.Warn("handle403: permission evidence inconclusive, not suppressing",
			slog.String("path", failedPath),
		)

		return PermissionCheckDecision{}
	case graph.PermissionWriteAccessReadOnly:
	}

	// Folder is read-only. Walk up to find the highest read-only ancestor.
	boundary := ph.walkPermissionBoundary(ctx, bl, parentFolder, root, remoteDriveID)

	return ph.remoteBoundaryDecision(
		boundary,
		"folder is read-only (no write access)",
		http.StatusForbidden,
		failedPath,
		actionType,
		remoteDriveID,
	)
}

// handlePermissionCheckError handles errors from ListItemPermissions during
// 403 processing. If the item is not found (404), records a permission issue
// to prevent infinite retries and returns true. Otherwise logs a warning and
// returns false (caller should proceed with normal failure recording).
func (ph *PermissionHandler) handlePermissionCheckError(
	_ context.Context,
	err error,
	failedPath,
	parentFolder string,
	actionType ActionType,
	remoteDriveID driveid.ID,
) PermissionCheckDecision {
	if errors.Is(err, graph.ErrNotFound) {
		ph.logger.Warn("handle403: folder not found, recording as permission denied",
			slog.String("path", parentFolder),
		)

		return ph.remoteBoundaryDecision(
			parentFolder,
			"folder not found on remote (deleted or inaccessible)",
			http.StatusNotFound,
			failedPath,
			actionType,
			remoteDriveID,
		)
	}

	ph.logger.Warn("handle403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.String("error", err.Error()),
	)

	return PermissionCheckDecision{}
}

func (ph *PermissionHandler) activeRemoteBoundary(ctx context.Context, failedPath string) (string, bool) {
	for _, boundary := range ph.ActiveRemoteBlockedBoundaries(ctx) {
		if remoteBoundaryContainsPath(failedPath, boundary) {
			return boundary, true
		}
	}

	return "", false
}

func (ph *PermissionHandler) remoteBoundaryDecision(
	boundary string,
	errMsg string,
	httpStatus int,
	failedPath string,
	actionType ActionType,
	failureDriveID driveid.ID,
) PermissionCheckDecision {
	scopeKey := SKPermRemote(boundary)

	return PermissionCheckDecision{
		Matched: true,
		Kind:    permissionCheckActivateDerivedScope,
		Failure: SyncFailureParams{
			Path:       failedPath,
			DriveID:    failureDriveID,
			Direction:  directionFromAction(actionType),
			ActionType: actionType,
			Role:       FailureRoleHeld,
			Category:   CategoryTransient,
			IssueType:  IssueSharedFolderBlocked,
			ErrMsg:     errMsg,
			HTTPStatus: httpStatus,
			ScopeKey:   scopeKey,
		},
		ScopeKey:     scopeKey,
		BoundaryPath: boundary,
		TriggerPath:  failedPath,
	}
}

// walkPermissionBoundary walks UP the folder hierarchy to find the highest
// read-only ancestor. Returns the boundary folder path.
func (ph *PermissionHandler) walkPermissionBoundary(
	ctx context.Context, bl *Baseline, startFolder string, root *remoteBoundaryRoot, remoteDriveID driveid.ID,
) string {
	boundary := startFolder

	for {
		parent, ok := remoteBoundaryParent(boundary, root.localPath)
		if !ok {
			break
		}

		parentID := resolveBoundaryRemoteItemID(bl, parent, remoteDriveID, root)
		if parentID == "" {
			break
		}

		parentPerms, parentErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentID)
		if parentErr != nil {
			break
		}

		if access := graph.EvaluateWriteAccess(parentPerms, ph.accountEmail); access == graph.PermissionWriteAccessWritable ||
			access == graph.PermissionWriteAccessInconclusive {
			break
		}

		boundary = parent
	}

	return boundary
}

// recheckPermissions re-queries all permission_denied sync_failures at the
// start of each sync pass. If a folder is now writable, the issue is cleared
// and writes resume. Runs every pass (typically 5 min in watch mode).
func (ph *PermissionHandler) recheckPermissions(
	ctx context.Context,
	bl *Baseline,
) []PermissionRecheckDecision {
	return ph.recheckPermissionsForScopeKeys(ctx, bl, nil)
}

func (ph *PermissionHandler) recheckPermissionsForScopeKeys(
	ctx context.Context,
	bl *Baseline,
	scopeFilter map[ScopeKey]bool,
) []PermissionRecheckDecision {
	if ph.permChecker == nil {
		return nil
	}

	issues, err := ph.store.ListRemoteBlockedFailures(ctx)
	if err != nil || len(issues) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision
	seen := make(map[ScopeKey]bool, len(issues))

	for i := range issues {
		issue := &issues[i]
		if !issue.ScopeKey.IsPermRemote() {
			continue
		}
		if seen[issue.ScopeKey] {
			continue
		}
		if len(scopeFilter) > 0 && !scopeFilter[issue.ScopeKey] {
			continue
		}
		seen[issue.ScopeKey] = true

		boundaryPath := issue.ScopeKey.RemotePath()

		root := ph.permissionRoot()
		if root == nil {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "configured remote root no longer present; releasing remote permission boundary",
			})
			continue
		}

		remoteDriveID := driveid.New(root.remoteDrive)
		remoteItemID := resolveBoundaryRemoteItemID(bl, boundaryPath, remoteDriveID, root)

		if remoteItemID == "" {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "remote permission boundary no longer resolvable; releasing stale scope",
			})
			continue
		}

		perms, permErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
		if permErr != nil {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "permission recheck inconclusive; failing open",
			})
			continue
		}

		switch graph.EvaluateWriteAccess(perms, ph.accountEmail) {
		case graph.PermissionWriteAccessWritable:
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "permission granted; releasing remote permission boundary",
			})
			continue
		case graph.PermissionWriteAccessInconclusive:
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "permission recheck inconclusive; failing open",
			})
			continue
		case graph.PermissionWriteAccessReadOnly:
		}

		decisions = append(decisions, PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: issue.ScopeKey,
			Reason:   "remote permission boundary still denied",
		})
	}

	return decisions
}

func (ph *PermissionHandler) permissionRoot() *remoteBoundaryRoot {
	if ph.rootItemID == "" {
		return nil
	}

	return &remoteBoundaryRoot{
		remoteDrive: ph.driveID.String(),
		remoteItem:  ph.rootItemID,
		localPath:   "",
	}
}

func resolveBoundaryRemoteItemID(
	bl *Baseline,
	boundaryPath string,
	driveID driveid.ID,
	root *remoteBoundaryRoot,
) string {
	if root != nil && boundaryPath == root.localPath {
		return root.remoteItem
	}

	return resolveRemoteItemID(bl, boundaryPath, driveID)
}

func remoteParentPath(path string, rootPath string) string {
	parent := filepath.Dir(path)
	if parent == "." || parent == "/" {
		return rootPath
	}

	return parent
}

func remoteBoundaryContainsPath(path string, boundary string) bool {
	if boundary == "" {
		return true
	}

	return path == boundary || strings.HasPrefix(path, boundary+"/")
}

func remoteBoundaryParent(boundary string, rootPath string) (string, bool) {
	if boundary == rootPath {
		return "", false
	}

	parent := filepath.Dir(boundary)
	if parent == "." || parent == "/" {
		return rootPath, true
	}
	if rootPath != "" && !remoteBoundaryContainsPath(parent, rootPath) {
		return "", false
	}

	return parent, true
}

// handleLocalPermission processes os.ErrPermission results from workers.
// It walks up from the failed path to find the deepest inaccessible ancestor
// directory, records a local_permission_denied failure, and creates a scope
// block for the directory subtree (R-2.10.12).
func (ph *PermissionHandler) handleLocalPermission(
	_ context.Context,
	r *WorkerResult,
) PermissionCheckDecision {
	// If the sync root itself is inaccessible, WARN loudly — don't silently
	// block everything behind a scope block. The sync root being inaccessible
	// is fundamentally different from a subdirectory denial: ALL operations
	// will fail, and the user needs a clear, actionable message.
	if !isDirAccessible(ph.syncTree, ".") {
		ph.logger.Warn("sync root directory is inaccessible",
			slog.String("path", ph.syncTree.Path()),
			slog.String("error", r.ErrMsg),
		)

		return ph.localFilePermissionDecision(
			r.Path,
			r.ActionType,
			"sync root directory not accessible (check filesystem permissions)",
		)
	}

	// Walk up from the file's parent directory to find the deepest inaccessible ancestor.
	absPath, absErr := ph.syncTree.Abs(r.Path)
	if absErr != nil {
		ph.logger.Warn("handleLocalPermission: failed to resolve sync-tree path",
			slog.String("path", r.Path),
			slog.String("error", absErr.Error()),
		)

		return ph.localFilePermissionDecision(
			r.Path,
			r.ActionType,
			"file not accessible (check filesystem permissions)",
		)
	}
	parentDir := filepath.Dir(absPath)

	// Check if the parent directory is accessible (readable). os.Stat is
	// insufficient — it succeeds on chmod 000 dirs because stat() only needs
	// parent execute permission. os.Open tests actual read access.
	if isDirAccessible(ph.syncTree, parentDir) {
		// Parent directory is accessible — this is a file-level permission issue.
		return ph.localFilePermissionDecision(
			r.Path,
			r.ActionType,
			"file not accessible (check filesystem permissions)",
		)
	}

	// Parent directory is inaccessible — walk up to find the deepest denied ancestor.
	boundary := ph.deepestDeniedBoundary(parentDir)

	// Convert boundary to relative path for recording.
	relBoundary, relErr := ph.syncTree.Rel(boundary)
	if relErr != nil {
		// Shouldn't happen — boundary is under syncRoot. Fall back to recording at file level.
		ph.logger.Warn("handleLocalPermission: failed to relativize boundary path",
			slog.String("boundary", boundary),
			slog.String("error", relErr.Error()),
		)

		return ph.localFilePermissionDecision(
			r.Path,
			r.ActionType,
			"file not accessible (check filesystem permissions)",
		)
	}

	return ph.localDirectoryPermissionDecision(relBoundary, r.Path, r.ActionType)
}

func (ph *PermissionHandler) localFilePermissionDecision(
	path string,
	actionType ActionType,
	errMsg string,
) PermissionCheckDecision {
	return PermissionCheckDecision{
		Matched: true,
		Kind:    permissionCheckRecordFileFailure,
		Failure: SyncFailureParams{
			Path:       path,
			DriveID:    ph.driveID,
			Direction:  directionFromAction(actionType),
			ActionType: actionType,
			Role:       FailureRoleItem,
			IssueType:  IssueLocalPermissionDenied,
			Category:   CategoryActionable,
			ErrMsg:     errMsg,
		},
	}
}

func (ph *PermissionHandler) localDirectoryPermissionDecision(
	boundaryPath string,
	triggerPath string,
	actionType ActionType,
) PermissionCheckDecision {
	scopeKey := SKPermDir(boundaryPath)

	return PermissionCheckDecision{
		Matched:  true,
		Kind:     permissionCheckActivateBoundaryScope,
		ScopeKey: scopeKey,
		Failure: SyncFailureParams{
			Path:       boundaryPath,
			DriveID:    ph.driveID,
			Direction:  directionFromAction(actionType),
			ActionType: actionType,
			Role:       FailureRoleBoundary,
			IssueType:  IssueLocalPermissionDenied,
			Category:   CategoryActionable,
			ErrMsg:     "directory not accessible (check filesystem permissions)",
			ScopeKey:   scopeKey,
		},
		ScopeBlock: ScopeBlock{
			Key:          scopeKey,
			IssueType:    IssueLocalPermissionDenied,
			TimingSource: ScopeTimingNone,
			BlockedAt:    ph.nowFn(),
		},
		BoundaryPath: boundaryPath,
		TriggerPath:  triggerPath,
	}
}

func (ph *PermissionHandler) deepestDeniedBoundary(parentDir string) string {
	boundary := parentDir
	for {
		parent := filepath.Dir(boundary)
		if parent == boundary {
			return boundary
		}
		if isDirAccessible(ph.syncTree, parent) {
			return boundary
		}
		boundary = parent
	}
}

// recheckLocalPermissions rechecks directory-level local permission denials
// at the start of each sync pass. If a directory is now accessible, clears
// the failure and releases the scope block (R-2.10.13).
func (ph *PermissionHandler) recheckLocalPermissions(ctx context.Context) []PermissionRecheckDecision {
	issues, err := ph.store.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	if err != nil || len(issues) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision

	for i := range issues {
		issue := &issues[i]

		// Only recheck directory-level issues (those with a perm:dir: scope key).
		if !issue.ScopeKey.IsPermDir() {
			continue
		}

		dirPath := issue.ScopeKey.DirPath()
		if !isDirAccessible(ph.syncTree, dirPath) {
			// Still inaccessible — keep the block.
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckKeepScope,
				Path:     issue.Path,
				ScopeKey: issue.ScopeKey,
				Reason:   "local permission denial still active",
			})
			continue
		}

		decisions = append(decisions, PermissionRecheckDecision{
			Kind:     permissionRecheckReleaseScope,
			Path:     issue.Path,
			ScopeKey: issue.ScopeKey,
			Reason:   "local permission restored, clearing denial",
		})
	}

	return decisions
}

// clearScannerResolvedPermissions checks whether the scanner observed paths
// that were previously blocked by local_permission_denied failures. If the
// scanner successfully accessed a path (it appeared in events), the
// permission issue is resolved — clear the failure and release any scope block.
//
// Implements R-2.10.10. Complements recheckLocalPermissions (R-2.10.13).
func (ph *PermissionHandler) clearScannerResolvedPermissions(
	ctx context.Context,
	observedPaths map[string]bool,
) []PermissionRecheckDecision {
	if len(observedPaths) == 0 {
		return nil
	}

	issues, err := ph.store.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	if err != nil || len(issues) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision

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

		if issue.ScopeKey.IsZero() {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:    permissionRecheckClearFileFailure,
				Path:    issue.Path,
				DriveID: issue.DriveID,
				Reason:  "scanner resolved permission denial",
			})
			continue
		}

		decisions = append(decisions, PermissionRecheckDecision{
			Kind:     permissionRecheckReleaseScope,
			Path:     issue.Path,
			ScopeKey: issue.ScopeKey,
			Reason:   "scanner resolved permission denial",
		})
	}

	return decisions
}
