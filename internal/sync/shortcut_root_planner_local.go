package sync

import (
	"errors"
	"os"
	"path"
	"syscall"

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

type shortcutRootLocalObservationKind string

const (
	shortcutRootLocalObservationBlocked             shortcutRootLocalObservationKind = "blocked"
	shortcutRootLocalObservationUnavailable         shortcutRootLocalObservationKind = "unavailable"
	shortcutRootLocalObservationReady               shortcutRootLocalObservationKind = "ready"
	shortcutRootLocalObservationRetiringPathMissing shortcutRootLocalObservationKind = "retiring_path_missing"
)

type shortcutRootLocalObservation struct {
	Kind     shortcutRootLocalObservationKind
	Detail   string
	Identity *synctree.FileIdentity
}

type shortcutRootLocalObservationPlan struct {
	Next    ShortcutRootRecord
	Keep    bool
	Changed bool
}

func shortcutRootLocalObservationForPathError(err error) shortcutRootLocalObservation {
	kind := shortcutRootLocalObservationUnavailable
	if errors.Is(err, synctree.ErrUnsafePath) ||
		errors.Is(err, syscall.ENOTDIR) {
		kind = shortcutRootLocalObservationBlocked
	}
	return shortcutRootLocalObservation{
		Kind:   kind,
		Detail: err.Error(),
	}
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootLocalObservation(
	record ShortcutRootRecord,
	observation shortcutRootLocalObservation,
) shortcutRootLocalObservationPlan {
	record = normalizeShortcutRootRecord(record)
	switch observation.Kind {
	case shortcutRootLocalObservationBlocked:
		next := planShortcutRootBlocked(record, observation.Detail)
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, next),
		}
	case shortcutRootLocalObservationUnavailable:
		next := planShortcutRootUnavailable(record, observation.Detail)
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, next),
		}
	case shortcutRootLocalObservationReady:
		if observation.Identity == nil {
			next := planShortcutRootUnavailable(record, "shortcut alias local root identity is unavailable")
			return shortcutRootLocalObservationPlan{
				Next:    next,
				Keep:    true,
				Changed: !shortcutRootRecordsEqual(record, next),
			}
		}
		next := planShortcutRootLocalReady(record, *observation.Identity)
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, next),
		}
	case shortcutRootLocalObservationRetiringPathMissing:
		next, keep := planRetiringShortcutRootMissing(record)
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    keep,
			Changed: true,
		}
	default:
		return shortcutRootLocalObservationPlan{
			Next: record,
			Keep: true,
		}
	}
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootPathError(record ShortcutRootRecord, err error) ShortcutRootRecord {
	if errors.Is(err, synctree.ErrUnsafePath) ||
		errors.Is(err, syscall.ENOTDIR) {
		return planShortcutRootBlocked(record, err.Error())
	}
	if errors.Is(err, os.ErrNotExist) {
		return planShortcutRootUnavailable(record, err.Error())
	}
	return planShortcutRootUnavailable(record, err.Error())
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootBlocked(record ShortcutRootRecord, detail string) ShortcutRootRecord {
	return plannedShortcutRootTransition(record,
		shortcutRootEventLocalPathBlocked,
		ShortcutRootStateBlockedPath,
		detail,
	)
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootUnavailable(record ShortcutRootRecord, detail string) ShortcutRootRecord {
	return plannedShortcutRootTransition(record,
		shortcutRootEventLocalPathBlocked,
		ShortcutRootStateLocalRootUnavailable,
		detail,
	)
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootAliasMutationFailure(record ShortcutRootRecord, err error) ShortcutRootRecord {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventAliasMutationFailed,
		ShortcutRootStateAliasMutationBlocked,
		detail,
	)
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutAliasRenameSuccess(record ShortcutRootRecord, mutation shortcutAliasMutation) ShortcutRootRecord {
	next := plannedShortcutRootTransition(record,
		shortcutRootEventAliasMutationSucceeded,
		ShortcutRootStateActive,
		"",
	)
	next.RelativeLocalPath = mutation.RelativeLocalPath
	next.LocalAlias = mutation.LocalAlias
	next.ProtectedPaths = protectedPathsForShortcutRoot(mutation.RelativeLocalPath, next.ProtectedPaths)
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutAliasDeleteSuccess(record ShortcutRootRecord) ShortcutRootRecord {
	return plannedShortcutRootTransition(record,
		shortcutRootEventAliasMutationSucceeded,
		ShortcutRootStateRemovedFinalDrain,
		"",
	)
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootLocalReady(record ShortcutRootRecord, identity synctree.FileIdentity) ShortcutRootRecord {
	next := record
	next.LocalRootIdentity = &identity
	if next.State == ShortcutRootStateBlockedPath ||
		next.State == ShortcutRootStateRenameAmbiguous ||
		next.State == ShortcutRootStateAliasMutationBlocked {
		next = plannedShortcutRootTransition(next,
			shortcutRootEventLocalRootReady,
			ShortcutRootStateActive,
			"",
		)
	}
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootMaterialized(record ShortcutRootRecord, identity synctree.FileIdentity) ShortcutRootRecord {
	next := record
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, next.ProtectedPaths)
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planRetiringShortcutRootMissing(record ShortcutRootRecord) (ShortcutRootRecord, bool) {
	if record.State == ShortcutRootStateSamePathReplacementWaiting && record.Waiting != nil {
		return shortcutRootRecordFromReplacement(record.NamespaceID, *record.Waiting), true
	}
	return ShortcutRootRecord{}, false
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
	next := planShortcutRootAliasMutationFailure(record, err)
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
