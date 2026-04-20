package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	catalogFileName = "catalog.json"
	catalogSchemaV1 = 1
)

// Catalog is the managed inventory authority for accounts and drives.
// User-editable config.toml continues to own only configured drive sections
// and global settings.
type Catalog struct {
	SchemaVersion int                       `json:"schema_version"`
	Accounts      map[string]CatalogAccount `json:"accounts"`
	Drives        map[string]CatalogDrive   `json:"drives"`
}

type CatalogAccount struct {
	CanonicalID           string           `json:"canonical_id"`
	Email                 string           `json:"email"`
	DriveType             string           `json:"drive_type"`
	UserID                string           `json:"user_id,omitempty"`
	DisplayName           string           `json:"display_name,omitempty"`
	OrgName               string           `json:"org_name,omitempty"`
	PrimaryDriveID        string           `json:"primary_drive_id,omitempty"`
	PrimaryDriveCanonical string           `json:"primary_drive_canonical_id,omitempty"`
	AuthRequirementReason authstate.Reason `json:"auth_requirement_reason,omitempty"`
}

type CatalogDrive struct {
	CanonicalID           string `json:"canonical_id"`
	OwnerAccountCanonical string `json:"owner_account_canonical_id,omitempty"`
	DriveType             string `json:"drive_type"`
	DisplayName           string `json:"display_name,omitempty"`
	SiteName              string `json:"site_name,omitempty"`
	LibraryName           string `json:"library_name,omitempty"`
	SharedOwnerName       string `json:"shared_owner_name,omitempty"`
	SharedOwnerEmail      string `json:"shared_owner_email,omitempty"`
	CachedAt              string `json:"cached_at,omitempty"`
	PrimaryForAccount     bool   `json:"primary_for_account,omitempty"`
	RetainedStatePresent  bool   `json:"retained_state_present,omitempty"`
	RemoteDriveID         string `json:"remote_drive_id,omitempty"`
	SiteID                string `json:"site_id,omitempty"`
}

func DefaultCatalog() *Catalog {
	return &Catalog{
		SchemaVersion: catalogSchemaV1,
		Accounts:      make(map[string]CatalogAccount),
		Drives:        make(map[string]CatalogDrive),
	}
}

func CatalogPath() string {
	return CatalogPathForDataDir(DefaultDataDir())
}

func CatalogPathForDataDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}

	return filepath.Join(dataDir, catalogFileName)
}

func LoadCatalog() (*Catalog, error) {
	return LoadCatalogForDataDir(DefaultDataDir())
}

func LoadCatalogForDataDir(dataDir string) (*Catalog, error) {
	return loadCatalogFromPath(CatalogPathForDataDir(dataDir))
}

func loadCatalogFromPath(path string) (*Catalog, error) {
	if path == "" {
		return DefaultCatalog(), nil
	}

	data, err := readManagedFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultCatalog(), nil
		}
		return nil, fmt.Errorf("reading catalog: %w", err)
	}

	var catalog Catalog
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decoding catalog: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decoding catalog: trailing data after top-level object")
		}
		return nil, fmt.Errorf("decoding catalog trailing data: %w", err)
	}
	if catalog.SchemaVersion != catalogSchemaV1 {
		return nil, fmt.Errorf("decoding catalog: unsupported schema version %d", catalog.SchemaVersion)
	}

	catalog.normalize()
	return &catalog, nil
}

func SaveCatalog(catalog *Catalog) error {
	return SaveCatalogForDataDir(DefaultDataDir(), catalog)
}

func SaveCatalogForDataDir(dataDir string, catalog *Catalog) error {
	return saveCatalogToPath(CatalogPathForDataDir(dataDir), catalog)
}

func saveCatalogToPath(path string, catalog *Catalog) error {
	if path == "" {
		return nil
	}
	if catalog == nil {
		catalog = DefaultCatalog()
	}

	catalog.normalize()
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding catalog: %w", err)
	}

	if err := atomicWriteFile(path, data); err != nil {
		return fmt.Errorf("writing catalog: %w", err)
	}

	return nil
}

func UpdateCatalog(update func(*Catalog) error) error {
	return UpdateCatalogForDataDir(DefaultDataDir(), update)
}

func UpdateCatalogForDataDir(dataDir string, update func(*Catalog) error) error {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return err
	}
	if err := update(catalog); err != nil {
		return err
	}
	return SaveCatalogForDataDir(dataDir, catalog)
}

func (c *Catalog) normalize() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = catalogSchemaV1
	}
	if c.Accounts == nil {
		c.Accounts = make(map[string]CatalogAccount)
	}
	if c.Drives == nil {
		c.Drives = make(map[string]CatalogDrive)
	}
}

func (c *Catalog) SortedAccountKeys() []string {
	keys := make([]string, 0, len(c.Accounts))
	for key := range c.Accounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (c *Catalog) SortedDriveKeys() []string {
	keys := make([]string, 0, len(c.Drives))
	for key := range c.Drives {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (c *Catalog) AccountByEmail(email string) (CatalogAccount, bool) {
	for _, key := range c.SortedAccountKeys() {
		account := c.Accounts[key]
		if account.Email == email {
			return account, true
		}
	}
	return CatalogAccount{}, false
}

func (c *Catalog) DriveByCanonicalID(cid driveid.CanonicalID) (CatalogDrive, bool) {
	if c == nil || cid.IsZero() {
		return CatalogDrive{}, false
	}
	drive, ok := c.Drives[cid.String()]
	return drive, ok
}

func (c *Catalog) AccountByCanonicalID(cid driveid.CanonicalID) (CatalogAccount, bool) {
	if c == nil || cid.IsZero() {
		return CatalogAccount{}, false
	}
	account, ok := c.Accounts[cid.String()]
	return account, ok
}

func (c *Catalog) UpsertAccount(account *CatalogAccount) {
	if account == nil {
		return
	}
	c.normalize()
	c.Accounts[account.CanonicalID] = *account
}

func (c *Catalog) UpsertDrive(drive *CatalogDrive) {
	if drive == nil {
		return
	}
	c.normalize()
	c.Drives[drive.CanonicalID] = *drive
}

func (c *Catalog) DeleteAccount(cid driveid.CanonicalID) {
	if c == nil || cid.IsZero() {
		return
	}
	delete(c.Accounts, cid.String())
}

func (c *Catalog) DeleteDrive(cid driveid.CanonicalID) {
	if c == nil || cid.IsZero() {
		return
	}
	delete(c.Drives, cid.String())
}

func (c *Catalog) DrivesForAccount(accountCID driveid.CanonicalID) []CatalogDrive {
	if c == nil || accountCID.IsZero() {
		return nil
	}
	var out []CatalogDrive
	for _, key := range c.SortedDriveKeys() {
		drive := c.Drives[key]
		if drive.OwnerAccountCanonical == accountCID.String() {
			out = append(out, drive)
		}
	}
	return out
}
