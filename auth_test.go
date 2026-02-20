package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthTokenPath_MissingDrive(t *testing.T) {
	old := flagDrive
	t.Cleanup(func() { flagDrive = old })

	flagDrive = ""

	_, err := authTokenPath()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

func TestAuthTokenPath_ValidDrive(t *testing.T) {
	old := flagDrive
	t.Cleanup(func() { flagDrive = old })

	flagDrive = "personal:user@example.com"

	path, err := authTokenPath()
	require.NoError(t, err)
	assert.NotEmpty(t, path)
	// The token path should contain the drive type and email components.
	assert.Contains(t, path, "personal")
}
