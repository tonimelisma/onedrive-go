package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ActiveRemoteBlockedBoundaries returns all persisted remote permission
// boundaries currently blocking write admission under read-only subtrees.
// Runtime blocking derives directly from persisted block_scopes.
func (ph *PermissionHandler) ActiveRemoteBlockedBoundaries(ctx context.Context) []string {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil {
		ph.logger.Warn("ActiveRemoteBlockedBoundaries: failed to list block scopes",
			slog.String("error", err.Error()),
		)

		return nil
	}

	seen := make(map[string]bool, len(blocks))
	var prefixes []string

	for i := range blocks {
		if blocks[i] == nil || !blocks[i].Key.IsPermRemoteWrite() {
			continue
		}

		boundary := blocks[i].Key.CoveredPath()
		if seen[boundary] {
			continue
		}

		seen[boundary] = true
		prefixes = append(prefixes, boundary)
	}

	sort.Strings(prefixes)

	return prefixes
}

// handle403 is called when a worker reports an HTTP 403 on a remote write.
// It probes Graph permissions to distinguish real write denial from a
// transient 403 and returns a decision for the engine-owned apply layer.
func (ph *PermissionHandler) handle403(
	ctx context.Context,
	bl *Baseline,
	failedPath string,
	actionType ActionType,
) PermissionEvidence {
	if ph.permChecker == nil {
		return PermissionEvidence{}
	}

	if boundary, ok := ph.activeRemoteBoundary(ctx, failedPath); ok {
		ph.logger.Debug("handle403: path already under known remote read-only boundary",
			slog.String("path", failedPath),
			slog.String("boundary", boundary),
		)

		return PermissionEvidence{
			Kind:         permissionEvidenceKnownActiveBoundary,
			BoundaryPath: boundary,
			TriggerPath:  failedPath,
			IssueType:    IssueRemoteWriteDenied,
		}
	}

	root := ph.permissionRoot()
	if root == nil {
		return PermissionEvidence{}
	}

	remoteDriveID := driveid.New(root.remoteDrive)
	parentFolder := remoteParentPath(failedPath, root.localPath)
	parentItemID := resolveBoundaryRemoteItemID(bl, parentFolder, remoteDriveID, root)
	if parentItemID == "" {
		parentFolder = root.localPath
		parentItemID = root.remoteItem
	}

	perms, err := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		return ph.handlePermissionCheckError(err, failedPath, parentFolder, actionType)
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

		return PermissionEvidence{}
	case graph.PermissionWriteAccessInconclusive:
		ph.logger.Warn("handle403: permission evidence inconclusive, not suppressing",
			slog.String("path", failedPath),
		)

		return PermissionEvidence{}
	case graph.PermissionWriteAccessReadOnly:
	}

	boundary := ph.walkPermissionBoundary(ctx, bl, parentFolder, root, remoteDriveID)

	return ph.remoteBoundaryEvidence(
		boundary,
		"folder is read-only (no write access)",
		http.StatusForbidden,
		failedPath,
		actionType,
	)
}

// handlePermissionCheckError handles errors from ListItemPermissions during
// remote write-denial probing. If the target folder is not found, treat the
// boundary as unreadable for writes; otherwise fall back to generic failure
// handling by returning an unmatched decision.
func (ph *PermissionHandler) handlePermissionCheckError(
	err error,
	failedPath string,
	parentFolder string,
	actionType ActionType,
) PermissionEvidence {
	if errors.Is(err, graph.ErrNotFound) {
		ph.logger.Warn("handle403: folder not found, recording as permission denied",
			slog.String("path", parentFolder),
		)

		return ph.remoteBoundaryEvidence(
			parentFolder,
			"folder not found on remote (deleted or inaccessible)",
			http.StatusNotFound,
			failedPath,
			actionType,
		)
	}

	ph.logger.Warn("handle403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.String("error", err.Error()),
	)

	return PermissionEvidence{}
}

func (ph *PermissionHandler) activeRemoteBoundary(ctx context.Context, failedPath string) (string, bool) {
	for _, boundary := range ph.ActiveRemoteBlockedBoundaries(ctx) {
		if remoteBoundaryContainsPath(failedPath, boundary) {
			return boundary, true
		}
	}

	return "", false
}

func (ph *PermissionHandler) remoteBoundaryEvidence(
	boundary string,
	errMsg string,
	httpStatus int,
	failedPath string,
	actionType ActionType,
) PermissionEvidence {
	_ = actionType

	return PermissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: boundary,
		TriggerPath:  failedPath,
		IssueType:    IssueRemoteWriteDenied,
		LastError:    errMsg,
		HTTPStatus:   httpStatus,
	}
}

func (ph *PermissionHandler) walkPermissionBoundary(
	ctx context.Context,
	bl *Baseline,
	startFolder string,
	root *remoteBoundaryRoot,
	remoteDriveID driveid.ID,
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

		access := graph.EvaluateWriteAccess(parentPerms, ph.accountEmail)
		if access == graph.PermissionWriteAccessWritable || access == graph.PermissionWriteAccessInconclusive {
			break
		}

		boundary = parent
	}

	return boundary
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
