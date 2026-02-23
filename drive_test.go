package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDriveCmd_Structure(t *testing.T) {
	cmd := newDriveCmd()
	assert.Equal(t, "drive", cmd.Name())

	subNames := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		subNames = append(subNames, sub.Name())
	}

	assert.Contains(t, subNames, "add")
	assert.Contains(t, subNames, "remove")
}

func TestNewDriveRemoveCmd_PurgeFlag(t *testing.T) {
	cmd := newDriveRemoveCmd()

	purgeFlag := cmd.Flags().Lookup("purge")
	require.NotNil(t, purgeFlag, "remove command should have --purge flag")
	assert.Equal(t, "false", purgeFlag.DefValue)
}

func TestNewDriveAddCmd_HasRunE(t *testing.T) {
	cmd := newDriveAddCmd()
	assert.NotNil(t, cmd.RunE)
}
