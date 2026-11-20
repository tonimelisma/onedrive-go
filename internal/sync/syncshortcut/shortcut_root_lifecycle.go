package syncshortcut

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	ShortcutRootCaseRenameAttempts  = 10
	ShortcutRootCaseRenameTempInfix = ".shortcut-alias-case-rename"
)

func ShortcutRootIdentityCandidatesFromLocalState(
	relativePath string,
	identity synctree.FileIdentity,
	localRows []syncdomain.LocalStateRow,
) []string {
	normalizedCurrent := filepath.ToSlash(relativePath)
	candidates := make([]string, 0)
	for i := range localRows {
		row := localRows[i]
		if row.ItemType != syncdomain.ItemTypeFolder {
			continue
		}
		if row.Path == normalizedCurrent {
			continue
		}
		if rowIdentity, known := row.FileIdentity(); !known || !synctree.SameIdentity(rowIdentity, identity) {
			continue
		}
		candidates = append(candidates, row.Path)
	}
	slices.Sort(candidates)
	return candidates
}

func UniqueSortedShortcutRootCandidates(candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(candidates))
	unique := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		normalized := NormalizedProtectedRootPath(candidate)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		unique = append(unique, normalized)
	}
	slices.Sort(unique)
	return unique
}

func previousProtectedProjectionCandidate(record *ShortcutRootRecord, candidates []string) (string, bool) {
	if record == nil || len(candidates) == 0 {
		return "", false
	}
	protected := make(map[string]struct{}, len(record.ProtectedPaths))
	for _, protectedPath := range record.ProtectedPaths {
		normalized := NormalizedProtectedRootPath(protectedPath)
		if normalized == "" || normalized == record.RelativeLocalPath {
			continue
		}
		protected[normalized] = struct{}{}
	}
	for _, candidate := range candidates {
		normalized := NormalizedProtectedRootPath(candidate)
		if _, ok := protected[normalized]; ok {
			return normalized, true
		}
	}
	return "", false
}

func ShortcutRootLocalPlanResult(plan *shortcutRootLocalObservationPlan) (ShortcutRootRecord, bool, bool, error) {
	if plan == nil {
		return ShortcutRootRecord{}, false, false, nil
	}
	return plan.Next, plan.Keep, plan.Changed, nil
}

func ShortcutRootLocalActionResult(plan *ShortcutRootLocalPlan) (ShortcutRootRecord, bool, bool, error) {
	if plan == nil {
		return ShortcutRootRecord{}, false, false, nil
	}
	return plan.Next, plan.Keep, plan.Changed, nil
}

func CleanShortcutRootRelativePath(relativeLocalPath string) (string, bool) {
	if relativeLocalPath == "" {
		return "", false
	}
	relativePath := filepath.Clean(filepath.FromSlash(relativeLocalPath))
	if filepath.IsAbs(relativePath) || relativePath == "." || relativePath == ".." {
		return "", false
	}
	if strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return relativePath, true
}

func shortcutRootStateAwaitsFinalDrainAck(state ShortcutRootState) bool {
	switch state {
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateSamePathReplacementWaiting:
		return true
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateDuplicateTarget:
		return false
	default:
		return false
	}
}

func ShortcutRootStateAwaitsReleaseCleanup(state ShortcutRootState) bool {
	switch state {
	case ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked:
		return true
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateSamePathReplacementWaiting,
		ShortcutRootStateDuplicateTarget:
		return false
	default:
		return false
	}
}

func appendUniqueProtectedRootPaths(paths []string, additions ...string) []string {
	merged := append([]string(nil), paths...)
	for _, addition := range additions {
		normalized := NormalizedProtectedRootPath(addition)
		if normalized == "" {
			continue
		}
		if slices.Contains(merged, normalized) {
			continue
		}
		merged = append(merged, normalized)
	}
	return merged
}
