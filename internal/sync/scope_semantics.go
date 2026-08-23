package sync

type scopeFamily string

const (
	scopeFamilyUnknown        scopeFamily = ""
	scopeFamilyThrottleTarget scopeFamily = "throttle_target"
	scopeFamilyService        scopeFamily = "service"
	scopeFamilyQuotaOwn       scopeFamily = "quota_own"
	scopeFamilyPermDir        scopeFamily = "perm_dir"
	scopeFamilyPermRemote     scopeFamily = "perm_remote"
	scopeFamilyDiskLocal      scopeFamily = "disk_local"
)

type scopeAccess string

const (
	scopeAccessUnknown scopeAccess = ""
	scopeAccessNone    scopeAccess = "none"
	scopeAccessRead    scopeAccess = "read"
	scopeAccessWrite   scopeAccess = "write"
)

type scopeSubjectKind string

const (
	scopeSubjectKindUnknown scopeSubjectKind = ""
	scopeSubjectKindNone    scopeSubjectKind = "none"
	scopeSubjectKindDrive   scopeSubjectKind = "drive"
	scopeSubjectKindPath    scopeSubjectKind = "path"
)

const scopePriorityMax = 99

const (
	scopePriorityThrottleTarget = 0
	scopePriorityService        = 1
	scopePriorityDiskLocal      = 2
	scopePriorityQuotaOwn       = 3
	scopePriorityPermDir        = 4
	scopePriorityPermRemote     = 5
)

type scopeDescriptor struct {
	Key                  ScopeKey
	Family               scopeFamily
	Access               scopeAccess
	SubjectKind          scopeSubjectKind
	SubjectValue         string
	DefaultConditionType string
	Priority             int
}

func describeScopeKey(key ScopeKey) scopeDescriptor {
	switch key.Kind {
	case ScopeThrottleTarget:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyThrottleTarget,
			Access:               scopeAccessNone,
			SubjectKind:          scopeSubjectKindDrive,
			SubjectValue:         key.Param,
			DefaultConditionType: issueRateLimited,
			Priority:             scopePriorityThrottleTarget,
		}
	case ScopeService:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyService,
			Access:               scopeAccessNone,
			SubjectKind:          scopeSubjectKindNone,
			DefaultConditionType: issueServiceOutage,
			Priority:             scopePriorityService,
		}
	case ScopeDiskLocal:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyDiskLocal,
			Access:               scopeAccessNone,
			SubjectKind:          scopeSubjectKindNone,
			DefaultConditionType: issueDiskFull,
			Priority:             scopePriorityDiskLocal,
		}
	case ScopeQuotaOwn:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyQuotaOwn,
			Access:               scopeAccessNone,
			SubjectKind:          scopeSubjectKindNone,
			DefaultConditionType: IssueQuotaExceeded,
			Priority:             scopePriorityQuotaOwn,
		}
	case ScopePermDirRead:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyPermDir,
			Access:               scopeAccessRead,
			SubjectKind:          scopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: issueLocalReadDenied,
			Priority:             scopePriorityPermDir,
		}
	case ScopePermDirWrite:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyPermDir,
			Access:               scopeAccessWrite,
			SubjectKind:          scopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: issueLocalWriteDenied,
			Priority:             scopePriorityPermDir,
		}
	case ScopePermRemoteRead:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyPermRemote,
			Access:               scopeAccessRead,
			SubjectKind:          scopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: issueRemoteReadDenied,
			Priority:             scopePriorityPermRemote,
		}
	case ScopePermRemoteWrite:
		return scopeDescriptor{
			Key:                  key,
			Family:               scopeFamilyPermRemote,
			Access:               scopeAccessWrite,
			SubjectKind:          scopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: IssueRemoteWriteDenied,
			Priority:             scopePriorityPermRemote,
		}
	default:
		return scopeDescriptor{
			Key:      key,
			Priority: scopePriorityMax,
		}
	}
}

func (d scopeDescriptor) IsZero() bool {
	return d.Family == scopeFamilyUnknown
}

func (d scopeDescriptor) ScopePath() string {
	if d.SubjectKind != scopeSubjectKindPath {
		return ""
	}

	return d.SubjectValue
}

// PersistsInBlockScopes reports whether this scope is a timed blocked-work
// scope that belongs in block_scopes. Read-denied subtree boundaries remain
// observation-owned facts carried on observation_issues via ScopeKey.
func (d scopeDescriptor) PersistsInBlockScopes() bool {
	if d.IsZero() {
		return false
	}

	if d.Family == scopeFamilyPermDir || d.Family == scopeFamilyPermRemote {
		return d.Access != scopeAccessRead
	}

	return true
}

func (d scopeDescriptor) Humanize() string {
	switch d.Family {
	case scopeFamilyUnknown:
		return d.Key.String()
	case scopeFamilyThrottleTarget:
		return "this drive (rate limited)"
	case scopeFamilyService:
		return "OneDrive service"
	case scopeFamilyQuotaOwn:
		return "this drive storage"
	case scopeFamilyPermDir, scopeFamilyPermRemote:
		if d.SubjectValue == "" {
			return "/"
		}
		return d.SubjectValue
	case scopeFamilyDiskLocal:
		return "local disk"
	default:
		return d.Key.String()
	}
}

func (d scopeDescriptor) BlocksAction(
	path string,
	throttleTargetKey string,
	actionType actionType,
) bool {
	switch d.Family {
	case scopeFamilyUnknown:
		return false
	case scopeFamilyService:
		return true
	case scopeFamilyThrottleTarget:
		return throttleTargetKey != "" && throttleTargetKey == d.SubjectValue
	case scopeFamilyDiskLocal:
		return actionType == ActionDownload
	case scopeFamilyQuotaOwn:
		return actionType == ActionUpload
	case scopeFamilyPermDir:
		if d.Access == scopeAccessRead {
			return false
		}
		return scopePathMatches(path, d.SubjectValue) && localWriteScopeBlocksAction(actionType)
	case scopeFamilyPermRemote:
		if d.Access == scopeAccessRead {
			return false
		}
		return scopePathMatches(path, d.SubjectValue) && remoteWriteScopeBlocksAction(actionType)
	default:
		return false
	}
}

func localWriteScopeBlocksAction(actionType actionType) bool {
	switch actionType {
	case ActionDownload,
		ActionLocalDelete,
		ActionLocalMove,
		ActionConflictCopy,
		ActionBaselineUpdate,
		ActionCleanup,
		ActionFolderCreate:
		return true
	case ActionUpload,
		ActionRemoteDelete,
		ActionRemoteMove:
		return false
	}
	return false
}

func remoteWriteScopeBlocksAction(actionType actionType) bool {
	switch actionType {
	case ActionUpload,
		ActionRemoteDelete,
		ActionRemoteMove,
		ActionFolderCreate:
		return true
	case ActionDownload,
		ActionLocalDelete,
		ActionLocalMove,
		ActionConflictCopy,
		ActionBaselineUpdate,
		ActionCleanup:
		return false
	}
	return false
}
