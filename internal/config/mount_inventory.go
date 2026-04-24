package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	mountsFileName   = "mounts.json"
	mountsSchemaV4   = 4
	stateMountPrefix = "state_mount_"
)

type DiscoveryMode string

const (
	DiscoveryModeDelta     DiscoveryMode = "delta"
	DiscoveryModeEnumerate DiscoveryMode = "enumerate"
)

type MountState string

const (
	MountStateActive         MountState = "active"
	MountStatePendingRemoval MountState = "pending_removal"
	MountStateConflict       MountState = "conflict"
	MountStateUnavailable    MountState = "unavailable"
)

const (
	MountStateReasonDuplicateContentRoot          = "duplicate_content_root"
	MountStateReasonExplicitStandaloneContentRoot = "explicit_standalone_content_root"
	MountStateReasonShortcutRemoved               = "shortcut_removed"
	MountStateReasonShortcutBindingUnavailable    = "shortcut_binding_unavailable"
	MountStateReasonLocalProjectionConflict       = "local_projection_conflict"
	MountStateReasonLocalProjectionUnavailable    = "local_projection_unavailable"
)

// MountInventory is the managed durable authority for local child-mount
// bindings. Catalog keeps account/drive discovery state; mount inventory keeps
// local projection ownership.
type MountInventory struct {
	SchemaVersion int                                `json:"schema_version"`
	Namespaces    map[string]NamespaceDiscoveryState `json:"namespaces,omitempty"`
	Mounts        map[string]MountRecord             `json:"mounts"`
}

// NamespaceDiscoveryState is the durable discovery cursor/state for one
// selected standalone namespace mount that owns automatic child shortcut
// reconciliation.
type NamespaceDiscoveryState struct {
	NamespaceID   string        `json:"namespace_id"`
	DeltaLink     string        `json:"delta_link,omitempty"`
	DiscoveryMode DiscoveryMode `json:"discovery_mode,omitempty"`
}

// MountRecord describes one child mount projected inside a selected parent
// sync root. The record is binding-owned: the local shortcut placeholder item
// ID is the durable identity, while the remote content root may change.
type MountRecord struct {
	MountID             string     `json:"mount_id"`
	NamespaceID         string     `json:"namespace_id"`
	BindingItemID       string     `json:"binding_item_id"`
	LocalAlias          string     `json:"local_alias,omitempty"`
	RelativeLocalPath   string     `json:"relative_local_path"`
	ReservedLocalPaths  []string   `json:"reserved_local_paths,omitempty"`
	TokenOwnerCanonical string     `json:"token_owner_canonical"`
	RemoteDriveID       string     `json:"remote_drive_id"`
	RemoteItemID        string     `json:"remote_item_id"`
	State               MountState `json:"state,omitempty"`
	StateReason         string     `json:"state_reason,omitempty"`
	Paused              bool       `json:"paused,omitempty"`
}

func DefaultMountInventory() *MountInventory {
	return &MountInventory{
		SchemaVersion: mountsSchemaV4,
		Namespaces:    make(map[string]NamespaceDiscoveryState),
		Mounts:        make(map[string]MountRecord),
	}
}

func MountsPath() string {
	return MountsPathForDataDir(DefaultDataDir())
}

func MountsPathForDataDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}

	return filepath.Join(dataDir, mountsFileName)
}

func LoadMountInventory() (*MountInventory, error) {
	return LoadMountInventoryForDataDir(DefaultDataDir())
}

func LoadMountInventoryForDataDir(dataDir string) (*MountInventory, error) {
	return loadMountInventoryFromPath(MountsPathForDataDir(dataDir))
}

func loadMountInventoryFromPath(path string) (*MountInventory, error) {
	if path == "" {
		return DefaultMountInventory(), nil
	}

	data, err := readManagedFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultMountInventory(), nil
		}

		return nil, fmt.Errorf("reading mount inventory: %w", err)
	}

	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decoding mount inventory: %w", err)
	}
	if header.SchemaVersion != mountsSchemaV4 {
		return nil, fmt.Errorf("decoding mount inventory: unsupported schema version %d", header.SchemaVersion)
	}

	var inventory MountInventory
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return nil, fmt.Errorf("decoding mount inventory: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decoding mount inventory: trailing data after top-level object")
		}

		return nil, fmt.Errorf("decoding mount inventory trailing data: %w", err)
	}

	if err := inventory.normalizeAndValidate(); err != nil {
		return nil, err
	}

	return &inventory, nil
}

func SaveMountInventory(inventory *MountInventory) error {
	return SaveMountInventoryForDataDir(DefaultDataDir(), inventory)
}

func SaveMountInventoryForDataDir(dataDir string, inventory *MountInventory) error {
	return saveMountInventoryToPath(MountsPathForDataDir(dataDir), inventory)
}

func saveMountInventoryToPath(path string, inventory *MountInventory) error {
	if path == "" {
		return nil
	}
	if inventory == nil {
		inventory = DefaultMountInventory()
	}

	if err := inventory.normalizeAndValidate(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding mount inventory: %w", err)
	}

	if err := atomicWriteFile(path, data); err != nil {
		return fmt.Errorf("writing mount inventory: %w", err)
	}

	return nil
}

func UpdateMountInventory(update func(*MountInventory) error) error {
	return UpdateMountInventoryForDataDir(DefaultDataDir(), update)
}

func UpdateMountInventoryForDataDir(dataDir string, update func(*MountInventory) error) error {
	inventory, err := LoadMountInventoryForDataDir(dataDir)
	if err != nil {
		return err
	}
	if err := update(inventory); err != nil {
		return err
	}

	return SaveMountInventoryForDataDir(dataDir, inventory)
}

func (i *MountInventory) normalizeAndValidate() error {
	if i == nil {
		return nil
	}
	if i.SchemaVersion == 0 {
		i.SchemaVersion = mountsSchemaV4
	}
	if i.SchemaVersion != mountsSchemaV4 {
		return fmt.Errorf("validating mount inventory: unsupported schema version %d", i.SchemaVersion)
	}
	if i.Namespaces == nil {
		i.Namespaces = make(map[string]NamespaceDiscoveryState)
	}
	if i.Mounts == nil {
		i.Mounts = make(map[string]MountRecord)
	}

	if err := i.normalizeAndValidateNamespaces(); err != nil {
		return err
	}
	if err := i.normalizeAndValidateMounts(); err != nil {
		return err
	}
	if err := validateSiblingMountPaths(i.Mounts); err != nil {
		return err
	}

	return nil
}

func (i *MountInventory) normalizeAndValidateNamespaces() error {
	for key, namespace := range i.Namespaces {
		if namespace.NamespaceID == "" {
			namespace.NamespaceID = key
		}
		if err := validateNamespaceDiscoveryState(namespace, key); err != nil {
			return err
		}
		i.Namespaces[namespace.NamespaceID] = namespace
		if namespace.NamespaceID != key {
			delete(i.Namespaces, key)
		}
	}

	return nil
}

func (i *MountInventory) normalizeAndValidateMounts() error {
	for key := range i.Mounts {
		record := i.Mounts[key]
		if record.MountID == "" {
			record.MountID = key
		}
		if record.State == "" {
			record.State = MountStateActive
		}
		normalized, err := normalizeMountRelativePath(record.RelativeLocalPath)
		if err != nil {
			return fmt.Errorf("validating mount %q relative_local_path: %w", record.MountID, err)
		}
		record.RelativeLocalPath = normalized
		record.ReservedLocalPaths, err = normalizeReservedLocalPaths(record.RelativeLocalPath, record.ReservedLocalPaths)
		if err != nil {
			return fmt.Errorf("validating mount %q reserved_local_paths: %w", record.MountID, err)
		}
		if err := validateMountRecord(&record, key); err != nil {
			return err
		}
		i.Mounts[record.MountID] = record
		if record.MountID != key {
			delete(i.Mounts, key)
		}
	}

	return nil
}

func validateMountRecord(record *MountRecord, key string) error {
	if record.MountID == "" {
		return fmt.Errorf("validating mount inventory: mount key %q has empty mount_id", key)
	}
	if record.MountID != key {
		return fmt.Errorf("validating mount inventory: mount key %q does not match mount_id %q", key, record.MountID)
	}
	if record.NamespaceID == "" {
		return fmt.Errorf("validating mount %q: namespace_id is required", record.MountID)
	}
	if record.BindingItemID == "" {
		return fmt.Errorf("validating mount %q: binding_item_id is required", record.MountID)
	}
	if record.RelativeLocalPath == "" {
		return fmt.Errorf("validating mount %q: relative_local_path is required", record.MountID)
	}
	if record.TokenOwnerCanonical == "" {
		return fmt.Errorf("validating mount %q: token_owner_canonical is required", record.MountID)
	}
	if _, err := driveid.NewCanonicalID(record.TokenOwnerCanonical); err != nil {
		return fmt.Errorf("validating mount %q token_owner_canonical: %w", record.MountID, err)
	}
	switch record.State {
	case MountStateActive, MountStatePendingRemoval, MountStateConflict, MountStateUnavailable:
	default:
		return fmt.Errorf("validating mount %q: unsupported state %q", record.MountID, record.State)
	}
	if err := validateMountStateReason(record.State, record.StateReason); err != nil {
		return fmt.Errorf("validating mount %q: %w", record.MountID, err)
	}
	if requiresRemoteContentIdentity(record.State, record.StateReason) {
		if record.RemoteDriveID == "" {
			return fmt.Errorf("validating mount %q: remote_drive_id is required", record.MountID)
		}
		if record.RemoteItemID == "" {
			return fmt.Errorf("validating mount %q: remote_item_id is required", record.MountID)
		}
	}

	return nil
}

func validateMountStateReason(state MountState, reason string) error {
	if reason == "" {
		return nil
	}

	switch reason {
	case MountStateReasonDuplicateContentRoot,
		MountStateReasonExplicitStandaloneContentRoot,
		MountStateReasonShortcutRemoved,
		MountStateReasonShortcutBindingUnavailable,
		MountStateReasonLocalProjectionConflict,
		MountStateReasonLocalProjectionUnavailable:
	default:
		return fmt.Errorf("unsupported state_reason %q", reason)
	}

	switch reason {
	case MountStateReasonDuplicateContentRoot,
		MountStateReasonExplicitStandaloneContentRoot,
		MountStateReasonLocalProjectionConflict:
		if state != MountStateConflict {
			return fmt.Errorf("state_reason %q requires state %q", reason, MountStateConflict)
		}
	case MountStateReasonShortcutRemoved:
		if state != MountStatePendingRemoval {
			return fmt.Errorf("state_reason %q requires state %q", reason, MountStatePendingRemoval)
		}
	case MountStateReasonShortcutBindingUnavailable,
		MountStateReasonLocalProjectionUnavailable:
		if state != MountStateUnavailable {
			return fmt.Errorf("state_reason %q requires state %q", reason, MountStateUnavailable)
		}
	}

	return nil
}

func requiresRemoteContentIdentity(state MountState, reason string) bool {
	return state != MountStateUnavailable || reason != MountStateReasonShortcutBindingUnavailable
}

func validateNamespaceDiscoveryState(namespace NamespaceDiscoveryState, key string) error {
	if namespace.NamespaceID == "" {
		return fmt.Errorf("validating namespace discovery state %q: namespace_id is required", key)
	}
	switch namespace.DiscoveryMode {
	case "", DiscoveryModeDelta, DiscoveryModeEnumerate:
		return nil
	default:
		return fmt.Errorf(
			"validating namespace discovery state %q: unsupported discovery_mode %q",
			namespace.NamespaceID,
			namespace.DiscoveryMode,
		)
	}
}

func validateSiblingMountPaths(mounts map[string]MountRecord) error {
	type siblingMount struct {
		mountID string
		path    string
	}

	byParent := make(map[string][]siblingMount)
	for mountID := range mounts {
		record := mounts[mountID]
		paths := append([]string{record.RelativeLocalPath}, record.ReservedLocalPaths...)
		for _, path := range paths {
			byParent[record.NamespaceID] = append(byParent[record.NamespaceID], siblingMount{
				mountID: record.MountID,
				path:    path,
			})
		}
	}

	for parentID, siblings := range byParent {
		sort.Slice(siblings, func(i, j int) bool {
			return siblings[i].path < siblings[j].path
		})
		for i := 0; i < len(siblings); i++ {
			for j := i + 1; j < len(siblings); j++ {
				if siblings[i].mountID == siblings[j].mountID {
					continue
				}
				if siblings[i].path == siblings[j].path {
					return fmt.Errorf(
						"validating mount inventory: parent %q has duplicate child path %q (%s, %s)",
						parentID,
						siblings[i].path,
						siblings[i].mountID,
						siblings[j].mountID,
					)
				}
				if slashAncestorOrDescendant(siblings[i].path, siblings[j].path) {
					return fmt.Errorf(
						"validating mount inventory: parent %q has nested child paths %q and %q (%s, %s)",
						parentID,
						siblings[i].path,
						siblings[j].path,
						siblings[i].mountID,
						siblings[j].mountID,
					)
				}
			}
		}
	}

	return nil
}

func normalizeMountRelativePath(raw string) (string, error) {
	normalized := filepath.ToSlash(strings.TrimSpace(raw))
	if normalized == "" {
		return "", fmt.Errorf("must be non-empty")
	}
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("must be relative, got %q", raw)
	}

	cleaned := path.Clean(normalized)
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("must not be current directory")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("must not escape parent directories")
	}

	return cleaned, nil
}

func normalizeReservedLocalPaths(relativeLocalPath string, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(raw))
	normalized := make([]string, 0, len(raw))
	for _, value := range raw {
		path, err := normalizeMountRelativePath(value)
		if err != nil {
			return nil, err
		}
		if path == relativeLocalPath {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}
	sort.Strings(normalized)
	if len(normalized) == 0 {
		return nil, nil
	}

	return normalized, nil
}

func slashAncestorOrDescendant(a, b string) bool {
	aSlash := strings.TrimSuffix(a, "/") + "/"
	bSlash := strings.TrimSuffix(b, "/") + "/"

	return strings.HasPrefix(bSlash, aSlash) || strings.HasPrefix(aSlash, bSlash)
}

func MountStatePath(mountID string) string {
	dataDir := DefaultDataDir()
	if dataDir == "" || strings.TrimSpace(mountID) == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(mountID))
	encodedDigest := base64.RawURLEncoding.EncodeToString(sum[:])
	return filepath.Join(dataDir, stateMountPrefix+encodedDigest+".db")
}

func ChildMountID(parentMountID, bindingItemID string) string {
	parent := strings.TrimSpace(parentMountID)
	binding := strings.TrimSpace(bindingItemID)
	if parent == "" || binding == "" {
		return ""
	}

	return parent + "|binding:" + binding
}
