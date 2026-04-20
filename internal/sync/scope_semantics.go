package sync

type ScopeFamily string

const (
	ScopeFamilyUnknown        ScopeFamily = ""
	ScopeFamilyThrottleTarget ScopeFamily = "throttle_target"
	ScopeFamilyService        ScopeFamily = "service"
	ScopeFamilyQuotaOwn       ScopeFamily = "quota_own"
	ScopeFamilyPermDir        ScopeFamily = "perm_dir"
	ScopeFamilyPermRemote     ScopeFamily = "perm_remote"
	ScopeFamilyDiskLocal      ScopeFamily = "disk_local"
)

type ScopeAccess string

const (
	ScopeAccessUnknown ScopeAccess = ""
	ScopeAccessNone    ScopeAccess = "none"
	ScopeAccessRead    ScopeAccess = "read"
	ScopeAccessWrite   ScopeAccess = "write"
)

type ScopeSubjectKind string

const (
	ScopeSubjectKindUnknown ScopeSubjectKind = ""
	ScopeSubjectKindNone    ScopeSubjectKind = "none"
	ScopeSubjectKindDrive   ScopeSubjectKind = "drive"
	ScopeSubjectKindPath    ScopeSubjectKind = "path"
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

type ScopeDescriptor struct {
	Key                  ScopeKey
	Family               ScopeFamily
	Access               ScopeAccess
	SubjectKind          ScopeSubjectKind
	SubjectValue         string
	DefaultConditionType string
	Priority             int
}

func DescribeScopeKey(key ScopeKey) ScopeDescriptor {
	switch key.Kind {
	case ScopeThrottleTarget:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyThrottleTarget,
			Access:               ScopeAccessNone,
			SubjectKind:          ScopeSubjectKindDrive,
			SubjectValue:         key.Param,
			DefaultConditionType: IssueRateLimited,
			Priority:             scopePriorityThrottleTarget,
		}
	case ScopeService:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyService,
			Access:               ScopeAccessNone,
			SubjectKind:          ScopeSubjectKindNone,
			DefaultConditionType: IssueServiceOutage,
			Priority:             scopePriorityService,
		}
	case ScopeDiskLocal:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyDiskLocal,
			Access:               ScopeAccessNone,
			SubjectKind:          ScopeSubjectKindNone,
			DefaultConditionType: IssueDiskFull,
			Priority:             scopePriorityDiskLocal,
		}
	case ScopeQuotaOwn:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyQuotaOwn,
			Access:               ScopeAccessNone,
			SubjectKind:          ScopeSubjectKindNone,
			DefaultConditionType: IssueQuotaExceeded,
			Priority:             scopePriorityQuotaOwn,
		}
	case ScopePermDirRead:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyPermDir,
			Access:               ScopeAccessRead,
			SubjectKind:          ScopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: IssueLocalReadDenied,
			Priority:             scopePriorityPermDir,
		}
	case ScopePermDirWrite:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyPermDir,
			Access:               ScopeAccessWrite,
			SubjectKind:          ScopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: IssueLocalWriteDenied,
			Priority:             scopePriorityPermDir,
		}
	case ScopePermRemoteRead:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyPermRemote,
			Access:               ScopeAccessRead,
			SubjectKind:          ScopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: IssueRemoteReadDenied,
			Priority:             scopePriorityPermRemote,
		}
	case ScopePermRemoteWrite:
		return ScopeDescriptor{
			Key:                  key,
			Family:               ScopeFamilyPermRemote,
			Access:               ScopeAccessWrite,
			SubjectKind:          ScopeSubjectKindPath,
			SubjectValue:         key.Param,
			DefaultConditionType: IssueRemoteWriteDenied,
			Priority:             scopePriorityPermRemote,
		}
	default:
		return ScopeDescriptor{
			Key:      key,
			Priority: scopePriorityMax,
		}
	}
}

func (d ScopeDescriptor) IsZero() bool {
	return d.Family == ScopeFamilyUnknown
}

func (d ScopeDescriptor) ScopePath() string {
	if d.SubjectKind != ScopeSubjectKindPath {
		return ""
	}

	return d.SubjectValue
}

func (d ScopeDescriptor) Humanize() string {
	switch d.Family {
	case ScopeFamilyUnknown:
		return d.Key.String()
	case ScopeFamilyThrottleTarget:
		return "this drive (rate limited)"
	case ScopeFamilyService:
		return "OneDrive service"
	case ScopeFamilyQuotaOwn:
		return "this drive storage"
	case ScopeFamilyPermDir, ScopeFamilyPermRemote:
		if d.SubjectValue == "" {
			return "/"
		}
		return d.SubjectValue
	case ScopeFamilyDiskLocal:
		return "local disk"
	default:
		return d.Key.String()
	}
}

func (d ScopeDescriptor) BlocksAction(
	path string,
	throttleTargetKey string,
	actionType ActionType,
) bool {
	switch d.Family {
	case ScopeFamilyUnknown:
		return false
	case ScopeFamilyService:
		return true
	case ScopeFamilyThrottleTarget:
		return throttleTargetKey != "" && throttleTargetKey == d.SubjectValue
	case ScopeFamilyDiskLocal:
		return actionType == ActionDownload
	case ScopeFamilyQuotaOwn:
		return actionType == ActionUpload
	case ScopeFamilyPermDir:
		if d.Access == ScopeAccessRead {
			return false
		}
		return scopePathMatches(path, d.SubjectValue) && localWriteScopeBlocksAction(actionType)
	case ScopeFamilyPermRemote:
		if d.Access == ScopeAccessRead {
			return false
		}
		return scopePathMatches(path, d.SubjectValue) && remoteWriteScopeBlocksAction(actionType)
	default:
		return false
	}
}

func localWriteScopeBlocksAction(actionType ActionType) bool {
	switch actionType {
	case ActionDownload,
		ActionLocalDelete,
		ActionLocalMove,
		ActionConflictCopy,
		ActionUpdateSynced,
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

func remoteWriteScopeBlocksAction(actionType ActionType) bool {
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
		ActionUpdateSynced,
		ActionCleanup:
		return false
	}
	return false
}
