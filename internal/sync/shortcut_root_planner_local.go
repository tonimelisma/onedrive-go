package sync

import (
	"errors"
	"os"
	"path"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// This file is the deterministic core for local shortcut alias lifecycle
// decisions. It must not perform filesystem, Graph, SQLite, logging, clock, or
// goroutine work; engine shell code feeds it observations and effect outcomes.
type shortcutRootLocalAction string

const (
	ShortcutRootLocalNoop            shortcutRootLocalAction = "noop"
	ShortcutRootLocalKeepRecord      shortcutRootLocalAction = "keepRecord"
	ShortcutRootLocalDropRecord      shortcutRootLocalAction = "dropRecord"
	ShortcutRootLocalMaterializeRoot shortcutRootLocalAction = "materializeRoot"
	ShortcutRootLocalMutateAlias     shortcutRootLocalAction = "mutateAlias"
	ShortcutRootLocalMoveProjection  shortcutRootLocalAction = "moveProjection"

	ShortcutRootMissingAliasNoop            shortcutRootLocalAction = ShortcutRootLocalNoop
	ShortcutRootMissingAliasDelete          shortcutRootLocalAction = ShortcutRootLocalDropRecord
	ShortcutRootMissingAliasRename          shortcutRootLocalAction = ShortcutRootLocalMutateAlias
	ShortcutRootMissingAliasMoveProjection  shortcutRootLocalAction = ShortcutRootLocalMoveProjection
	ShortcutRootMissingAliasRenameAmbiguous shortcutRootLocalAction = ShortcutRootLocalKeepRecord
)

type ShortcutRootLocalObservation struct {
	RelativePath   string
	RelativePathOK bool
	SymlinkErr     error
	PathState      synctree.PathState
	PathErr        error
	Identity       *synctree.FileIdentity
	IdentityErr    error
	Candidates     []string
	CandidateErr   error
}

type ShortcutRootLocalPlan struct {
	Action           shortcutRootLocalAction
	Mutation         ShortcutAliasMutation
	FromRelativePath string
	ToRelativePath   string
	CandidatePath    string
	Next             ShortcutRootRecord
	Keep             bool
	Changed          bool
}

type shortcutRootLocalObservationPlan struct {
	Next    ShortcutRootRecord
	Keep    bool
	Changed bool
}

type ShortcutRootMaterializeResult struct {
	Identity    *synctree.FileIdentity
	CreateErr   error
	IdentityErr error
}

type ShortcutRootProjectionMoveResult struct {
	Identity    *synctree.FileIdentity
	MoveErr     error
	IdentityErr error
}

type ShortcutRootAliasMutationResult struct {
	MutationErr error
	Identity    *synctree.FileIdentity
	IdentityErr error
}

func PlanShortcutRootLocalObservation(
	record *ShortcutRootRecord,
	observation *ShortcutRootLocalObservation,
) ShortcutRootLocalPlan {
	if record == nil {
		return ShortcutRootLocalPlan{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	if observation == nil {
		return ShortcutRootLocalPlan{Action: ShortcutRootLocalKeepRecord, Next: normalized, Keep: true}
	}
	if !observation.RelativePathOK {
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    planShortcutRootBlocked(&normalized, "the shared folder's local path leaves the sync folder"),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.SymlinkErr != nil {
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.SymlinkErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathErr != nil {
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.PathErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathState.Exists {
		if !observation.PathState.IsDir {
			return ShortcutRootLocalPlan{
				Action:  ShortcutRootLocalKeepRecord,
				Next:    planShortcutRootBlocked(&normalized, "the shared folder's local path is not a directory"),
				Keep:    true,
				Changed: true,
			}
		}
		if observation.IdentityErr != nil {
			return ShortcutRootLocalPlan{
				Action:  ShortcutRootLocalKeepRecord,
				Next:    planShortcutRootUnavailable(&normalized, observation.IdentityErr.Error()),
				Keep:    true,
				Changed: true,
			}
		}
		if observation.Identity != nil {
			next := planShortcutRootLocalReady(&normalized, *observation.Identity)
			return ShortcutRootLocalPlan{
				Action:  ShortcutRootLocalKeepRecord,
				Next:    next,
				Keep:    true,
				Changed: !shortcutRootRecordsEqual(&normalized, &next),
			}
		}
	}
	if normalized.LocalRootIdentity == nil {
		return ShortcutRootLocalPlan{
			Action: ShortcutRootLocalMaterializeRoot,
			Next:   normalized,
			Keep:   true,
		}
	}
	return ShortcutRootLocalPlan{
		Action: ShortcutRootLocalNoop,
		Next:   normalized,
		Keep:   true,
	}
}

func PlanRetiringShortcutRootLocalObservation(
	record *ShortcutRootRecord,
	observation *ShortcutRootLocalObservation,
) ShortcutRootLocalPlan {
	if record == nil {
		return ShortcutRootLocalPlan{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	if observation == nil {
		return ShortcutRootLocalPlan{Action: ShortcutRootLocalKeepRecord, Next: normalized, Keep: true}
	}
	if !observation.RelativePathOK {
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    planShortcutRootBlocked(&normalized, "the shared folder's local path leaves the sync folder"),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.SymlinkErr != nil {
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.SymlinkErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathErr != nil {
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.PathErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathState.Exists {
		if !observation.PathState.IsDir {
			return ShortcutRootLocalPlan{
				Action:  ShortcutRootLocalKeepRecord,
				Next:    planShortcutRootCleanupBlocked(&normalized, errors.New("the shared folder's local path is not a directory")),
				Keep:    true,
				Changed: true,
			}
		}
		return ShortcutRootLocalPlan{
			Action: ShortcutRootLocalKeepRecord,
			Next:   normalized,
			Keep:   true,
		}
	}
	next, keep := planRetiringShortcutRootMissing(&normalized)
	action := ShortcutRootLocalDropRecord
	if keep {
		action = ShortcutRootLocalKeepRecord
	}
	return ShortcutRootLocalPlan{
		Action:  action,
		Next:    next,
		Keep:    keep,
		Changed: true,
	}
}

func PlanMissingMaterializedShortcutRootObservation(
	record *ShortcutRootRecord,
	observation *ShortcutRootLocalObservation,
) ShortcutRootLocalPlan {
	if record == nil {
		return ShortcutRootLocalPlan{}
	}
	if observation == nil {
		return ShortcutRootLocalPlan{Action: ShortcutRootLocalKeepRecord, Next: NormalizeShortcutRootRecord(record), Keep: true}
	}
	if observation.CandidateErr != nil {
		normalized := NormalizeShortcutRootRecord(record)
		next := planShortcutRootUnavailable(record, observation.CandidateErr.Error())
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	return planMissingMaterializedShortcutRoot(record, observation.RelativePath, observation.Candidates)
}

func planShortcutRootPathError(record *ShortcutRootRecord, err error) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	if errors.Is(err, synctree.ErrUnsafePath) ||
		errors.Is(err, syscall.ENOTDIR) {
		return planShortcutRootBlocked(record, err.Error())
	}
	if errors.Is(err, os.ErrNotExist) {
		return planShortcutRootUnavailable(record, err.Error())
	}
	return planShortcutRootUnavailable(record, err.Error())
}

func PlanShortcutRootMaterializeResult(
	record *ShortcutRootRecord,
	result ShortcutRootMaterializeResult,
) shortcutRootLocalObservationPlan {
	if record == nil {
		return shortcutRootLocalObservationPlan{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	if result.CreateErr != nil {
		next := planShortcutRootPathError(&normalized, result.CreateErr)
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	if result.IdentityErr != nil {
		next := planShortcutRootUnavailable(&normalized, result.IdentityErr.Error())
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	if result.Identity == nil {
		next := planShortcutRootUnavailable(&normalized, "the shared folder's local folder could not be identified")
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	next := planShortcutRootMaterialized(&normalized, *result.Identity)
	return shortcutRootLocalObservationPlan{
		Next:    next,
		Keep:    true,
		Changed: !shortcutRootRecordsEqual(&normalized, &next),
	}
}

func PlanShortcutProjectionMoveResult(
	record *ShortcutRootRecord,
	result ShortcutRootProjectionMoveResult,
) shortcutRootLocalObservationPlan {
	if record == nil {
		return shortcutRootLocalObservationPlan{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	if result.MoveErr != nil {
		next := planShortcutRootBlocked(&normalized, result.MoveErr.Error())
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	if result.IdentityErr != nil {
		next := planShortcutRootUnavailable(&normalized, result.IdentityErr.Error())
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	if result.Identity == nil {
		next := planShortcutRootUnavailable(&normalized, "the shared folder's local folder could not be identified")
		return shortcutRootLocalObservationPlan{
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	next := planShortcutProjectionMoveSuccess(&normalized, *result.Identity)
	return shortcutRootLocalObservationPlan{
		Next:    next,
		Keep:    true,
		Changed: !shortcutRootRecordsEqual(&normalized, &next),
	}
}

func planShortcutRootBlocked(record *ShortcutRootRecord, detail string) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventLocalPathBlocked,
		ShortcutRootStateBlockedPath,
		detail,
	)
}

func planShortcutRootUnavailable(record *ShortcutRootRecord, detail string) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventLocalPathBlocked,
		ShortcutRootStateLocalRootUnavailable,
		detail,
	)
}

func planShortcutRootAliasMutationFailure(record *ShortcutRootRecord, err error) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
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

func PlanShortcutAliasRenameSuccess(record *ShortcutRootRecord, mutation ShortcutAliasMutation) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
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

func PlanShortcutAliasDeleteSuccess(record *ShortcutRootRecord) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventAliasMutationSucceeded,
		ShortcutRootStateRemovedFinalDrain,
		"",
	)
}

func planShortcutRootLocalReady(record *ShortcutRootRecord, identity synctree.FileIdentity) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	next := *record
	next.LocalRootIdentity = &identity
	if next.State == ShortcutRootStateBlockedPath ||
		next.State == ShortcutRootStateRenameAmbiguous ||
		next.State == ShortcutRootStateAliasMutationBlocked {
		next = plannedShortcutRootTransition(&next,
			shortcutRootEventLocalRootReady,
			ShortcutRootStateActive,
			"",
		)
	}
	return next
}

func planShortcutRootMaterialized(record *ShortcutRootRecord, identity synctree.FileIdentity) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	next := *record
	next = plannedShortcutRootTransition(&next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, next.ProtectedPaths)
	return next
}

func planRetiringShortcutRootMissing(record *ShortcutRootRecord) (ShortcutRootRecord, bool) {
	if record == nil {
		return ShortcutRootRecord{}, false
	}
	if record.State == ShortcutRootStateSamePathReplacementWaiting && record.Waiting != nil {
		return shortcutRootRecordFromReplacement(record.NamespaceID, *record.Waiting), true
	}
	return ShortcutRootRecord{}, false
}

func planMissingMaterializedShortcutRoot(
	record *ShortcutRootRecord,
	relativePath string,
	candidates []string,
) ShortcutRootLocalPlan {
	if record == nil {
		return ShortcutRootLocalPlan{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	if previousPath, ok := previousProtectedProjectionCandidate(&normalized, candidates); ok {
		return ShortcutRootLocalPlan{
			Action:           ShortcutRootMissingAliasMoveProjection,
			FromRelativePath: previousPath,
			ToRelativePath:   relativePath,
			Keep:             true,
		}
	}
	switch len(candidates) {
	case 0:
		return ShortcutRootLocalPlan{
			Action: ShortcutRootMissingAliasDelete,
			Mutation: ShortcutAliasMutation{
				Kind:          ShortcutAliasMutationDelete,
				BindingItemID: normalized.BindingItemID,
			},
		}
	case 1:
		alias := path.Base(candidates[0])
		return ShortcutRootLocalPlan{
			Action:        ShortcutRootMissingAliasRename,
			CandidatePath: candidates[0],
			Mutation: ShortcutAliasMutation{
				Kind:              ShortcutAliasMutationRename,
				BindingItemID:     normalized.BindingItemID,
				RelativeLocalPath: candidates[0],
				LocalAlias:        alias,
			},
			Keep: true,
		}
	default:
		next := plannedShortcutRootTransition(&normalized,
			shortcutRootEventAliasRenameAmbiguous,
			ShortcutRootStateRenameAmbiguous,
			"more than one renamed folder could be this shared folder",
		)
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidates...)
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootMissingAliasRenameAmbiguous,
			Next:    next,
			Keep:    true,
			Changed: true,
		}
	}
}

func planMissingAliasMutationFailure(
	record *ShortcutRootRecord,
	candidatePath string,
	err error,
) ShortcutRootRecord {
	next := planShortcutRootAliasMutationFailure(record, err)
	if candidatePath != "" {
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidatePath)
	}
	return next
}

func PlanMissingAliasDeleteResult(
	record *ShortcutRootRecord,
	result ShortcutRootAliasMutationResult,
) ShortcutRootLocalPlan {
	if record == nil {
		return ShortcutRootLocalPlan{}
	}
	if result.MutationErr != nil {
		next := planMissingAliasMutationFailure(record, "", result.MutationErr)
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	return ShortcutRootLocalPlan{
		Action:  ShortcutRootLocalDropRecord,
		Keep:    false,
		Changed: true,
	}
}

func PlanMissingAliasRenameResult(
	record *ShortcutRootRecord,
	candidatePath string,
	result ShortcutRootAliasMutationResult,
) ShortcutRootLocalPlan {
	if record == nil {
		return ShortcutRootLocalPlan{}
	}
	if result.MutationErr != nil {
		next := planMissingAliasMutationFailure(record, candidatePath, result.MutationErr)
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	if result.IdentityErr != nil {
		next := planShortcutRootUnavailable(record, result.IdentityErr.Error())
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	if result.Identity == nil {
		next := planShortcutRootUnavailable(record, "the shared folder's local folder could not be identified")
		return ShortcutRootLocalPlan{
			Action:  ShortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	next := planMissingAliasRenameSuccess(record, candidatePath, *result.Identity)
	return ShortcutRootLocalPlan{
		Action:  ShortcutRootLocalKeepRecord,
		Next:    next,
		Keep:    true,
		Changed: !shortcutRootRecordsEqual(record, &next),
	}
}

func planMissingAliasRenameSuccess(
	record *ShortcutRootRecord,
	candidatePath string,
	identity synctree.FileIdentity,
) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	next := *record
	next.RelativeLocalPath = candidatePath
	next.LocalAlias = path.Base(candidatePath)
	next = plannedShortcutRootTransition(&next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, append(record.ProtectedPaths, record.RelativeLocalPath))
	return next
}

func planShortcutProjectionMoveSuccess(
	record *ShortcutRootRecord,
	identity synctree.FileIdentity,
) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	next := *record
	next = plannedShortcutRootTransition(&next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, nil)
	return next
}
