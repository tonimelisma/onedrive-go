package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

const driveAddTestSharedName = "Shared Folder"

// Validates: R-3.3.5
func TestDriveAdd_RerunCompletesASectionWithoutItsCatalogRecord(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("business:add@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_add@example.com.json")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := filepath.Join(t.TempDir(), "Business")
	// A section with no catalog record: written by hand, or by an earlier
	// version of the client that wrote the config before the catalog.
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))

	cc, stdout, _ := newJSONCommandContext(cfgPath)
	require.NoError(t, runDriveAddWithContext(t.Context(), cc, []string{cid.String()}))

	// Decoded by field name, so the test also pins the JSON names.
	var got map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	added, ok := got["added"].(bool)
	require.True(t, ok, "added is a JSON boolean")
	assert.False(t, added)
	completed, ok := got["completed"].(bool)
	require.True(t, ok, "completed is a JSON boolean")
	assert.True(t, completed)
	assert.Equal(t, syncDir, got["sync_dir"])

	drive, found := loadCatalogDrive(t, cid)
	require.True(t, found, "the rerun must record the drive in the catalog")
	assert.Equal(t, cid.String(), drive.OwnerAccountCanonical)

	info, err := os.Stat(syncDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// Validates: R-3.3.5
func TestDriveAdd_SharedRerunWritesTheMissingDisplayName(t *testing.T) {
	setTestDriveHome(t)

	ownerCID := driveid.MustCanonicalID("personal:owner@example.com")
	sharedCID := driveid.MustCanonicalID("shared:owner@example.com:driveX:itemY")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_owner@example.com.json")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// A shared section without display_name: written by hand, or by an
	// earlier version of the client that wrote the name in a second step.
	require.NoError(t, config.AppendDriveSection(cfgPath, sharedCID, "~/OneDrive-Shared/"+driveAddTestSharedName))
	seedCatalogDrive(t, sharedCID, func(drive *config.CatalogDrive) {
		drive.OwnerAccountCanonical = ownerCID.String()
		drive.DisplayName = driveAddTestSharedName
	})

	var out bytes.Buffer
	result, err := addSharedDrive(
		t.Context(),
		cfgPath,
		&out,
		sharedCID,
		driveAddTestSharedName,
		testDriveLogger(t),
		driveops.NewSessionRuntime(nil, "test-agent", testDriveLogger(t)),
	)
	require.NoError(t, err)
	assert.False(t, result.Added)
	assert.Contains(t, out.String(), "Completed")

	cfg, err := config.Load(cfgPath, testDriveLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveAddTestSharedName, cfg.Drives[sharedCID].DisplayName)
}
