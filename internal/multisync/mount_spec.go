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
)

type mountID string

func (id mountID) String() string {
	return string(id)
}

type mountProjectionKind string

const (
	mountProjectionStandalone mountProjectionKind = "standalone"
	mountProjectionChild      mountProjectionKind = "child"
)

// StandaloneMountConfig is the CLI/config to multisync boundary for a
// configured top-level mount. Config resolution owns producing these values;
// multisync consumes them without reaching back into config-owned drive shapes.
type StandaloneMountConfig struct {
	SelectionIndex            int
	CanonicalID               driveid.CanonicalID
	DisplayName               string
	SyncRoot                  string
	StatePath                 string
	RemoteDriveID             driveid.ID
	RemoteRootItemID          string
	TokenOwnerCanonical       driveid.CanonicalID
	AccountEmail              string
	Paused                    bool
	EnableWebsocket           bool
	RootedSubtreeDeltaCapable bool
	TransferWorkers           int
	CheckWorkers              int
	MinFreeSpaceBytes         int64
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
	projectionKind            mountProjectionKind
	selectionIndex            int
	canonicalID               driveid.CanonicalID
	displayName               string
	syncRoot                  string
	statePath                 string
	remoteDriveID             driveid.ID
	remoteRootItemID          string
	tokenOwnerCanonical       driveid.CanonicalID
	accountEmail              string
	paused                    bool
	enableWebsocket           bool
	rootedSubtreeDeltaCapable bool
	transferWorkers           int
	checkWorkers              int
	minFreeSpace              int64
	localSkipDirs             []string
}

type compiledMountSet struct {
	Mounts          []*mountSpec
	Skipped         []MountStartupResult
	RemovedMountIDs []string
}

type childMountCandidate struct {
	parentMountID     mountID
	relativeLocalPath string
	record            config.MountRecord
	mount             *mountSpec
	contentRootKey    string
	skipErr           error
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
	inventory *config.MountInventory,
	logger *slog.Logger,
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	parentByID, standaloneByRoot := indexStandaloneMounts(parents)
	candidatesByParent, unmatchedChildren, err := buildChildMountCandidates(parentByID, normalizeMountInventory(inventory))
	if err != nil {
		return nil, err
	}
	conflictedChildRoots := markChildProjectionConflicts(parents, candidatesByParent, standaloneByRoot)
	finalMounts, skipped := assembleRuntimeMountSet(parents, candidatesByParent, conflictedChildRoots, unmatchedChildren, logger)

	return &compiledMountSet{
		Mounts:  finalMounts,
		Skipped: skipped,
	}, nil
}

func normalizeMountInventory(inventory *config.MountInventory) *config.MountInventory {
	if inventory == nil {
		return config.DefaultMountInventory()
	}

	return inventory
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
	inventory *config.MountInventory,
) (map[mountID][]*childMountCandidate, []config.MountRecord, error) {
	candidatesByParent := make(map[mountID][]*childMountCandidate)
	unmatchedChildren := make([]config.MountRecord, 0)
	for _, record := range sortedMountRecords(inventory) {
		parent := parentByID[mountID(record.ParentMountID)]
		if parent == nil {
			unmatchedChildren = append(unmatchedChildren, record)
			continue
		}

		candidate, err := buildChildMountCandidate(parent, record)
		if err != nil {
			return nil, nil, err
		}
		candidatesByParent[parent.mountID] = append(candidatesByParent[parent.mountID], candidate)
	}

	return candidatesByParent, unmatchedChildren, nil
}

func markChildProjectionConflicts(
	parents []*mountSpec,
	candidatesByParent map[mountID][]*childMountCandidate,
	standaloneByRoot map[string]mountID,
) map[string]struct{} {
	conflictedChildRoots := make(map[string]struct{})
	firstChildByRoot := make(map[string]*childMountCandidate)
	for _, parent := range parents {
		for _, candidate := range candidatesByParent[parent.mountID] {
			if standaloneID, found := standaloneByRoot[candidate.contentRootKey]; found {
				candidate.skipErr = fmt.Errorf(
					"content root already projected by standalone mount %s",
					standaloneID,
				)
				continue
			}
			if existing, found := firstChildByRoot[candidate.contentRootKey]; found {
				conflictedChildRoots[candidate.contentRootKey] = struct{}{}
				existing.skipErr = fmt.Errorf(
					"content root also projected by child mount %s",
					candidate.mount.mountID,
				)
				candidate.skipErr = fmt.Errorf(
					"content root also projected by child mount %s",
					existing.mount.mountID,
				)
				continue
			}

			firstChildByRoot[candidate.contentRootKey] = candidate
		}
	}

	return conflictedChildRoots
}

func assembleRuntimeMountSet(
	parents []*mountSpec,
	candidatesByParent map[mountID][]*childMountCandidate,
	conflictedChildRoots map[string]struct{},
	unmatchedChildren []config.MountRecord,
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
			if _, conflicted := conflictedChildRoots[candidate.contentRootKey]; conflicted && candidate.skipErr == nil {
				candidate.skipErr = fmt.Errorf("content root is duplicated by another child mount")
			}

			candidate.mount.selectionIndex = nextIndex
			nextIndex++
			if candidate.skipErr != nil {
				skipped = append(skipped, skippedChildMountResult(candidate, parent.mountID, logger))
				continue
			}

			finalMounts = append(finalMounts, candidate.mount)
			parent.localSkipDirs = append(parent.localSkipDirs, candidate.relativeLocalPath)
		}
	}

	skipped = append(skipped, unmatchedChildStartupResults(unmatchedChildren, nextIndex)...)
	return finalMounts, skipped
}

func skippedChildMountResult(candidate *childMountCandidate, parentID mountID, logger *slog.Logger) MountStartupResult {
	if logger != nil {
		logger.Warn("skipping child mount",
			"mount_id", candidate.mount.mountID.String(),
			"parent_mount_id", parentID.String(),
			"relative_local_path", candidate.relativeLocalPath,
			"error", candidate.skipErr,
		)
	}

	return driveStartupResultForMount(candidate.mount, candidate.skipErr)
}

func unmatchedChildStartupResults(records []config.MountRecord, startIndex int) []MountStartupResult {
	results := make([]MountStartupResult, 0, len(records))
	nextIndex := startIndex
	for _, record := range records {
		displayName := record.DisplayName
		if displayName == "" {
			displayName = path.Base(record.RelativeLocalPath)
		}
		results = append(results, MountStartupResult{
			SelectionIndex: nextIndex,
			CanonicalID:    driveid.CanonicalID{},
			DisplayName:    displayName,
			Status:         MountStartupFatal,
			Err: fmt.Errorf(
				"child mount %s references missing parent mount %s",
				record.MountID,
				record.ParentMountID,
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
		projectionKind:            mountProjectionStandalone,
		selectionIndex:            cfg.SelectionIndex,
		canonicalID:               cfg.CanonicalID,
		displayName:               cfg.DisplayName,
		syncRoot:                  cfg.SyncRoot,
		statePath:                 cfg.StatePath,
		remoteDriveID:             cfg.RemoteDriveID,
		remoteRootItemID:          cfg.RemoteRootItemID,
		tokenOwnerCanonical:       cfg.TokenOwnerCanonical,
		accountEmail:              accountEmail,
		paused:                    cfg.Paused,
		enableWebsocket:           cfg.EnableWebsocket,
		rootedSubtreeDeltaCapable: cfg.RootedSubtreeDeltaCapable,
		transferWorkers:           cfg.TransferWorkers,
		checkWorkers:              cfg.CheckWorkers,
		minFreeSpace:              cfg.MinFreeSpaceBytes,
	}, nil
}

func buildChildMountCandidate(parent *mountSpec, record config.MountRecord) (*childMountCandidate, error) {
	canonicalID, err := driveid.ConstructShared(
		parent.tokenOwnerCanonical.Email(),
		record.RemoteDriveID,
		record.RemoteRootItemID,
	)
	if err != nil {
		return nil, fmt.Errorf("multisync: constructing child canonical ID for %s: %w", record.MountID, err)
	}

	statePath := config.MountStatePath(record.MountID)
	if statePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for child mount %s", record.MountID)
	}

	displayName := record.DisplayName
	if displayName == "" {
		displayName = path.Base(record.RelativeLocalPath)
	}

	child := &mountSpec{
		mountID:                   mountID(record.MountID),
		projectionKind:            mountProjectionChild,
		canonicalID:               canonicalID,
		displayName:               displayName,
		syncRoot:                  filepath.Join(parent.syncRoot, filepath.FromSlash(record.RelativeLocalPath)),
		statePath:                 statePath,
		remoteDriveID:             driveid.New(record.RemoteDriveID),
		remoteRootItemID:          record.RemoteRootItemID,
		tokenOwnerCanonical:       parent.tokenOwnerCanonical,
		accountEmail:              parent.accountEmail,
		paused:                    parent.paused || record.Paused,
		enableWebsocket:           parent.enableWebsocket,
		rootedSubtreeDeltaCapable: parent.rootedSubtreeDeltaCapable,
		transferWorkers:           parent.transferWorkers,
		checkWorkers:              parent.checkWorkers,
		minFreeSpace:              parent.minFreeSpace,
	}

	return &childMountCandidate{
		parentMountID:     parent.mountID,
		relativeLocalPath: record.RelativeLocalPath,
		record:            record,
		mount:             child,
		contentRootKey:    child.contentRootKey(),
	}, nil
}

func sortedMountRecords(inventory *config.MountInventory) []config.MountRecord {
	if inventory == nil || len(inventory.Mounts) == 0 {
		return nil
	}

	keys := make([]string, 0, len(inventory.Mounts))
	for key := range inventory.Mounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	records := make([]config.MountRecord, 0, len(keys))
	for _, key := range keys {
		records = append(records, inventory.Mounts[key])
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].ParentMountID == records[j].ParentMountID {
			if records[i].RelativeLocalPath == records[j].RelativeLocalPath {
				return records[i].MountID < records[j].MountID
			}

			return records[i].RelativeLocalPath < records[j].RelativeLocalPath
		}

		return records[i].ParentMountID < records[j].ParentMountID
	})

	return records
}

func (m *mountSpec) contentRootKey() string {
	rootItemID := m.remoteRootItemID
	if rootItemID == "" {
		rootItemID = "root"
	}

	return fmt.Sprintf("%s|%s|%s", m.tokenOwnerCanonical.String(), m.remoteDriveID.String(), rootItemID)
}

func (m *mountSpec) syncSessionConfig() *driveops.MountSessionConfig {
	return &driveops.MountSessionConfig{
		TokenOwnerCanonical: m.tokenOwnerCanonical,
		DriveID:             m.remoteDriveID,
		RootItemID:          m.remoteRootItemID,
		AccountEmail:        m.accountEmail,
	}
}
