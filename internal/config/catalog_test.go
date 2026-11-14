package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

func TestLoadCatalog_RejectsMissingSchemaVersion(t *testing.T) {
	setConfigTestHome(t)

	path := CatalogPathForDataDir(DefaultDataDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"accounts":{},"drives":{}}`), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 0")
}

func TestLoadCatalog_RejectsUnsupportedSchemaVersion(t *testing.T) {
	setConfigTestHome(t)

	path := CatalogPathForDataDir(DefaultDataDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":2,"accounts":{},"drives":{}}`), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 2")
}

func TestLoadCatalog_RejectsUnknownFields(t *testing.T) {
	setConfigTestHome(t)

	path := CatalogPathForDataDir(DefaultDataDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":1,"accounts":{},"drives":{},"unexpected":true}`), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestSaveCatalog_WritesSchemaVersion(t *testing.T) {
	dataDir := t.TempDir()

	require.NoError(t, saveCatalogForDataDir(dataDir, &Catalog{}))
	loaded, err := LoadCatalogForDataDir(dataDir)
	require.NoError(t, err)
	assert.Equal(t, catalogSchemaV1, loaded.SchemaVersion)
}

func TestLoadCatalog_RejectsTrailingJSON(t *testing.T) {
	setConfigTestHome(t)

	path := CatalogPathForDataDir(DefaultDataDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{\"schema_version\":1,\"accounts\":{},\"drives\":{}}\n{}"), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing data after top-level object")
}

const (
	catalogTestAccount = "personal:a@example.com"
	catalogTestLibrary = "sharepoint:a@example.com:site:Docs"
)

// wellFormedCatalogForTest is a catalog every reader accepts: one account,
// its primary drive and one more drive it owns.
func wellFormedCatalogForTest() *Catalog {
	catalog := DefaultCatalog()
	catalog.UpsertAccount(&CatalogAccount{
		CanonicalID:           catalogTestAccount,
		Email:                 "a@example.com",
		DriveType:             driveTypePersonal,
		PrimaryDriveCanonical: catalogTestAccount,
	})
	catalog.UpsertDrive(&CatalogDrive{
		CanonicalID:           catalogTestAccount,
		OwnerAccountCanonical: catalogTestAccount,
		DriveType:             driveTypePersonal,
	})
	catalog.UpsertDrive(&CatalogDrive{
		CanonicalID:           catalogTestLibrary,
		OwnerAccountCanonical: catalogTestAccount,
		DriveType:             driveTypeSharePoint,
	})

	return catalog
}

// malformedCatalogCase spoils one ID field of a well-formed catalog.
type malformedCatalogCase struct {
	name      string
	spoil     func(catalog *Catalog)
	wantKey   string
	wantField string
}

// addDriveRecord adds a drive record under key, owned by the test account.
func addDriveRecord(catalog *Catalog, key string) {
	catalog.Drives[key] = CatalogDrive{CanonicalID: key, OwnerAccountCanonical: catalogTestAccount}
}

func setAccountPrimary(catalog *Catalog, primary string) {
	account := catalog.Accounts[catalogTestAccount]
	account.PrimaryDriveCanonical = primary
	catalog.Accounts[catalogTestAccount] = account
}

func setLibraryOwner(catalog *Catalog, owner string) {
	drive := catalog.Drives[catalogTestLibrary]
	drive.OwnerAccountCanonical = owner
	catalog.Drives[catalogTestLibrary] = drive
}

// malformedCatalogCases is one case per ID field the catalog check reads, and
// one per spelling a SharePoint ID parses from but does not render as (an
// empty site or library, a trailing colon): every lookup goes through
// CanonicalID.String, so such a key would never be found.
func malformedCatalogCases() []malformedCatalogCase {
	return []malformedCatalogCase{
		{
			name: "an account key",
			spoil: func(catalog *Catalog) {
				catalog.Accounts["a@example.com"] = CatalogAccount{CanonicalID: "a@example.com", Email: "a@example.com"}
			},
			wantKey:   "a@example.com",
			wantField: "key",
		},
		{
			name: "an account's canonical_id",
			spoil: func(catalog *Catalog) {
				account := catalog.Accounts[catalogTestAccount]
				account.CanonicalID = "personal"
				catalog.Accounts[catalogTestAccount] = account
			},
			wantKey:   catalogTestAccount,
			wantField: "canonical_id",
		},
		{
			name:      "an account's primary drive",
			spoil:     func(catalog *Catalog) { setAccountPrimary(catalog, "drive-123") },
			wantKey:   catalogTestAccount,
			wantField: "primary_drive_canonical_id",
		},
		{
			name:      "a drive key",
			spoil:     func(catalog *Catalog) { addDriveRecord(catalog, "drive-123") },
			wantKey:   "drive-123",
			wantField: "key",
		},
		{
			name: "a drive's canonical_id",
			spoil: func(catalog *Catalog) {
				drive := catalog.Drives[catalogTestLibrary]
				drive.CanonicalID = "shared:a@example.com:only-three"
				catalog.Drives[catalogTestLibrary] = drive
			},
			wantKey:   catalogTestLibrary,
			wantField: "canonical_id",
		},
		{
			name:      "a drive's owner",
			spoil:     func(catalog *Catalog) { setLibraryOwner(catalog, "a@example.com") },
			wantKey:   catalogTestLibrary,
			wantField: "owner_account_canonical_id",
		},
		{
			name:      "a drive key with a trailing colon",
			spoil:     func(catalog *Catalog) { addDriveRecord(catalog, "sharepoint:a@example.com:") },
			wantKey:   "sharepoint:a@example.com:",
			wantField: "key",
		},
		{
			name:      "a drive key with an empty site",
			spoil:     func(catalog *Catalog) { addDriveRecord(catalog, "sharepoint:a@example.com::Docs") },
			wantKey:   "sharepoint:a@example.com::Docs",
			wantField: "key",
		},
		{
			name:      "a drive key with an empty library",
			spoil:     func(catalog *Catalog) { addDriveRecord(catalog, "sharepoint:a@example.com:site:") },
			wantKey:   "sharepoint:a@example.com:site:",
			wantField: "key",
		},
		{
			name:      "an owner with a trailing colon",
			spoil:     func(catalog *Catalog) { setLibraryOwner(catalog, "sharepoint:a@example.com:") },
			wantKey:   catalogTestLibrary,
			wantField: "owner_account_canonical_id",
		},
		{
			name:      "a primary drive with an empty site",
			spoil:     func(catalog *Catalog) { setAccountPrimary(catalog, "sharepoint:a@example.com::Docs") },
			wantKey:   catalogTestAccount,
			wantField: "primary_drive_canonical_id",
		},
	}
}

func assertCatalogIDRefusal(t *testing.T, err error, path string, tc *malformedCatalogCase) {
	t.Helper()

	require.Error(t, err)
	assert.Contains(t, err.Error(), path, "the error names the file")
	assert.Contains(t, err.Error(), `"`+tc.wantKey+`"`, "the error names the record")
	assert.Contains(t, err.Error(), tc.wantField, "the error names the field")
	assert.Contains(t, err.Error(), "onedrive-go login", "the error says how to recover")
}

// Validates: R-4.1.4, R-4.1.5
func TestDecodeCatalog_RefusesAMalformedCanonicalID(t *testing.T) {
	for _, tc := range malformedCatalogCases() {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			catalog := wellFormedCatalogForTest()
			tc.spoil(catalog)
			data, err := json.Marshal(catalog)
			require.NoError(t, err)
			path := CatalogPathForDataDir(dataDir)
			require.NoError(t, os.WriteFile(path, data, 0o600))

			_, err = LoadCatalogForDataDir(dataDir)
			assertCatalogIDRefusal(t, err, path, &tc)
		})
	}
}

// Validates: R-4.1.4, R-4.1.5
func TestUpdateCatalog_RefusesToStoreAMalformedCanonicalID(t *testing.T) {
	for _, tc := range malformedCatalogCases() {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			require.NoError(t, saveCatalogForDataDir(dataDir, wellFormedCatalogForTest()))
			path := CatalogPathForDataDir(dataDir)
			before, err := localpath.ReadFile(path)
			require.NoError(t, err)

			err = UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
				tc.spoil(catalog)
				return nil
			})
			assertCatalogIDRefusal(t, err, path, &tc)

			after, err := localpath.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused write leaves the file as it was")
		})
	}
}

// Validates: R-4.1.4
func TestDecodeCatalog_AcceptsWhatTheWritersWrite(t *testing.T) {
	dataDir := t.TempDir()
	require.NoError(t, saveCatalogForDataDir(dataDir, wellFormedCatalogForTest()))

	loaded, err := LoadCatalogForDataDir(dataDir)
	require.NoError(t, err)
	assert.Len(t, loaded.Drives, 2)
}
