package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewResolveCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newResolveCmd()
	assert.Equal(t, "resolve", cmd.Use)

	deletesCmd, _, err := cmd.Find([]string{"deletes"})
	require.NoError(t, err)
	assert.Equal(t, "deletes", deletesCmd.Use)
	assert.NotNil(t, deletesCmd.RunE)

	for _, subcommand := range []string{"local", "remote", "both"} {
		sub, _, findErr := cmd.Find([]string{subcommand})
		require.NoError(t, findErr)
		assert.Equal(t, subcommand, sub.Name())
		assert.NotNil(t, sub.Flags().Lookup("all"))
	}
}

func TestResolveActionCmd_RequiresTargetWithoutAll(t *testing.T) {
	t.Parallel()

	cmd := newResolveActionCmd("local", resolutionKeepLocal)
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, &CLIContext{}))
	cmd.SetArgs(nil)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "specify a conflict path or ID")
}

func TestResolveActionCmd_RejectsTargetWithAll(t *testing.T) {
	t.Parallel()

	cmd := newResolveActionCmd("remote", resolutionKeepRemote)
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, &CLIContext{}))
	cmd.SetArgs([]string{"--all", "/conflict.txt"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all and a specific conflict argument are mutually exclusive")
}

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
