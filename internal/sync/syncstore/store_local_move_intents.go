package syncstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// LocalMoveIntent is a local file move between the two renames it can take:
// recorded before the rename, cleared once the old name is gone. One that
// outlives its move names an old name the move could not remove
// (synctree.ErrSourceNotRemoved), which the planner does not see
// (sqlHideLocalMoveLeftovers) and the engine's next start settles. Both
// paths are stored paths.
type LocalMoveIntent struct {
	OldPath string
	NewPath string
}

// RecordLocalMoveIntent durably records that the file at oldPath is about to
// be moved to newPath, before the rename.
func (s *SyncStore) RecordLocalMoveIntent(ctx context.Context, oldPath, newPath string) error {
	if oldPath == "" || newPath == "" {
		return errors.New("sync: recording a local move intent: missing path")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO local_move_intents (old_path, new_path, intended_at)
		VALUES (?, ?, ?)
		ON CONFLICT(old_path) DO UPDATE SET new_path = excluded.new_path, intended_at = excluded.intended_at`,
		oldPath, newPath, syncdomain.StoredTimeNano(s.nowFunc()),
	); err != nil {
		return fmt.Errorf("sync: recording the move of %s to %s: %w", oldPath, newPath, err)
	}

	return nil
}

// ClearLocalMoveIntent forgets the move from oldPath: its old name is gone,
// or what is there now is the user's to sync.
func (s *SyncStore) ClearLocalMoveIntent(ctx context.Context, oldPath string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM local_move_intents WHERE old_path = ?`, oldPath); err != nil {
		return fmt.Errorf("sync: clearing the move of %s: %w", oldPath, err)
	}

	return nil
}

// ListLocalMoveIntents returns every recorded local move, by old path.
func (s *SyncStore) ListLocalMoveIntents(ctx context.Context) (intents []LocalMoveIntent, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT old_path, new_path FROM local_move_intents ORDER BY old_path`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing local move intents: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("sync: closing local move intents: %w", closeErr)
		}
	}()
	for rows.Next() {
		var intent LocalMoveIntent
		if scanErr := rows.Scan(&intent.OldPath, &intent.NewPath); scanErr != nil {
			return nil, fmt.Errorf("sync: reading a local move intent: %w", scanErr)
		}
		intents = append(intents, intent)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, fmt.Errorf("sync: iterating local move intents: %w", iterErr)
	}

	return intents, nil
}
