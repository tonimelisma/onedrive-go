package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func TestDriveState_Enabled(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, "ready", driveState(d, tokenStateValid))
}

func TestDriveState_Paused(t *testing.T) {
	enabled := false
	d := &config.Drive{Enabled: &enabled}
	assert.Equal(t, "paused", driveState(d, tokenStateValid))
}

func TestDriveState_NoToken(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, "no token", driveState(d, tokenStateMissing))
}

func TestDriveState_PausedOverridesNoToken(t *testing.T) {
	// Paused takes priority over no token — the drive is intentionally paused.
	enabled := false
	d := &config.Drive{Enabled: &enabled}
	assert.Equal(t, "paused", driveState(d, tokenStateMissing))
}

func TestGroupDrivesByAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[string]config.Drive{
			"personal:alice@example.com":   {},
			"business:alice@example.com":   {},
			"personal:bob@example.com":     {},
			"business:charlie@example.com": {},
		},
	}

	grouped, order := groupDrivesByAccount(cfg)

	// Order should be sorted alphabetically.
	assert.Len(t, order, 3)
	assert.Equal(t, "alice@example.com", order[0])
	assert.Equal(t, "bob@example.com", order[1])
	assert.Equal(t, "charlie@example.com", order[2])

	// alice has 2 drives.
	assert.Len(t, grouped["alice@example.com"], 2)
	assert.Len(t, grouped["bob@example.com"], 1)
	assert.Len(t, grouped["charlie@example.com"], 1)
}

func TestGroupDrivesByAccount_WithSharePoint(t *testing.T) {
	// With the fixed email extraction, SharePoint drives are now grouped
	// under the same account as personal/business drives.
	cfg := &config.Config{
		Drives: map[string]config.Drive{
			"business:alice@contoso.com":                    {},
			"sharepoint:alice@contoso.com:marketing:Docs":   {},
			"sharepoint:alice@contoso.com:engineering:Wiki": {},
		},
	}

	grouped, order := groupDrivesByAccount(cfg)

	// All three drives belong to alice@contoso.com.
	assert.Len(t, order, 1)
	assert.Equal(t, "alice@contoso.com", order[0])
	assert.Len(t, grouped["alice@contoso.com"], 3)
}

func TestGroupDrivesByAccount_Empty(t *testing.T) {
	cfg := &config.Config{
		Drives: map[string]config.Drive{},
	}

	grouped, order := groupDrivesByAccount(cfg)

	assert.Empty(t, order)
	assert.Empty(t, grouped)
}

func TestNewStatusCmd_Structure(t *testing.T) {
	cmd := newStatusCmd()
	assert.Equal(t, "status", cmd.Name())
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.RunE)
}
