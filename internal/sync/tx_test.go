package sync

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRollbacker struct {
	err       error
	callCount int
}

func (s *stubRollbacker) Rollback() error {
	s.callCount++
	return s.err
}

func TestFinalizeTxRollback_IgnoresTxDone(t *testing.T) {
	t.Parallel()

	tx := &stubRollbacker{err: sql.ErrTxDone}
	err := error(nil)

	err = finalizeTxRollback(err, tx, "rollback test tx")

	require.NoError(t, err)
	assert.Equal(t, 1, tx.callCount)
}

func TestFinalizeTxRollback_SurfacesRollbackFailure(t *testing.T) {
	t.Parallel()

	rollbackErr := errors.New("disk I/O failed")
	tx := &stubRollbacker{err: rollbackErr}
	err := error(nil)

	err = finalizeTxRollback(err, tx, "rollback test tx")

	require.Error(t, err)
	require.ErrorIs(t, err, rollbackErr)
	assert.Contains(t, err.Error(), "rollback test tx")
	assert.Equal(t, 1, tx.callCount)
}

func TestFinalizeTxRollback_JoinsExistingError(t *testing.T) {
	t.Parallel()

	originalErr := errors.New("primary failure")
	rollbackErr := errors.New("rollback failure")
	tx := &stubRollbacker{err: rollbackErr}
	err := originalErr

	err = finalizeTxRollback(err, tx, "rollback test tx")

	require.Error(t, err)
	require.ErrorIs(t, err, originalErr)
	require.ErrorIs(t, err, rollbackErr)
	assert.Equal(t, 1, tx.callCount)
}
