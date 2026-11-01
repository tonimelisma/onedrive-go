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

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The handle403 probes below log at Debug only: each runs once per failing
// remote write, alongside the console's own failure line, and a boundary
// they record is reported once as a block scope when it starts. That covers
// the Graph probes; a failure to read the persisted block scopes is a store
// error and is returned, never read as "no blocked boundaries".
//
// ActiveRemoteBlockedBoundaries returns all persisted remote permission
// boundaries currently blocking write admission under read-only subtrees.
// Runtime blocking derives directly from persisted block_scopes.
func (ph *permissionHandler) ActiveRemoteBlockedBoundaries(ctx context.Context) ([]string, error) {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list remote block scopes: %w", err)
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

	return prefixes, nil
}

// handle403 is called when a worker reports an HTTP 403 on a remote write.
// It probes Graph permissions to distinguish real write denial from a
// transient 403 and returns a decision for the engine-owned apply layer. The
// error is a store failure reading the persisted boundaries; Graph probe
// failures are evidence (none), not errors.
func (ph *permissionHandler) handle403(
	ctx context.Context,
	bl *Baseline,
	failedPath string,
	actionType actionType,
) (permissionEvidence, error) {
	if ph.permChecker == nil {
		return permissionEvidence{}, nil
	}

	boundary, known, err := ph.activeRemoteBoundary(ctx, failedPath)
	if err != nil {
		return permissionEvidence{}, err
	}
	if known {
		ph.logger.Debug("handle403: path already under known remote read-only boundary",
			slog.String("path", failedPath),
			slog.String("boundary", boundary),
		)

		return permissionEvidence{
			Kind:         permissionEvidenceKnownActiveBoundary,
			BoundaryPath: boundary,
			TriggerPath:  failedPath,
			IssueType:    IssueRemoteWriteDenied,
		}, nil
	}

	if ph.remoteRootItemID == "" {
		return permissionEvidence{}, nil
	}

	remoteDriveID := ph.driveID
	parentFolder := remoteParentPath(failedPath)
	parentItemID := resolveBoundaryRemoteItemID(bl, parentFolder, remoteDriveID, ph.remoteRootItemID)
	if parentItemID == "" {
		parentFolder = ""
		parentItemID = ph.remoteRootItemID
	}

	perms, err := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, parentItemID)
	if err != nil {
		return ph.handlePermissionCheckError(err, failedPath, parentFolder, actionType), nil
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

		return permissionEvidence{}, nil
	case graph.PermissionWriteAccessInconclusive:
		ph.logger.Debug("handle403: permission evidence inconclusive, not suppressing",
			slog.String("path", failedPath),
		)

		return permissionEvidence{}, nil
	case graph.PermissionWriteAccessReadOnly:
	}

	boundary = ph.walkPermissionBoundary(ctx, bl, parentFolder, ph.remoteRootItemID, remoteDriveID)

	return ph.remoteBoundaryEvidence(
		boundary,
		"folder is read-only (no write access)",
		http.StatusForbidden,
		failedPath,
		actionType,
	), nil
}

// handlePermissionCheckError handles errors from ListItemPermissions during
// remote write-denial probing. If the target folder is not found, treat the
// boundary as unreadable for writes; otherwise fall back to generic failure
// handling by returning an unmatched decision.
func (ph *permissionHandler) handlePermissionCheckError(
	err error,
	failedPath string,
	parentFolder string,
	actionType actionType,
) permissionEvidence {
	if errors.Is(err, graph.ErrNotFound) {
		ph.logger.Debug("handle403: folder not found, recording as permission denied",
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

	ph.logger.Debug("handle403: permission check failed, not suppressing",
		slog.String("path", failedPath),
		slog.Any("error", err),
	)

	return permissionEvidence{}
}

func (ph *permissionHandler) activeRemoteBoundary(ctx context.Context, failedPath string) (string, bool, error) {
	boundaries, err := ph.ActiveRemoteBlockedBoundaries(ctx)
	if err != nil {
		return "", false, err
	}
	for _, boundary := range boundaries {
		if remoteBoundaryContainsPath(failedPath, boundary) {
			return boundary, true, nil
		}
	}

	return "", false, nil
}

func (ph *permissionHandler) remoteBoundaryEvidence(
	boundary string,
	errMsg string,
	httpStatus int,
	failedPath string,
	actionType actionType,
) permissionEvidence {
	_ = actionType

	return permissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: boundary,
		TriggerPath:  failedPath,
		IssueType:    IssueRemoteWriteDenied,
		LastError:    errMsg,
		HTTPStatus:   httpStatus,
	}
}

func (ph *permissionHandler) walkPermissionBoundary(
	ctx context.Context,
	bl *Baseline,
	startFolder string,
	remoteRootItemID string,
	remoteDriveID driveid.ID,
) string {
	boundary := startFolder

	for {
		parent, ok := remoteBoundaryParent(boundary)
		if !ok {
			break
		}

		parentID := resolveBoundaryRemoteItemID(bl, parent, remoteDriveID, remoteRootItemID)
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

func resolveBoundaryRemoteItemID(
	bl *Baseline,
	boundaryPath string,
	driveID driveid.ID,
	remoteRootItemID string,
) string {
	if remoteRootItemID != "" && boundaryPath == "" {
		return remoteRootItemID
	}

	return resolveRemoteItemID(bl, boundaryPath, driveID)
}

func remoteParentPath(path string) string {
	parent := filepath.Dir(path)
	if parent == "." || parent == "/" {
		return ""
	}

	return parent
}

func remoteBoundaryContainsPath(path string, boundary string) bool {
	if boundary == "" {
		return true
	}

	return path == boundary || strings.HasPrefix(path, boundary+"/")
}

func remoteBoundaryParent(boundary string) (string, bool) {
	if boundary == "" {
		return "", false
	}

	parent := filepath.Dir(boundary)
	if parent == "." || parent == "/" {
		return "", true
	}

	return parent, true
}
