package cli

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.3.7
func TestSync_RefusesRootWithoutAllowRoot(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	cc := &CLIContext{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	cc.seams.effectiveUID = func() int { return 0 }

	cmd := newSyncCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))
	cmd.SetArgs([]string{"--watch"})
	err := cmd.Execute()

	var usage *usageError
	require.ErrorAs(t, err, &usage, "a usage error: nothing ran, exit 2")
	require.ErrorIs(t, err, errSyncAsRoot)
	assert.Equal(t, exitUsage, commandExitCode(err, false))

	allowed := newSyncCmd()
	require.NoError(t, allowed.Flags().Set(allowRootFlag, "true"))
	require.NoError(t, refuseSyncAsRoot(allowed, cc))
	assert.Contains(t, logs.String(), "--allow-root was given")

	cc.seams.effectiveUID = func() int { return 1000 }
	require.NoError(t, refuseSyncAsRoot(newSyncCmd(), cc), "an ordinary user is never refused")
}
