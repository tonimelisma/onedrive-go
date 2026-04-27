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
	mountID                     mountID
	projectionKind              MountProjectionKind
	selectionIndex              int
	canonicalID                 driveid.CanonicalID
	driveType                   string
	rejectSharePointRootForms   bool
	displayName                 string
	syncRoot                    string
	statePath                   string
	remoteDriveID               driveid.ID
	remoteRootItemID            string
	tokenOwnerCanonical         driveid.CanonicalID
	accountEmail                string
	paused                      bool
	enableWebsocket             bool
	remoteRootDeltaCapable      bool
	transferWorkers             int
	checkWorkers                int
	minFreeSpace                int64
	child                       *childMountSpec
	parentRunnerPublicationSink syncengine.ShortcutChildRunnerSink
}

type childMountSpec struct {
	parentMountID            mountID
	bindingItemID            string
	finalDrain               bool
	expectedSyncRootIdentity *syncengine.ShortcutRootIdentity
}

type runnerDecisionSet struct {
	Mounts             []*mountSpec
	Skipped            []MountStartupResult
	FinalDrainMountIDs []string
	CleanupChildren    []shortcutChildArtifactCleanup
}

type childRunnerDecision struct {
	namespaceID       mountID
	bindingItemID     string
	localAlias        string
	relativeLocalPath string
	runnerAction      syncengine.ShortcutChildRunnerAction
	runnerDetail      string
	mount             *mountSpec
	skipErr           error
}

type childWithoutConfiguredParent struct {
	namespaceID mountID
	child       syncengine.ShortcutChildRunner
}

func filterMountSpecsByProjection(
	mounts []*mountSpec,
	kind MountProjectionKind,
) []*mountSpec {
	filtered := make([]*mountSpec, 0, len(mounts))
	for _, mount := range mounts {
		if mount == nil || mount.projectionKind != kind {
			continue
		}
		filtered = append(filtered, mount)
	}
	return filtered
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

func buildRunnerDecisions(
	standaloneMounts []StandaloneMountConfig,
	publications map[mountID]syncengine.ShortcutChildRunnerPublication,
) (*runnerDecisionSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	return buildRunnerDecisionsForParents(parents, publications, nil)
}

func buildRunnerDecisionsForParents(
	parents []*mountSpec,
	publications map[mountID]syncengine.ShortcutChildRunnerPublication,
	logger *slog.Logger,
) (*runnerDecisionSet, error) {
	parentByID := indexStandaloneMounts(parents)
	childDecisionsByParent, childrenWithoutConfiguredParent, err := buildChildRunnerDecisions(parentByID, publications)
	if err != nil {
		return nil, err
	}
	finalMounts, skipped := assembleRuntimeMountSet(
		parents,
		childDecisionsByParent,
		childrenWithoutConfiguredParent,
		logger,
	)

	cleanupChildren, err := shortcutChildArtifactCleanups(publications)
	if err != nil {
		return nil, err
	}

	return &runnerDecisionSet{
		Mounts:             finalMounts,
		Skipped:            skipped,
		FinalDrainMountIDs: finalDrainMountIDs(publications),
		CleanupChildren:    cleanupChildren,
	}, nil
}

func indexStandaloneMounts(parents []*mountSpec) map[mountID]*mountSpec {
	parentByID := make(map[mountID]*mountSpec, len(parents))
	for i := range parents {
		parentByID[parents[i].mountID] = parents[i]
	}

	return parentByID
}

func buildChildRunnerDecisions(
	parentByID map[mountID]*mountSpec,
	publications map[mountID]syncengine.ShortcutChildRunnerPublication,
) (map[mountID][]*childRunnerDecision, []childWithoutConfiguredParent, error) {
	childDecisionsByParent := make(map[mountID][]*childRunnerDecision)
	childrenWithoutConfiguredParent := make([]childWithoutConfiguredParent, 0)
	declaredChildren := sortedPublishedShortcutChildren(publications)
	for i := range declaredChildren {
		declared := declaredChildren[i]
		parent := parentByID[declared.namespaceID]
		if parent == nil {
			childrenWithoutConfiguredParent = append(childrenWithoutConfiguredParent, childWithoutConfiguredParent(declared))
			continue
		}

		decision, err := buildChildRunnerDecision(parent, &declared.child)
		if err != nil {
			return nil, nil, err
		}
		childDecisionsByParent[parent.mountID] = append(childDecisionsByParent[parent.mountID], decision)
	}

	return childDecisionsByParent, childrenWithoutConfiguredParent, nil
}

type publishedShortcutChild struct {
	namespaceID mountID
	child       syncengine.ShortcutChildRunner
}

func sortedPublishedShortcutChildren(
	publications map[mountID]syncengine.ShortcutChildRunnerPublication,
) []publishedShortcutChild {
	if len(publications) == 0 {
		return nil
	}
	children := make([]publishedShortcutChild, 0)
	for parentID, publication := range publications {
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

func assembleRuntimeMountSet(
	parents []*mountSpec,
	childDecisionsByParent map[mountID][]*childRunnerDecision,
	childrenWithoutConfiguredParent []childWithoutConfiguredParent,
	logger *slog.Logger,
) ([]*mountSpec, []MountStartupResult) {
	finalMounts := make([]*mountSpec, 0, len(parents))
	skipped := make([]MountStartupResult, 0, len(childrenWithoutConfiguredParent))
	nextIndex := 0
	for _, parent := range parents {
		parent.selectionIndex = nextIndex
		nextIndex++
		finalMounts = append(finalMounts, parent)

		children := childDecisionsByParent[parent.mountID]
		sort.Slice(children, func(i, j int) bool {
			if children[i].relativeLocalPath == children[j].relativeLocalPath {
				return children[i].mount.mountID < children[j].mount.mountID
			}

			return children[i].relativeLocalPath < children[j].relativeLocalPath
		})

		for _, decision := range children {
			decision.mount.selectionIndex = nextIndex
			nextIndex++
			if decision.skipErr != nil {
				skipped = append(skipped, skippedChildMountResult(decision, parent.mountID, logger))
				continue
			}

			finalMounts = append(finalMounts, decision.mount)
		}
	}

	skipped = append(skipped, childWithoutConfiguredParentStartupResults(childrenWithoutConfiguredParent, nextIndex)...)
	return finalMounts, skipped
}

func skippedChildMountResult(decision *childRunnerDecision, parentID mountID, logger *slog.Logger) MountStartupResult {
	if logger != nil {
		logger.Warn("skipping child mount",
			"mount_id", decision.mount.mountID.String(),
			"namespace_id", parentID.String(),
			"relative_local_path", decision.relativeLocalPath,
			"error", decision.skipErr,
		)
	}

	return mountStartupResultForMount(decision.mount, decision.skipErr)
}

func childWithoutConfiguredParentStartupResults(children []childWithoutConfiguredParent, startIndex int) []MountStartupResult {
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

func buildChildRunnerDecision(parent *mountSpec, child *syncengine.ShortcutChildRunner) (*childRunnerDecision, error) {
	if parent == nil || child == nil || child.BindingItemID == "" {
		return nil, fmt.Errorf("multisync: parent-declared child runner publication is incomplete")
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
		mountID:                mountID(childMountID),
		projectionKind:         MountProjectionChild,
		canonicalID:            driveid.CanonicalID{},
		displayName:            displayName,
		syncRoot:               filepath.Join(parent.syncRoot, filepath.FromSlash(relativePath)),
		statePath:              statePath,
		remoteDriveID:          driveid.New(child.RemoteDriveID),
		remoteRootItemID:       child.RemoteItemID,
		tokenOwnerCanonical:    tokenOwner,
		accountEmail:           tokenOwner.Email(),
		paused:                 parent.paused,
		enableWebsocket:        parent.enableWebsocket,
		remoteRootDeltaCapable: config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
		transferWorkers:        parent.transferWorkers,
		checkWorkers:           parent.checkWorkers,
		minFreeSpace:           parent.minFreeSpace,
		child: &childMountSpec{
			parentMountID:            parent.mountID,
			bindingItemID:            child.BindingItemID,
			finalDrain:               child.RunnerAction == syncengine.ShortcutChildActionFinalDrain,
			expectedSyncRootIdentity: cloneChildRootIdentity(child.LocalRootIdentity),
		},
	}
	skipErr := shortcutChildRunnerSkipError(childMountID, child.RunnerAction, child.RunnerDetail)

	return &childRunnerDecision{
		namespaceID:       parent.mountID,
		bindingItemID:     child.BindingItemID,
		localAlias:        displayName,
		relativeLocalPath: relativePath,
		runnerAction:      child.RunnerAction,
		runnerDetail:      child.RunnerDetail,
		mount:             childMount,
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
	case "":
		return fmt.Errorf("child mount %s is missing a parent-declared runner action", childMountID)
	default:
		return fmt.Errorf("child mount %s has unsupported runner action %q", childMountID, action)
	}
}

func (m *mountSpec) identity() MountIdentity {
	if m == nil {
		return MountIdentity{}
	}

	return MountIdentity{
		MountID:        m.mountID.String(),
		ParentMountID:  m.childParentMountID().String(),
		ProjectionKind: m.projectionKind,
		CanonicalID:    m.canonicalID,
	}
}

func (m *mountSpec) childBindingItemID() string {
	if m == nil || m.child == nil {
		return ""
	}
	return m.child.bindingItemID
}

func (m *mountSpec) childParentMountID() mountID {
	if m == nil || m.child == nil {
		return ""
	}
	return m.child.parentMountID
}

func (m *mountSpec) isFinalDrainChild() bool {
	return m != nil && m.child != nil && m.child.finalDrain
}

func (m *mountSpec) expectedChildRootIdentity() *syncengine.ShortcutRootIdentity {
	if m == nil || m.child == nil {
		return nil
	}
	return m.child.expectedSyncRootIdentity
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
