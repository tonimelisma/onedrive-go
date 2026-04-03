package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssuesService_WriteEmptyIssues(t *testing.T) {
	t.Parallel()

	t.Run("current", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		svc := newIssuesService(&CLIContext{OutputWriter: &out})
		require.NoError(t, svc.writeEmptyIssues(false))
		assert.Equal(t, "No issues.\n", out.String())
	})

	t.Run("history", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		svc := newIssuesService(&CLIContext{OutputWriter: &out})
		require.NoError(t, svc.writeEmptyIssues(true))
		assert.Equal(t, "No issues in history.\n", out.String())
	})
}
