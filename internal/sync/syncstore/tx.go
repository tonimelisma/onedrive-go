package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

type txRollbacker interface {
	Rollback() error
}

// sqlTxRunner is what a store query runs on: the store's database or one of
// its transactions.
type sqlTxRunner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	PrepareContext(context.Context, string) (*sql.Stmt, error)
}

// perfTx is a store transaction that records itself on the perf collector of
// the context it began with, once, when it ends: on Commit, whether or not the
// commit succeeded (database/sql marks the transaction done before it asks the
// driver to commit, and a commit refused for an ended context leaves the
// rollback to the context's watcher), or on a Rollback that ended it. The
// Rollback every store function defers after its Commit returns
// sql.ErrTxDone and records nothing. A perfTx belongs to the goroutine that
// began it, like the function that defers its rollback.
type perfTx struct {
	*sql.Tx
	collector *perf.Collector
	startedAt time.Time
	recorded  bool
}

func beginPerfTx(ctx context.Context, db *sql.DB) (*perfTx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	return &perfTx{
		Tx:        tx,
		collector: perf.FromContext(ctx),
		startedAt: syncdomain.WallNow(),
	}, nil
}

func (tx *perfTx) Commit() error {
	if tx == nil || tx.Tx == nil {
		return nil
	}

	err := tx.Tx.Commit()
	tx.record()
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (tx *perfTx) Rollback() error {
	if tx == nil || tx.Tx == nil {
		return nil
	}

	err := tx.Tx.Rollback()
	if !errors.Is(err, sql.ErrTxDone) {
		tx.record()
	}
	if err != nil {
		return fmt.Errorf("rollback transaction: %w", err)
	}

	return nil
}

// finalizeTxRollback reports rollback failures on the function's returned
// error while still allowing successful commits to suppress sql.ErrTxDone.
func finalizeTxRollback(err error, tx txRollbacker, action string) error {
	if tx == nil {
		return err
	}

	rollbackErr := tx.Rollback()
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return err
	}

	rollbackErr = fmt.Errorf("%s: %w", action, rollbackErr)
	if err == nil {
		return rollbackErr
	}

	return errors.Join(err, rollbackErr)
}

// record records the transaction's time from begin to now on its collector,
// the first time it is called.
func (tx *perfTx) record() {
	if tx.recorded {
		return
	}
	tx.recorded = true
	if tx.collector == nil {
		return
	}

	tx.collector.RecordDBTransaction(syncdomain.WallSince(tx.startedAt))
}
