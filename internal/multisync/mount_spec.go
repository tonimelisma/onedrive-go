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
	localSkipDirs             []string
	localReservations         []syncengine.ManagedRootReservation
	shortcutTopologyHandler   syncengine.ShortcutTopologyHandler
}

type compiledMountSet struct {
	Mounts             []*mountSpec
	Skipped            []MountStartupResult
	FinalDrainMountIDs []string
}

type childMountCandidate struct {
	namespaceID        mountID
	relativeLocalPath  string
	reservedLocalPaths []string
	record             childTopologyRecord
	mount              *mountSpec
	contentRootKey     string
	skipErr            error
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
	topology *childMountTopology,
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	return compileRuntimeMountsForParents(parents, topology, nil)
}

func compileRuntimeMountsForParents(
	parents []*mountSpec,
	topology *childMountTopology,
	logger *slog.Logger,
) (*compiledMountSet, error) {
	parentByID, standaloneByRoot := indexStandaloneMounts(parents)
	candidatesByParent, unmatchedChildren, err := buildChildMountCandidates(parentByID, normalizeChildMountTopology(topology))
	if err != nil {
		return nil, err
	}
	conflictedChildRoots := markChildProjectionConflicts(parents, candidatesByParent, standaloneByRoot)
	finalMounts, skipped := assembleRuntimeMountSet(
		parents,
		candidatesByParent,
		conflictedChildRoots,
		unmatchedChildren,
		logger,
	)

	return &compiledMountSet{
		Mounts:             finalMounts,
		Skipped:            skipped,
		FinalDrainMountIDs: finalDrainMountIDs(topology),
	}, nil
}

func normalizeChildMountTopology(topology *childMountTopology) *childMountTopology {
	if topology == nil {
		return defaultChildMountTopology()
	}
	if topology.mounts == nil {
		topology.mounts = make(map[string]childTopologyRecord)
	}

	return topology
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
	topology *childMountTopology,
) (map[mountID][]*childMountCandidate, []childTopologyRecord, error) {
	candidatesByParent := make(map[mountID][]*childMountCandidate)
	unmatchedChildren := make([]childTopologyRecord, 0)
	records := sortedChildTopologyRecords(topology)
	for i := range records {
		record := &records[i]
		parent := parentByID[mountID(record.namespaceID)]
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
	unmatchedChildren []childTopologyRecord,
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
			parent.localSkipDirs = append(parent.localSkipDirs, candidate.reservedLocalPaths...)
			parent.localReservations = append(parent.localReservations, managedRootReservationsForCandidate(candidate)...)
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

func managedRootReservationsForCandidate(candidate *childMountCandidate) []syncengine.ManagedRootReservation {
	if candidate == nil {
		return nil
	}
	paths := append([]string{candidate.relativeLocalPath}, candidate.reservedLocalPaths...)
	reservations := make([]syncengine.ManagedRootReservation, 0, len(paths))
	for _, relPath := range paths {
		if relPath == "" {
			continue
		}
		reservation := syncengine.ManagedRootReservation{
			Path:      relPath,
			MountID:   candidate.record.mountID,
			BindingID: candidate.record.bindingItemID,
		}
		reservations = append(reservations, reservation)
	}

	return reservations
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

func unmatchedChildStartupResults(records []childTopologyRecord, startIndex int) []MountStartupResult {
	results := make([]MountStartupResult, 0, len(records))
	nextIndex := startIndex
	for i := range records {
		record := &records[i]
		displayName := record.localAlias
		if displayName == "" {
			displayName = path.Base(record.relativeLocalPath)
		}
		results = append(results, MountStartupResult{
			SelectionIndex: nextIndex,
			Identity: MountIdentity{
				MountID:        record.mountID,
				ParentMountID:  record.namespaceID,
				ProjectionKind: MountProjectionChild,
			},
			DisplayName: displayName,
			Status:      MountStartupFatal,
			Err: fmt.Errorf(
				"child mount %s references missing parent mount %s",
				record.mountID,
				record.namespaceID,
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

func buildChildMountCandidate(parent *mountSpec, record *childTopologyRecord) (*childMountCandidate, error) {
	statePath := config.MountStatePath(record.mountID)
	if statePath == "" {
		return nil, fmt.Errorf("multisync: state path is required for child mount %s", record.mountID)
	}

	displayName := record.localAlias
	if displayName == "" {
		displayName = path.Base(record.relativeLocalPath)
	}
	tokenOwner, err := driveid.NewCanonicalID(record.tokenOwnerCanonical)
	if err != nil {
		return nil, fmt.Errorf("multisync: token owner for child mount %s: %w", record.mountID, err)
	}

	child := &mountSpec{
		mountID:                mountID(record.mountID),
		parentMountID:          parent.mountID,
		bindingItemID:          record.bindingItemID,
		projectionKind:         MountProjectionChild,
		canonicalID:            driveid.CanonicalID{},
		displayName:            displayName,
		syncRoot:               filepath.Join(parent.syncRoot, filepath.FromSlash(record.relativeLocalPath)),
		statePath:              statePath,
		remoteDriveID:          driveid.New(record.remoteDriveID),
		remoteRootItemID:       record.remoteItemID,
		tokenOwnerCanonical:    tokenOwner,
		accountEmail:           tokenOwner.Email(),
		paused:                 parent.paused,
		enableWebsocket:        parent.enableWebsocket,
		remoteRootDeltaCapable: config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
		transferWorkers:        parent.transferWorkers,
		checkWorkers:           parent.checkWorkers,
		minFreeSpace:           parent.minFreeSpace,
		finalDrain: record.state == childTopologyStatePendingRemoval &&
			record.stateReason == childTopologyStateReasonShortcutRemoved,
	}
	skipErr := childTopologyStateSkipError(record)

	return &childMountCandidate{
		namespaceID:        parent.mountID,
		relativeLocalPath:  record.relativeLocalPath,
		reservedLocalPaths: append([]string(nil), record.reservedLocalPaths...),
		record:             *record,
		mount:              child,
		contentRootKey:     child.contentRootKey(),
		skipErr:            skipErr,
	}, nil
}

func childTopologyStateSkipError(record *childTopologyRecord) error {
	switch record.state {
	case "", childTopologyStateActive:
		return nil
	case childTopologyStatePendingRemoval:
		if record.stateReason == childTopologyStateReasonShortcutRemoved {
			return nil
		}
		return fmt.Errorf("child mount %s is pending removal", record.mountID)
	case childTopologyStateConflict:
		if record.stateReason != "" {
			return fmt.Errorf("child mount %s is conflicted: %s", record.mountID, record.stateReason)
		}
		return fmt.Errorf("child mount %s is conflicted", record.mountID)
	case childTopologyStateUnavailable:
		if record.stateReason != "" {
			return fmt.Errorf("child mount %s is unavailable: %s", record.mountID, record.stateReason)
		}
		return fmt.Errorf("child mount %s is unavailable", record.mountID)
	default:
		return fmt.Errorf("child mount %s has unsupported state %q", record.mountID, record.state)
	}
}

func sortedChildTopologyRecords(topology *childMountTopology) []childTopologyRecord {
	if topology == nil || len(topology.mounts) == 0 {
		return nil
	}

	keys := make([]string, 0, len(topology.mounts))
	for key := range topology.mounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	records := make([]childTopologyRecord, 0, len(keys))
	for _, key := range keys {
		records = append(records, topology.mounts[key])
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].namespaceID == records[j].namespaceID {
			if records[i].relativeLocalPath == records[j].relativeLocalPath {
				return records[i].mountID < records[j].mountID
			}

			return records[i].relativeLocalPath < records[j].relativeLocalPath
		}

		return records[i].namespaceID < records[j].namespaceID
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
