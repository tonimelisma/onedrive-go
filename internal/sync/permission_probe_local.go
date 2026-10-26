package sync

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// handleLocalPermission probes the local filesystem after os.ErrPermission or
// EROFS and returns a file-level retry row or a boundary-scoped local block.
func (ph *permissionHandler) handleLocalPermission(
	_ context.Context,
	r *actionCompletion,
) permissionEvidence {
	if driveops.ClassifyLocalError(r.Err) == driveops.LocalErrorReadOnly {
		return ph.readOnlyFilesystemEvidence(r)
	}

	issueType := localPermissionIssueType(r)

	if !isDirAccessible(ph.syncTree, ".") {
		if ph.rootInaccessibleReported.CompareAndSwap(false, true) {
			ph.logger.Warn("Cannot access the sync folder "+ph.syncTree.Path()+": permission denied. Check its permissions",
				slog.String("error", r.ErrMsg),
			)
		} else {
			ph.logger.Debug("sync root directory is inaccessible",
				slog.String("path", ph.syncTree.Path()),
				slog.String("error", r.ErrMsg),
			)
		}

		return ph.localFilePermissionEvidence(
			r.Path,
			r.ActionType,
			issueType,
			"sync root directory not accessible (check filesystem permissions)",
		)
	}

	ph.rootInaccessibleReported.Store(false)

	absPath, absErr := ph.syncTree.Abs(r.Path)
	if absErr != nil {
		ph.logger.Debug("handleLocalPermission: failed to resolve sync-tree path",
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
		ph.logger.Debug("handleLocalPermission: failed to relativize boundary path",
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

// readOnlyFilesystemEvidence turns an EROFS write failure into one boundary
// scope. Every write under a read-only mount fails the same way, so before
// this each file was its own "unexpected" failure. The boundary is the
// highest sync-folder directory still on the read-only filesystem: the inner
// mount point, or the sync root itself when the whole folder is read-only.
// The scope is revalidated by the normal timed write trial, so a remount
// read-write resumes syncing without user action.
func (ph *permissionHandler) readOnlyFilesystemEvidence(r *actionCompletion) permissionEvidence {
	fileEvidence := permissionEvidence{
		Kind:        permissionEvidenceFileDenied,
		TriggerPath: r.Path,
		IssueType:   issueLocalReadOnly,
		LastError:   "the local filesystem is read-only",
	}

	absPath, err := ph.syncTree.Abs(r.Path)
	if err != nil {
		return fileEvidence
	}

	readOnly := ph.readOnlyDir
	if readOnly == nil {
		readOnly = isOnReadOnlyFilesystem
	}

	root := ph.syncTree.Path()
	dir := filepath.Dir(absPath)
	if !readOnly(dir) {
		// The directory itself is writable, so the failing path is a mount
		// of its own (a read-only file bind-mounted in): keep it per file.
		return fileEvidence
	}

	boundary := dir
	for boundary != root {
		parent := filepath.Dir(boundary)
		if parent == boundary || !readOnly(parent) {
			break
		}
		boundary = parent
	}

	relBoundary, err := ph.syncTree.Rel(boundary)
	if err != nil {
		return fileEvidence
	}
	if relBoundary == "." {
		relBoundary = ""
	}

	return permissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: relBoundary,
		TriggerPath:  r.Path,
		IssueType:    issueLocalReadOnly,
		LastError:    "the local filesystem is read-only",
	}
}
