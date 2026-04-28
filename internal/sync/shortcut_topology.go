package sync

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type shortcutTopologyObservationKind string

const (
	shortcutTopologyObservationIncremental shortcutTopologyObservationKind = "incremental"
	shortcutTopologyObservationComplete    shortcutTopologyObservationKind = "complete"
)

type shortcutTopologyBatch struct {
	NamespaceID string
	Kind        shortcutTopologyObservationKind
	Upserts     []shortcutBindingUpsert
	Deletes     []shortcutBindingDelete
	Unavailable []shortcutBindingUnavailable
}

type shortcutBindingUpsert struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	Complete          bool
}

type shortcutBindingDelete struct {
	BindingItemID string
}

type shortcutBindingUnavailable struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	Reason            string
}

type ShortcutChildWorkSink func(context.Context, ShortcutChildWorkSnapshot) error

type ShortcutChildRunMode string

const (
	ShortcutChildRunModeNormal     ShortcutChildRunMode = "normal"
	ShortcutChildRunModeFinalDrain ShortcutChildRunMode = "final_drain"
)

// ShortcutChildWorkSnapshot is the parent engine's child-work output
// to multisync. Raw shortcut observations and parent-visible status facts stay
// inside internal/sync; multisync sees only child engine commands plus artifact
// cleanup requests.
type ShortcutChildWorkSnapshot struct {
	NamespaceID     string
	RunCommands     []ShortcutChildRunCommand
	CleanupCommands []ShortcutChildCleanupCommand
}

type ShortcutChildRunCommand struct {
	ChildMountID string
	DisplayName  string
	Engine       ShortcutChildEngineSpec
	Mode         ShortcutChildRunMode
	AckRef       ShortcutChildAckRef
}

type ShortcutChildEngineSpec struct {
	LocalRoot         string
	RemoteDriveID     string
	RemoteItemID      string
	LocalRootIdentity *ShortcutRootIdentity
}

func ValidateShortcutChildRunCommand(command *ShortcutChildRunCommand) (string, error) {
	if command == nil {
		return "", fmt.Errorf("sync: shortcut child run command is incomplete")
	}
	childMountID := command.ChildMountID
	if childMountID == "" {
		return "", fmt.Errorf("sync: shortcut child run command is missing a child mount ID")
	}
	if !config.IsChildMountID(childMountID) {
		return "", fmt.Errorf("sync: shortcut child run command has invalid child mount ID %s", childMountID)
	}
	if command.Engine.LocalRoot == "" {
		return "", fmt.Errorf("sync: shortcut child %s is missing a local root", childMountID)
	}
	if command.Engine.RemoteDriveID == "" || command.Engine.RemoteItemID == "" {
		return "", fmt.Errorf("sync: shortcut child %s is missing remote root identity", childMountID)
	}
	switch command.Mode {
	case ShortcutChildRunModeNormal, ShortcutChildRunModeFinalDrain:
	default:
		return "", fmt.Errorf("sync: shortcut child %s has unsupported run mode %q", childMountID, command.Mode)
	}
	if command.Mode == ShortcutChildRunModeFinalDrain && command.AckRef.IsZero() {
		return "", fmt.Errorf("sync: shortcut final-drain child %s is missing an acknowledgement reference", childMountID)
	}
	return childMountID, nil
}

func ApplyShortcutChildRunCommandToEngineMountConfig(
	command *ShortcutChildRunCommand,
	cfg *EngineMountConfig,
) error {
	if _, err := ValidateShortcutChildRunCommand(command); err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("sync: child engine mount config is required")
	}
	cfg.SyncRoot = command.Engine.LocalRoot
	cfg.DriveID = driveid.New(command.Engine.RemoteDriveID)
	cfg.RemoteRootItemID = command.Engine.RemoteItemID
	cfg.ExpectedSyncRootIdentity = cloneShortcutRootIdentity(command.Engine.LocalRootIdentity)
	return nil
}

func ShortcutChildEngineSpecsEqual(a ShortcutChildEngineSpec, b ShortcutChildEngineSpec) bool {
	return a.LocalRoot == b.LocalRoot &&
		a.RemoteDriveID == b.RemoteDriveID &&
		a.RemoteItemID == b.RemoteItemID &&
		shortcutRootIdentityPointersEqual(a.LocalRootIdentity, b.LocalRootIdentity)
}

func cloneShortcutRootIdentity(identity *ShortcutRootIdentity) *ShortcutRootIdentity {
	if identity == nil {
		return nil
	}
	cloned := *identity
	return &cloned
}

type ShortcutChildCleanupCommand struct {
	ChildMountID string
	LocalRoot    string
	Reason       ShortcutChildArtifactCleanupReason
	AckRef       ShortcutChildAckRef
}

// ShortcutChildAckHandle is the live-parent acknowledgement capability that
// multisync receives from a running parent engine. It is intentionally a value
// handle instead of an interface so control-plane code can invoke acknowledgements
// without owning or re-opening parent shortcut lifecycle state.
type ShortcutChildAckHandle struct {
	ackFinalDrain      func(context.Context, ShortcutChildDrainAck) (ShortcutChildWorkSnapshot, error)
	ackArtifactsPurged func(context.Context, ShortcutChildArtifactCleanupAck) (ShortcutChildWorkSnapshot, error)
}

func newShortcutChildAckHandle(
	ackFinalDrain func(context.Context, ShortcutChildDrainAck) (ShortcutChildWorkSnapshot, error),
	ackArtifactsPurged func(context.Context, ShortcutChildArtifactCleanupAck) (ShortcutChildWorkSnapshot, error),
) ShortcutChildAckHandle {
	return ShortcutChildAckHandle{
		ackFinalDrain:      ackFinalDrain,
		ackArtifactsPurged: ackArtifactsPurged,
	}
}

func (h ShortcutChildAckHandle) IsZero() bool {
	return h.ackFinalDrain == nil && h.ackArtifactsPurged == nil
}

func (h ShortcutChildAckHandle) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (ShortcutChildWorkSnapshot, error) {
	if h.ackFinalDrain == nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: shortcut child final-drain ack requires live parent")
	}
	return h.ackFinalDrain(ctx, ack)
}

func (h ShortcutChildAckHandle) AcknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildWorkSnapshot, error) {
	if h.ackArtifactsPurged == nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: shortcut child artifact cleanup ack requires live parent")
	}
	return h.ackArtifactsPurged(ctx, ack)
}

// ShortcutChildAckRef is the parent-issued opaque token for child completion
// acknowledgements. Multisync may carry and compare the token, but only sync
// can read the binding identity behind it.
type ShortcutChildAckRef struct {
	bindingItemID string
}

func newShortcutChildAckRef(bindingItemID string) ShortcutChildAckRef {
	return ShortcutChildAckRef{bindingItemID: bindingItemID}
}

func (r ShortcutChildAckRef) IsZero() bool {
	return r.bindingItemID == ""
}

// ShortcutRootIdentity is the parent-engine-issued local directory identity
// token for a managed shortcut root. Control-plane code carries this value as
// child engine input, but only sync opens the filesystem and decides whether it
// still matches.
type ShortcutRootIdentity struct {
	Device uint64
	Inode  uint64
}

func sameShortcutRootIdentity(a ShortcutRootIdentity, b ShortcutRootIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode
}

func shortcutRootIdentityFromFileIdentity(identity *synctree.FileIdentity) *ShortcutRootIdentity {
	if identity == nil {
		return nil
	}
	return &ShortcutRootIdentity{
		Device: identity.Device,
		Inode:  identity.Inode,
	}
}

func shortcutRootIdentityToFileIdentity(identity *ShortcutRootIdentity) *synctree.FileIdentity {
	if identity == nil {
		return nil
	}
	return &synctree.FileIdentity{
		Device: identity.Device,
		Inode:  identity.Inode,
	}
}

type ShortcutChildArtifactCleanupReason string

const (
	ShortcutChildArtifactCleanupParentRemoved ShortcutChildArtifactCleanupReason = "parent_removed"
)

type ShortcutChildDrainAck struct {
	Ref ShortcutChildAckRef
}

type ShortcutChildArtifactCleanupAck struct {
	Ref ShortcutChildAckRef
}

func NormalizeShortcutChildWorkSnapshot(
	namespaceID string,
	snapshot ShortcutChildWorkSnapshot,
) ShortcutChildWorkSnapshot {
	if snapshot.NamespaceID == "" {
		snapshot.NamespaceID = namespaceID
	}
	snapshot.RunCommands = cloneShortcutChildRunCommands(snapshot.RunCommands)
	snapshot.CleanupCommands = append([]ShortcutChildCleanupCommand(nil), snapshot.CleanupCommands...)
	if len(snapshot.RunCommands) == 0 {
		snapshot.RunCommands = nil
	}
	if len(snapshot.CleanupCommands) == 0 {
		snapshot.CleanupCommands = nil
	}
	slices.SortFunc(snapshot.RunCommands, func(a, b ShortcutChildRunCommand) int {
		if byMount := cmp.Compare(a.ChildMountID, b.ChildMountID); byMount != 0 {
			return byMount
		}
		return cmp.Compare(a.DisplayName, b.DisplayName)
	})
	slices.SortFunc(snapshot.CleanupCommands, func(a, b ShortcutChildCleanupCommand) int {
		if byMount := cmp.Compare(a.ChildMountID, b.ChildMountID); byMount != 0 {
			return byMount
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	return snapshot
}

func ShortcutChildWorkSnapshotsEqual(
	a ShortcutChildWorkSnapshot,
	b ShortcutChildWorkSnapshot,
) bool {
	if a.NamespaceID != b.NamespaceID ||
		len(a.RunCommands) != len(b.RunCommands) ||
		len(a.CleanupCommands) != len(b.CleanupCommands) {
		return false
	}
	for i := range a.RunCommands {
		if !shortcutChildRunCommandEqual(&a.RunCommands[i], &b.RunCommands[i]) {
			return false
		}
	}
	for i := range a.CleanupCommands {
		if a.CleanupCommands[i] != b.CleanupCommands[i] {
			return false
		}
	}
	return true
}

func cloneShortcutChildRunCommands(commands []ShortcutChildRunCommand) []ShortcutChildRunCommand {
	cloned := append([]ShortcutChildRunCommand(nil), commands...)
	for i := range cloned {
		if cloned[i].Engine.LocalRootIdentity != nil {
			identity := *cloned[i].Engine.LocalRootIdentity
			cloned[i].Engine.LocalRootIdentity = &identity
		}
	}
	return cloned
}

func shortcutChildRunCommandEqual(a *ShortcutChildRunCommand, b *ShortcutChildRunCommand) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if shortcutChildRunCommandComparableFor(a) != shortcutChildRunCommandComparableFor(b) {
		return false
	}
	return shortcutRootIdentityPointersEqual(a.Engine.LocalRootIdentity, b.Engine.LocalRootIdentity)
}

type shortcutChildRunCommandComparable struct {
	ChildMountID  string
	DisplayName   string
	LocalRoot     string
	RemoteDriveID string
	RemoteItemID  string
	Mode          ShortcutChildRunMode
	AckRef        ShortcutChildAckRef
}

func shortcutChildRunCommandComparableFor(command *ShortcutChildRunCommand) shortcutChildRunCommandComparable {
	if command == nil {
		return shortcutChildRunCommandComparable{}
	}
	return shortcutChildRunCommandComparable{
		ChildMountID:  command.ChildMountID,
		DisplayName:   command.DisplayName,
		LocalRoot:     command.Engine.LocalRoot,
		RemoteDriveID: command.Engine.RemoteDriveID,
		RemoteItemID:  command.Engine.RemoteItemID,
		Mode:          command.Mode,
		AckRef:        command.AckRef,
	}
}

func shortcutRootIdentityPointersEqual(a *ShortcutRootIdentity, b *ShortcutRootIdentity) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return sameShortcutRootIdentity(*a, *b)
	}
}

func (b shortcutTopologyBatch) hasFacts() bool {
	return len(b.Upserts) > 0 || len(b.Deletes) > 0 || len(b.Unavailable) > 0
}

func (b shortcutTopologyBatch) shouldApply() bool {
	return b.hasFacts() || b.Kind == shortcutTopologyObservationComplete
}

func (b *shortcutTopologyBatch) appendUpsert(fact shortcutBindingUpsert) {
	if fact.BindingItemID == "" {
		return
	}
	for i := range b.Upserts {
		if b.Upserts[i].BindingItemID == fact.BindingItemID {
			b.Upserts[i] = fact
			return
		}
	}
	b.removeUnavailable(fact.BindingItemID)
	b.removeDelete(fact.BindingItemID)
	b.Upserts = append(b.Upserts, fact)
}

func (b *shortcutTopologyBatch) appendDelete(fact shortcutBindingDelete) {
	if fact.BindingItemID == "" {
		return
	}
	b.removeUpsert(fact.BindingItemID)
	b.removeUnavailable(fact.BindingItemID)
	if slices.ContainsFunc(b.Deletes, func(existing shortcutBindingDelete) bool {
		return existing.BindingItemID == fact.BindingItemID
	}) {
		return
	}
	b.Deletes = append(b.Deletes, fact)
}

func (b *shortcutTopologyBatch) appendUnavailable(fact shortcutBindingUnavailable) {
	if fact.BindingItemID == "" {
		return
	}
	for i := range b.Unavailable {
		if b.Unavailable[i].BindingItemID == fact.BindingItemID {
			b.Unavailable[i] = fact
			return
		}
	}
	b.removeUpsert(fact.BindingItemID)
	b.removeDelete(fact.BindingItemID)
	b.Unavailable = append(b.Unavailable, fact)
}

func (b *shortcutTopologyBatch) removeUpsert(bindingItemID string) {
	b.Upserts = slices.DeleteFunc(b.Upserts, func(existing shortcutBindingUpsert) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func (b *shortcutTopologyBatch) removeDelete(bindingItemID string) {
	b.Deletes = slices.DeleteFunc(b.Deletes, func(existing shortcutBindingDelete) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func (b *shortcutTopologyBatch) removeUnavailable(bindingItemID string) {
	b.Unavailable = slices.DeleteFunc(b.Unavailable, func(existing shortcutBindingUnavailable) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func protectedRootByBinding(protectedRoots []ProtectedRoot) map[string]ProtectedRoot {
	byBinding := make(map[string]ProtectedRoot, len(protectedRoots))
	for i := range protectedRoots {
		protectedRoot := protectedRoots[i]
		if protectedRoot.BindingID == "" {
			continue
		}
		byBinding[protectedRoot.BindingID] = protectedRoot
	}
	return byBinding
}

func protectedRootPrimaryName(relPath string) string {
	if relPath == "" {
		return ""
	}
	return path.Base(relPath)
}
