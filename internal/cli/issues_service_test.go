package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubIssuesMutationStore struct {
	approveErr error
	closeErr   error
	closeCalls int
}

func (s *stubIssuesMutationStore) ApproveHeldDeletes(context.Context) error {
	return s.approveErr
}

func (s *stubIssuesMutationStore) Close(context.Context) error {
	s.closeCalls++
	return s.closeErr
}

func TestIssuesService_WriteEmptyIssues(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	svc := newIssuesService(&CLIContext{OutputWriter: &out})
	require.NoError(t, svc.writeEmptyIssues())
	assert.Equal(t, "No issues.\n", out.String())
}

func TestIssuesService_RunApproveDeletesWithStore_CloseFailureSuppressesSuccessOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	svc := newIssuesService(&CLIContext{OutputWriter: &out})
	closeErr := errors.New("db close failed")
	store := &stubIssuesMutationStore{closeErr: closeErr}

	err := svc.runApproveDeletesWithStore(t.Context(), store)
	require.Error(t, err)
	require.ErrorIs(t, err, closeErr)
	assert.Contains(t, err.Error(), "close sync store")
	assert.Empty(t, out.String())
	assert.Equal(t, 1, store.closeCalls)
}

func TestIssuesService_RunApproveDeletesWithStore_JoinsApproveAndCloseErrors(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	svc := newIssuesService(&CLIContext{OutputWriter: &out})
	approveErr := errors.New("approve failed")
	closeErr := errors.New("db close failed")
	store := &stubIssuesMutationStore{
		approveErr: approveErr,
		closeErr:   closeErr,
	}

	err := svc.runApproveDeletesWithStore(t.Context(), store)
	require.Error(t, err)
	require.ErrorIs(t, err, approveErr)
	require.ErrorIs(t, err, closeErr)
	assert.Contains(t, err.Error(), "approve held deletes")
	assert.Contains(t, err.Error(), "close sync store")
	assert.Empty(t, out.String())
	assert.Equal(t, 1, store.closeCalls)
}
