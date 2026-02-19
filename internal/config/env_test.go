package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReadEnvOverrides_AllSet(t *testing.T) {
	t.Setenv("ONEDRIVE_GO_CONFIG", "/custom/config.toml")
	t.Setenv("ONEDRIVE_GO_PROFILE", "work")
	t.Setenv("ONEDRIVE_GO_SYNC_DIR", "/custom/sync")

	overrides := ReadEnvOverrides()
	assert.Equal(t, "/custom/config.toml", overrides.ConfigPath)
	assert.Equal(t, "work", overrides.Profile)
	assert.Equal(t, "/custom/sync", overrides.SyncDir)
}

func TestReadEnvOverrides_NoneSet(t *testing.T) {
	// t.Setenv with empty string to ensure they're unset in test scope
	t.Setenv("ONEDRIVE_GO_CONFIG", "")
	t.Setenv("ONEDRIVE_GO_PROFILE", "")
	t.Setenv("ONEDRIVE_GO_SYNC_DIR", "")

	overrides := ReadEnvOverrides()
	assert.Empty(t, overrides.ConfigPath)
	assert.Empty(t, overrides.Profile)
	assert.Empty(t, overrides.SyncDir)
}

func TestReadEnvOverrides_PartiallySet(t *testing.T) {
	t.Setenv("ONEDRIVE_GO_CONFIG", "")
	t.Setenv("ONEDRIVE_GO_PROFILE", "personal")
	t.Setenv("ONEDRIVE_GO_SYNC_DIR", "")

	overrides := ReadEnvOverrides()
	assert.Empty(t, overrides.ConfigPath)
	assert.Equal(t, "personal", overrides.Profile)
	assert.Empty(t, overrides.SyncDir)
}

func TestEnvVarConstants(t *testing.T) {
	assert.Equal(t, "ONEDRIVE_GO_CONFIG", EnvConfig)
	assert.Equal(t, "ONEDRIVE_GO_PROFILE", EnvProfile)
	assert.Equal(t, "ONEDRIVE_GO_SYNC_DIR", EnvSyncDir)
}
