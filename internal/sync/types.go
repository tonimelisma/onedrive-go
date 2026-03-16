// Package sync implements the event-driven sync engine for bidirectional
// OneDrive synchronization. Types are defined in the synctypes package;
// this file re-exports them for backward compatibility during the package
// split migration.
package sync

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Type aliases — all point to synctypes canonical definitions
// ---------------------------------------------------------------------------

type (
	ChangeSource     = synctypes.ChangeSource
	ChangeType       = synctypes.ChangeType
	ItemType         = synctypes.ItemType
	SyncMode         = synctypes.SyncMode
	ActionType       = synctypes.ActionType
	FolderCreateSide = synctypes.FolderCreateSide

	SkippedItem    = synctypes.SkippedItem
	ScanResult     = synctypes.ScanResult
	ChangeEvent    = synctypes.ChangeEvent
	BaselineEntry  = synctypes.BaselineEntry
	Baseline       = synctypes.Baseline
	DirLowerKey    = synctypes.DirLowerKey
	PathChanges    = synctypes.PathChanges
	RemoteState    = synctypes.RemoteState
	LocalState     = synctypes.LocalState
	PathView       = synctypes.PathView
	ConflictRecord = synctypes.ConflictRecord
	Shortcut       = synctypes.Shortcut
	VerifyResult   = synctypes.VerifyResult
	VerifyReport   = synctypes.VerifyReport

	Action     = synctypes.Action
	ActionPlan = synctypes.ActionPlan
	Outcome    = synctypes.Outcome

	TrackedAction = synctypes.TrackedAction
	WorkerResult  = synctypes.WorkerResult

	ScopeKey          = synctypes.ScopeKey
	ScopeKeyKind      = synctypes.ScopeKeyKind
	ScopeBlock        = synctypes.ScopeBlock
	ScopeUpdateResult = synctypes.ScopeUpdateResult

	SyncFailureParams = synctypes.SyncFailureParams
	SyncFailureRow    = synctypes.SyncFailureRow
	ActionableFailure = synctypes.ActionableFailure
	ObservedItem      = synctypes.ObservedItem
	RemoteStateRow    = synctypes.RemoteStateRow
	PendingRetryGroup = synctypes.PendingRetryGroup

	ObservationWriter   = synctypes.ObservationWriter
	OutcomeWriter       = synctypes.OutcomeWriter
	StateReader         = synctypes.StateReader
	SyncFailureRecorder = synctypes.SyncFailureRecorder
	StateAdmin          = synctypes.StateAdmin
	ScopeBlockStore     = synctypes.ScopeBlockStore

	DeltaFetcher       = synctypes.DeltaFetcher
	ItemClient         = synctypes.ItemClient
	DriveVerifier      = synctypes.DriveVerifier
	FolderDeltaFetcher = synctypes.FolderDeltaFetcher
	RecursiveLister    = synctypes.RecursiveLister
	PermissionChecker  = synctypes.PermissionChecker

	IssueMessage = synctypes.IssueMessage

	EngineConfig = synctypes.EngineConfig
	RunOpts      = synctypes.RunOpts
	WatchOpts    = synctypes.WatchOpts
	SafetyConfig = synctypes.SafetyConfig
	SyncReport   = synctypes.SyncReport
	DriveReport  = synctypes.DriveReport
)

// ---------------------------------------------------------------------------
// Enum constant aliases
// ---------------------------------------------------------------------------

const (
	SourceRemote = synctypes.SourceRemote
	SourceLocal  = synctypes.SourceLocal

	ChangeCreate   = synctypes.ChangeCreate
	ChangeModify   = synctypes.ChangeModify
	ChangeDelete   = synctypes.ChangeDelete
	ChangeMove     = synctypes.ChangeMove
	ChangeShortcut = synctypes.ChangeShortcut

	ItemTypeFile   = synctypes.ItemTypeFile
	ItemTypeFolder = synctypes.ItemTypeFolder
	ItemTypeRoot   = synctypes.ItemTypeRoot

	SyncBidirectional = synctypes.SyncBidirectional
	SyncDownloadOnly  = synctypes.SyncDownloadOnly
	SyncUploadOnly    = synctypes.SyncUploadOnly

	ActionDownload     = synctypes.ActionDownload
	ActionUpload       = synctypes.ActionUpload
	ActionLocalDelete  = synctypes.ActionLocalDelete
	ActionRemoteDelete = synctypes.ActionRemoteDelete
	ActionLocalMove    = synctypes.ActionLocalMove
	ActionRemoteMove   = synctypes.ActionRemoteMove
	ActionFolderCreate = synctypes.ActionFolderCreate
	ActionConflict     = synctypes.ActionConflict
	ActionUpdateSynced = synctypes.ActionUpdateSynced
	ActionCleanup      = synctypes.ActionCleanup

	CreateLocal  = synctypes.CreateLocal
	CreateRemote = synctypes.CreateRemote
)

// String constant aliases — used internally by store and observer code.
const (
	strRemote       = synctypes.StrRemote
	strLocal        = synctypes.StrLocal
	strFile         = synctypes.StrFile
	strFolder       = synctypes.StrFolder
	strRoot         = synctypes.StrRoot
	strDownload     = synctypes.StrDownload
	strUpload       = synctypes.StrUpload
	strDelete       = synctypes.StrDelete
	strActionable   = synctypes.StrActionable
	strTransient    = synctypes.StrTransient
	strLocalDelete  = synctypes.StrLocalDelete
	strRemoteDelete = synctypes.StrRemoteDelete
	strLocalMove    = synctypes.StrLocalMove
	strRemoteMove   = synctypes.StrRemoteMove
	strFolderCreate = synctypes.StrFolderCreate
	strConflict     = synctypes.StrConflict
	strUpdateSynced = synctypes.StrUpdateSynced
	strCleanup      = synctypes.StrCleanup
)

// Resolution/conflict/resolvedBy constant aliases.
const (
	ResolutionKeepLocal  = synctypes.ResolutionKeepLocal
	ResolutionKeepRemote = synctypes.ResolutionKeepRemote
	ResolutionKeepBoth   = synctypes.ResolutionKeepBoth
	ResolutionUnresolved = synctypes.ResolutionUnresolved

	ConflictEditEdit     = synctypes.ConflictEditEdit
	ConflictEditDelete   = synctypes.ConflictEditDelete
	ConflictCreateCreate = synctypes.ConflictCreateCreate

	ResolvedByAuto = synctypes.ResolvedByAuto
	ResolvedByUser = synctypes.ResolvedByUser
)

// Observation strategy aliases.
const (
	ObservationUnknown   = synctypes.ObservationUnknown
	ObservationDelta     = synctypes.ObservationDelta
	ObservationEnumerate = synctypes.ObservationEnumerate
)

// Scope key kind aliases.
const (
	ScopeThrottleAccount = synctypes.ScopeThrottleAccount
	ScopeService         = synctypes.ScopeService
	ScopeQuotaOwn        = synctypes.ScopeQuotaOwn
	ScopeQuotaShortcut   = synctypes.ScopeQuotaShortcut
	ScopePermDir         = synctypes.ScopePermDir
	ScopeDiskLocal       = synctypes.ScopeDiskLocal
)

// Scope key constructor/variable aliases.
var (
	SKThrottleAccount = synctypes.SKThrottleAccount
	SKService         = synctypes.SKService
	SKQuotaOwn        = synctypes.SKQuotaOwn
	SKDiskLocal       = synctypes.SKDiskLocal
)

// Scope key constructor function aliases.
var (
	SKQuotaShortcut   = synctypes.SKQuotaShortcut
	SKPermDir         = synctypes.SKPermDir
	ParseScopeKey     = synctypes.ParseScopeKey
	ScopeKeyForStatus = synctypes.ScopeKeyForStatus
)

// Issue type constant aliases.
const (
	IssueInvalidFilename       = synctypes.IssueInvalidFilename
	IssuePathTooLong           = synctypes.IssuePathTooLong
	IssueFileTooLarge          = synctypes.IssueFileTooLarge
	IssueHashPanic             = synctypes.IssueHashPanic
	IssueBigDeleteHeld         = synctypes.IssueBigDeleteHeld
	IssuePermissionDenied      = synctypes.IssuePermissionDenied
	IssueQuotaExceeded         = synctypes.IssueQuotaExceeded
	IssueRateLimited           = synctypes.IssueRateLimited
	IssueLocalPermissionDenied = synctypes.IssueLocalPermissionDenied
	IssueCaseCollision         = synctypes.IssueCaseCollision
	IssueDiskFull              = synctypes.IssueDiskFull
	IssueServiceOutage         = synctypes.IssueServiceOutage
	IssueFileTooLargeForSpace  = synctypes.IssueFileTooLargeForSpace
)

// Function aliases.
var (
	ParseItemType       = synctypes.ParseItemType
	NewBaselineForTest  = synctypes.NewBaselineForTest
	MessageForIssueType = synctypes.MessageForIssueType
	DirLowerKeyFromPath = synctypes.DirLowerKeyFromPath
	DefaultSafetyConfig = synctypes.DefaultSafetyConfig
)

// defaultBigDeleteThreshold aliases the canonical constant from synctypes.
// Used by engine.go when EngineConfig.BigDeleteThreshold == 0.
const defaultBigDeleteThreshold = synctypes.DefaultBigDeleteThreshold

// Error variable aliases.
var (
	ErrPathEscapesSyncRoot = synctypes.ErrPathEscapesSyncRoot
	ErrSyncRootDeleted     = synctypes.ErrSyncRootDeleted
	ErrWatchLimitExhausted = synctypes.ErrWatchLimitExhausted
	ErrDeltaExpired        = synctypes.ErrDeltaExpired
	ErrSyncRootMissing     = synctypes.ErrSyncRootMissing
	ErrNosyncGuard         = synctypes.ErrNosyncGuard
	ErrBigDeleteTriggered  = synctypes.ErrBigDeleteTriggered
	ErrDependencyCycle     = synctypes.ErrDependencyCycle
)

// ---------------------------------------------------------------------------
// SyncStore re-export — for CLI callers that import sync.SyncStore directly.
// During the package split migration, the sync package acts as the public
// facade; sub-packages are internal implementation details.
// ---------------------------------------------------------------------------

// SyncStore is the sync state database. Type alias to syncstore.SyncStore so
// existing CLI code that imports sync.SyncStore continues to compile.
type SyncStore = syncstore.SyncStore

// NewSyncStore creates a SyncStore from a database path. Delegates to
// syncstore.NewSyncStore; the sync package exposes it as sync.NewSyncStore
// for backward compatibility with CLI callers.
func NewSyncStore(dbPath string, logger *slog.Logger) (*SyncStore, error) {
	return syncstore.NewSyncStore(dbPath, logger)
}

// VerifyBaseline re-exports syncstore.VerifyBaseline for CLI callers that
// import sync.VerifyBaseline directly.
var VerifyBaseline = syncstore.VerifyBaseline

// ---------------------------------------------------------------------------
// Sub-package type aliases — used by test files in this package that were
// written before the package split. These aliases allow existing tests to
// compile without requiring them to be moved to the sub-packages first.
// ---------------------------------------------------------------------------

// Executor and config types from syncexec.
type (
	ExecutorConfig = syncexec.ExecutorConfig
	Executor       = syncexec.Executor
)

// Worker pool from syncexec.
type WorkerPool = syncexec.WorkerPool

// Dependency graph and scope orchestration from syncdispatch.
type (
	DepGraph      = syncdispatch.DepGraph
	ScopeGate     = syncdispatch.ScopeGate
	ScopeState    = syncdispatch.ScopeState
	DeleteCounter = syncdispatch.DeleteCounter
)

// Event buffer from syncobserve.
type Buffer = syncobserve.Buffer

// ---------------------------------------------------------------------------
// Sub-package constructor aliases
// ---------------------------------------------------------------------------

var (
	// Executor construction.
	NewExecutorConfig = syncexec.NewExecutorConfig
	NewExecution      = syncexec.NewExecution

	// Worker pool.
	NewWorkerPool = syncexec.NewWorkerPool

	// Dependency graph and scope orchestration.
	NewDepGraph      = syncdispatch.NewDepGraph
	NewScopeGate     = syncdispatch.NewScopeGate
	NewScopeState    = syncdispatch.NewScopeState
	NewDeleteCounter = syncdispatch.NewDeleteCounter

	// Event buffer.
	NewBuffer = syncobserve.NewBuffer

	// Planner.
	NewPlanner = syncplan.NewPlanner
)

// newDeleteCounter is the unexported alias for syncdispatch.NewDeleteCounter,
// used by tests in this package that reference the old unexported name.
var newDeleteCounter = syncdispatch.NewDeleteCounter

// ---------------------------------------------------------------------------
// Status constant aliases — unexported, for test files in this package that
// use the status string constants directly (e.g. commit_observation_test.go).
// ---------------------------------------------------------------------------

const (
	statusPendingDownload = syncstore.StatusPendingDownload
	statusDownloading     = syncstore.StatusDownloading
	statusDownloadFailed  = syncstore.StatusDownloadFailed
	statusSynced          = syncstore.StatusSynced
	statusPendingDelete   = syncstore.StatusPendingDelete
	statusDeleting        = syncstore.StatusDeleting
	statusDeleteFailed    = syncstore.StatusDeleteFailed
	statusDeleted         = syncstore.StatusDeleted
	statusFiltered        = syncstore.StatusFiltered
)

// ---------------------------------------------------------------------------
// Verify constant aliases — for verify_test.go which uses the unqualified names.
// ---------------------------------------------------------------------------

const (
	VerifyMissing      = syncstore.VerifyMissing
	VerifyHashMismatch = syncstore.VerifyHashMismatch
	VerifySizeMismatch = syncstore.VerifySizeMismatch
)

// ---------------------------------------------------------------------------
// Observer type aliases — for test files that reference these types directly.
// ---------------------------------------------------------------------------

type (
	LocalObserver  = syncobserve.LocalObserver
	RemoteObserver = syncobserve.RemoteObserver
	FsWatcher      = syncobserve.FsWatcher

	// Internal types exposed for tests (were in the sync package before split).
	inflightParent   = syncobserve.InflightParent
	itemConverter    = syncobserve.ItemConverter
	observerCounters = syncobserve.ObserverCounters
	hashRequest      = syncobserve.HashRequest
)

// Observer constructor aliases.
var (
	NewLocalObserver  = syncobserve.NewLocalObserver
	NewRemoteObserver = syncobserve.NewRemoteObserver

	// Unexported aliases used by tests.
	newPrimaryConverter  = syncobserve.NewPrimaryConverter
	newShortcutConverter = syncobserve.NewShortcutConverter

	// Shortcut item conversion (tests use old unexported names).
	convertShortcutItems  = syncobserve.ConvertShortcutItems
	detectShortcutOrphans = syncobserve.DetectShortcutOrphans
)

// Observer constants (unexported aliases for test access).
const (
	hashRequestBufSize    = syncobserve.HashRequestBufSize
	maxCoalesceRetries    = syncobserve.MaxCoalesceRetries
	maxOneDriveFileSize   = syncobserve.MaxOneDriveFileSize
	maxOneDrivePathLength = syncobserve.MaxOneDrivePathLength
	minPollInterval       = syncobserve.MinPollInterval
)

// Observer function aliases (unexported, for tests).
var (
	classifyItemType     = syncobserve.ClassifyItemType
	toUnixNano           = syncobserve.ToUnixNano
	timeSleep            = syncobserve.TimeSleep
	skipEntry            = syncobserve.SkipEntry
	syncRootExists       = syncobserve.SyncRootExists
	validateOneDriveName = syncobserve.ValidateOneDriveName
	shouldObserve        = syncobserve.ShouldObserve
	isAlwaysExcluded     = syncobserve.IsAlwaysExcluded
	detectCaseCollisions = syncobserve.DetectCaseCollisions
	asciiLower           = syncobserve.AsciiLower
	readInotifyLimit     = syncobserve.ReadInotifyLimit
	checkInotifyCapacity = syncobserve.CheckInotifyCapacity
	isWatchLimitError    = syncobserve.IsWatchLimitError
)

// ---------------------------------------------------------------------------
// Planner function aliases — for test files that reference internal functions.
// ---------------------------------------------------------------------------

var (
	ActionsOfType          = syncplan.ActionsOfType
	makeAction             = syncplan.MakeAction
	buildDependencies      = syncplan.BuildDependencies
	detectLocalChange      = syncplan.DetectLocalChange
	detectRemoteChange     = syncplan.DetectRemoteChange
	isCrossDriveLocalMove  = syncplan.IsCrossDriveLocalMove
	isCrossDriveRemoteMove = syncplan.IsCrossDriveRemoteMove
	exceedsDeleteThreshold = syncplan.ExceedsDeleteThreshold
	resolvePathDriveID     = syncplan.ResolvePathDriveID
	isActionableIssue      = syncstore.IsActionableIssue
	detectDependencyCycle  = syncplan.DetectDependencyCycle
	isWriteDenied          = syncplan.IsWriteDenied
)

// Planner constants (unexported aliases for test access).
const (
	defaultInitialTrialInterval = syncdispatch.DefaultInitialTrialInterval
	defaultMaxTrialInterval     = syncdispatch.DefaultMaxTrialInterval
)

// ---------------------------------------------------------------------------
// Executor function aliases — for test files that reference internal functions.
// ---------------------------------------------------------------------------

var (
	findNonDisposable = syncexec.FindNonDisposable
	isDisposable      = syncexec.IsDisposable
	conflictCopyPath  = syncexec.ConflictCopyPath
	conflictStemExt   = syncexec.ConflictStemExt
	containedPath     = syncexec.ContainedPath
	extractHTTPStatus = syncexec.ExtractHTTPStatus
	extractRetryAfter = syncexec.ExtractRetryAfter
)

// ---------------------------------------------------------------------------
// Syncstore function aliases — for test files that reference internal functions.
// ---------------------------------------------------------------------------

var computeNewStatus = syncstore.ComputeNewStatus
