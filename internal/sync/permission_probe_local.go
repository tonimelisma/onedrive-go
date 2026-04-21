package sync

import (
	"context"
	"log/slog"
	"path/filepath"
)

// handleLocalPermission probes the local filesystem after os.ErrPermission and
// returns a file-level retry row or a boundary-scoped local permission block.
func (ph *PermissionHandler) handleLocalPermission(
	_ context.Context,
	r *ActionCompletion,
) PermissionEvidence {
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

func (ph *PermissionHandler) localFilePermissionEvidence(
	path string,
	actionType ActionType,
	issueType string,
	errMsg string,
) PermissionEvidence {
	_ = actionType

	return PermissionEvidence{
		Kind:        permissionEvidenceFileDenied,
		TriggerPath: path,
		IssueType:   issueType,
		LastError:   errMsg,
	}
}

func (ph *PermissionHandler) localDirectoryPermissionEvidence(
	boundaryPath string,
	triggerPath string,
	actionType ActionType,
	issueType string,
) PermissionEvidence {
	_ = actionType

	return PermissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: boundaryPath,
		TriggerPath:  triggerPath,
		IssueType:    issueType,
		LastError:    "directory not accessible (check filesystem permissions)",
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

func localPermissionIssueType(r *ActionCompletion) string {
	if r == nil {
		return IssueLocalReadDenied
	}
	if r.FailureCapability == PermissionCapabilityLocalWrite {
		return IssueLocalWriteDenied
	}
	return IssueLocalReadDenied
}
