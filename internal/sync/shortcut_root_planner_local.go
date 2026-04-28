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
	shortcutRootLocalNoop            shortcutRootLocalAction = "noop"
	shortcutRootLocalKeepRecord      shortcutRootLocalAction = "keepRecord"
	shortcutRootLocalDropRecord      shortcutRootLocalAction = "dropRecord"
	shortcutRootLocalMaterializeRoot shortcutRootLocalAction = "materializeRoot"
	shortcutRootLocalMutateAlias     shortcutRootLocalAction = "mutateAlias"
	shortcutRootLocalMoveProjection  shortcutRootLocalAction = "moveProjection"

	shortcutRootMissingAliasNoop            shortcutRootLocalAction = shortcutRootLocalNoop
	shortcutRootMissingAliasDelete          shortcutRootLocalAction = shortcutRootLocalDropRecord
	shortcutRootMissingAliasRename          shortcutRootLocalAction = shortcutRootLocalMutateAlias
	shortcutRootMissingAliasMoveProjection  shortcutRootLocalAction = shortcutRootLocalMoveProjection
	shortcutRootMissingAliasRenameAmbiguous shortcutRootLocalAction = shortcutRootLocalKeepRecord
)

type shortcutRootLocalObservation struct {
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

type shortcutRootLocalPlan struct {
	Action           shortcutRootLocalAction
	Mutation         shortcutAliasMutation
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

type shortcutRootMaterializeResult struct {
	Identity    *synctree.FileIdentity
	CreateErr   error
	IdentityErr error
}

type shortcutRootProjectionMoveResult struct {
	Identity    *synctree.FileIdentity
	MoveErr     error
	IdentityErr error
}

type shortcutRootAliasMutationResult struct {
	MutationErr error
	Identity    *synctree.FileIdentity
	IdentityErr error
}

func planShortcutRootLocalObservation(
	record *ShortcutRootRecord,
	observation *shortcutRootLocalObservation,
) shortcutRootLocalPlan {
	if record == nil {
		return shortcutRootLocalPlan{}
	}
	normalized := normalizeShortcutRootRecord(record)
	if observation == nil {
		return shortcutRootLocalPlan{Action: shortcutRootLocalKeepRecord, Next: normalized, Keep: true}
	}
	if !observation.RelativePathOK {
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    planShortcutRootBlocked(&normalized, "shortcut alias path escapes parent sync root"),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.SymlinkErr != nil {
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.SymlinkErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathErr != nil {
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.PathErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathState.Exists {
		if !observation.PathState.IsDir {
			return shortcutRootLocalPlan{
				Action:  shortcutRootLocalKeepRecord,
				Next:    planShortcutRootBlocked(&normalized, "shortcut alias path is not a directory"),
				Keep:    true,
				Changed: true,
			}
		}
		if observation.IdentityErr != nil {
			return shortcutRootLocalPlan{
				Action:  shortcutRootLocalKeepRecord,
				Next:    planShortcutRootUnavailable(&normalized, observation.IdentityErr.Error()),
				Keep:    true,
				Changed: true,
			}
		}
		if observation.Identity != nil {
			next := planShortcutRootLocalReady(&normalized, *observation.Identity)
			return shortcutRootLocalPlan{
				Action:  shortcutRootLocalKeepRecord,
				Next:    next,
				Keep:    true,
				Changed: !shortcutRootRecordsEqual(&normalized, &next),
			}
		}
	}
	if normalized.LocalRootIdentity == nil {
		return shortcutRootLocalPlan{
			Action: shortcutRootLocalMaterializeRoot,
			Next:   normalized,
			Keep:   true,
		}
	}
	return shortcutRootLocalPlan{
		Action: shortcutRootLocalNoop,
		Next:   normalized,
		Keep:   true,
	}
}

func planRetiringShortcutRootLocalObservation(
	record *ShortcutRootRecord,
	observation *shortcutRootLocalObservation,
) shortcutRootLocalPlan {
	if record == nil {
		return shortcutRootLocalPlan{}
	}
	normalized := normalizeShortcutRootRecord(record)
	if observation == nil {
		return shortcutRootLocalPlan{Action: shortcutRootLocalKeepRecord, Next: normalized, Keep: true}
	}
	if !observation.RelativePathOK {
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    planShortcutRootBlocked(&normalized, "shortcut alias path escapes parent sync root"),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.SymlinkErr != nil {
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.SymlinkErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathErr != nil {
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    planShortcutRootPathError(&normalized, observation.PathErr),
			Keep:    true,
			Changed: true,
		}
	}
	if observation.PathState.Exists {
		if !observation.PathState.IsDir {
			return shortcutRootLocalPlan{
				Action:  shortcutRootLocalKeepRecord,
				Next:    planShortcutRootCleanupBlocked(&normalized, errors.New("shortcut alias path is not a directory")),
				Keep:    true,
				Changed: true,
			}
		}
		return shortcutRootLocalPlan{
			Action: shortcutRootLocalKeepRecord,
			Next:   normalized,
			Keep:   true,
		}
	}
	next, keep := planRetiringShortcutRootMissing(&normalized)
	action := shortcutRootLocalDropRecord
	if keep {
		action = shortcutRootLocalKeepRecord
	}
	return shortcutRootLocalPlan{
		Action:  action,
		Next:    next,
		Keep:    keep,
		Changed: true,
	}
}

func planMissingMaterializedShortcutRootObservation(
	record *ShortcutRootRecord,
	observation *shortcutRootLocalObservation,
) shortcutRootLocalPlan {
	if record == nil {
		return shortcutRootLocalPlan{}
	}
	if observation == nil {
		return shortcutRootLocalPlan{Action: shortcutRootLocalKeepRecord, Next: normalizeShortcutRootRecord(record), Keep: true}
	}
	if observation.CandidateErr != nil {
		normalized := normalizeShortcutRootRecord(record)
		next := planShortcutRootUnavailable(record, observation.CandidateErr.Error())
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
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

func planShortcutRootMaterializeResult(
	record *ShortcutRootRecord,
	result shortcutRootMaterializeResult,
) shortcutRootLocalObservationPlan {
	if record == nil {
		return shortcutRootLocalObservationPlan{}
	}
	normalized := normalizeShortcutRootRecord(record)
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
		next := planShortcutRootUnavailable(&normalized, "shortcut alias local root identity is unavailable")
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

func planShortcutProjectionMoveResult(
	record *ShortcutRootRecord,
	result shortcutRootProjectionMoveResult,
) shortcutRootLocalObservationPlan {
	if record == nil {
		return shortcutRootLocalObservationPlan{}
	}
	normalized := normalizeShortcutRootRecord(record)
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
		next := planShortcutRootUnavailable(&normalized, "shortcut alias local root identity is unavailable")
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

func planShortcutAliasRenameSuccess(record *ShortcutRootRecord, mutation shortcutAliasMutation) ShortcutRootRecord {
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

func planShortcutAliasDeleteSuccess(record *ShortcutRootRecord) ShortcutRootRecord {
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
) shortcutRootLocalPlan {
	if record == nil {
		return shortcutRootLocalPlan{}
	}
	normalized := normalizeShortcutRootRecord(record)
	if previousPath, ok := previousProtectedProjectionCandidate(&normalized, candidates); ok {
		return shortcutRootLocalPlan{
			Action:           shortcutRootMissingAliasMoveProjection,
			FromRelativePath: previousPath,
			ToRelativePath:   relativePath,
			Keep:             true,
		}
	}
	switch len(candidates) {
	case 0:
		return shortcutRootLocalPlan{
			Action: shortcutRootMissingAliasDelete,
			Mutation: shortcutAliasMutation{
				Kind:          shortcutAliasMutationDelete,
				BindingItemID: normalized.BindingItemID,
			},
		}
	case 1:
		alias := path.Base(candidates[0])
		return shortcutRootLocalPlan{
			Action:        shortcutRootMissingAliasRename,
			CandidatePath: candidates[0],
			Mutation: shortcutAliasMutation{
				Kind:              shortcutAliasMutationRename,
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
			"multiple same-parent shortcut alias rename candidates",
		)
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidates...)
		return shortcutRootLocalPlan{
			Action:  shortcutRootMissingAliasRenameAmbiguous,
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

func planMissingAliasDeleteResult(
	record *ShortcutRootRecord,
	result shortcutRootAliasMutationResult,
) shortcutRootLocalPlan {
	if record == nil {
		return shortcutRootLocalPlan{}
	}
	if result.MutationErr != nil {
		next := planMissingAliasMutationFailure(record, "", result.MutationErr)
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	return shortcutRootLocalPlan{
		Action:  shortcutRootLocalDropRecord,
		Keep:    false,
		Changed: true,
	}
}

func planMissingAliasRenameResult(
	record *ShortcutRootRecord,
	candidatePath string,
	result shortcutRootAliasMutationResult,
) shortcutRootLocalPlan {
	if record == nil {
		return shortcutRootLocalPlan{}
	}
	if result.MutationErr != nil {
		next := planMissingAliasMutationFailure(record, candidatePath, result.MutationErr)
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	if result.IdentityErr != nil {
		next := planShortcutRootUnavailable(record, result.IdentityErr.Error())
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	if result.Identity == nil {
		next := planShortcutRootUnavailable(record, "shortcut alias local root identity is unavailable")
		return shortcutRootLocalPlan{
			Action:  shortcutRootLocalKeepRecord,
			Next:    next,
			Keep:    true,
			Changed: !shortcutRootRecordsEqual(record, &next),
		}
	}
	next := planMissingAliasRenameSuccess(record, candidatePath, *result.Identity)
	return shortcutRootLocalPlan{
		Action:  shortcutRootLocalKeepRecord,
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
