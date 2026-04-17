package sync

import (
	"context"
	"fmt"
	"sort"
)

const (
	sqlDeleteLocalState = `DELETE FROM local_state`
	sqlInsertLocalState = `INSERT INTO local_state
		(path, item_type, hash, size, mtime, content_identity, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	sqlListLocalState = `SELECT
		path, item_type, hash, size, mtime, content_identity, observed_at
		FROM local_state
		ORDER BY path`
)

// ReplaceLocalState atomically replaces the durable local snapshot with the
// current admissible observation result for the drive.
func (m *SyncStore) ReplaceLocalState(
	ctx context.Context,
	rows []LocalStateRow,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning local_state transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state transaction")
	}()

	if _, err := tx.ExecContext(ctx, sqlDeleteLocalState); err != nil {
		return fmt.Errorf("sync: deleting local_state rows: %w", err)
	}

	for i := range rows {
		row := rows[i]
		if _, err := tx.ExecContext(ctx, sqlInsertLocalState,
			row.Path,
			row.ItemType,
			nullString(row.Hash),
			nullKnownInt64(row.Size, true),
			nullOptionalInt64(row.Mtime),
			nullString(row.ContentIdentity),
			row.ObservedAt,
		); err != nil {
			return fmt.Errorf("sync: inserting local_state row for %s: %w", row.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state transaction: %w", err)
	}

	return nil
}

// ListLocalState returns the durable local snapshot rows in path order.
func (m *SyncStore) ListLocalState(ctx context.Context) ([]LocalStateRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListLocalState)
	if err != nil {
		return nil, fmt.Errorf("sync: querying local_state: %w", err)
	}
	defer rows.Close()

	var result []LocalStateRow
	for rows.Next() {
		var row LocalStateRow
		if err := rows.Scan(
			&row.Path,
			&row.ItemType,
			&row.Hash,
			&row.Size,
			&row.Mtime,
			&row.ContentIdentity,
			&row.ObservedAt,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning local_state row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating local_state rows: %w", err)
	}

	return result, nil
}

func buildLocalStateRows(
	bl *Baseline,
	result ScanResult,
	observedAt int64,
) []LocalStateRow {
	current := make(map[string]LocalStateRow)
	if bl != nil {
		bl.ForEachPath(func(path string, entry *BaselineEntry) {
			if entry == nil {
				return
			}

			row := LocalStateRow{
				Path:       path,
				ItemType:   entry.ItemType,
				Hash:       entry.LocalHash,
				Mtime:      entry.LocalMtime,
				ObservedAt: observedAt,
			}
			if entry.LocalSizeKnown {
				row.Size = entry.LocalSize
			}
			if entry.ItemType == ItemTypeFile {
				row.ContentIdentity = entry.LocalHash
			}

			current[path] = row
		})
	}

	for i := range result.Events {
		event := result.Events[i]
		if event.Source != SourceLocal {
			continue
		}

		switch event.Type {
		case ChangeDelete:
			delete(current, event.Path)
		case ChangeMove:
			if event.OldPath != "" {
				delete(current, event.OldPath)
			}
			current[event.Path] = localStateRowFromEvent(event, observedAt)
		case ChangeCreate, ChangeModify:
			current[event.Path] = localStateRowFromEvent(event, observedAt)
		}
	}

	paths := make([]string, 0, len(current))
	for path := range current {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rows := make([]LocalStateRow, 0, len(paths))
	for _, path := range paths {
		rows = append(rows, current[path])
	}

	return rows
}

func localStateRowFromEvent(event ChangeEvent, observedAt int64) LocalStateRow {
	row := LocalStateRow{
		Path:       event.Path,
		ItemType:   event.ItemType,
		Hash:       event.Hash,
		Size:       event.Size,
		Mtime:      event.Mtime,
		ObservedAt: observedAt,
	}
	if event.ItemType == ItemTypeFile {
		row.ContentIdentity = event.Hash
	}

	return row
}
