package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/perf"
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

var errTestTxRolledBack = errors.New("test transaction rolled back")

// runTestPerfTx runs one transaction the way every store function does:
// beginPerfTx, a statement, then Commit, or an error that the deferred
// finalizeTxRollback rolls back. The transaction's start is moved startedAgo
// into the past, so its recorded duration is at least that long.
func runTestPerfTx(ctx context.Context, db *sql.DB, commit bool, startedAgo time.Duration) (err error) {
	tx, err := beginPerfTx(ctx, db)
	if err != nil {
		return err
	}
	tx.startedAt = tx.startedAt.Add(-startedAgo)
	defer func() {
		err = finalizeTxRollback(err, tx, "rollback test tx")
	}()

	if _, err = tx.ExecContext(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("exec test statement: %w", err)
	}
	if !commit {
		return errTestTxRolledBack
	}

	return tx.Commit()
}

// Validates: R-6.6.15
func TestPerfTx_RecordsEachEndedTransactionOnce(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	collector := perf.NewCollector(nil)
	ctx := perf.WithCollector(t.Context(), collector)

	require.NoError(t, runTestPerfTx(ctx, store.db, true, 0), "committed transaction")
	require.ErrorIs(t, runTestPerfTx(ctx, store.db, false, 0), errTestTxRolledBack, "rolled-back transaction")

	assert.Equal(t, 2, collector.Snapshot().DBTransactionCount,
		"one committed and one rolled-back transaction, each recorded once")
}

// Validates: R-6.6.15
func TestPerfTx_RecordsTheTimeFromBeginToCommitOnce(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	collector := perf.NewCollector(nil)
	ctx := perf.WithCollector(t.Context(), collector)

	callStarted := time.Now()
	require.NoError(t, runTestPerfTx(ctx, store.db, true, time.Second))
	upperBound := time.Second + time.Since(callStarted)

	snapshot := collector.Snapshot()
	assert.Equal(t, 1, snapshot.DBTransactionCount)
	assert.GreaterOrEqual(t, snapshot.DBTransactionTimeMS, time.Second.Milliseconds(),
		"the time from begin to commit")
	assert.LessOrEqual(t, snapshot.DBTransactionTimeMS, upperBound.Milliseconds(),
		"that time, recorded once")
}

// A commit refused because the transaction's context ended leaves the
// rollback to database/sql's context watcher, so the deferred rollback either
// returns sql.ErrTxDone or rolls back itself. Either way the transaction is
// recorded once.
//
// Validates: R-6.6.15
func TestPerfTx_RecordsAFailedCommitOnce(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	collector := perf.NewCollector(nil)
	ctx, cancel := context.WithCancel(perf.WithCollector(t.Context(), collector))

	tx, err := beginPerfTx(ctx, store.db)
	require.NoError(t, err)
	cancel()
	commitErr := tx.Commit()
	require.Error(t, commitErr, "a commit after the context ended")
	require.NoError(t, finalizeTxRollback(nil, tx, "rollback test tx"),
		"the deferred rollback ends nothing more or rolls back")

	assert.Equal(t, 1, collector.Snapshot().DBTransactionCount)
}
