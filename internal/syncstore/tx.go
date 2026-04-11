package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

type txRollbacker interface {
	Rollback() error
}

type sqlTxRunner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	PrepareContext(context.Context, string) (*sql.Stmt, error)
}

type sqlCommitTx interface {
	sqlTxRunner
	Commit() error
}

type perfTx struct {
	*sql.Tx
	collector *perf.Collector
	startedAt time.Time
}

func beginPerfTx(ctx context.Context, db *sql.DB) (*perfTx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	return &perfTx{
		Tx:        tx,
		collector: perf.FromContext(ctx),
		startedAt: time.Now(),
	}, nil
}

func (tx *perfTx) Commit() error {
	if tx == nil || tx.Tx == nil {
		return nil
	}

	err := tx.Tx.Commit()
	recordPerfTx(tx)
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
	recordPerfTx(tx)
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

func recordPerfTx(tx *perfTx) {
	if tx == nil || tx.collector == nil {
		return
	}

	tx.collector.RecordDBTransaction(time.Since(tx.startedAt))
}
