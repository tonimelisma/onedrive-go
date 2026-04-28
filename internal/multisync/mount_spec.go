package multisync

import (
	"fmt"
	"log/slog"
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

// mountSpec is the control plane's runtime unit. It is deliberately a
// discriminated parent/child shape: common process fields live in the concrete
// parent or child payload, and child-only lifecycle inputs cannot be attached
// to a parent runtime mount.
type mountSpec struct {
	parent *parentMountRuntime
	child  *childMountRuntime
}

type mountRuntimeCommon struct {
	mountID                mountID
	selectionIndex         int
	displayName            string
	syncRoot               string
	statePath              string
	remoteDriveID          driveid.ID
	remoteRootItemID       string
	tokenOwnerCanonical    driveid.CanonicalID
	accountEmail           string
	paused                 bool
	enableWebsocket        bool
	remoteRootDeltaCapable bool
	transferWorkers        int
	checkWorkers           int
	minFreeSpace           int64
}

type parentMountSpec struct {
	mountID                   mountID
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
}

type childMountSpec struct {
	parentMountID          mountID
	mountID                mountID
	displayName            string
	syncRoot               string
	statePath              string
	remoteDriveID          driveid.ID
	remoteRootItemID       string
	tokenOwnerCanonical    driveid.CanonicalID
	accountEmail           string
	paused                 bool
	enableWebsocket        bool
	remoteRootDeltaCapable bool
	transferWorkers        int
	checkWorkers           int
	minFreeSpace           int64
	mode                   syncengine.ShortcutChildRunMode
	ackRef                 syncengine.ShortcutChildAckRef
	engine                 syncengine.ShortcutChildEngineSpec
}

type childMountRuntime struct {
	common        mountRuntimeCommon
	parentMountID mountID
	mode          syncengine.ShortcutChildRunMode
	ackRef        syncengine.ShortcutChildAckRef
	engine        syncengine.ShortcutChildEngineSpec
}

type parentMountRuntime struct {
	common                    mountRuntimeCommon
	canonicalID               driveid.CanonicalID
	driveType                 string
	rejectSharePointRootForms bool
	childWorkSink             syncengine.ShortcutChildWorkSink
}

type runtimeWorkSet struct {
	Mounts          []*mountSpec
	Skipped         []MountStartupResult
	CleanupChildren []shortcutChildArtifactCleanup
}

func filterMountSpecsByProjection(
	mounts []*mountSpec,
	kind MountProjectionKind,
) []*mountSpec {
	filtered := make([]*mountSpec, 0, len(mounts))
	for _, mount := range mounts {
		if mount == nil || mount.projectionKind() != kind {
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

func buildRuntimeWork(
	standaloneMounts []StandaloneMountConfig,
	snapshots map[mountID]syncengine.ShortcutChildWorkSnapshot,
	dataDir string,
) (*runtimeWorkSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	return buildRuntimeWorkForParents(parents, snapshots, dataDir, nil)
}

func buildRuntimeWorkForParents(
	parents []*mountSpec,
	snapshots map[mountID]syncengine.ShortcutChildWorkSnapshot,
	dataDir string,
	logger *slog.Logger,
) (*runtimeWorkSet, error) {
	parentByID := indexStandaloneMounts(parents)
	childrenByParent, skipped, err := buildChildMountSpecs(parentByID, snapshots, dataDir)
	if err != nil {
		return nil, err
	}
	finalMounts := assembleRuntimeMountSet(
		parents,
		childrenByParent,
		logger,
	)

	cleanupChildren, err := shortcutChildArtifactCleanups(snapshots)
	if err != nil {
		return nil, err
	}

	return &runtimeWorkSet{
		Mounts:          finalMounts,
		Skipped:         skipped,
		CleanupChildren: cleanupChildren,
	}, nil
}

func indexStandaloneMounts(parents []*mountSpec) map[mountID]*mountSpec {
	parentByID := make(map[mountID]*mountSpec, len(parents))
	for i := range parents {
		parentByID[parents[i].id()] = parents[i]
	}

	return parentByID
}

func buildChildMountSpecs(
	parentByID map[mountID]*mountSpec,
	snapshots map[mountID]syncengine.ShortcutChildWorkSnapshot,
	dataDir string,
) (map[mountID][]*mountSpec, []MountStartupResult, error) {
	childrenByParent := make(map[mountID][]*mountSpec)
	skipped := make([]MountStartupResult, 0)
	commands := sortedChildRunCommands(snapshots)
	for i := range commands {
		declared := commands[i]
		childMountID, err := validateChildRunCommand(&declared.command)
		if err != nil {
			return nil, nil, err
		}
		parent := parentByID[declared.namespaceID]
		if parent == nil {
			skipped = append(skipped, childCommandStartupResult(declared.namespaceID, &declared.command, 0, fmt.Errorf(
				"child mount %s references missing parent mount %s",
				childMountID,
				declared.namespaceID,
			)))
			continue
		}

		childMount, err := buildChildMountFromCommand(parent, &declared.command, dataDir)
		if err != nil {
			return nil, nil, err
		}
		childrenByParent[parent.id()] = append(childrenByParent[parent.id()], childMount)
	}

	return childrenByParent, skipped, nil
}

type parentChildRunCommand struct {
	namespaceID mountID
	command     syncengine.ShortcutChildRunCommand
}

func sortedChildRunCommands(
	snapshots map[mountID]syncengine.ShortcutChildWorkSnapshot,
) []parentChildRunCommand {
	if len(snapshots) == 0 {
		return nil
	}
	commands := make([]parentChildRunCommand, 0)
	for parentID, snapshot := range snapshots {
		namespaceID := parentID
		if snapshot.NamespaceID != "" {
			namespaceID = mountID(snapshot.NamespaceID)
		}
		for i := range snapshot.RunCommands {
			commands = append(commands, parentChildRunCommand{
				namespaceID: namespaceID,
				command:     snapshot.RunCommands[i],
			})
		}
	}
	sort.SliceStable(commands, func(i, j int) bool {
		if commands[i].namespaceID == commands[j].namespaceID {
			return commands[i].command.ChildMountID < commands[j].command.ChildMountID
		}
		return commands[i].namespaceID < commands[j].namespaceID
	})
	return commands
}

func assembleRuntimeMountSet(
	parents []*mountSpec,
	childrenByParent map[mountID][]*mountSpec,
	logger *slog.Logger,
) []*mountSpec {
	finalMounts := make([]*mountSpec, 0, len(parents))
	nextIndex := 0
	for _, parent := range parents {
		parent.setSelectionIndex(nextIndex)
		nextIndex++
		finalMounts = append(finalMounts, parent)

		children := childrenByParent[parent.id()]
		sort.Slice(children, func(i, j int) bool {
			return children[i].id() < children[j].id()
		})

		for _, child := range children {
			child.setSelectionIndex(nextIndex)
			nextIndex++
			finalMounts = append(finalMounts, child)
		}
	}

	if logger != nil {
		logger.Debug("assembled runtime mount set", "mounts", len(finalMounts))
	}
	return finalMounts
}

func childCommandStartupResult(
	parentID mountID,
	command *syncengine.ShortcutChildRunCommand,
	selectionIndex int,
	err error,
) MountStartupResult {
	childMountID := ""
	displayName := ""
	if command != nil {
		childMountID = command.ChildMountID
		displayName = command.DisplayName
	}
	return MountStartupResult{
		SelectionIndex: selectionIndex,
		Identity: MountIdentity{
			MountID:        childMountID,
			ParentMountID:  parentID.String(),
			ProjectionKind: MountProjectionChild,
		},
		DisplayName: displayName,
		Status:      MountStartupFatal,
		Err:         err,
	}
}

func buildStandaloneMountSpec(cfg *StandaloneMountConfig) (*mountSpec, error) {
	spec, err := newParentMountSpec(cfg)
	if err != nil {
		return nil, err
	}
	return spec.runtimeMountSpec(), nil
}

func newParentMountSpec(cfg *StandaloneMountConfig) (parentMountSpec, error) {
	if cfg == nil || cfg.CanonicalID.IsZero() {
		return parentMountSpec{}, fmt.Errorf("multisync: standalone mount canonical ID is required")
	}
	if cfg.StatePath == "" {
		return parentMountSpec{}, fmt.Errorf("multisync: state path is required for %s", cfg.CanonicalID)
	}
	accountEmail := cfg.AccountEmail
	if accountEmail == "" {
		accountEmail = cfg.TokenOwnerCanonical.Email()
	}

	return parentMountSpec{
		mountID:                   mountID(cfg.CanonicalID.String()),
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

func (spec *parentMountSpec) runtimeMountSpec() *mountSpec {
	return &mountSpec{
		parent: &parentMountRuntime{
			common:                    spec.runtimeCommon(),
			canonicalID:               spec.canonicalID,
			driveType:                 spec.driveType,
			rejectSharePointRootForms: spec.rejectSharePointRootForms,
		},
	}
}

func (spec *parentMountSpec) runtimeCommon() mountRuntimeCommon {
	return mountRuntimeCommon{
		mountID:                spec.mountID,
		selectionIndex:         spec.selectionIndex,
		displayName:            spec.displayName,
		syncRoot:               spec.syncRoot,
		statePath:              spec.statePath,
		remoteDriveID:          spec.remoteDriveID,
		remoteRootItemID:       spec.remoteRootItemID,
		tokenOwnerCanonical:    spec.tokenOwnerCanonical,
		accountEmail:           spec.accountEmail,
		paused:                 spec.paused,
		enableWebsocket:        spec.enableWebsocket,
		remoteRootDeltaCapable: spec.remoteRootDeltaCapable,
		transferWorkers:        spec.transferWorkers,
		checkWorkers:           spec.checkWorkers,
		minFreeSpace:           spec.minFreeSpace,
	}
}

func buildChildMountFromCommand(
	parent *mountSpec,
	command *syncengine.ShortcutChildRunCommand,
	dataDir string,
) (*mountSpec, error) {
	if parent == nil || command == nil {
		return nil, fmt.Errorf("multisync: parent-declared child run command is incomplete")
	}
	childMountID, err := validateChildRunCommand(command)
	if err != nil {
		return nil, err
	}
	statePath := config.MountStatePathForDataDir(dataDir, childMountID)
	if statePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for child mount %s", childMountID)
	}

	displayName := command.DisplayName
	tokenOwner := parent.tokenOwnerCanonical()

	childSpec := newChildMountSpec(parent, command, childMountID, displayName, statePath, tokenOwner)
	return childSpec.runtimeMountSpec(), nil
}

func validateChildRunCommand(command *syncengine.ShortcutChildRunCommand) (string, error) {
	childMountID, err := syncengine.ValidateShortcutChildRunCommand(command)
	if err != nil {
		return "", fmt.Errorf("multisync: parent-declared child run command: %w", err)
	}
	return childMountID, nil
}

func newChildMountSpec(
	parent *mountSpec,
	command *syncengine.ShortcutChildRunCommand,
	childMountID string,
	displayName string,
	statePath string,
	tokenOwner driveid.CanonicalID,
) childMountSpec {
	return childMountSpec{
		parentMountID:          parent.id(),
		mountID:                mountID(childMountID),
		displayName:            displayName,
		syncRoot:               command.Engine.LocalRoot,
		statePath:              statePath,
		remoteDriveID:          driveid.New(command.Engine.RemoteDriveID),
		remoteRootItemID:       command.Engine.RemoteItemID,
		tokenOwnerCanonical:    tokenOwner,
		accountEmail:           tokenOwner.Email(),
		paused:                 parent.paused(),
		enableWebsocket:        parent.enableWebsocket(),
		remoteRootDeltaCapable: config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
		transferWorkers:        parent.transferWorkers(),
		checkWorkers:           parent.checkWorkers(),
		minFreeSpace:           parent.minFreeSpace(),
		mode:                   command.Mode,
		ackRef:                 command.AckRef,
		engine:                 command.Engine,
	}
}

func (spec *childMountSpec) runtimeMountSpec() *mountSpec {
	return &mountSpec{
		child: &childMountRuntime{
			common:        spec.runtimeCommon(),
			parentMountID: spec.parentMountID,
			mode:          spec.mode,
			ackRef:        spec.ackRef,
			engine:        spec.engine,
		},
	}
}

func (spec *childMountSpec) runtimeCommon() mountRuntimeCommon {
	return mountRuntimeCommon{
		mountID:                spec.mountID,
		displayName:            spec.displayName,
		syncRoot:               spec.syncRoot,
		statePath:              spec.statePath,
		remoteDriveID:          spec.remoteDriveID,
		remoteRootItemID:       spec.remoteRootItemID,
		tokenOwnerCanonical:    spec.tokenOwnerCanonical,
		accountEmail:           spec.accountEmail,
		paused:                 spec.paused,
		enableWebsocket:        spec.enableWebsocket,
		remoteRootDeltaCapable: spec.remoteRootDeltaCapable,
		transferWorkers:        spec.transferWorkers,
		checkWorkers:           spec.checkWorkers,
		minFreeSpace:           spec.minFreeSpace,
	}
}

func (m *mountSpec) identity() MountIdentity {
	if m == nil {
		return MountIdentity{}
	}

	return MountIdentity{
		MountID:        m.id().String(),
		ParentMountID:  m.childParentMountID().String(),
		ProjectionKind: m.projectionKind(),
		CanonicalID:    m.parentCanonicalID(),
	}
}

func (m *mountSpec) common() *mountRuntimeCommon {
	if m == nil {
		return nil
	}
	switch {
	case m.parent != nil:
		return &m.parent.common
	case m.child != nil:
		return &m.child.common
	default:
		return nil
	}
}

func (m *mountSpec) id() mountID {
	common := m.common()
	if common == nil {
		return ""
	}
	return common.mountID
}

func (m *mountSpec) projectionKind() MountProjectionKind {
	switch {
	case m != nil && m.parent != nil:
		return MountProjectionStandalone
	case m != nil && m.child != nil:
		return MountProjectionChild
	default:
		return ""
	}
}

func (m *mountSpec) selectionIndex() int {
	common := m.common()
	if common == nil {
		return 0
	}
	return common.selectionIndex
}

func (m *mountSpec) setSelectionIndex(index int) {
	common := m.common()
	if common != nil {
		common.selectionIndex = index
	}
}

func (m *mountSpec) displayName() string {
	common := m.common()
	if common == nil {
		return ""
	}
	return common.displayName
}

func (m *mountSpec) syncRoot() string {
	common := m.common()
	if common == nil {
		return ""
	}
	return common.syncRoot
}

func (m *mountSpec) statePath() string {
	common := m.common()
	if common == nil {
		return ""
	}
	return common.statePath
}

func (m *mountSpec) remoteDriveID() driveid.ID {
	common := m.common()
	if common == nil {
		return driveid.ID{}
	}
	return common.remoteDriveID
}

func (m *mountSpec) remoteRootItemID() string {
	common := m.common()
	if common == nil {
		return ""
	}
	return common.remoteRootItemID
}

func (m *mountSpec) tokenOwnerCanonical() driveid.CanonicalID {
	common := m.common()
	if common == nil {
		return driveid.CanonicalID{}
	}
	return common.tokenOwnerCanonical
}

func (m *mountSpec) accountEmail() string {
	common := m.common()
	if common == nil {
		return ""
	}
	return common.accountEmail
}

func (m *mountSpec) paused() bool {
	common := m.common()
	return common != nil && common.paused
}

func (m *mountSpec) enableWebsocket() bool {
	common := m.common()
	return common != nil && common.enableWebsocket
}

func (m *mountSpec) remoteRootDeltaCapable() bool {
	common := m.common()
	return common != nil && common.remoteRootDeltaCapable
}

func (m *mountSpec) transferWorkers() int {
	common := m.common()
	if common == nil {
		return 0
	}
	return common.transferWorkers
}

func (m *mountSpec) checkWorkers() int {
	common := m.common()
	if common == nil {
		return 0
	}
	return common.checkWorkers
}

func (m *mountSpec) minFreeSpace() int64 {
	common := m.common()
	if common == nil {
		return 0
	}
	return common.minFreeSpace
}

func (m *mountSpec) parentCanonicalID() driveid.CanonicalID {
	if m == nil || m.parent == nil {
		return driveid.CanonicalID{}
	}
	return m.parent.canonicalID
}

func (m *mountSpec) parentDriveType() string {
	if m == nil || m.parent == nil {
		return ""
	}
	return m.parent.driveType
}

func (m *mountSpec) rejectSharePointRootForms() bool {
	return m != nil && m.parent != nil && m.parent.rejectSharePointRootForms
}

func (m *mountSpec) shortcutChildWorkSink() syncengine.ShortcutChildWorkSink {
	if m == nil || m.parent == nil {
		return nil
	}
	return m.parent.childWorkSink
}

func (m *mountSpec) childParentMountID() mountID {
	if m == nil || m.child == nil {
		return ""
	}
	return m.child.parentMountID
}

func (m *mountSpec) isFinalDrainChild() bool {
	return m != nil && m.child != nil && m.child.mode == syncengine.ShortcutChildRunModeFinalDrain
}

func (m *mountSpec) expectedChildRootIdentity() *syncengine.ShortcutRootIdentity {
	if m == nil || m.child == nil {
		return nil
	}
	return m.child.engine.LocalRootIdentity
}

func (m *mountSpec) shortcutChildAckRef() syncengine.ShortcutChildAckRef {
	if m == nil || m.child == nil {
		return syncengine.ShortcutChildAckRef{}
	}
	return m.child.ackRef
}

func (m *mountSpec) shortcutChildRunCommand() *syncengine.ShortcutChildRunCommand {
	if m == nil || m.child == nil {
		return nil
	}
	return &syncengine.ShortcutChildRunCommand{
		ChildMountID: m.id().String(),
		DisplayName:  m.displayName(),
		Engine:       cloneShortcutChildEngineSpec(m.child.engine),
		Mode:         m.child.mode,
		AckRef:       m.child.ackRef,
	}
}

func cloneShortcutChildEngineSpec(spec syncengine.ShortcutChildEngineSpec) syncengine.ShortcutChildEngineSpec {
	if spec.LocalRootIdentity != nil {
		identity := *spec.LocalRootIdentity
		spec.LocalRootIdentity = &identity
	}
	return spec
}

func (m *mountSpec) label() string {
	identity := m.identity()
	return identity.Label()
}

func (m *mountSpec) syncSessionConfig() *driveops.MountSessionConfig {
	return &driveops.MountSessionConfig{
		TokenOwnerCanonical: m.tokenOwnerCanonical(),
		DriveID:             m.remoteDriveID(),
		RemoteRootItemID:    m.remoteRootItemID(),
		AccountEmail:        m.accountEmail(),
	}
}
