package multisync

import (
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type mountID string

func (id mountID) String() string {
	return string(id)
}

// StandaloneMountConfig is the CLI/config to multisync boundary for a
// configured top-level mount. Config resolution owns producing these values;
// multisync consumes them without reaching back into config-owned drive shapes.
type StandaloneMountConfig struct {
	SelectionIndex         int
	CanonicalID            driveid.CanonicalID
	DisplayName            string
	SyncRoot               string
	StatePath              string
	RemoteDriveID          driveid.ID
	RemoteRootItemID       string
	TokenOwnerCanonical    driveid.CanonicalID
	AccountEmail           string
	Paused                 bool
	EnableWebsocket        bool
	RemoteRootDeltaCapable bool
	TransferWorkers        int
	CheckWorkers           int
	MinFreeSpaceBytes      int64
}

// StandaloneMountSelection carries the configured top-level mounts that are
// eligible for runtime construction plus mount-local startup failures discovered
// while compiling selected config drives at the CLI boundary.
type StandaloneMountSelection struct {
	Mounts         []StandaloneMountConfig
	StartupResults []MountStartupResult
}

// mountSpec is the control plane's runtime unit.
type mountSpec struct {
	mountID                   mountID
	parentMountID             mountID
	bindingItemID             string
	projectionKind            MountProjectionKind
	selectionIndex            int
	canonicalID               driveid.CanonicalID
	driveType                 string
	rejectSharePointRootForms bool
	displayName               string
	syncRoot                  string
	statePath                 string
	remoteDriveID             driveid.ID
	remoteRootItemID          string
	tokenOwnerCanonical       driveid.CanonicalID
	accountEmail              string
	paused                    bool
	enableWebsocket           bool
	remoteRootDeltaCapable    bool
	transferWorkers           int
	checkWorkers              int
	minFreeSpace              int64
	finalDrain                bool
	expectedSyncRootIdentity  *syncengine.ShortcutRootIdentity
	shortcutTopologyHandler   syncengine.ShortcutChildTopologySink
}

type compiledMountSet struct {
	Mounts             []*mountSpec
	Skipped            []MountStartupResult
	FinalDrainMountIDs []string
	ReleasedChildren   []releasedShortcutChild
}

type childMountCandidate struct {
	namespaceID       mountID
	bindingItemID     string
	localAlias        string
	relativeLocalPath string
	runnerAction      syncengine.ShortcutChildRunnerAction
	blockedDetail     string
	mount             *mountSpec
	contentRootKey    string
	skipErr           error
}

type unmatchedShortcutChild struct {
	namespaceID mountID
	child       syncengine.ShortcutChildTopology
}

func buildStandaloneMountSpecs(configs []StandaloneMountConfig) ([]*mountSpec, error) {
	mounts := make([]*mountSpec, 0, len(configs))
	for i := range configs {
		mount, err := buildStandaloneMountSpec(&configs[i])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}

	return mounts, nil
}

func compileRuntimeMounts(
	standaloneMounts []StandaloneMountConfig,
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	return compileRuntimeMountsForParents(parents, topologies, nil)
}

func compileRuntimeMountsForParents(
	parents []*mountSpec,
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
	logger *slog.Logger,
) (*compiledMountSet, error) {
	parentByID, standaloneByRoot := indexStandaloneMounts(parents)
	candidatesByParent, unmatchedChildren, err := buildChildMountCandidates(parentByID, normalizeParentShortcutTopologies(topologies))
	if err != nil {
		return nil, err
	}
	markChildProjectionConflicts(parents, candidatesByParent, standaloneByRoot)
	finalMounts, skipped := assembleRuntimeMountSet(
		parents,
		candidatesByParent,
		unmatchedChildren,
		logger,
	)

	return &compiledMountSet{
		Mounts:             finalMounts,
		Skipped:            skipped,
		FinalDrainMountIDs: finalDrainMountIDs(topologies),
		ReleasedChildren:   releasedChildren(topologies),
	}, nil
}

func normalizeParentShortcutTopologies(
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) map[mountID]syncengine.ShortcutChildTopologyPublication {
	if topologies == nil {
		return make(map[mountID]syncengine.ShortcutChildTopologyPublication)
	}
	return topologies
}

func indexStandaloneMounts(parents []*mountSpec) (map[mountID]*mountSpec, map[string]mountID) {
	parentByID := make(map[mountID]*mountSpec, len(parents))
	standaloneByRoot := make(map[string]mountID, len(parents))
	for i := range parents {
		parentByID[parents[i].mountID] = parents[i]
		standaloneByRoot[parents[i].contentRootKey()] = parents[i].mountID
	}

	return parentByID, standaloneByRoot
}

func buildChildMountCandidates(
	parentByID map[mountID]*mountSpec,
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) (map[mountID][]*childMountCandidate, []unmatchedShortcutChild, error) {
	candidatesByParent := make(map[mountID][]*childMountCandidate)
	unmatchedChildren := make([]unmatchedShortcutChild, 0)
	declaredChildren := sortedPublishedShortcutChildren(topologies)
	for i := range declaredChildren {
		declared := declaredChildren[i]
		parent := parentByID[declared.namespaceID]
		if parent == nil {
			unmatchedChildren = append(unmatchedChildren, unmatchedShortcutChild(declared))
			continue
		}

		candidate, err := buildChildMountCandidate(parent, &declared.child)
		if err != nil {
			return nil, nil, err
		}
		candidatesByParent[parent.mountID] = append(candidatesByParent[parent.mountID], candidate)
	}

	return candidatesByParent, unmatchedChildren, nil
}

type publishedShortcutChild struct {
	namespaceID mountID
	child       syncengine.ShortcutChildTopology
}

func sortedPublishedShortcutChildren(
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) []publishedShortcutChild {
	if len(topologies) == 0 {
		return nil
	}
	children := make([]publishedShortcutChild, 0)
	for parentID, publication := range topologies {
		namespaceID := parentID
		if publication.NamespaceID != "" {
			namespaceID = mountID(publication.NamespaceID)
		}
		for i := range publication.Children {
			if publication.Children[i].BindingItemID == "" {
				continue
			}
			children = append(children, publishedShortcutChild{
				namespaceID: namespaceID,
				child:       publication.Children[i],
			})
		}
	}
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].namespaceID == children[j].namespaceID {
			if children[i].child.RelativeLocalPath == children[j].child.RelativeLocalPath {
				return children[i].child.BindingItemID < children[j].child.BindingItemID
			}
			return children[i].child.RelativeLocalPath < children[j].child.RelativeLocalPath
		}
		return children[i].namespaceID < children[j].namespaceID
	})
	return children
}

func markChildProjectionConflicts(
	parents []*mountSpec,
	candidatesByParent map[mountID][]*childMountCandidate,
	standaloneByRoot map[string]mountID,
) {
	for _, parent := range parents {
		for _, candidate := range candidatesByParent[parent.mountID] {
			if candidate.skipErr != nil {
				continue
			}
			if standaloneID, found := standaloneByRoot[candidate.contentRootKey]; found {
				candidate.skipErr = fmt.Errorf(
					"content root already projected by standalone mount %s",
					standaloneID,
				)
				continue
			}
		}
	}
}

func assembleRuntimeMountSet(
	parents []*mountSpec,
	candidatesByParent map[mountID][]*childMountCandidate,
	unmatchedChildren []unmatchedShortcutChild,
	logger *slog.Logger,
) ([]*mountSpec, []MountStartupResult) {
	finalMounts := make([]*mountSpec, 0, len(parents))
	skipped := make([]MountStartupResult, 0, len(unmatchedChildren))
	nextIndex := 0
	for _, parent := range parents {
		parent.selectionIndex = nextIndex
		nextIndex++
		finalMounts = append(finalMounts, parent)

		children := candidatesByParent[parent.mountID]
		sort.Slice(children, func(i, j int) bool {
			if children[i].relativeLocalPath == children[j].relativeLocalPath {
				return children[i].mount.mountID < children[j].mount.mountID
			}

			return children[i].relativeLocalPath < children[j].relativeLocalPath
		})

		for _, candidate := range children {
			candidate.mount.selectionIndex = nextIndex
			nextIndex++
			if candidate.skipErr != nil {
				skipped = append(skipped, skippedChildMountResult(candidate, parent.mountID, logger))
				continue
			}

			finalMounts = append(finalMounts, candidate.mount)
		}
	}

	skipped = append(skipped, unmatchedChildStartupResults(unmatchedChildren, nextIndex)...)
	return finalMounts, skipped
}

func skippedChildMountResult(candidate *childMountCandidate, parentID mountID, logger *slog.Logger) MountStartupResult {
	if logger != nil {
		logger.Warn("skipping child mount",
			"mount_id", candidate.mount.mountID.String(),
			"namespace_id", parentID.String(),
			"relative_local_path", candidate.relativeLocalPath,
			"error", candidate.skipErr,
		)
	}

	return mountStartupResultForMount(candidate.mount, candidate.skipErr)
}

func unmatchedChildStartupResults(children []unmatchedShortcutChild, startIndex int) []MountStartupResult {
	results := make([]MountStartupResult, 0, len(children))
	nextIndex := startIndex
	for i := range children {
		child := children[i].child
		displayName := child.LocalAlias
		if displayName == "" {
			displayName = path.Base(child.RelativeLocalPath)
		}
		childMountID := config.ChildMountID(children[i].namespaceID.String(), child.BindingItemID)
		results = append(results, MountStartupResult{
			SelectionIndex: nextIndex,
			Identity: MountIdentity{
				MountID:        childMountID,
				ParentMountID:  children[i].namespaceID.String(),
				ProjectionKind: MountProjectionChild,
			},
			DisplayName: displayName,
			Status:      MountStartupFatal,
			Err: fmt.Errorf(
				"child mount %s references missing parent mount %s",
				childMountID,
				children[i].namespaceID,
			),
		})
		nextIndex++
	}

	return results
}

func buildStandaloneMountSpec(cfg *StandaloneMountConfig) (*mountSpec, error) {
	if cfg == nil || cfg.CanonicalID.IsZero() {
		return nil, fmt.Errorf("multisync: standalone mount canonical ID is required")
	}
	if cfg.StatePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for %s", cfg.CanonicalID)
	}
	accountEmail := cfg.AccountEmail
	if accountEmail == "" {
		accountEmail = cfg.TokenOwnerCanonical.Email()
	}

	return &mountSpec{
		mountID:                   mountID(cfg.CanonicalID.String()),
		projectionKind:            MountProjectionStandalone,
		selectionIndex:            cfg.SelectionIndex,
		canonicalID:               cfg.CanonicalID,
		driveType:                 cfg.CanonicalID.DriveType(),
		rejectSharePointRootForms: cfg.CanonicalID.IsSharePoint(),
		displayName:               cfg.DisplayName,
		syncRoot:                  cfg.SyncRoot,
		statePath:                 cfg.StatePath,
		remoteDriveID:             cfg.RemoteDriveID,
		remoteRootItemID:          cfg.RemoteRootItemID,
		tokenOwnerCanonical:       cfg.TokenOwnerCanonical,
		accountEmail:              accountEmail,
		paused:                    cfg.Paused,
		enableWebsocket:           cfg.EnableWebsocket,
		remoteRootDeltaCapable:    cfg.RemoteRootDeltaCapable,
		transferWorkers:           cfg.TransferWorkers,
		checkWorkers:              cfg.CheckWorkers,
		minFreeSpace:              cfg.MinFreeSpaceBytes,
	}, nil
}

func buildChildMountCandidate(parent *mountSpec, child *syncengine.ShortcutChildTopology) (*childMountCandidate, error) {
	if parent == nil || child == nil || child.BindingItemID == "" {
		return nil, fmt.Errorf("multisync: parent-declared child topology is incomplete")
	}
	relativePath := child.RelativeLocalPath
	if relativePath == "" {
		return nil, fmt.Errorf("multisync: parent-declared child %s is missing a local path", child.BindingItemID)
	}
	childMountID := config.ChildMountID(parent.mountID.String(), child.BindingItemID)
	statePath := config.MountStatePath(childMountID)
	if statePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for child mount %s", childMountID)
	}

	displayName := child.LocalAlias
	if displayName == "" {
		displayName = path.Base(relativePath)
	}
	tokenOwner := parent.tokenOwnerCanonical

	childMount := &mountSpec{
		mountID:                  mountID(childMountID),
		parentMountID:            parent.mountID,
		bindingItemID:            child.BindingItemID,
		projectionKind:           MountProjectionChild,
		canonicalID:              driveid.CanonicalID{},
		displayName:              displayName,
		syncRoot:                 filepath.Join(parent.syncRoot, filepath.FromSlash(relativePath)),
		statePath:                statePath,
		remoteDriveID:            driveid.New(child.RemoteDriveID),
		remoteRootItemID:         child.RemoteItemID,
		tokenOwnerCanonical:      tokenOwner,
		accountEmail:             tokenOwner.Email(),
		paused:                   parent.paused,
		enableWebsocket:          parent.enableWebsocket,
		remoteRootDeltaCapable:   config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
		transferWorkers:          parent.transferWorkers,
		checkWorkers:             parent.checkWorkers,
		minFreeSpace:             parent.minFreeSpace,
		finalDrain:               child.RunnerAction == syncengine.ShortcutChildActionFinalDrain,
		expectedSyncRootIdentity: cloneChildRootIdentity(child.LocalRootIdentity),
	}
	skipErr := shortcutChildRunnerSkipError(childMountID, child.RunnerAction, child.BlockedDetail)

	return &childMountCandidate{
		namespaceID:       parent.mountID,
		bindingItemID:     child.BindingItemID,
		localAlias:        displayName,
		relativeLocalPath: relativePath,
		runnerAction:      child.RunnerAction,
		blockedDetail:     child.BlockedDetail,
		mount:             childMount,
		contentRootKey:    childMount.contentRootKey(),
		skipErr:           skipErr,
	}, nil
}

func shortcutChildRunnerSkipError(
	childMountID string,
	action syncengine.ShortcutChildRunnerAction,
	blockedDetail string,
) error {
	switch action {
	case syncengine.ShortcutChildActionRun:
		return nil
	case syncengine.ShortcutChildActionFinalDrain:
		return nil
	case syncengine.ShortcutChildActionSkipParentBlocked:
		if blockedDetail != "" {
			return fmt.Errorf("child mount %s is blocked by parent shortcut lifecycle: %s", childMountID, blockedDetail)
		}
		return fmt.Errorf("child mount %s is blocked by parent shortcut lifecycle", childMountID)
	case syncengine.ShortcutChildActionSkipWaitingReplacement:
		return fmt.Errorf("child mount %s is waiting for an older shortcut at the same path to finish final drain", childMountID)
	case "":
		return fmt.Errorf("child mount %s is missing a parent-declared runner action", childMountID)
	default:
		return fmt.Errorf("child mount %s has unsupported runner action %q", childMountID, action)
	}
}

func (m *mountSpec) contentRootKey() string {
	remoteRootItemID := m.remoteRootItemID
	if remoteRootItemID == "" {
		remoteRootItemID = "root"
	}

	return fmt.Sprintf("%s|%s|%s", m.tokenOwnerCanonical.String(), m.remoteDriveID.String(), remoteRootItemID)
}

func (m *mountSpec) identity() MountIdentity {
	if m == nil {
		return MountIdentity{}
	}

	return MountIdentity{
		MountID:        m.mountID.String(),
		ParentMountID:  m.parentMountID.String(),
		ProjectionKind: m.projectionKind,
		CanonicalID:    m.canonicalID,
	}
}

func (m *mountSpec) label() string {
	identity := m.identity()
	return identity.Label()
}

func (m *mountSpec) syncSessionConfig() *driveops.MountSessionConfig {
	return &driveops.MountSessionConfig{
		TokenOwnerCanonical: m.tokenOwnerCanonical,
		DriveID:             m.remoteDriveID,
		RemoteRootItemID:    m.remoteRootItemID,
		AccountEmail:        m.accountEmail,
	}
}
