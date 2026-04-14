package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// PermissionHandler encapsulates capability-aware permission handling. It owns
// remote write classification, local read/write denial handling, and
// maintenance rechecks that are the only path allowed to clear permission
// blocks.
type PermissionHandler struct {
	store        *SyncStore
	permChecker  PermissionChecker
	remoteReader driveops.Downloader
	syncTree     *synctree.Root
	driveID      driveid.ID
	accountEmail string
	rootItemID   string
	logger       *slog.Logger
	nowFn        func() time.Time
}

// HasPermChecker reports whether a remote permission checker is configured.
// Used by the engine to skip permission-related API calls when no checker
// is available (e.g., personal drives without shared folders).
func (ph *PermissionHandler) HasPermChecker() bool {
	return ph.permChecker != nil
}

// HasRemoteReadProbe reports whether the handler can probe remote readability.
func (ph *PermissionHandler) HasRemoteReadProbe() bool {
	return ph.remoteReader != nil
}

// DeniedPrefixes returns all active remote write-denied boundaries. The
// planner uses these prefixes to suppress remote-mutating actions under known
// blocked shared subtrees before they reach execution.
func (ph *PermissionHandler) DeniedPrefixes(ctx context.Context) []string {
	issues, err := ph.store.ListRemoteBlockedFailures(ctx)
	if err != nil {
		ph.logger.Warn("DeniedPrefixes: failed to list remote blocked failures",
			slog.String("error", err.Error()),
		)

		return nil
	}

	seen := make(map[string]bool, len(issues))
	var prefixes []string

	for i := range issues {
		if !issues[i].ScopeKey.IsPermRemoteWrite() {
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

// handleRemoteWrite403 is called when a worker reports an HTTP 403 while
// attempting a remote write action on shared content. It queries Graph
// permissions to determine whether the folder is truly not writable and
// returns a decision for the engine to apply.
func (ph *PermissionHandler) handleRemoteWrite403(
	ctx context.Context,
	bl *Baseline,
	r *WorkerResult,
	shortcuts []Shortcut,
) PermissionCheckDecision {
	if ph.permChecker == nil {
		return PermissionCheckDecision{}
	}

	failedPath := r.FailurePath
	if failedPath == "" {
		failedPath = r.Path
	}

	if boundary, ok := ph.activeRemoteBoundary(ctx, failedPath); ok {
		ph.logger.Debug("handleRemoteWrite403: path already under known remote write-denied boundary",
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

	sc := ph.findPermissionShortcut(shortcuts, failedPath)
	if sc == nil {
		return PermissionCheckDecision{}
	}

	remoteDriveID := driveid.New(sc.RemoteDrive)

	// Resolve the parent folder's remote item ID from baseline.
	// If not in baseline (e.g., brand-new local file), fall back to the
	// shortcut root. This means the boundary walk won't find intermediate
	// read-only folders for brand-new content, but will still correctly
	// suppress at the shortcut root level.
	parentFolder := remoteParentPath(failedPath, sc.LocalPath)
	parentItemID := resolveBoundaryRemoteItemID(bl, parentFolder, remoteDriveID, sc)

	if parentItemID == "" {
		parentFolder = sc.LocalPath
		parentItemID = sc.RemoteItem
	}

	// Query permissions on the parent folder.
	perms, err := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		return ph.handlePermissionCheckError(ctx, err, failedPath, parentFolder, r.ActionType, remoteDriveID)
	}

	access := graph.EvaluateWriteAccess(perms, ph.accountEmail)
	ph.logger.Debug("handleRemoteWrite403: evaluated folder permissions",
		slog.String("path", failedPath),
		slog.String("account_email", ph.accountEmail),
		slog.String("access", access.String()),
		slog.Int("permission_count", len(perms)),
	)

	switch access {
	case graph.PermissionWriteAccessWritable:
		ph.logger.Debug("handleRemoteWrite403: transient 403, folder is writable",
			slog.String("path", failedPath),
		)

		return PermissionCheckDecision{}
	case graph.PermissionWriteAccessInconclusive:
		ph.logger.Warn("handleRemoteWrite403: permission evidence inconclusive, not suppressing",
			slog.String("path", failedPath),
		)

		return PermissionCheckDecision{}
	case graph.PermissionWriteAccessReadOnly:
	}

	// Folder is read-only. Walk up to find the highest read-only ancestor.
	boundary := ph.walkPermissionBoundary(ctx, bl, parentFolder, sc, remoteDriveID)

	return ph.remoteBoundaryDecision(
		boundary,
		"folder is not writable",
		http.StatusForbidden,
		failedPath,
		r.ActionType,
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
		ph.logger.Warn("handleRemoteWrite403: folder not found, recording as remote write denied",
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

	ph.logger.Warn("handleRemoteWrite403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.String("error", err.Error()),
	)

	return PermissionCheckDecision{}
}

func (ph *PermissionHandler) activeRemoteBoundary(ctx context.Context, failedPath string) (string, bool) {
	for _, boundary := range ph.DeniedPrefixes(ctx) {
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
	scopeKey := SKPermRemoteWrite(boundary)

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
			IssueType:  IssueRemoteWriteDenied,
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
	ctx context.Context, bl *Baseline, startFolder string, sc *Shortcut, remoteDriveID driveid.ID,
) string {
	boundary := startFolder

	for {
		parent, ok := remoteBoundaryParent(boundary, sc.LocalPath)
		if !ok {
			break
		}

		parentID := resolveBoundaryRemoteItemID(bl, parent, remoteDriveID, sc)
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

// recheckRemoteWritePermissions re-queries all remote write-denied held rows at
// the start of each sync pass. If a folder is now writable, the scope is
// released and writes resume.
func (ph *PermissionHandler) recheckRemoteWritePermissions(
	ctx context.Context,
	bl *Baseline,
	shortcuts []Shortcut,
) []PermissionRecheckDecision {
	return ph.recheckRemoteWritePermissionsForScopeKeys(ctx, bl, shortcuts, nil)
}

func (ph *PermissionHandler) recheckRemoteWritePermissionsForScopeKeys(
	ctx context.Context,
	bl *Baseline,
	shortcuts []Shortcut,
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
		if !issue.ScopeKey.IsPermRemoteWrite() {
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

		sc := ph.findPermissionShortcut(shortcuts, boundaryPath)
		if sc == nil {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "shortcut no longer present; releasing remote write scope",
			})
			continue
		}

		remoteDriveID := driveid.New(sc.RemoteDrive)
		remoteItemID := resolveBoundaryRemoteItemID(bl, boundaryPath, remoteDriveID, sc)

		if remoteItemID == "" {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "remote write boundary no longer resolvable; releasing stale scope",
			})
			continue
		}

		perms, permErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
		if permErr != nil {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "remote write recheck inconclusive; failing open",
			})
			continue
		}

		switch graph.EvaluateWriteAccess(perms, ph.accountEmail) {
		case graph.PermissionWriteAccessWritable:
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "remote write permission granted; releasing remote write scope",
			})
			continue
		case graph.PermissionWriteAccessInconclusive:
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     boundaryPath,
				ScopeKey: issue.ScopeKey,
				Reason:   "remote write recheck inconclusive; failing open",
			})
			continue
		case graph.PermissionWriteAccessReadOnly:
		}

		decisions = append(decisions, PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: issue.ScopeKey,
			Reason:   "remote write boundary still denied",
		})
	}

	return decisions
}

var errRemoteReadProbeSatisfied = errors.New("sync: remote read probe satisfied")

type remoteReadProbeWriter struct {
	written bool
}

func (w *remoteReadProbeWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !w.written {
		w.written = true
		return 1, errRemoteReadProbeSatisfied
	}

	return 0, errRemoteReadProbeSatisfied
}

func (ph *PermissionHandler) probeRemoteRead(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
) bool {
	if ph.remoteReader == nil || itemID == "" {
		return false
	}

	writer := &remoteReadProbeWriter{}

	var err error
	if downloader, ok := ph.remoteReader.(driveops.RangeDownloader); ok {
		_, err = downloader.DownloadRange(ctx, driveID, itemID, writer, 0)
	} else {
		_, err = ph.remoteReader.Download(ctx, driveID, itemID, writer)
	}

	return err == nil || errors.Is(err, errRemoteReadProbeSatisfied)
}

func (ph *PermissionHandler) recheckRemoteReadPermissions(
	ctx context.Context,
) []PermissionRecheckDecision {
	if ph.remoteReader == nil {
		return nil
	}

	issues, err := ph.store.ListSyncFailuresByIssueType(ctx, IssueRemoteReadDenied)
	if err != nil || len(issues) == 0 {
		return nil
	}

	decisions := make([]PermissionRecheckDecision, 0, len(issues))
	for i := range issues {
		issue := &issues[i]
		if issue.ItemID == "" {
			continue
		}
		if !ph.probeRemoteRead(ctx, issue.DriveID, issue.ItemID) {
			continue
		}
		decisions = append(decisions, PermissionRecheckDecision{
			Kind:    permissionRecheckClearFileFailure,
			Path:    issue.Path,
			DriveID: issue.DriveID,
			Reason:  "remote read permission restored",
		})
	}

	return decisions
}

func (ph *PermissionHandler) findPermissionShortcut(shortcuts []Shortcut, path string) *Shortcut {
	return findShortcutForPath(ph.permissionShortcuts(shortcuts), path)
}

func (ph *PermissionHandler) permissionShortcuts(shortcuts []Shortcut) []Shortcut {
	if ph.rootItemID == "" {
		return shortcuts
	}

	for i := range shortcuts {
		if shortcuts[i].LocalPath == "" && shortcuts[i].RemoteItem == ph.rootItemID {
			return shortcuts
		}
	}

	augmented := make([]Shortcut, 0, len(shortcuts)+1)
	augmented = append(augmented, shortcuts...)
	augmented = append(augmented, Shortcut{
		ItemID:      ph.rootItemID,
		RemoteDrive: ph.driveID.String(),
		RemoteItem:  ph.rootItemID,
		LocalPath:   "",
	})

	return augmented
}

func resolveBoundaryRemoteItemID(
	bl *Baseline,
	boundaryPath string,
	driveID driveid.ID,
	sc *Shortcut,
) string {
	if sc != nil && boundaryPath == sc.LocalPath {
		return sc.RemoteItem
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

// handleLocalPermission processes local permission failures using the concrete
// capability and path reported by the worker result.
func (ph *PermissionHandler) handleLocalPermission(
	_ context.Context,
	r *WorkerResult,
) PermissionCheckDecision {
	capability := r.FailureCapability
	if capability == PermissionCapabilityUnknown {
		capability = defaultLocalCapabilityForAction(r.ActionType)
	}
	targetPath := r.FailurePath
	if targetPath == "" {
		targetPath = r.Path
	}

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
			targetPath,
			r.ActionType,
			capability,
			fmt.Sprintf("sync root directory not %s (check filesystem permissions)", localCapabilityAdjective(capability)),
		)
	}

	absPath, absErr := ph.syncTree.Abs(targetPath)
	if absErr != nil {
		ph.logger.Warn("handleLocalPermission: failed to resolve sync-tree path",
			slog.String("path", targetPath),
			slog.String("error", absErr.Error()),
		)

		return ph.localFilePermissionDecision(
			targetPath,
			r.ActionType,
			capability,
			fmt.Sprintf("path not %s (check filesystem permissions)", localCapabilityAdjective(capability)),
		)
	}
	parentDir := filepath.Dir(absPath)

	if isDirAccessibleForCapability(ph.syncTree, parentDir, capability) {
		return ph.localFilePermissionDecision(
			targetPath,
			r.ActionType,
			capability,
			fmt.Sprintf("path not %s (check filesystem permissions)", localCapabilityAdjective(capability)),
		)
	}

	boundary := ph.deepestDeniedBoundary(parentDir, capability)

	relBoundary, relErr := ph.syncTree.Rel(boundary)
	if relErr != nil {
		ph.logger.Warn("handleLocalPermission: failed to relativize boundary path",
			slog.String("boundary", boundary),
			slog.String("error", relErr.Error()),
		)

		return ph.localFilePermissionDecision(
			targetPath,
			r.ActionType,
			capability,
			fmt.Sprintf("path not %s (check filesystem permissions)", localCapabilityAdjective(capability)),
		)
	}

	return ph.localDirectoryPermissionDecision(relBoundary, targetPath, r.ActionType, capability)
}

func (ph *PermissionHandler) localFilePermissionDecision(
	path string,
	actionType ActionType,
	capability PermissionCapability,
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
			IssueType:  issueTypeForPermissionCapability(capability),
			Category:   CategoryActionable,
			ErrMsg:     errMsg,
		},
	}
}

func (ph *PermissionHandler) localDirectoryPermissionDecision(
	boundaryPath string,
	triggerPath string,
	actionType ActionType,
	capability PermissionCapability,
) PermissionCheckDecision {
	scopeKey := localScopeKeyForCapability(boundaryPath, capability)
	issueType := issueTypeForPermissionCapability(capability)

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
			IssueType:  issueType,
			Category:   CategoryActionable,
			ErrMsg:     fmt.Sprintf("directory not %s (check filesystem permissions)", localCapabilityAdjective(capability)),
			ScopeKey:   scopeKey,
		},
		ScopeBlock: ScopeBlock{
			Key:          scopeKey,
			IssueType:    issueType,
			TimingSource: ScopeTimingNone,
			BlockedAt:    ph.nowFn(),
		},
		BoundaryPath: boundaryPath,
		TriggerPath:  triggerPath,
	}
}

const localCapabilityAccessible = "accessible"

func localScopeKeyForCapability(path string, capability PermissionCapability) ScopeKey {
	switch capability {
	case PermissionCapabilityUnknown:
		return ScopeKey{}
	case PermissionCapabilityLocalRead:
		return SKPermLocalRead(path)
	case PermissionCapabilityLocalWrite:
		return SKPermLocalWrite(path)
	case PermissionCapabilityRemoteRead, PermissionCapabilityRemoteWrite:
		return ScopeKey{}
	default:
		return ScopeKey{}
	}
}

func localCapabilityAdjective(capability PermissionCapability) string {
	switch capability {
	case PermissionCapabilityUnknown:
		return localCapabilityAccessible
	case PermissionCapabilityLocalRead:
		return "readable"
	case PermissionCapabilityLocalWrite:
		return "writable"
	case PermissionCapabilityRemoteRead, PermissionCapabilityRemoteWrite:
		return localCapabilityAccessible
	default:
		return localCapabilityAccessible
	}
}

func defaultLocalCapabilityForAction(actionType ActionType) PermissionCapability {
	switch actionType {
	case ActionUpload:
		return PermissionCapabilityLocalRead
	case ActionDownload, ActionLocalDelete, ActionLocalMove, ActionFolderCreate, ActionConflict, ActionCleanup:
		return PermissionCapabilityLocalWrite
	case ActionRemoteDelete, ActionRemoteMove, ActionUpdateSynced:
		return PermissionCapabilityUnknown
	default:
		return PermissionCapabilityUnknown
	}
}

func (ph *PermissionHandler) deepestDeniedBoundary(parentDir string, capability PermissionCapability) string {
	boundary := parentDir
	rootPath := filepath.Clean(ph.syncTree.Path())
	for {
		if filepath.Clean(boundary) == rootPath {
			return boundary
		}
		parent := filepath.Dir(boundary)
		if parent == boundary {
			return boundary
		}
		if _, err := ph.syncTree.Rel(parent); err != nil {
			return boundary
		}
		if isDirAccessibleForCapability(ph.syncTree, parent, capability) {
			return boundary
		}
		boundary = parent
	}
}

func (ph *PermissionHandler) recheckLocalPermissions(ctx context.Context) []PermissionRecheckDecision {
	issues := ph.listLocalPermissionIssues(ctx)
	if len(issues) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision

	for i := range issues {
		issue := &issues[i]

		switch {
		case issue.ScopeKey.IsPermLocalRead():
			if !isDirAccessibleForCapability(ph.syncTree, issue.ScopeKey.DirPath(), PermissionCapabilityLocalRead) {
				decisions = append(decisions, PermissionRecheckDecision{
					Kind:     permissionRecheckKeepScope,
					Path:     issue.Path,
					ScopeKey: issue.ScopeKey,
					Reason:   "local read scope still denied",
				})
				continue
			}
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     issue.Path,
				ScopeKey: issue.ScopeKey,
				Reason:   "local read permission restored",
			})
		case issue.ScopeKey.IsPermLocalWrite():
			if !isDirAccessibleForCapability(ph.syncTree, issue.ScopeKey.DirPath(), PermissionCapabilityLocalWrite) {
				decisions = append(decisions, PermissionRecheckDecision{
					Kind:     permissionRecheckKeepScope,
					Path:     issue.Path,
					ScopeKey: issue.ScopeKey,
					Reason:   "local write scope still denied",
				})
				continue
			}
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckReleaseScope,
				Path:     issue.Path,
				ScopeKey: issue.ScopeKey,
				Reason:   "local write permission restored",
			})
		case issue.IssueType == IssueLocalReadDenied:
			if isFileReadable(ph.syncTree, issue.Path) {
				decisions = append(decisions, PermissionRecheckDecision{
					Kind:    permissionRecheckClearFileFailure,
					Path:    issue.Path,
					DriveID: issue.DriveID,
					Reason:  "local read permission restored",
				})
			}
		case issue.IssueType == IssueLocalWriteDenied:
			if isPathWritable(ph.syncTree, issue.Path) {
				decisions = append(decisions, PermissionRecheckDecision{
					Kind:    permissionRecheckClearFileFailure,
					Path:    issue.Path,
					DriveID: issue.DriveID,
					Reason:  "local write permission restored",
				})
			}
		}
	}

	return decisions
}

func (ph *PermissionHandler) listLocalPermissionIssues(ctx context.Context) []SyncFailureRow {
	readIssues, readErr := ph.store.ListSyncFailuresByIssueType(ctx, IssueLocalReadDenied)
	writeIssues, writeErr := ph.store.ListSyncFailuresByIssueType(ctx, IssueLocalWriteDenied)
	if readErr != nil && writeErr != nil {
		return nil
	}

	issues := make([]SyncFailureRow, 0, len(readIssues)+len(writeIssues))
	issues = append(issues, readIssues...)
	issues = append(issues, writeIssues...)

	return issues
}
