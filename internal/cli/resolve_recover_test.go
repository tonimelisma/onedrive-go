package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRecoverCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newRecoverCmd()
	assert.Equal(t, "recover", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("yes"))
	assert.NotNil(t, cmd.RunE)
}

func TestConfirmRecoverIntent_NonInteractiveRequiresYes(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{}
	cmd := newRecoverCmd()
	cmd.SetIn(&bytes.Buffer{})
	err := confirmRecoverIntent(cmd, cc, recoverPreflight{HasStateDB: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires confirmation")
}
