package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReadEnvOverrides_AllSet(t *testing.T) {
	t.Setenv("ONEDRIVE_GO_CONFIG", "/custom/config.toml")
	t.Setenv("ONEDRIVE_GO_DRIVE", "work")

	overrides := ReadEnvOverrides(testLogger(t))
	assert.Equal(t, "/custom/config.toml", overrides.ConfigPath)
	assert.Equal(t, "work", overrides.Drive)
}

func TestReadEnvOverrides_NoneSet(t *testing.T) {
	t.Setenv("ONEDRIVE_GO_CONFIG", "")
	t.Setenv("ONEDRIVE_GO_DRIVE", "")

	overrides := ReadEnvOverrides(testLogger(t))
	assert.Empty(t, overrides.ConfigPath)
	assert.Empty(t, overrides.Drive)
}

func TestReadEnvOverrides_PartiallySet(t *testing.T) {
	t.Setenv("ONEDRIVE_GO_CONFIG", "")
	t.Setenv("ONEDRIVE_GO_DRIVE", "personal")

	overrides := ReadEnvOverrides(testLogger(t))
	assert.Empty(t, overrides.ConfigPath)
	assert.Equal(t, "personal", overrides.Drive)
}

func TestEnvVarConstants(t *testing.T) {
	assert.Equal(t, "ONEDRIVE_GO_CONFIG", EnvConfig)
	assert.Equal(t, "ONEDRIVE_GO_DRIVE", EnvDrive)
}
