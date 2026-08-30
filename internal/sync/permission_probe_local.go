package sync

import (
	"context"
	"log/slog"
	"path/filepath"
)

// handleLocalPermission probes the local filesystem after os.ErrPermission and
// returns a file-level retry row or a boundary-scoped local permission block.
func (ph *permissionHandler) handleLocalPermission(
	_ context.Context,
	r *actionCompletion,
) permissionEvidence {
	issueType := localPermissionIssueType(r)

	if !isDirAccessible(ph.syncTree, ".") {
		ph.logger.Warn("sync root directory is inaccessible",
			slog.String("path", ph.syncTree.Path()),
			slog.String("error", r.ErrMsg),
		)

		return ph.localFilePermissionEvidence(
			r.Path,
			r.ActionType,
			issueType,
			"sync root directory not accessible (check filesystem permissions)",
		)
	}

	absPath, absErr := ph.syncTree.Abs(r.Path)
	if absErr != nil {
		ph.logger.Warn("handleLocalPermission: failed to resolve sync-tree path",
			slog.String("path", r.Path),
			slog.String("error", absErr.Error()),
		)

		return ph.localFilePermissionEvidence(
			r.Path,
			r.ActionType,
			issueType,
			"file not accessible (check filesystem permissions)",
		)
	}
	parentDir := filepath.Dir(absPath)

	if isDirAccessible(ph.syncTree, parentDir) {
		return ph.localFilePermissionEvidence(
			r.Path,
			r.ActionType,
			issueType,
			"file not accessible (check filesystem permissions)",
		)
	}

	boundary := ph.deepestDeniedBoundary(parentDir)
	relBoundary, relErr := ph.syncTree.Rel(boundary)
	if relErr != nil {
		ph.logger.Warn("handleLocalPermission: failed to relativize boundary path",
			slog.String("boundary", boundary),
			slog.String("error", relErr.Error()),
		)

		return ph.localFilePermissionEvidence(
			r.Path,
			r.ActionType,
			issueType,
			"file not accessible (check filesystem permissions)",
		)
	}

	return ph.localDirectoryPermissionEvidence(
		relBoundary,
		r.Path,
		r.ActionType,
		issueType,
	)
}

func (ph *permissionHandler) localFilePermissionEvidence(
	path string,
	actionType actionType,
	issueType string,
	errMsg string,
) permissionEvidence {
	_ = actionType

	return permissionEvidence{
		Kind:        permissionEvidenceFileDenied,
		TriggerPath: path,
		IssueType:   issueType,
		LastError:   errMsg,
	}
}

func (ph *permissionHandler) localDirectoryPermissionEvidence(
	boundaryPath string,
	triggerPath string,
	actionType actionType,
	issueType string,
) permissionEvidence {
	_ = actionType

	return permissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: boundaryPath,
		TriggerPath:  triggerPath,
		IssueType:    issueType,
		LastError:    "directory not accessible (check filesystem permissions)",
	}
}

func (ph *permissionHandler) deepestDeniedBoundary(parentDir string) string {
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

func localPermissionIssueType(r *actionCompletion) string {
	if r == nil {
		return issueLocalReadDenied
	}
	if r.FailureCapability == permissionCapabilityLocalWrite {
		return issueLocalWriteDenied
	}
	return issueLocalReadDenied
}
