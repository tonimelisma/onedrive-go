package syncstore

import (
	"database/sql"
	"errors"
	"fmt"
)

type txRollbacker interface {
	Rollback() error
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
