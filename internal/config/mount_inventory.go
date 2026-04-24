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

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

const (
	mountsFileName   = "mounts.json"
	mountsSchemaV1   = 1
	mountsSchemaV2   = 2
	stateMountPrefix = "state_mount_"
)

type DiscoveryMode string

const (
	DiscoveryModeDelta     DiscoveryMode = "delta"
	DiscoveryModeEnumerate DiscoveryMode = "enumerate"
)

// MountInventory is the managed durable authority for local child-mount
// bindings. Catalog keeps account/drive discovery state; mount inventory keeps
// local projection ownership.
type MountInventory struct {
	SchemaVersion int                             `json:"schema_version"`
	Parents       map[string]ParentDiscoveryState `json:"parents,omitempty"`
	Mounts        map[string]MountRecord          `json:"mounts"`
}

// ParentDiscoveryState is the durable discovery cursor/state for one selected
// standalone parent mount that owns automatic child shortcut reconciliation.
type ParentDiscoveryState struct {
	ParentMountID string        `json:"parent_mount_id"`
	DeltaLink     string        `json:"delta_link,omitempty"`
	DiscoveryMode DiscoveryMode `json:"discovery_mode,omitempty"`
}

// MountRecord describes one child mount projected inside a selected parent
// sync root. The record is binding-owned: the local shortcut placeholder item
// ID is the durable identity, while the remote content root may change.
type MountRecord struct {
	MountID           string `json:"mount_id"`
	ParentMountID     string `json:"parent_mount_id"`
	BindingItemID     string `json:"binding_item_id"`
	DisplayName       string `json:"display_name,omitempty"`
	RelativeLocalPath string `json:"relative_local_path"`
	RemoteDriveID     string `json:"remote_drive_id"`
	RemoteRootItemID  string `json:"remote_root_item_id"`
	Paused            bool   `json:"paused,omitempty"`
}

func DefaultMountInventory() *MountInventory {
	return &MountInventory{
		SchemaVersion: mountsSchemaV2,
		Parents:       make(map[string]ParentDiscoveryState),
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
	switch inventory.SchemaVersion {
	case mountsSchemaV2:
	case mountsSchemaV1:
		if err := moveAsideLegacyMountInventory(path); err != nil {
			return nil, err
		}
		return DefaultMountInventory(), nil
	default:
		return nil, fmt.Errorf("decoding mount inventory: unsupported schema version %d", inventory.SchemaVersion)
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
		i.SchemaVersion = mountsSchemaV2
	}
	if i.Parents == nil {
		i.Parents = make(map[string]ParentDiscoveryState)
	}
	if i.Mounts == nil {
		i.Mounts = make(map[string]MountRecord)
	}

	for key, parent := range i.Parents {
		if parent.ParentMountID == "" {
			parent.ParentMountID = key
		}
		if err := validateParentDiscoveryState(parent, key); err != nil {
			return err
		}
		i.Parents[parent.ParentMountID] = parent
		if parent.ParentMountID != key {
			delete(i.Parents, key)
		}
	}

	for key, record := range i.Mounts {
		if record.MountID == "" {
			record.MountID = key
		}
		normalized, err := normalizeMountRelativePath(record.RelativeLocalPath)
		if err != nil {
			return fmt.Errorf("validating mount %q relative_local_path: %w", record.MountID, err)
		}
		record.RelativeLocalPath = normalized
		if err := validateMountRecord(record, key); err != nil {
			return err
		}
		i.Mounts[record.MountID] = record
		if record.MountID != key {
			delete(i.Mounts, key)
		}
	}

	if err := validateSiblingMountPaths(i.Mounts); err != nil {
		return err
	}

	return nil
}

func validateMountRecord(record MountRecord, key string) error {
	if record.MountID == "" {
		return fmt.Errorf("validating mount inventory: mount key %q has empty mount_id", key)
	}
	if record.MountID != key {
		return fmt.Errorf("validating mount inventory: mount key %q does not match mount_id %q", key, record.MountID)
	}
	if record.ParentMountID == "" {
		return fmt.Errorf("validating mount %q: parent_mount_id is required", record.MountID)
	}
	if record.BindingItemID == "" {
		return fmt.Errorf("validating mount %q: binding_item_id is required", record.MountID)
	}
	if record.RelativeLocalPath == "" {
		return fmt.Errorf("validating mount %q: relative_local_path is required", record.MountID)
	}
	if record.RemoteDriveID == "" {
		return fmt.Errorf("validating mount %q: remote_drive_id is required", record.MountID)
	}
	if record.RemoteRootItemID == "" {
		return fmt.Errorf("validating mount %q: remote_root_item_id is required", record.MountID)
	}

	return nil
}

func validateParentDiscoveryState(parent ParentDiscoveryState, key string) error {
	if parent.ParentMountID == "" {
		return fmt.Errorf("validating parent discovery state %q: parent_mount_id is required", key)
	}
	switch parent.DiscoveryMode {
	case "", DiscoveryModeDelta, DiscoveryModeEnumerate:
		return nil
	default:
		return fmt.Errorf(
			"validating parent discovery state %q: unsupported discovery_mode %q",
			parent.ParentMountID,
			parent.DiscoveryMode,
		)
	}
}

func validateSiblingMountPaths(mounts map[string]MountRecord) error {
	type siblingMount struct {
		mountID string
		path    string
	}

	byParent := make(map[string][]siblingMount)
	for _, record := range mounts {
		byParent[record.ParentMountID] = append(byParent[record.ParentMountID], siblingMount{
			mountID: record.MountID,
			path:    record.RelativeLocalPath,
		})
	}

	for parentID, siblings := range byParent {
		sort.Slice(siblings, func(i, j int) bool {
			return siblings[i].path < siblings[j].path
		})
		for i := 0; i < len(siblings); i++ {
			for j := i + 1; j < len(siblings); j++ {
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

func moveAsideLegacyMountInventory(path string) error {
	backupPath := path + ".v1.bak"
	if err := removeManagedPathIfExists(backupPath); err != nil {
		return fmt.Errorf("preparing legacy mount inventory backup: %w", err)
	}

	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening managed root for legacy mount inventory %s: %w", path, err)
	}
	if err := root.Rename(name, filepath.Base(backupPath)); err != nil {
		return fmt.Errorf("moving legacy mount inventory aside: %w", err)
	}

	return nil
}

func removeManagedPathIfExists(path string) error {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening managed root for %s: %w", path, err)
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing managed path %s: %w", path, err)
	}

	return nil
}
