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
	RemoteDriveID         string `json:"remote_drive_id,omitempty"`
	SiteID                string `json:"site_id,omitempty"`
}

// ID returns the account's own canonical ID, and whether the record holds one.
func (a *CatalogAccount) ID() (driveid.CanonicalID, bool) {
	return parseCatalogID(a.CanonicalID)
}

// PrimaryDrive returns the canonical ID of the account's primary drive, and
// whether the record names one.
func (a *CatalogAccount) PrimaryDrive() (driveid.CanonicalID, bool) {
	return parseCatalogID(a.PrimaryDriveCanonical)
}

// Owner returns the canonical ID of the account the drive's record names as
// its owner, and whether it names one.
func (d *CatalogDrive) Owner() (driveid.CanonicalID, bool) {
	return parseCatalogID(d.OwnerAccountCanonical)
}

// parseCatalogID is the one place a catalog record's ID or reference is
// parsed. A catalog that was read or written holds only rendered canonical
// IDs (checkCanonicalIDs), so a value that is set parses; one that does not,
// which only a catalog built in memory past those checks could hold, reads
// as not set, and the parse error is dropped on purpose.
func parseCatalogID(value string) (driveid.CanonicalID, bool) {
	if value == "" {
		return driveid.CanonicalID{}, false
	}

	cid, err := driveid.NewCanonicalID(value)
	if err != nil {
		return driveid.CanonicalID{}, false
	}

	return cid, true
}

func DefaultCatalog() *Catalog {
	return &Catalog{
		SchemaVersion: catalogSchemaV1,
		Accounts:      make(map[string]CatalogAccount),
		Drives:        make(map[string]CatalogDrive),
	}
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

	return decodeCatalog(path, data)
}

// decodeCatalog strictly decodes one catalog file's bytes (see "Catalog JSON
// Policy" in config.md); path only names the file in errors.
func decodeCatalog(path string, data []byte) (*Catalog, error) {
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
	if err := catalog.checkCanonicalIDs(path); err != nil {
		return nil, err
	}

	return &catalog, nil
}

// catalogRepairHint is what a refused catalog tells the user to do. Every
// command reads the catalog, so the way out is the file itself: correct or
// remove the entry, or remove the file and sign in again, which rebuilds the
// account and its primary drive (drive add then completes any other drive
// config.toml still names).
const catalogRepairHint = "correct or remove that entry, or remove the file and run 'onedrive-go login' for each account"

// checkCanonicalIDs refuses the whole catalog when a record key, a record's
// own canonical_id, a drive's owner_account_canonical_id or an account's
// primary_drive_canonical_id is not a canonical drive ID as CanonicalID.String
// renders it, or a canonical_id differs from its key. Every lookup goes
// through String, so a spelling that parses but renders differently (an
// empty SharePoint site or library, a trailing colon) would never be found.
// Both reading and writing the catalog run it: a write that would store such
// a value fails, so a catalog on disk never holds one and no reader needs to
// guess around it.
func (c *Catalog) checkCanonicalIDs(path string) error {
	for _, key := range c.SortedAccountKeys() {
		account := c.Accounts[key]
		if err := checkCatalogRecordID(path, "accounts", key, account.CanonicalID); err != nil {
			return err
		}
		if err := checkCatalogReference(path, "accounts", key, "primary_drive_canonical_id", account.PrimaryDriveCanonical); err != nil {
			return err
		}
	}

	for _, key := range c.SortedDriveKeys() {
		drive := c.Drives[key]
		if err := checkCatalogRecordID(path, "drives", key, drive.CanonicalID); err != nil {
			return err
		}
		if err := checkCatalogReference(path, "drives", key, "owner_account_canonical_id", drive.OwnerAccountCanonical); err != nil {
			return err
		}
	}

	return nil
}

// checkCatalogRecordID refuses a record whose key is not a rendered canonical
// ID or whose canonical_id is not its key.
func checkCatalogRecordID(path, table, key, canonicalID string) error {
	if err := checkRenderedCanonicalID(key); err != nil {
		return fmt.Errorf("catalog %s: %s[%q] key %w; %s", path, table, key, err, catalogRepairHint)
	}
	if canonicalID != key {
		return fmt.Errorf("catalog %s: %s[%q] canonical_id is %q, not the record's key; %s",
			path, table, key, canonicalID, catalogRepairHint)
	}

	return nil
}

// checkCatalogReference refuses a reference to another record that is set
// but is not a rendered canonical ID. An empty reference is left to
// LoadValidatedState.
func checkCatalogReference(path, table, key, field, value string) error {
	if value == "" {
		return nil
	}
	if err := checkRenderedCanonicalID(value); err != nil {
		return fmt.Errorf("catalog %s: %s[%q] %s %q %w; %s",
			path, table, key, field, value, err, catalogRepairHint)
	}

	return nil
}

// checkRenderedCanonicalID accepts s only if it parses as a canonical drive
// ID and is exactly that ID's rendering.
func checkRenderedCanonicalID(s string) error {
	cid, err := driveid.NewCanonicalID(s)
	if err != nil {
		return fmt.Errorf("is not a drive ID (%w)", err)
	}
	if rendered := cid.String(); rendered != s {
		return fmt.Errorf("is not a drive ID as onedrive-go writes it (%q)", rendered)
	}

	return nil
}

// saveCatalogForDataDir writes the whole catalog without taking its lock: the
// caller holds it (updateCatalog) or plays another process that does (tests).
// Every other writer goes through UpdateCatalogForDataDir or AddDrive. A
// catalog the decoder would refuse is not written.
func saveCatalogForDataDir(dataDir string, catalog *Catalog) error {
	path := CatalogPathForDataDir(dataDir)
	if path == "" {
		return nil
	}

	data, err := encodeCatalog(path, catalog)
	if err != nil {
		return fmt.Errorf("writing catalog: %w", err)
	}

	if err := atomicWriteFile(path, data); err != nil {
		return fmt.Errorf("writing catalog: %w", err)
	}

	return nil
}

// encodeCatalog renders the catalog file, refusing what decodeCatalog would
// refuse (checkCanonicalIDs) so that no writer can store a catalog every
// later command fails on; path only names the file in errors.
func encodeCatalog(path string, catalog *Catalog) ([]byte, error) {
	if catalog == nil {
		catalog = DefaultCatalog()
	}

	catalog.normalize()
	if err := catalog.checkCanonicalIDs(path); err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding catalog: %w", err)
	}

	return data, nil
}

func UpdateCatalog(update func(*Catalog) error) error {
	return UpdateCatalogForDataDir(DefaultDataDir(), update)
}

// UpdateCatalogForDataDir is the catalog's read-modify-write under the
// catalog lock (see configLocker.updateCatalog). update must not take a config
// or catalog lock itself.
func UpdateCatalogForDataDir(dataDir string, update func(*Catalog) error) error {
	return defaultConfigLocker().updateCatalog(dataDir, update)
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
