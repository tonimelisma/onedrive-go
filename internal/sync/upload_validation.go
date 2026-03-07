package sync

import (
	"path"
	"strings"
)

// Issue type constants for upload validation failures.
const (
	IssueInvalidFilename = "invalid_filename"
	IssuePathTooLong     = "path_too_long"
	IssueFileTooLarge    = "file_too_large"
)

// Upload validation constants.
const (
	// maxOneDrivePathLength is the maximum total path length OneDrive allows.
	maxOneDrivePathLength = 400
	// maxOneDriveFileSize is the maximum file size OneDrive allows (250 GB).
	maxOneDriveFileSize = 250 * 1024 * 1024 * 1024 // 250 GB
)

// ValidationFailure records a pre-upload validation failure.
type ValidationFailure struct {
	Index     int // index in the original Actions slice
	Path      string
	IssueType string // IssueInvalidFilename, IssuePathTooLong, IssueFileTooLarge
	Error     string
}

// validateUploadActions checks upload actions for issues that would always
// fail at the API level. Returns valid actions (indices to keep) and any
// validation failures. Non-upload actions always pass.
// When multiple issues exist for the same file, they are combined into a
// single ValidationFailure using the first (most severe) IssueType and all
// error messages joined with "; ".
func validateUploadActions(actions []Action) (keep []int, failures []ValidationFailure) {
	for i := range actions {
		if actions[i].Type != ActionUpload {
			keep = append(keep, i)
			continue
		}

		fails := validateSingleUpload(&actions[i])
		if len(fails) > 0 {
			// Combine multiple failures: use first issue type, join all errors.
			var errMsgs []string
			for j := range fails {
				errMsgs = append(errMsgs, fails[j].Error)
			}

			failures = append(failures, ValidationFailure{
				Index:     i,
				Path:      actions[i].Path,
				IssueType: fails[0].IssueType,
				Error:     strings.Join(errMsgs, "; "),
			})
		} else {
			keep = append(keep, i)
		}
	}

	return keep, failures
}

// validateSingleUpload checks a single upload action for all validation issues.
// Returns all failures found (empty slice if valid). Checks are ordered by
// severity: invalid filename > path too long > file too large.
// isValidOneDriveName (scanner.go) covers reserved names, invalid chars, etc.
func validateSingleUpload(a *Action) []ValidationFailure {
	var fails []ValidationFailure
	name := path.Base(a.Path)

	if !isValidOneDriveName(name) {
		fails = append(fails, ValidationFailure{
			Path:      a.Path,
			IssueType: IssueInvalidFilename,
			Error:     "file name is not valid for OneDrive: " + name,
		})
	}

	if len(a.Path) > maxOneDrivePathLength {
		fails = append(fails, ValidationFailure{
			Path:      a.Path,
			IssueType: IssuePathTooLong,
			Error:     "path exceeds OneDrive maximum length of 400 characters",
		})
	}

	// Check file size from the PathView local state.
	if a.View != nil && a.View.Local != nil && a.View.Local.Size > maxOneDriveFileSize {
		fails = append(fails, ValidationFailure{
			Path:      a.Path,
			IssueType: IssueFileTooLarge,
			Error:     "file exceeds OneDrive maximum size of 250 GB",
		})
	}

	return fails
}

// removeActionsByIndex rebuilds an ActionPlan keeping only the indices in keep.
// Dependency indices are re-mapped to the new positions.
func removeActionsByIndex(plan *ActionPlan, keep []int) *ActionPlan {
	if len(keep) == len(plan.Actions) {
		return plan
	}

	// Build old→new index mapping.
	oldToNew := make(map[int]int, len(keep))
	for newIdx, oldIdx := range keep {
		oldToNew[oldIdx] = newIdx
	}

	newActions := make([]Action, len(keep))
	newDeps := make([][]int, len(keep))

	for newIdx, oldIdx := range keep {
		newActions[newIdx] = plan.Actions[oldIdx]

		// Re-map dependency indices, dropping removed deps.
		var remapped []int
		for _, depOld := range plan.Deps[oldIdx] {
			if depNew, ok := oldToNew[depOld]; ok {
				remapped = append(remapped, depNew)
			}
		}

		newDeps[newIdx] = remapped
	}

	return &ActionPlan{
		Actions: newActions,
		Deps:    newDeps,
	}
}
