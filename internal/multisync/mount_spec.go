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
	localSkipDirs             []string
}

type compiledMountSet struct {
	Mounts          []*mountSpec
	Skipped         []MountStartupResult
	RemovedMountIDs []string
}

type childMountCandidate struct {
	namespaceID       mountID
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
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	return compileRuntimeMountsForParents(parents, inventory, nil)
}

func compileRuntimeMountsForParents(
	parents []*mountSpec,
	inventory *config.MountInventory,
	logger *slog.Logger,
) (*compiledMountSet, error) {
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
	records := sortedMountRecords(inventory)
	for i := range records {
		record := &records[i]
		if record.State == config.MountStatePendingRemoval {
			// Pending-removal records only drive cleanup; they no longer own a
			// child runner or reserve the parent subtree during runtime compile.
			continue
		}
		parent := parentByID[mountID(record.NamespaceID)]
		if parent == nil {
			unmatchedChildren = append(unmatchedChildren, *record)
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
			conflictKey := string(candidate.namespaceID) + "|" + candidate.contentRootKey
			if existing, found := firstChildByRoot[conflictKey]; found {
				conflictedChildRoots[conflictKey] = struct{}{}
				candidate.skipErr = fmt.Errorf(
					"content root also projected by child mount %s",
					existing.mount.mountID,
				)
				continue
			}

			firstChildByRoot[conflictKey] = candidate
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
			conflictKey := string(candidate.namespaceID) + "|" + candidate.contentRootKey
			if _, conflicted := conflictedChildRoots[conflictKey]; conflicted && candidate.skipErr == nil {
				candidate.skipErr = fmt.Errorf("content root is duplicated by another child mount")
			}

			candidate.mount.selectionIndex = nextIndex
			nextIndex++
			parent.localSkipDirs = append(parent.localSkipDirs, candidate.relativeLocalPath)
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

func unmatchedChildStartupResults(records []config.MountRecord, startIndex int) []MountStartupResult {
	results := make([]MountStartupResult, 0, len(records))
	nextIndex := startIndex
	for i := range records {
		record := &records[i]
		displayName := record.LocalAlias
		if displayName == "" {
			displayName = path.Base(record.RelativeLocalPath)
		}
		results = append(results, MountStartupResult{
			SelectionIndex: nextIndex,
			Identity: MountIdentity{
				MountID:        record.MountID,
				ParentMountID:  record.NamespaceID,
				ProjectionKind: MountProjectionChild,
			},
			DisplayName: displayName,
			Status:      MountStartupFatal,
			Err: fmt.Errorf(
				"child mount %s references missing parent mount %s",
				record.MountID,
				record.NamespaceID,
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

func buildChildMountCandidate(parent *mountSpec, record *config.MountRecord) (*childMountCandidate, error) {
	statePath := config.MountStatePath(record.MountID)
	if statePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for child mount %s", record.MountID)
	}

	displayName := record.LocalAlias
	if displayName == "" {
		displayName = path.Base(record.RelativeLocalPath)
	}
	tokenOwner, err := driveid.NewCanonicalID(record.TokenOwnerCanonical)
	if err != nil {
		return nil, fmt.Errorf("multisync: token owner for child mount %s: %w", record.MountID, err)
	}

	child := &mountSpec{
		mountID:                mountID(record.MountID),
		parentMountID:          parent.mountID,
		projectionKind:         MountProjectionChild,
		canonicalID:            driveid.CanonicalID{},
		displayName:            displayName,
		syncRoot:               filepath.Join(parent.syncRoot, filepath.FromSlash(record.RelativeLocalPath)),
		statePath:              statePath,
		remoteDriveID:          driveid.New(record.RemoteDriveID),
		remoteRootItemID:       record.RemoteItemID,
		tokenOwnerCanonical:    tokenOwner,
		accountEmail:           tokenOwner.Email(),
		paused:                 parent.paused || record.Paused,
		enableWebsocket:        parent.enableWebsocket,
		remoteRootDeltaCapable: config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
		transferWorkers:        parent.transferWorkers,
		checkWorkers:           parent.checkWorkers,
		minFreeSpace:           parent.minFreeSpace,
	}
	skipErr := childMountStateSkipError(record)

	return &childMountCandidate{
		namespaceID:       parent.mountID,
		relativeLocalPath: record.RelativeLocalPath,
		record:            *record,
		mount:             child,
		contentRootKey:    child.contentRootKey(),
		skipErr:           skipErr,
	}, nil
}

func childMountStateSkipError(record *config.MountRecord) error {
	switch record.State {
	case "", config.MountStateActive:
		return nil
	case config.MountStatePendingRemoval:
		return fmt.Errorf("child mount %s is pending removal", record.MountID)
	case config.MountStateConflict:
		if record.StateReason != "" {
			return fmt.Errorf("child mount %s is conflicted: %s", record.MountID, record.StateReason)
		}
		return fmt.Errorf("child mount %s is conflicted", record.MountID)
	case config.MountStateUnavailable:
		if record.StateReason != "" {
			return fmt.Errorf("child mount %s is unavailable: %s", record.MountID, record.StateReason)
		}
		return fmt.Errorf("child mount %s is unavailable", record.MountID)
	default:
		return fmt.Errorf("child mount %s has unsupported state %q", record.MountID, record.State)
	}
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
		if records[i].NamespaceID == records[j].NamespaceID {
			if records[i].RelativeLocalPath == records[j].RelativeLocalPath {
				return records[i].MountID < records[j].MountID
			}

			return records[i].RelativeLocalPath < records[j].RelativeLocalPath
		}

		return records[i].NamespaceID < records[j].NamespaceID
	})

	return records
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
