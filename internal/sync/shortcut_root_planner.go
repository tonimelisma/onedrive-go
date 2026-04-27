package sync

import (
	"path"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type shortcutRootMissingAliasAction string

const (
	shortcutRootMissingAliasNoop            shortcutRootMissingAliasAction = ""
	shortcutRootMissingAliasDelete          shortcutRootMissingAliasAction = "delete_alias"
	shortcutRootMissingAliasRename          shortcutRootMissingAliasAction = "rename_alias"
	shortcutRootMissingAliasMoveProjection  shortcutRootMissingAliasAction = "move_projection"
	shortcutRootMissingAliasRenameAmbiguous shortcutRootMissingAliasAction = "rename_ambiguous"
)

type shortcutRootMissingAliasPlan struct {
	Action           shortcutRootMissingAliasAction
	Mutation         shortcutAliasMutation
	FromRelativePath string
	ToRelativePath   string
	CandidatePath    string
	Next             ShortcutRootRecord
	Keep             bool
	Changed          bool
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planMissingMaterializedShortcutRoot(
	record ShortcutRootRecord,
	relativePath string,
	candidates []string,
) shortcutRootMissingAliasPlan {
	record = normalizeShortcutRootRecord(record)
	if previousPath, ok := previousProtectedProjectionCandidate(&record, candidates); ok {
		return shortcutRootMissingAliasPlan{
			Action:           shortcutRootMissingAliasMoveProjection,
			FromRelativePath: previousPath,
			ToRelativePath:   relativePath,
			Keep:             true,
		}
	}
	switch len(candidates) {
	case 0:
		return shortcutRootMissingAliasPlan{
			Action: shortcutRootMissingAliasDelete,
			Mutation: shortcutAliasMutation{
				Kind:          shortcutAliasMutationDelete,
				BindingItemID: record.BindingItemID,
			},
		}
	case 1:
		alias := path.Base(candidates[0])
		return shortcutRootMissingAliasPlan{
			Action:        shortcutRootMissingAliasRename,
			CandidatePath: candidates[0],
			Mutation: shortcutAliasMutation{
				Kind:              shortcutAliasMutationRename,
				BindingItemID:     record.BindingItemID,
				RelativeLocalPath: candidates[0],
				LocalAlias:        alias,
			},
			Keep: true,
		}
	default:
		next := plannedShortcutRootTransition(record,
			shortcutRootEventAliasRenameAmbiguous,
			ShortcutRootStateRenameAmbiguous,
			"multiple same-parent shortcut alias rename candidates",
		)
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidates...)
		return shortcutRootMissingAliasPlan{
			Action:  shortcutRootMissingAliasRenameAmbiguous,
			Next:    next,
			Keep:    true,
			Changed: true,
		}
	}
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planMissingAliasMutationFailure(
	record ShortcutRootRecord,
	candidatePath string,
	err error,
) ShortcutRootRecord {
	next := aliasMutationBlockedShortcutRoot(record, err)
	if candidatePath != "" {
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidatePath)
	}
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planMissingAliasRenameSuccess(
	record ShortcutRootRecord,
	candidatePath string,
	identity synctree.FileIdentity,
) ShortcutRootRecord {
	next := record
	next.RelativeLocalPath = candidatePath
	next.LocalAlias = path.Base(candidatePath)
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, append(record.ProtectedPaths, record.RelativeLocalPath))
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutProjectionMoveSuccess(
	record ShortcutRootRecord,
	identity synctree.FileIdentity,
) ShortcutRootRecord {
	next := record
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, nil)
	return next
}

type shortcutRootReleaseCleanupPlan struct {
	Records []ShortcutRootRecord
	Changed bool
	Err     error
}

// planShortcutRootReleaseCleanup is the deterministic core for the parent
// release-cleanup phase. The engine shell owns filesystem removal; this helper
// only translates that outcome into the next durable shortcut-root records.
func planShortcutRootReleaseCleanup(
	record *ShortcutRootRecord,
	cleanupErr error,
) shortcutRootReleaseCleanupPlan {
	if record == nil {
		return shortcutRootReleaseCleanupPlan{}
	}
	normalized := normalizeShortcutRootRecord(*record)
	if !shortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{normalized},
		}
	}
	if cleanupErr != nil {
		next := shortcutRootCleanupBlocked(normalized, cleanupErr)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(normalized, next),
			Err:     cleanupErr,
		}
	}
	cleanupPending := shortcutRootChildCleanupPending(normalized)
	nextRecords := []ShortcutRootRecord{cleanupPending}
	if normalized.Waiting != nil {
		nextRecords = append(nextRecords, shortcutRootRecordFromReplacement(normalized.NamespaceID, *normalized.Waiting))
	}
	return shortcutRootReleaseCleanupPlan{
		Records: nextRecords,
		Changed: true,
	}
}
