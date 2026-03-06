package sync

import (
	"path"
	"strings"
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
	IssueType string // "invalid_filename", "path_too_long", "file_too_large"
	Error     string
}

// validateUploadActions checks upload actions for issues that would always
// fail at the API level. Returns valid actions (indices to keep) and any
// validation failures. Non-upload actions always pass.
func validateUploadActions(actions []Action) (keep []int, failures []ValidationFailure) {
	for i := range actions {
		if actions[i].Type != ActionUpload {
			keep = append(keep, i)
			continue
		}

		if fail := validateSingleUpload(&actions[i]); fail != nil {
			fail.Index = i
			failures = append(failures, *fail)
		} else {
			keep = append(keep, i)
		}
	}

	return keep, failures
}

// validateSingleUpload checks a single upload action. Returns nil if valid.
func validateSingleUpload(a *Action) *ValidationFailure {
	name := path.Base(a.Path)

	if !isValidOneDriveName(name) {
		return &ValidationFailure{
			Path:      a.Path,
			IssueType: "invalid_filename",
			Error:     "file name is not valid for OneDrive: " + name,
		}
	}

	if len(a.Path) > maxOneDrivePathLength {
		return &ValidationFailure{
			Path:      a.Path,
			IssueType: "path_too_long",
			Error:     "path exceeds OneDrive maximum length of 400 characters",
		}
	}

	// Check file size from the PathView local state.
	if a.View != nil && a.View.Local != nil && a.View.Local.Size > maxOneDriveFileSize {
		return &ValidationFailure{
			Path:      a.Path,
			IssueType: "file_too_large",
			Error:     "file exceeds OneDrive maximum size of 250 GB",
		}
	}

	return nil
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

// isReservedOneDriveName checks Windows reserved device names (for the
// upload validation path). Delegates to the existing scanner validation.
// This function exists solely as documentation that we reuse isValidOneDriveName
// from scanner.go, which is in the same package.
func isReservedOneDriveName(name string) bool {
	return isReservedDeviceName(strings.ToLower(name))
}
