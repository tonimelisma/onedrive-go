package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestValidateDrives_NoDrives_Valid(t *testing.T) {
	cfg := DefaultConfig()
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_ValidDrive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_MultipleDrives_Valid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_MissingSyncDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir is required")
}

func TestValidateDrives_InvalidPollInterval(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{
		SyncDir:      "~/OneDrive",
		PollInterval: "1m", // too short (min 5m)
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval")
}

func TestValidateDrives_ValidPollInterval(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{
		SyncDir:      "~/OneDrive",
		PollInterval: "10m",
	}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_DuplicateSyncDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/OneDrive"}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same sync_dir")
}

func TestValidateDrives_DuplicateSyncDirTildeExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: filepath.Join(home, "OneDrive")}

	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same sync_dir")
}

func TestValidateDrives_SharePointDrive_Valid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")] = Drive{
		SyncDir: "~/Marketing",
	}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_InvalidCanonicalID_RejectedAtParseTime(t *testing.T) {
	// Invalid canonical IDs are now rejected at parse time (decodeDriveSections),
	// not at validation time. This test verifies that the TOML load path catches them.
	// The key must contain ":" to be recognized as a drive section.
	path := writeTestConfig(t, `
["badtype:user@example.com"]
sync_dir = "~/OneDrive"
`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid canonical ID")
}
