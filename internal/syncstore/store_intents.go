package syncstore

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// DurableIntentCounts summarizes queued user decisions for control-socket
// status. Counts are read-only diagnostics; mutation still goes through the
// typed store methods.
type DurableIntentCounts struct {
	PendingHeldDeleteApprovals int
	PendingConflictRequests    int
	ResolvingConflictRequests  int
	FailedConflictRequests     int
}

func (m *SyncStore) CountDurableIntents(ctx context.Context) (DurableIntentCounts, error) {
	var counts DurableIntentCounts

	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM held_deletes WHERE state = ?`,
		synctypes.HeldDeleteStateApproved,
	).Scan(&counts.PendingHeldDeleteApprovals); err != nil {
		return DurableIntentCounts{}, fmt.Errorf("sync: count approved held deletes: %w", err)
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM conflicts
		WHERE state IN (?, ?, ?)
		GROUP BY state`,
		synctypes.ConflictStateResolutionRequested,
		synctypes.ConflictStateResolving,
		synctypes.ConflictStateResolveFailed,
	)
	if err != nil {
		return DurableIntentCounts{}, fmt.Errorf("sync: count conflict requests: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			state string
			count int
		)
		if err := rows.Scan(&state, &count); err != nil {
			return DurableIntentCounts{}, fmt.Errorf("sync: scan conflict request count: %w", err)
		}
		switch state {
		case synctypes.ConflictStateResolutionRequested:
			counts.PendingConflictRequests = count
		case synctypes.ConflictStateResolving:
			counts.ResolvingConflictRequests = count
		case synctypes.ConflictStateResolveFailed:
			counts.FailedConflictRequests = count
		}
	}
	if err := rows.Err(); err != nil {
		return DurableIntentCounts{}, fmt.Errorf("sync: iterate conflict request counts: %w", err)
	}

	return counts, nil
}
